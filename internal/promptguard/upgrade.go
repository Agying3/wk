package promptguard

// FlushChecked is called only after the upgrade barrier has drained probes.
func (m *Manager) FlushChecked() (err error) {
	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		if sharedErr := m.SharedError(); sharedErr != nil {
			err = sharedErr
		}
	}()
	return m.saveLocked()
}
