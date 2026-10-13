package reqproxy

// RouteAtPort captures the node actually selected for a request's local slot.
// Only safe identity fields are returned; protocol credentials stay internal.
func (m *Manager) RouteAtPort(port int) (nodeID, nodeName, groupID, groupName string, ok bool) {
	m.pool.mu.Lock()
	for _, slot := range m.pool.slots {
		if slot.Port != port {
			continue
		}
		if n := m.pool.nodes[slot.NodeID]; n != nil {
			nodeID, nodeName, groupID, ok = n.ID, n.Name, n.GroupID, true
		}
		break
	}
	m.pool.mu.Unlock()
	if ok {
		m.store.View(func(st *State) {
			if g := st.ProxyGroupByID(groupID); g != nil {
				groupName = g.Name
			}
		})
	}
	return
}
