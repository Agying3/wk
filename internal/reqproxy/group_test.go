package reqproxy

import (
	"testing"
)

// resolverStub 固定映射的分组解析器（测试用）。
type resolverStub map[string][]string

func (r resolverStub) AllowedProxyGroups(uid string) map[string]bool {
	ids, ok := r[uid]
	if !ok {
		return nil // 未登记 = 不限制
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// newTestPool 带分组定义的测试池。
func newGroupTestPool(t *testing.T, groups []ProxyGroup, resolver GroupResolver) *Pool {
	t.Helper()
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	store.Update(func(st *State) {
		st.ProxyGroups = groups
	})
	p := NewPool(store, 31080, 31999)
	if resolver != nil {
		p.SetGroupResolver(resolver)
	}
	return p
}

// TestNodeServesRegion D3 区域隔离：未分组节点两区域可见；归组节点仅同区可见。
func TestNodeServesRegion(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_cn", Region: "cn"},
		{ID: "pg_gl", Region: "global"},
	}, nil)
	p.SetNodes([]NodeSpec{
		{ID: "n_free", Name: "free"},
		{ID: "n_cn", Name: "cn", GroupID: "pg_cn"},
		{ID: "n_gl", Name: "gl", GroupID: "pg_gl"},
	})
	if !p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_free"}}, "cn") {
		t.Fatal("ungrouped node must serve cn")
	}
	if !p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_free"}}, "global") {
		t.Fatal("ungrouped node must serve global")
	}
	if !p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_cn", GroupID: "pg_cn"}}, "cn") {
		t.Fatal("cn-group node must serve cn")
	}
	if p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_cn", GroupID: "pg_cn"}}, "global") {
		t.Fatal("cn-group node must NOT serve global")
	}
	if p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_gl", GroupID: "pg_gl"}}, "cn") {
		t.Fatal("global-group node must NOT serve cn")
	}
	// 分组消失（订阅删除窗口期）：按未分组放行，等 rebuildPool 出池。
	if !p.nodeServesRegionLocked(Node{NodeSpec: NodeSpec{ID: "n_x", GroupID: "pg_gone"}}, "cn") {
		t.Fatal("vanished group must be treated as ungrouped")
	}
}

// TestCandidatesGroupFilter 候选过滤：区域隔离 + 绑定限制。
func TestCandidatesGroupFilter(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_cn", Region: "cn"},
		{ID: "pg_gl", Region: "global"},
	}, nil)
	p.SetNodes([]NodeSpec{
		{ID: "n_free", Name: "free"},
		{ID: "n_cna", Name: "cna", GroupID: "pg_cn"},
		{ID: "n_cnb", Name: "cnb", GroupID: "pg_cn"},
		{ID: "n_gl", Name: "gl", GroupID: "pg_gl"},
	})
	// 无绑定限制：cn 候选 = free + cna + cnb。
	cn := p.Candidates("cn")
	if len(cn) != 3 {
		t.Fatalf("cn candidates = %d, want 3", len(cn))
	}
	global := p.Candidates("global")
	if len(global) != 2 {
		t.Fatalf("global candidates = %d, want 2", len(global))
	}
	// 限制到 pg_cn：只剩 cna/cnb（绑定存在时未分组节点也不可用——
	// 绑定语义是"只能走这些分组"）。
	p.mu.Lock()
	limited := p.candidatesLocked("cn", map[string]bool{"pg_cn": true})
	p.mu.Unlock()
	if len(limited) != 2 {
		t.Fatalf("pg_cn limited = %d, want 2", len(limited))
	}
	// 空集限制：无任何可用节点。
	p.mu.Lock()
	empty := p.candidatesLocked("cn", map[string]bool{})
	p.mu.Unlock()
	if len(empty) != 0 {
		t.Fatalf("empty allowed = %v, want none", empty)
	}
}

