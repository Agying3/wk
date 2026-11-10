package reqproxy

import "fmt"

func (m *Manager) Ready() bool {
	if m.SharedError() != nil {
		return false
	}
	select {
	case <-m.initialized:
	default:
		return false
	}
	if len(m.pool.NodesSnapshot()) == 0 {
		return m.SharedError() == nil
	}
	ready := len(m.pool.Candidates("cn"))+len(m.pool.Candidates("global")) > 0
	return ready && m.SharedError() == nil
}

func (m *Manager) FlushChecked() error {
	m.store.Flush()
	if err := m.SharedError(); err != nil {
		return err
	}
	m.store.mu.Lock()
	defer m.store.mu.Unlock()
	if m.store.dirty {
		return fmt.Errorf("proxy state persistence failed")
	}
	return nil
}
