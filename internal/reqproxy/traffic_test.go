package reqproxy

import "testing"

func TestTrafficRouteCapturesSafeNodeAndGroupIdentity(t *testing.T) {
	m := newTestManager(t)
	m.store.Update(func(st *State) {
		st.ProxyGroups = append(st.ProxyGroups, ProxyGroup{ID: "pg-test", Name: "测试组", Region: "all"})
	})
	m.pool.SetNodes([]NodeSpec{{ID: "node-test", Name: "测试节点", GroupID: "pg-test", Raw: "http://user:secret@127.0.0.1:8080", Spec: map[string]any{"password": "secret"}}})
	m.pool.slots["slot-test"] = &Slot{ID: "slot-test", Port: 31081, NodeID: "node-test"}
	id, name, gid, gname, ok := m.RouteAtPort(31081)
	if !ok || id != "node-test" || name != "测试节点" || gid != "pg-test" || gname != "测试组" {
		t.Fatalf("safe route=%q/%q/%q/%q/%v", id, name, gid, gname, ok)
	}
	if _, _, _, _, ok := m.RouteAtPort(31082); ok {
		t.Fatal("unallocated port returned a node")
	}
	// A later slot migration must not mutate the strings already captured.
	m.pool.nodes["node-new"] = &Node{NodeSpec: NodeSpec{ID: "node-new", Name: "新节点", GroupID: DefaultProxyGroupID}}
	m.pool.slots["slot-test"].NodeID = "node-new"
	newID, _, _, _, _ := m.RouteAtPort(31081)
	if id != "node-test" || newID != "node-new" {
		t.Fatal("route snapshot followed a later migration")
	}
}