// TestAssignRespectsGroupBinding Assign 限制到绑定分组的节点。
func TestAssignRespectsGroupBinding(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_a", Region: "cn"},
		{ID: "pg_b", Region: "cn"},
	}, resolverStub{"u1": {"pg_b"}})
	_ = p.SetNodes([]NodeSpec{
		{ID: "n_a", Name: "a", GroupID: "pg_a"},
		{ID: "n_b", Name: "b", GroupID: "pg_b"},
	})
	// u1 绑定到 pg_b：分配到的节点必须是 n_b（pg_a 的 n_a 不可用）。
	_, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if op == nil || op.NodeSpec.ID != "n_b" {
		t.Fatalf("assigned node = %+v, want n_b", op)
	}
	// u2 未登记：不受限。n_a/n_b 都可用（可能并入 n_b 槽位或新建 n_a 槽位），
	// 断言绑定的节点在两节点集合内即可。
	port2, op2, err := p.Assign("u2", "cn")
	if err != nil {
		t.Fatalf("assign u2: %v", err)
	}
	node2 := ""
	if op2 != nil {
		node2 = op2.NodeSpec.ID
	} else {
		p.mu.Lock()
		if b, ok := p.bindngs["u2"]; ok {
			if s := p.slots[b.SlotID]; s != nil {
				node2 = s.NodeID
			}
		}
		p.mu.Unlock()
	}
	if node2 != "n_a" && node2 != "n_b" {
		t.Fatalf("u2 assigned node = %q (port=%d), want n_a or n_b", node2, port2)
	}
}

// TestAssignLegacyBindingRegionViolation D3 生效后，历史绑定指向区域不符的
// 节点时落入重选（不被旧绑定绕过隔离）。
func TestAssignLegacyBindingRegionViolation(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_gl", Region: "global"},
	}, nil)
	_ = p.SetNodes([]NodeSpec{
		{ID: "n_gl", Name: "gl", GroupID: "pg_gl"},
		{ID: "n_free", Name: "free"},
	})
	// 预置一条 u1→cn 槽位指向 n_gl 的历史绑定（分组设置前的遗留）。
	slot := &Slot{ID: "slot_x", Port: 31080, NodeID: "n_gl", Region: "cn"}
	p.mu.Lock()
	p.slots["slot_x"] = slot
	p.bindngs["u1"] = Binding{SlotID: "slot_x"}
	p.mu.Unlock()
	// cn 账号再请求：n_gl 属 global 组不能服务 cn 槽 → 重选到 n_free。
	port, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if port == 31080 {
		t.Fatal("must not keep the region-violating legacy binding")
	}
	if op == nil || op.NodeSpec.ID != "n_free" {
		t.Fatalf("reassigned = %+v, want n_free", op)
	}
}

// TestAssignLegacyBindingGroupViolation D4 生效后，历史绑定指向越权分组
// 节点时同样落入重选。
func TestAssignLegacyBindingGroupViolation(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_a", Region: "cn"},
		{ID: "pg_b", Region: "cn"},
	}, resolverStub{"u1": {"pg_a"}})
	_ = p.SetNodes([]NodeSpec{
		{ID: "n_a", Name: "a", GroupID: "pg_a"},
		{ID: "n_b", Name: "b", GroupID: "pg_b"},
	})
	slot := &Slot{ID: "slot_x", Port: 31080, NodeID: "n_b", Region: "cn"}
	p.mu.Lock()
	p.slots["slot_x"] = slot
	p.bindngs["u1"] = Binding{SlotID: "slot_x"}
	p.mu.Unlock()
	port, op, err := p.Assign("u1", "cn")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if port == 31080 {
		t.Fatal("must not keep the group-violating legacy binding")
	}
	if op == nil || op.NodeSpec.ID != "n_a" {
		t.Fatalf("reassigned = %+v, want n_a", op)
	}
}

