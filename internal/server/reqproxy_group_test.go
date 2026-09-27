package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/reqproxy"
)

// newProxyGroupTestHandler 带分组存储 + reqproxy 的测试 handler。
func newProxyGroupTestHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	gs, err := groups.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := reqproxy.NewManager(reqproxy.Config{
		StateFile: filepath.Join(dir, "reqproxy-state.json"),
		PortMin:   31080,
		PortMax:   31999,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	// default 账号分组保持 all（迁移默认），建一个 cn 组测试区域约束。
	if err := gs.CreateWithRegion("cn-accounts", groups.RegionCN); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{
		APIKey:      "admin-key",
		AuthDir:     dir,
		Groups:      gs,
		ReqProxy:    m,
		FrontendDir: "frontend",
	})
	return h
}

func TestReqProxyGroupEndpoints(t *testing.T) {
	h := newProxyGroupTestHandler(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 建手动分组（响应里直接拿 id；列表首个 id 现在可能是默认分组）。
	rec := do(http.MethodPost, "/admin/reqproxy/groups", `{"name":"手动组","region":"cn"}`)
	if rec.Code != 200 {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	pgID := extractJSONString(t, rec.Body.String(), "id")
	if rec := do(http.MethodPost, "/admin/reqproxy/groups", `{"name":"bad","region":"mars"}`); rec.Code != 400 {
		t.Fatalf("invalid region = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodPost, "/admin/reqproxy/groups", `{"name":"no-region","region":"all"}`); rec.Code != 200 {
		t.Fatalf("all region = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// 列表。
	rec = do(http.MethodGet, "/admin/reqproxy/groups", "")
	if rec.Code != 200 {
		t.Fatalf("list = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "手动组") || !strings.Contains(body, `"region":"cn"`) {
		t.Fatalf("list body = %s", body)
	}

	// 订阅分组（专属组）不可改名/删除。
	subRec := do(http.MethodPost, "/admin/reqproxy/subscriptions", `{"name":"机场A","url":"http://127.0.0.1:1/sub","region":"cn"}`)
	if subRec.Code != 200 {
		t.Fatalf("sub create = %d: %s", subRec.Code, subRec.Body.String())
	}
	subGroupID := extractJSONString(t, subRec.Body.String(), "group_id")
	if subGroupID == "" {
		t.Fatalf("sub group_id missing: %s", subRec.Body.String())
	}
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"name":"新名"}`); rec.Code != 400 {
		t.Fatalf("rename sub-owned = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodDelete, "/admin/reqproxy/groups/"+subGroupID, ""); rec.Code != 400 {
		t.Fatalf("delete sub-owned = %d, want 400", rec.Code)
	}

	// 账号分组绑定：cn 账号分组不能绑 global 代理分组。
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"region":"global"}`); rec.Code != 200 {
		t.Fatalf("sub region flip = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":["`+subGroupID+`"]}`); rec.Code != 400 {
		t.Fatalf("cn account group bind global proxy group = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// 换回 cn 后可绑定。
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"region":"cn"}`); rec.Code != 200 {
		t.Fatalf("sub region flip back = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":["`+subGroupID+`"]}`); rec.Code != 200 {
		t.Fatalf("bind = %d: %s", rec.Code, rec.Body.String())
	}
	// 读回。
	if rec := do(http.MethodGet, "/admin/groups/cn-accounts/proxy-bindings", ""); !strings.Contains(rec.Body.String(), subGroupID) {
		t.Fatalf("bindings read back = %s", rec.Body.String())
	}

	// 代理分组区域变更的反向校验：绑定了它的账号分组是 cn → 不能改 global。
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"region":"global"}`); rec.Code != 400 {
		t.Fatalf("region flip with bound cn group = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// 解绑后区域可改。
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":[]}`); rec.Code != 200 {
		t.Fatalf("unbind = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"region":"global"}`); rec.Code != 200 {
		t.Fatalf("region flip after unbind = %d: %s", rec.Code, rec.Body.String())
	}

	// 删手动组：绑定引用被清理。
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":["`+pgID+`"]}`); rec.Code != 200 {
		t.Fatalf("bind manual group = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/admin/reqproxy/groups/"+pgID, ""); rec.Code != 200 {
		t.Fatalf("delete manual group = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/admin/groups/cn-accounts/proxy-bindings", ""); strings.Contains(rec.Body.String(), pgID) {
		t.Fatalf("binding ref must be cleaned after group delete: %s", rec.Body.String())
	}

	// 删订阅连带删专属组 + 绑定引用清理。
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":["`+subGroupID+`"]}`); rec.Code != 200 {
		// subGroupID 已是 global 区，cn 账号分组不能绑 → 先验证这个约束。
		if !strings.Contains(rec.Body.String(), "国际") && !strings.Contains(rec.Body.String(), "global") {
			t.Fatalf("bind global group to cn = %d: %s", rec.Code, rec.Body.String())
		}
	}
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+subGroupID, `{"region":"cn"}`); rec.Code != 200 {
		t.Fatalf("flip sub group back to cn = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, "/admin/groups/cn-accounts/proxy-bindings", `{"proxy_groups":["`+subGroupID+`"]}`); rec.Code != 200 {
		t.Fatalf("bind sub group = %d: %s", rec.Code, rec.Body.String())
	}
	subID := extractJSONString(t, subRec.Body.String(), "id")
	if rec := do(http.MethodDelete, "/admin/reqproxy/subscriptions/"+subID, ""); rec.Code != 200 {
		t.Fatalf("delete sub = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(http.MethodGet, "/admin/groups/cn-accounts/proxy-bindings", "")
	if strings.Contains(rec.Body.String(), subGroupID) {
		t.Fatalf("sub-owned group binding must be cleaned: %s", rec.Body.String())
	}
	rec = do(http.MethodGet, "/admin/reqproxy/groups", "")
	if strings.Contains(rec.Body.String(), subGroupID) {
		t.Fatalf("sub-owned group must be gone: %s", rec.Body.String())
	}
}

// extractJSONString 从 JSON 里提取第一个 "key":"value"（测试辅助，够用即可）。
func extractJSONString(t *testing.T, body, key string) string {
	t.Helper()
	idx := strings.Index(body, `"`+key+`":"`)
	if idx < 0 {
		t.Fatalf("key %q not found in %s", key, body)
	}
	rest := body[idx+len(key)+4:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("unterminated value in %s", body)
	}
	return rest[:end]
}

// TestResolverUnionSemantics resolver 并集语义的单元级验证。
func TestResolverUnionSemantics(t *testing.T) {
	h := newProxyGroupTestHandler(t)
	r := proxyGroupResolver{h: h}

	// 未登记 → 不受限。
	if got := r.AllowedProxyGroups("nobody"); got != nil {
		t.Fatalf("unregistered = %v, want nil", got)
	}
	// 只在一个组、该组无绑定 → 不受限。
	if err := h.cfg.Groups.SetAccountGroups("u1", []string{"cn-accounts"}); err != nil {
		t.Fatal(err)
	}
	if got := r.AllowedProxyGroups("u1"); got != nil {
		t.Fatalf("no bindings = %v, want nil", got)
	}
	// 绑定后 → 并集。
	if err := h.cfg.Groups.SetProxyBindings("cn-accounts", []string{"pg1", "pg2"}); err != nil {
		t.Fatal(err)
	}
	got := r.AllowedProxyGroups("u1")
	if len(got) != 2 || !got["pg1"] || !got["pg2"] {
		t.Fatalf("union = %v", got)
	}
	// 两个组，一个有绑定一个没有 → 不受限。
	if err := h.cfg.Groups.CreateWithRegion("other", groups.RegionCN); err != nil {
		t.Fatal(err)
	}
	if err := h.cfg.Groups.SetAccountGroups("u1", []string{"cn-accounts", "other"}); err != nil {
		t.Fatal(err)
	}
	if got := r.AllowedProxyGroups("u1"); got != nil {
		t.Fatalf("partial bindings = %v, want nil", got)
	}
	// 两个组都有绑定 → 并集。
	if err := h.cfg.Groups.SetProxyBindings("other", []string{"pg3"}); err != nil {
		t.Fatal(err)
	}
	got = r.AllowedProxyGroups("u1")
	if len(got) != 3 || !got["pg3"] {
		t.Fatalf("full union = %v", got)
	}
}

// TestBatchNodeGroup 批量挪组 API：一次 PUT 挪多个节点。
func TestBatchNodeGroup(t *testing.T) {
	h := newProxyGroupTestHandler(t)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// 建两个手动组 + 导入 3 个节点到组 A。
	if rec := do(http.MethodPost, "/admin/reqproxy/groups", `{"name":"A","region":"cn"}`); rec.Code != 200 {
		t.Fatalf("create A = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/admin/reqproxy/groups", `{"name":"B","region":"global"}`); rec.Code != 200 {
		t.Fatalf("create B = %d: %s", rec.Code, rec.Body.String())
	}
	groupA := h.cfg.ReqProxy.ProxyGroupsSnapshot()[0].ID
	groupB := h.cfg.ReqProxy.ProxyGroupsSnapshot()[1].ID
	if rec := do(http.MethodPost, "/admin/reqproxy/nodes/import", `{"text":"1.2.3.4:8080:u:p\n1.2.3.5:8080:u:p\n1.2.3.6:8080:u:p","group_id":"`+groupA+`"}`); rec.Code != 200 {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body.String())
	}
	var ids []string
	h.cfg.ReqProxy.View(func(st *reqproxy.State) {
		for _, n := range st.ManualNodes {
			ids = append(ids, n.ID)
		}
	})
	if len(ids) != 3 {
		t.Fatalf("imported = %d", len(ids))
	}
	// 空 node_ids → 400。
	if rec := do(http.MethodPut, "/admin/reqproxy/nodes/group", `{"node_ids":[],"group_id":"`+groupB+`"}`); rec.Code != 400 {
		t.Fatalf("empty ids = %d, want 400", rec.Code)
	}
	// 不存在的组 → 400。
	if rec := do(http.MethodPut, "/admin/reqproxy/nodes/group", `{"node_ids":["x"],"group_id":"pg_nope"}`); rec.Code != 400 {
		t.Fatalf("unknown group = %d, want 400", rec.Code)
	}
	// 批量挪 3 个到 B（含一个不存在的 ID，跳过）→ moved=3。
	if rec := do(http.MethodPut, "/admin/reqproxy/nodes/group", `{"node_ids":["`+strings.Join(ids, `","`)+`","node_missing"],"group_id":"`+groupB+`"}`); rec.Code != 200 {
		t.Fatalf("batch = %d: %s", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), `"moved":3`) {
		t.Fatalf("moved = %s", rec.Body.String())
	}
	h.cfg.ReqProxy.View(func(st *reqproxy.State) {
		for _, n := range st.ManualNodes {
			if n.GroupID != groupB {
				t.Fatalf("node %s group = %q", n.ID, n.GroupID)
			}
		}
	})
}

// TestDefaultProxyGroupSeed 默认分组 + default 账号分组自动绑定的种子语义：
//   - NewHandler 时 default 账号分组自动绑定 pg_default（首次）；
//   - default 账号分组的成员被限制到 pg_default；
//   - 管理员清空绑定后 = 不限制，且重启（重新 NewHandler）不复活；
//   - 默认分组不可改名/改区域/删除（API 层）；列表带 is_default 标记。
func TestDefaultProxyGroupSeed(t *testing.T) {
	h := newProxyGroupTestHandler(t)
	r := proxyGroupResolver{h: h}

	// 种子已生效：default 组账号被限制到 pg_default。
	if err := h.cfg.Groups.SetAccountGroups("u9", []string{groups.DefaultGroup}); err != nil {
		t.Fatal(err)
	}
	got := r.AllowedProxyGroups("u9")
	if got == nil || !got[reqproxy.DefaultProxyGroupID] {
		t.Fatalf("default account group must be seeded to default proxy group, got %v", got)
	}

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer admin-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// 列表带 is_default + 默认分组存在。
	rec := do(http.MethodGet, "/admin/reqproxy/groups", "")
	if !strings.Contains(rec.Body.String(), `"is_default":true`) || !strings.Contains(rec.Body.String(), reqproxy.DefaultProxyGroupID) {
		t.Fatalf("group list must include default group with is_default: %s", rec.Body.String())
	}
	// 默认分组保护：改名/改区域/删除全部 400。
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+reqproxy.DefaultProxyGroupID, `{"name":"x"}`); rec.Code != 400 {
		t.Fatalf("default rename = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodPut, "/admin/reqproxy/groups/"+reqproxy.DefaultProxyGroupID, `{"region":"cn"}`); rec.Code != 400 {
		t.Fatalf("default region change = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodDelete, "/admin/reqproxy/groups/"+reqproxy.DefaultProxyGroupID, ""); rec.Code != 400 {
		t.Fatalf("default delete = %d, want 400", rec.Code)
	}

	// 管理员清空 default 绑定 → 不限制。
	if rec := do(http.MethodPut, "/admin/groups/default/proxy-bindings", `{"proxy_groups":[]}`); rec.Code != 200 {
		t.Fatalf("clear default bindings = %d: %s", rec.Code, rec.Body.String())
	}
	if got := r.AllowedProxyGroups("u9"); got != nil {
		t.Fatalf("after clear, default account must be unrestricted, got %v", got)
	}

	// 重新 NewHandler（模拟重启）：显式清空不被种子复活。
	dir := t.TempDir()
	gs2, err := groups.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := reqproxy.NewManager(reqproxy.Config{
		StateFile: filepath.Join(dir, "reqproxy-state.json"),
		PortMin:   31080,
		PortMax:   31999,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.Close)
	if err := gs2.SetProxyBindings(groups.DefaultGroup, []string{}); err != nil {
		t.Fatal(err)
	}
	h2 := NewHandler(Config{
		APIKey:      "admin-key",
		AuthDir:     dir,
		Groups:      gs2,
		ReqProxy:    m2,
		FrontendDir: "frontend",
	})
	r2 := proxyGroupResolver{h: h2}
	if err := gs2.SetAccountGroups("u9", []string{groups.DefaultGroup}); err != nil {
		t.Fatal(err)
	}
	if got := r2.AllowedProxyGroups("u9"); got != nil {
		t.Fatalf("explicitly cleared binding must not re-seed, got %v", got)
	}
}
