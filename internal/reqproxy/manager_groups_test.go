package reqproxy

import (
	"path/filepath"
	"testing"
)

// TestProxyGroupCRUD 手动分组：建/改名/改区域/删除。
func TestProxyGroupCRUD(t *testing.T) {
	m := newTestManager(t)

	g, err := m.CreateProxyGroup("CN 出口", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || g.Region != "cn" {
		t.Fatalf("created = %+v", g)
	}
	if _, err := m.CreateProxyGroup("x", "mars"); err == nil {
		t.Fatal("invalid region must fail")
	}
	if _, err := m.CreateProxyGroup("", "cn"); err == nil {
		t.Fatal("empty name must fail")
	}
	if err := m.RenameProxyGroup(g.ID, "CN 出口 2"); err != nil {
		t.Fatal(err)
	}
	groups := m.ProxyGroupsSnapshot()
	if len(groups) != 1 || groups[0].Name != "CN 出口 2" {
		t.Fatalf("groups = %+v", groups)
	}
	if err := m.RenameProxyGroup("pg_missing", "y"); err == nil {
		t.Fatal("unknown id must fail")
	}
	// 改区域。
	if err := m.SetProxyGroupRegion(g.ID, "global"); err != nil {
		t.Fatal(err)
	}
	if gs := m.ProxyGroupsSnapshot(); gs[0].Region != "global" {
		t.Fatalf("region after set = %q", gs[0].Region)
	}
	// 删除（不存在的）。
	if _, err := m.DeleteProxyGroup("pg_missing"); err == nil {
		t.Fatal("delete unknown must fail")
	}
	if _, err := m.DeleteProxyGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if gs := m.ProxyGroupsSnapshot(); len(gs) != 0 {
		t.Fatalf("after delete = %+v", gs)
	}
}

// TestSubscriptionAutoGroup 订阅创建自动建专属组；删除订阅连带删组。
func TestSubscriptionAutoGroup(t *testing.T) {
	m := newTestManager(t)
	sub, err := m.AddSubscription("机场A", "https://example.com/sub", true, "1h", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if sub.GroupID == "" {
		t.Fatal("subscription must have a dedicated group")
	}
	gs := m.ProxyGroupsSnapshot()
	if len(gs) != 1 || gs[0].SubID != sub.ID || gs[0].Region != "cn" {
		t.Fatalf("groups = %+v", gs)
	}
	// 专属组不可改名、不可单独删除。
	if err := m.RenameProxyGroup(sub.GroupID, "new name"); err == nil {
		t.Fatal("sub-owned group rename must fail")
	}
	if _, err := m.DeleteProxyGroup(sub.GroupID); err == nil {
		t.Fatal("sub-owned group delete must fail")
	}
	// 无区域的新建订阅被拒。
	if _, err := m.AddSubscription("机场B", "https://example.com/sub2", true, "1h", ""); err == nil {
		t.Fatal("empty region must fail")
	}
	// 变更订阅区域 = 组区域同步。
	if err := m.SetSubscriptionRegion(sub.ID, "global"); err != nil {
		t.Fatal(err)
	}
	gs = m.ProxyGroupsSnapshot()
	if gs[0].Region != "global" {
		t.Fatalf("region after flip = %q, want global", gs[0].Region)
	}
	// 删除订阅连带删组。
	pgID := m.DeleteSubscription(sub.ID)
	if pgID != sub.GroupID {
		t.Fatalf("removed group = %q, want %q", pgID, sub.GroupID)
	}
	if gs = m.ProxyGroupsSnapshot(); len(gs) != 0 {
		t.Fatalf("groups after sub delete = %+v", gs)
	}
}

// TestManualNodeGroupAssignment 手动节点归组/挪组/随组删除归未分组。
func TestManualNodeGroupAssignment(t *testing.T) {
	m := newTestManager(t)
	g, err := m.CreateProxyGroup("手动CN", "cn")
	if err != nil {
		t.Fatal(err)
	}
	// 带组导入。
	if _, err := m.ImportManualNodes("1.2.3.4:8080:user:pass", g.ID); err != nil {
		t.Fatal(err)
	}
	nodes := m.NodesSnapshot()
	if len(nodes) != 1 || nodes[0].GroupID != g.ID {
		t.Fatalf("imported node group = %+v", nodes)
	}
	// 导入到订阅专属组 → 拒（无订阅场景下用一个不存在的组也拒）。
	if _, err := m.ImportManualNodes("1.2.3.5:8080:user:pass", "pg_nope"); err == nil {
		t.Fatal("import to unknown group must fail")
	}
	// 挪到未分组。
	var nodeID string
	for _, n := range nodes {
		nodeID = n.ID
	}
	if err := m.SetManualNodeGroup(nodeID, ""); err != nil {
		t.Fatal(err)
	}
	if ns := m.NodesSnapshot(); ns[0].GroupID != "" {
		t.Fatalf("after ungroup = %q", ns[0].GroupID)
	}
	// 挪回组再删组：节点归未分组。
	if err := m.SetManualNodeGroup(nodeID, g.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := m.DeleteProxyGroup(g.ID); err != nil || n != 1 {
		t.Fatalf("delete group affected = %d err = %v", n, err)
	}
	if ns := m.NodesSnapshot(); ns[0].GroupID != "" {
		t.Fatalf("node group after group delete = %q, want empty", ns[0].GroupID)
	}
}

// newPortManager 带槽位端口段的纯逻辑 Manager（多槽位测试用）。
func newPortManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(Config{
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		PortMin:   31080,
		PortMax:   31999,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// TestSetGroupRegionSweepsSlots 分组区域变更后，指向该组节点的违规槽位被清扫。
func TestSetGroupRegionSweepsSlots(t *testing.T) {
	m := newPortManager(t)
	g, err := m.CreateProxyGroup("CN组", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ImportManualNodes("1.2.3.4:8080:user:pass", g.ID); err != nil {
		t.Fatal(err)
	}
	// 给账号建 cn 槽位（节点在 cn 组，合法）。
	if _, _, err := m.pool.Assign("u1", "cn"); err != nil {
		t.Fatal(err)
	}
	// 分组改 global → 节点不再服务 cn → 槽位被清扫（换向无解时保留，
	// 这里池里只有一个节点，无替代 → 保留 + warn）。
	if err := m.SetProxyGroupRegion(g.ID, "global"); err != nil {
		t.Fatal(err)
	}
	slots, _ := m.Views()
	if len(slots) != 1 {
		t.Fatalf("slots = %d", len(slots))
	}
	// cn 候选为空 → Assign 返回 ErrNoNode（不是直连）。
	if _, _, err := m.pool.Assign("u2", "cn"); err == nil {
		t.Fatal("cn assign must fail after group flips to global")
	}
	// global 账号可分配。
	if _, _, err := m.pool.Assign("u3", "global"); err != nil {
		t.Fatalf("global assign after flip: %v", err)
	}
}

// TestNormalizeLegacyState 旧 state.json（无 proxy_groups 字段）兼容。
func TestNormalizeLegacyState(t *testing.T) {
	m := newTestManager(t)
	m.View(func(st *State) {
		if st.ProxyGroups == nil {
			t.Fatal("proxy_groups must normalize to empty slice")
		}
	})
}
