package reqproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeUpstream serves a fixed exit identity for ordinary GET and tunneled GET.
// Xray's HTTP outbound uses CONNECT even when the target is cleartext HTTP.
func fakeUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			_, _ = io.WriteString(w, body)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		request.Body.Close()
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	}))
	return srv
}

// TestKernelSlotLifecycle P2 冒烟：开槽位 → 端口可代理 → 换指向 → 端口不变。
// 用两个本地 http 上游模拟两个"节点"（经 http 代理 outbound 指向它们）。
func TestKernelSlotLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("冒烟测试需要真实内核")
	}
	upA := fakeUpstream(t, "node-A")
	defer upA.Close()
	upB := fakeUpstream(t, "node-B")
	defer upB.Close()

	k, err := NewKernel()
	if err != nil {
		t.Fatalf("启动内核: %v", err)
	}
	defer k.Close()

	// 两个"http 节点"指向本地测试服务器
	nodeA := httpNodeSpec("nodeA", strings.TrimPrefix(upA.URL, "http://"))
	nodeB := httpNodeSpec("nodeB", strings.TrimPrefix(upB.URL, "http://"))

	// 开槽位指向 A
	slotID := "slot_test1"
	port := 32180
	if err := k.AddSlot(slotID, port, nodeA); err != nil {
		t.Fatalf("AddSlot: %v", err)
	}

	// 经槽位端口代理请求 → 应打到 A
	got := proxyGet(t, port, upA.URL)
	if got != "node-A" {
		t.Fatalf("槽位出口 = %q, want node-A", got)
	}

	// 换指向 B：端口不变，出口变
	if err := k.RetargetSlot(slotID, nodeB); err != nil {
		t.Fatalf("RetargetSlot: %v", err)
	}
	// Keep the target unchanged: the response must follow the selected exit,
	// so a direct request or an unchanged outbound cannot satisfy this check.
	got = proxyGet(t, port, upA.URL)
	if got != "node-B" {
		t.Fatalf("换指向后出口 = %q, want node-B（端口 %d 应不变）", got, port)
	}

	// 删槽位：端口不再监听
	if err := k.RemoveSlot(slotID); err != nil {
		t.Fatalf("RemoveSlot: %v", err)
	}
	if _, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		t.Error("删槽位后端口仍在监听")
	}
}

// proxyGet 经 127.0.0.1:port 的 http 代理请求 target。
func proxyGet(t *testing.T, port int, target string) string {
	t.Helper()
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(fmt.Sprintf("http://127.0.0.1:%d", port))),
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取代理响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("代理响应状态: %d", resp.StatusCode)
	}
	return string(buf)
}

// httpNodeSpec 构造指向 host:port 的 http 代理节点。
func httpNodeSpec(name, hostport string) NodeSpec {
	host, port, _ := net.SplitHostPort(hostport)
	return NodeSpec{
		ID: name, Name: name, Protocol: "http",
		Spec: map[string]any{"host": host, "port": toInt(port, 80)},
	}
}
