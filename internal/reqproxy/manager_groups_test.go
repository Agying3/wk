package reqproxy

import (
	"path/filepath"
	"testing"
)

// TestProxyGroupCRUD 手动分组：建/改名/改区域/删除；默认分组保护。
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
	// all 区分组可建（不限制区域）。
	if _, err := m.CreateProxyGroup("通用出口", RegionAll); err != nil {
		t.Fatalf("all region must be accepted: %v", err)
	}
	if err := m.RenameProxyGroup(g.ID, "CN 出口 2"); err != nil {
		t.Fatal(err)
	}
	// 默认分组：不可改名/改区域/删除，区域恒 all。
	if err := m.RenameProxyGroup(DefaultProxyGroupID, "x"); err == nil {
		t.Fatal("default group rename must fail")
	}
	if err := m.SetProxyGroupRegion(DefaultProxyGroupID, "cn"); err == nil {
		t.Fatal("default group region change must fail")
	}
	if _, err := m.DeleteProxyGroup(DefaultProxyGroupID); err == nil {
		t.Fatal("default group delete must fail")
	}
	groups := m.ProxyGroupsSnapshot()
	if len(groups) != 3 {
		t.Fatalf("groups = %+v", groups)
	}
	var def *ProxyGroup
	for i := range groups {
		if groups[i].ID == DefaultProxyGroupID {
			def = &groups[i]
		}
	}
	if def == nil || def.Region != RegionAll || def.Name != DefaultProxyGroupName || def.SubID != "" {
		t.Fatalf("default group = %+v", def)
	}
	if err := m.RenameProxyGroup("pg_missing", "y"); err == nil {
		t.Fatal("unknown id must fail")
	}
	// 改区域。
	if err := m.SetProxyGroupRegion(g.ID, "global"); err != nil {
		t.Fatal(err)
	}
	regionOf := func(id string) string {
		var r string
		m.View(func(st *State) {
			if x := st.ProxyGroupByID(id); x != nil {
				r = x.Region
			}
		})
		return r
	}
	if regionOf(g.ID) != "global" {
		t.Fatalf("region after set = %q", regionOf(g.ID))
	}
	// 删除（不存在的）。
	if _, err := m.DeleteProxyGroup("pg_missing"); err == nil {
		t.Fatal("delete unknown must fail")
	}
	if _, err := m.DeleteProxyGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if gs := m.ProxyGroupsSnapshot(); len(gs) != 2 {
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
	if len(gs) != 2 {
		t.Fatalf("groups = %+v", gs)
	}
	var subG *ProxyGroup
	for i := range gs {
		if gs[i].SubID == sub.ID {
			subG = &gs[i]
		}
	}
	if subG == nil || subG.Region != "cn" {
		t.Fatalf("subscription group = %+v", subG)
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
	regionOf := func(id string) string {
		var r string
		m.View(func(st *State) {
			if g := st.ProxyGroupByID(id); g != nil {
				r = g.Region
			}
		})
		return r
	}
	if regionOf(sub.GroupID) != "global" {
		t.Fatalf("region after flip = %q, want global", regionOf(sub.GroupID))
	}
	// 订阅区域也允许 all（不限制区域）。
	if err := m.SetSubscriptionRegion(sub.ID, RegionAll); err != nil {
		t.Fatal(err)
	}
	if regionOf(sub.GroupID) != RegionAll {
		t.Fatalf("region after all = %q", regionOf(sub.GroupID))
	}
	if err := m.SetSubscriptionRegion(sub.ID, "cn"); err != nil {
		t.Fatal(err)
	}
	// 删除订阅连带删组（默认分组保留）。
	pgID := m.DeleteSubscription(sub.ID)
	if pgID != sub.GroupID {
		t.Fatalf("removed group = %q, want %q", pgID, sub.GroupID)
	}
	if gs = m.ProxyGroupsSnapshot(); len(gs) != 1 || gs[0].ID != DefaultProxyGroupID {
		t.Fatalf("groups after sub delete = %+v", gs)
	}
}

// TestManualNodeGroupAssignment 手动节点归组/挪组/随组删除回落默认分组。
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
	// 挪回默认分组（空 groupID = 默认分组）。
	var nodeID string
	for _, n := range nodes {
		nodeID = n.ID
	}
	if err := m.SetManualNodeGroup(nodeID, ""); err != nil {
		t.Fatal(err)
	}
	if ns := m.NodesSnapshot(); ns[0].GroupID != DefaultProxyGroupID {
		t.Fatalf("after ungroup = %q, want %q", ns[0].GroupID, DefaultProxyGroupID)
	}
	// 不带组导入 → 默认分组。
	if n, err := m.ImportManualNodes("1.2.3.6:8080:user:pass", ""); err != nil || n != 1 {
		t.Fatalf("import without group = %d err = %v", n, err)
	}
	found := false
	for _, n := range m.NodesSnapshot() {
		if n.Raw == "1.2.3.6:8080:user:pass" && n.GroupID == DefaultProxyGroupID {
			found = true
		}
	}
	if !found {
		t.Fatal("import without group must land in default group")
	}
	// 批量挪组：一次调用挪多个节点；目标组不存在时整体失败。
	g2, err := m.CreateProxyGroup("批量目标", "cn")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range m.NodesSnapshot() {
		ids = append(ids, n.ID)
	}
	if n, err := m.SetManualNodesGroup(ids, "pg_nope"); err == nil || n != 0 {
		t.Fatalf("batch move to unknown group must fail: n=%d err=%v", n, err)
	}
	if n, err := m.SetManualNodesGroup(ids, g2.ID); err != nil || n != 2 {
		t.Fatalf("batch move = %d err = %v, want 2", n, err)
	}
	for _, n := range m.NodesSnapshot() {
		if n.GroupID != g2.ID {
			t.Fatalf("node %s group = %q, want %q", n.ID, n.GroupID, g2.ID)
		}
	}
	// 幂等：再挪一次（同组）不计数。
	if n, err := m.SetManualNodesGroup(ids, g2.ID); err != nil || n != 0 {
		t.Fatalf("idempotent batch move = %d err = %v, want 0", n, err)
	}
	// 混入不存在的 ID：跳过不报错。
	if n, err := m.SetManualNodesGroup(append(ids, "node_missing"), ""); err != nil || n != 2 {
		t.Fatalf("batch move to default = %d err = %v, want 2", n, err)
	}
	// 挪回组再删组：节点回落默认分组。
	if err := m.SetManualNodeGroup(nodeID, g.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := m.DeleteProxyGroup(g.ID); err != nil || n != 1 {
		t.Fatalf("delete group affected = %d err = %v", n, err)
	}
	if ns := m.NodesSnapshot(); ns[0].GroupID != DefaultProxyGroupID {
		t.Fatalf("node group after group delete = %q, want default", ns[0].GroupID)
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

// TestNormalizeLegacyState 旧 state.json（无 proxy_groups 字段）兼容：
// 默认分组被补齐，未分组的存量手动节点迁移进默认分组。
func TestNormalizeLegacyState(t *testing.T) {
	m := newTestManager(t)
	// 模拟旧状态：手动节点没有分组、没有默认分组。
	m.store.Update(func(st *State) {
		st.ProxyGroups = nil
		st.ManualNodes = []ManualNode{
			{ID: "node_1", Name: "a", Raw: "1.1.1.1:8080", GroupID: ""},
			{ID: "node_2", Name: "b", Raw: "2.2.2.2:8080", GroupID: "pg_gone"},
		}
		st.Normalize()
	})
	m.View(func(st *State) {
		if st.ProxyGroups == nil {
			t.Fatal("proxy_groups must normalize to non-nil")
		}
		if g := st.ProxyGroupByID(DefaultProxyGroupID); g == nil || g.Region != RegionAll || g.SubID != "" {
			t.Fatalf("default group after normalize = %+v", g)
		}
		for _, n := range st.ManualNodes {
			if n.GroupID != DefaultProxyGroupID {
				t.Fatalf("legacy node %s group = %q, want default", n.ID, n.GroupID)
			}
		}
	})
	// 已分组的节点不被迁移（幂等）。
	g, err := m.CreateProxyGroup("x", "cn")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetManualNodeGroup("node_1", g.ID); err != nil {
		t.Fatal(err)
	}
	m.store.Update(func(st *State) { st.Normalize() })
	m.View(func(st *State) {
		if n := st.ManualNodes[0]; n.GroupID != g.ID {
			t.Fatalf("grouped node migrated: %+v", n)
		}
	})
}