// TestRetargetSlotIntersection D5 交集：槽上账号约束冲突时换向失败。
func TestRetargetSlotIntersection(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_a", Region: "cn"},
		{ID: "pg_b", Region: "cn"},
	}, resolverStub{
		"u1": {"pg_a"},
		"u2": {"pg_b"},
	})
	_ = p.SetNodes([]NodeSpec{
		{ID: "n_a", Name: "a", GroupID: "pg_a"},
		{ID: "n_b", Name: "b", GroupID: "pg_b"},
	})
	// 共享槽：u1+u2 都在 slot_x。u1∪u2 的交集为空 → 任何节点都不可用 → 失败。
	slot := &Slot{ID: "slot_x", Port: 31080, NodeID: "n_a", Region: "cn"}
	p.mu.Lock()
	p.slots["slot_x"] = slot
	p.bindngs["u1"] = Binding{SlotID: "slot_x"}
	p.bindngs["u2"] = Binding{SlotID: "slot_x"}
	p.mu.Unlock()
	if _, err := p.Retarget("slot_x", "cn"); err == nil {
		t.Fatal("conflicting constraints must fail retarget")
	}
	// u1 也允许 pg_b（u1: pg_a+pg_b）：交集 = pg_b → 可换向到 n_b。
	p.SetGroupResolver(resolverStub{
		"u1": {"pg_a", "pg_b"},
		"u2": {"pg_b"},
	})
	op, err := p.Retarget("slot_x", "cn")
	if err != nil {
		t.Fatalf("retarget: %v", err)
	}
	if op == nil || op.NodeSpec.ID != "n_b" {
		t.Fatalf("retargeted = %+v, want n_b (交集内节点)", op)
	}
	// 单账号槽：正常换到其分组内。
	slot2 := &Slot{ID: "slot_y", Port: 31081, NodeID: "n_a", Region: "cn"}
	p.mu.Lock()
	p.slots["slot_y"] = slot2
	p.bindngs["u1"] = Binding{SlotID: "slot_y"}
	delete(p.bindngs, "u2")
	p.mu.Unlock()
	op, err = p.Retarget("slot_y", "cn")
	if err != nil {
		t.Fatalf("retarget slot_y: %v", err)
	}
	if op == nil || op.NodeSpec.GroupID != "pg_a" && op.NodeSpec.GroupID != "pg_b" {
		t.Fatalf("retargeted slot_y = %+v, want pg_a/pg_b group node", op)
	}
}

// TestSetNodesGroupChangeIsDiff GroupID 变化视为定义变更（进 Changed）。
func TestSetNodesGroupChangeIsDiff(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_a", Region: "cn"},
		{ID: "pg_b", Region: "cn"},
	}, nil)
	_ = p.SetNodes([]NodeSpec{{ID: "n1", Name: "x", Raw: "raw1", GroupID: "pg_a"}})
	diff := p.SetNodes([]NodeSpec{{ID: "n1", Name: "x", Raw: "raw1", GroupID: "pg_b"}})
	if len(diff.Changed) != 1 {
		t.Fatalf("changed = %d, want 1 (group moved)", len(diff.Changed))
	}
	if diff.Changed[0].GroupID != "pg_b" {
		t.Fatalf("changed group = %q, want pg_b", diff.Changed[0].GroupID)
	}
	// Raw 不变、分组也不变 → 无 diff。
	diff = p.SetNodes([]NodeSpec{{ID: "n1", Name: "x", Raw: "raw1", GroupID: "pg_b"}})
	if len(diff.Changed) != 0 {
		t.Fatalf("no-change diff = %d, want 0", len(diff.Changed))
	}
}

// TestRegionIndexRebuild 分组区域变更后索引重建（SetNodes 触发）。
func TestRegionIndexRebuild(t *testing.T) {
	p := newGroupTestPool(t, []ProxyGroup{
		{ID: "pg_x", Region: "cn"},
	}, nil)
	_ = p.SetNodes([]NodeSpec{{ID: "n1", Name: "x", GroupID: "pg_x"}})
	// pg_x 改为 global 区域。
	p.store.Update(func(st *State) {
		st.ProxyGroups[0].Region = "global"
	})
	// SetNodes 重建索引后：pg_x 节点不再服务 cn。
	_ = p.SetNodes([]NodeSpec{{ID: "n1", Name: "x", GroupID: "pg_x"}})
	if cn := p.Candidates("cn"); len(cn) != 0 {
		t.Fatalf("cn candidates after region flip = %d, want 0", len(cn))
	}
	if gl := p.Candidates("global"); len(gl) != 1 {
		t.Fatalf("global candidates after region flip = %d, want 1", len(gl))
	}
}
