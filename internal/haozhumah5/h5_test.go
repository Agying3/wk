package haozhumah5

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newFakeVeeto 起 fake veeto 服务器并接管 BaseURL。
// 未定制的路径返回 code:404 模拟"上游没有这个接口"。
type fakeVeeto struct {
	srv   *httptest.Server
	token string // 期望的 Bearer token；空表示不校验

	searchResp   string
	codesResp    string
	bindingsResp []string // 按 page 依次返回
	joinResp     string
	batchResp    string

	lastJoinBody string
	lastBatch    string
	searchHits   int
}

func newFakeVeeto(t *testing.T) *fakeVeeto {
	t.Helper()
	f := &fakeVeeto{token: "tok-ok"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if f.token != "" {
			if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"code":401,"message":"unauthorized","data":{}}`)
				return
			}
		}

		switch {
		case r.URL.Path == "/projects/search":
			f.searchHits++
			if f.searchResp == "" {
				fmt.Fprint(w, `{"code":0,"message":"ok","data":{"list":[]}}`)
				return
			}
			fmt.Fprint(w, f.searchResp)
		case strings.HasSuffix(r.URL.Path, "/public-codes"):
			if f.codesResp == "" {
				fmt.Fprint(w, `{"code":0,"message":"ok","data":[]}`)
				return
			}
			fmt.Fprint(w, f.codesResp)
		case r.URL.Path == "/bindings" && r.Method == http.MethodGet:
			page := r.URL.Query().Get("page")
			idx := 0
			if page != "" && page != "1" {
				idx = 1
			}
			if idx < len(f.bindingsResp) {
				fmt.Fprint(w, f.bindingsResp[idx])
				return
			}
			fmt.Fprint(w, `{"code":0,"message":"ok","data":{"list":[],"total":0,"page":1,"size":100}}`)
		case r.URL.Path == "/bindings" && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			f.lastJoinBody = string(raw)
			if f.joinResp == "" {
				// 默认成功：按请求体里的码回显（上游真实行为就是逐码回结果）。
				var sent struct {
					Codes []string `json:"codes"`
				}
				_ = json.Unmarshal(raw, &sent)
				results := make([]string, 0, len(sent.Codes))
				for _, c := range sent.Codes {
					results = append(results, fmt.Sprintf(`{"code":%q,"ok":true,"message":"添加成功"}`, c))
				}
				fmt.Fprintf(w, `{"code":0,"message":"ok","data":{"results":[%s]}}`, strings.Join(results, ","))
				return
			}
			fmt.Fprint(w, f.joinResp)
		case r.URL.Path == "/bindings/batch":
			raw, _ := io.ReadAll(r.Body)
			f.lastBatch = string(raw)
			if f.batchResp == "" {
				fmt.Fprint(w, `{"code":0,"message":"ok","data":{"affected":1}}`)
				return
			}
			fmt.Fprint(w, f.batchResp)
		default:
			fmt.Fprint(w, `{"code":404,"message":"Not Found","data":{}}`)
		}
	}))
	t.Cleanup(func() {
		f.srv.Close()
		BaseURL = "https://api.veeto.cn/v1"
	})
	BaseURL = f.srv.URL
	return f
}

func TestProjectsParsesStructuredFields(t *testing.T) {
	f := newFakeVeeto(t)
	f.searchResp = `{"code":0,"message":"ok","data":{"list":[
		{"id":"61903","sid":"5b02b39a13d7c2de","name":"腾讯科技2次[限对接]"},
		{"id":"52283","sid":"70da814e38b466b6","name":"腾讯科技[限对接]"}
	],"total":2,"page":1,"size":50}}`

	c := New("tok-ok")
	got, err := c.Projects(context.Background(), "腾讯科技")
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 projects, got %d", len(got))
	}
	// 数字 ID 必须原样保真（官方 getPhone 的 sid 参数），SID 是查码用的引用。
	byID := map[string]Project{}
	for _, p := range got {
		byID[p.ProjectID] = p
	}
	if p := byID["61903"]; p.SID != "5b02b39a13d7c2de" || p.Name != "腾讯科技2次[限对接]" {
		t.Fatalf("61903 解析错误: %+v", p)
	}
	// 新站结构化返回，不再需要从「【52283】名称」里抠 ID。
	if !strings.Contains(byID["52283"].Name, "【") == false {
		t.Fatalf("名称不应被改写: %q", byID["52283"].Name)
	}
}

func TestProjectsRequiresKeyword(t *testing.T) {
	newFakeVeeto(t)
	c := New("tok-ok")
	if _, err := c.Projects(context.Background(), "   "); err == nil {
		t.Fatal("空关键词应当报错")
	}
}

func TestProjectsCached(t *testing.T) {
	f := newFakeVeeto(t)
	f.searchResp = `{"code":0,"message":"ok","data":{"list":[{"id":"1","sid":"abc","name":"X"}]}}`
	c := New("tok-ok")
	for i := 0; i < 3; i++ {
		if _, err := c.Projects(context.Background(), "x"); err != nil {
			t.Fatalf("Projects: %v", err)
		}
	}
	if f.searchHits != 1 {
		t.Fatalf("缓存未命中：上游被调用 %d 次", f.searchHits)
	}
}

func TestUIDsParsesAndSorts(t *testing.T) {
	f := newFakeVeeto(t)
	f.codesResp = `{"code":0,"message":"ok","data":[
		{"code":"61903-DEAD","projectId":"61903","projectName":"腾讯科技2次","price":"0.230","available":0,"operators":["虚拟运营商"],"provinces":["山东"],"prefixes":["171"],"segmentType":"虚拟号段","pinned":false},
		{"code":"61903-VALID","projectId":"61903","projectName":"腾讯科技2次","price":"0.495","available":22,"operators":["虚拟运营商"],"provinces":["内蒙古"],"prefixes":["170","167"],"segmentType":"虚拟号段","pinned":false},
		{"code":"61903-REAL","projectId":"61903","projectName":"腾讯科技2次","price":"41.800","available":130,"operators":["移动"],"provinces":["广东"],"prefixes":["198"],"segmentType":"正常号段","pinned":true}
	]}`

	c := New("tok-ok")
	got, err := c.UIDs(context.Background(), "5b02b39a13d7c2de")
	if err != nil {
		t.Fatalf("UIDs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// 有货优先，且同库存下价格低→高：REAL(130) 在前，VALID(22) 次之，DEAD(0) 最后。
	if got[0].UID != "61903-REAL" || got[1].UID != "61903-VALID" || got[2].UID != "61903-DEAD" {
		t.Fatalf("排序不对: %v %v %v", got[0].UID, got[1].UID, got[2].UID)
	}
	if got[1].Price != 0.495 || got[1].Stock != 22 {
		t.Fatalf("字段解析错误: %+v", got[1])
	}
	if len(got[1].Segments) != 2 || got[1].Segments[0] != "170" {
		t.Fatalf("号段解析错误: %+v", got[1].Segments)
	}
	if !got[0].Pinned || got[0].SegmentType != "正常号段" {
		t.Fatalf("置顶/号段类型解析错误: %+v", got[0])
	}
}

func TestUIDsRequiresSID(t *testing.T) {
	newFakeVeeto(t)
	c := New("tok-ok")
	if _, err := c.UIDs(context.Background(), ""); err == nil {
		t.Fatal("空 sid 应当报错")
	}
}

func TestMyUIDsPaginates(t *testing.T) {
	f := newFakeVeeto(t)
	// 第一页满 100 条，第二页 2 条 —— 必须翻到第二页才完整。
	page1 := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		page1 = append(page1, fmt.Sprintf(`{"code":"61903-C%03d","projectName":"腾讯科技","price":"0.5","available":1}`, i))
	}
	f.bindingsResp = []string{
		`{"code":0,"message":"ok","data":{"list":[` + strings.Join(page1, ",") + `],"total":102,"page":1,"size":100}}`,
		`{"code":0,"message":"ok","data":{"list":[{"code":"61903-Z1","projectName":"腾讯科技","price":"0.5","available":1},{"code":"61903-Z2","projectName":"联通项目","price":"0.6","available":2}],"total":102,"page":2,"size":100}}`,
	}
	c := New("tok-ok")
	all, err := c.MyUIDs(context.Background(), "")
	if err != nil {
		t.Fatalf("MyUIDs: %v", err)
	}
	if len(all) != 102 {
		t.Fatalf("翻页不完整: want 102, got %d", len(all))
	}
	// 本地过滤。
	filtered, err := c.MyUIDs(context.Background(), "联通")
	if err != nil {
		t.Fatalf("MyUIDs filtered: %v", err)
	}
	if len(filtered) != 1 || filtered[0].UID != "61903-Z2" {
		t.Fatalf("过滤结果不对: %+v", filtered)
	}
}

func TestAddUIDSuccess(t *testing.T) {
	f := newFakeVeeto(t)
	c := New("tok-ok")
	if err := c.AddUID(context.Background(), "61903-81H9C4KYDZ"); err != nil {
		t.Fatalf("AddUID: %v", err)
	}
	var sent struct {
		Codes []string `json:"codes"`
	}
	if err := json.Unmarshal([]byte(f.lastJoinBody), &sent); err != nil {
		t.Fatalf("请求体不是 JSON: %s", f.lastJoinBody)
	}
	if len(sent.Codes) != 1 || sent.Codes[0] != "61903-81H9C4KYDZ" {
		t.Fatalf("请求体不对: %s", f.lastJoinBody)
	}
}

func TestAddUIDUpstreamRejects(t *testing.T) {
	f := newFakeVeeto(t)
	f.joinResp = `{"code":0,"message":"ok","data":{"results":[{"code":"61903-BAD","ok":false,"message":"库存不足"}]}}`
	c := New("tok-ok")
	err := c.AddUID(context.Background(), "61903-BAD")
	if err == nil {
		t.Fatal("上游 ok:false 必须报错")
	}
	if !strings.Contains(err.Error(), "库存不足") {
		t.Fatalf("错误文案应带上游原因: %v", err)
	}
}

func TestAddUIDMissingResult(t *testing.T) {
	f := newFakeVeeto(t)
	// 上游返回了别人的码结果，没回我们这条 —— 不能当成功。
	f.joinResp = `{"code":0,"message":"ok","data":{"results":[{"code":"OTHER","ok":true,"message":"添加成功"}]}}`
	c := New("tok-ok")
	if err := c.AddUID(context.Background(), "61903-MINE"); err == nil {
		t.Fatal("上游未返回该码结果时必须报错")
	}
}

func TestBatchActions(t *testing.T) {
	f := newFakeVeeto(t)
	c := New("tok-ok")
	for _, tc := range []struct {
		name   string
		call   func(context.Context, []string) error
		action string
	}{
		{"delete", c.RemoveUIDs, "delete"},
		{"pause", c.PauseUIDs, "pause"},
		{"resume", c.ResumeUIDs, "resume"},
	} {
		if err := tc.call(context.Background(), []string{"61903-A", " ", "61903-B"}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var sent struct {
			Action string   `json:"action"`
			Codes  []string `json:"codes"`
		}
		if err := json.Unmarshal([]byte(f.lastBatch), &sent); err != nil {
			t.Fatalf("%s 请求体不是 JSON: %s", tc.name, f.lastBatch)
		}
		if sent.Action != tc.action {
			t.Fatalf("%s action=%q", tc.name, sent.Action)
		}
		// 空白项必须被剔掉。
		if len(sent.Codes) != 2 {
			t.Fatalf("%s 空码未剔除: %+v", tc.name, sent.Codes)
		}
	}
}

func TestBatchRejectsEmpty(t *testing.T) {
	newFakeVeeto(t)
	c := New("tok-ok")
	if err := c.RemoveUIDs(context.Background(), []string{"  ", ""}); err == nil {
		t.Fatal("空列表应当报错")
	}
}

func TestNoSession(t *testing.T) {
	newFakeVeeto(t)
	c := New("")
	_, err := c.Projects(context.Background(), "腾讯")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

func TestTokenExpiredOn401(t *testing.T) {
	f := newFakeVeeto(t)
	f.token = "tok-new" // 服务端只认新 token，客户端还拿旧的
	c := New("tok-old")
	_, err := c.Projects(context.Background(), "腾讯")
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("want ErrSessionExpired, got %v", err)
	}
	if !c.Expired() {
		t.Fatal("失效标记未置位")
	}
	// 重贴 token 应清失效标记。
	c.Session("tok-new")
	if c.Expired() {
		t.Fatal("重贴后失效标记应清除")
	}
	if _, err := c.Projects(context.Background(), "腾讯"); err != nil {
		t.Fatalf("重贴后应可用: %v", err)
	}
}

func TestExpiredOnEnvelopeCode(t *testing.T) {
	f := newFakeVeeto(t)
	f.token = "" // 不校验头，改由 body 表达失效
	f.searchResp = `{"code":401,"message":"登录已过期","data":{}}`
	c := New("tok-ok")
	_, err := c.Projects(context.Background(), "腾讯")
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("code=401 应判定失效, got %v", err)
	}
}

func TestUpstreamErrorSurfaces(t *testing.T) {
	f := newFakeVeeto(t)
	f.token = ""
	f.codesResp = `{"code":500,"message":"内部错误","data":{}}`
	c := New("tok-ok")
	_, err := c.UIDs(context.Background(), "abc123")
	if err == nil {
		t.Fatal("上游 code=500 必须报错")
	}
	if !strings.Contains(err.Error(), "内部错误") {
		t.Fatalf("错误文案应带上游 message: %v", err)
	}
}

func TestSessionSwitchFlushesCache(t *testing.T) {
	f := newFakeVeeto(t)
	f.token = "" // 两个 token 都放行
	f.codesResp = `{"code":0,"message":"ok","data":[{"code":"61903-A","price":"1","available":5}]}`
	c := New("tok-1")
	if _, err := c.UIDs(context.Background(), "sid1"); err != nil {
		t.Fatalf("UIDs: %v", err)
	}
	// 换账号后必须重新拉，不能命中旧账号的缓存（否则会显示别人的库存）。
	c.Session("tok-2")
	f.codesResp = `{"code":0,"message":"ok","data":[{"code":"61903-B","price":"2","available":9}]}`
	got, err := c.UIDs(context.Background(), "sid1")
	if err != nil {
		t.Fatalf("UIDs after switch: %v", err)
	}
	if len(got) != 1 || got[0].UID != "61903-B" {
		t.Fatalf("缓存未随 token 清空: %+v", got)
	}
}

func TestKeepaliveIsNoop(t *testing.T) {
	c := New("tok")
	c.StartKeepalive() // 不应 panic 或起 goroutine
	c.Close()
}
