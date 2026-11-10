package reqproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"workbuddy2api/internal/coordination"
)

type sharedSnapshot struct {
	State *State           `json:"state"`
	Nodes map[string]*Node `json:"nodes"`
}

// Pool and Store share one lock: all binding, slot, node and configuration
// mutations reload the same PostgreSQL authority before touching a mirror.
type stateMutex struct {
	local  sync.Mutex
	shared *coordination.Store
	store  *Store
	pool   *Pool
	nodes  map[string]*Node
	tx     *coordination.Tx
	before json.RawMessage
	cancel context.CancelFunc
	err    error
	fenced error
}

func (s *stateMutex) Lock() {
	s.local.Lock()
	if s.shared == nil {
		return
	}
	if s.fenced != nil {
		s.err = s.fenced
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	s.cancel = cancel
	s.tx, s.err = s.shared.Begin(ctx, "reqproxy/runtime")
	if s.err == nil {
		var next sharedSnapshot
		if err := json.Unmarshal(s.tx.Value(), &next); err != nil || next.State == nil || next.Nodes == nil {
			s.err = errors.New("invalid shared proxy runtime")
		} else {
			next.State.Normalize()
			s.store.st = next.State
			s.nodes = next.Nodes
			if s.pool != nil {
				p := s.pool
				p.nodes = next.Nodes
				p.slots = map[string]*Slot{}
				p.bindngs = next.State.Bindings
				p.groupRegions = next.State.GroupRegionIndex()
				p.nextPort = p.portMin
				for _, slot := range next.State.Slots {
					cp := slot
					p.slots[slot.ID] = &cp
					if slot.Port >= p.nextPort {
						p.nextPort = slot.Port + 1
					}
				}
			}
			s.before, s.err = json.Marshal(next)
		}
	}
	if s.err != nil {
		s.fenced = s.err
	}
}

func (s *stateMutex) Unlock() {
	if s.tx != nil {
		if s.err == nil {
			nodes := s.nodes
			if s.pool != nil {
				nodes = s.pool.nodes
			}
			raw, err := json.Marshal(sharedSnapshot{State: s.store.st, Nodes: nodes})
			if err == nil && !bytes.Equal(raw, s.before) {
				err = s.tx.Commit(raw)
			} else {
				_ = s.tx.Rollback()
			}
			s.err = err
			if err == nil {
				s.store.dirty = false
			}
		} else {
			_ = s.tx.Rollback()
		}
		s.tx = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.err != nil {
		s.fenced = s.err
	}
	s.local.Unlock()
}

func (s *Store) enableShared(shared *coordination.Store, owner bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := shared.Update(ctx, "reqproxy/runtime", func(raw json.RawMessage) (json.RawMessage, error) {
		if string(raw) != "null" {
			return raw, nil
		}
		if !owner {
			return nil, errors.New("proxy owner has not initialized shared state")
		}
		return json.Marshal(sharedSnapshot{State: s.st, Nodes: map[string]*Node{}})
	})
	if err != nil {
		return err
	}
	s.mu.local.Lock()
	s.mu.shared = shared
	s.mu.store = s
	s.mu.local.Unlock()
	s.mu.Lock()
	s.mu.Unlock()
	s.mu.local.Lock()
	defer s.mu.local.Unlock()
	return s.mu.err
}

func NewSharedManager(cfg Config, kernel *Kernel, shared *coordination.Store, listenIP string, owner bool) (*Manager, error) {
	if shared == nil {
		return nil, errors.New("shared proxy requires coordinator")
	}
	if err := validateListenIP(listenIP); err != nil {
		return nil, err
	}
	if kernel != nil && kernel.listenIP != listenIP {
		return nil, errors.New("proxy kernel listen address differs from relay address")
	}
	return newManager(cfg, kernel, shared, listenIP, owner)
}

func (m *Manager) SharedError() error {
	m.store.mu.local.Lock()
	defer m.store.mu.local.Unlock()
	return m.store.mu.err
}

func (m *Manager) ProxyHost() string {
	if m.listenIP != "" {
		return m.listenIP
	}
	return "127.0.0.1"
}

func proxyHost(dial TempPortDialer) string {
	if h, ok := dial.(interface{ ProxyHost() string }); ok {
		return h.ProxyHost()
	}
	return "127.0.0.1"
}

// ensureLocalSlot installs the authoritative binding in this process's kernel.
// Actual listening ports are local and never written back to shared slots.
func (m *Manager) ensureLocalSlot(uid string) (int, error) {
	m.pool.mu.Lock()
	b, ok := m.pool.bindngs[uid]
	var slot Slot
	var node NodeSpec
	if ok {
		s := m.pool.slots[b.SlotID]
		if s != nil {
			slot = *s
			if n := m.pool.nodes[s.NodeID]; n != nil {
				node = n.NodeSpec
			}
		}
	}
	m.pool.mu.Unlock()
	if err := m.SharedError(); err != nil {
		return 0, err
	}
	if slot.ID == "" || node.ID == "" {
		return 0, ErrNoNode
	}
	if m.kernel == nil {
		return slot.Port, nil
	}
	return m.ensureLocalSlotSpec(slot, node)
}

func (m *Manager) freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(m.ProxyHost(), "0"))
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	err = listener.Close()
	return port, err
}

func (m *Manager) ensureLocalSlotSpec(slot Slot, node NodeSpec) (int, error) {
	if err := m.SharedError(); err != nil {
		return 0, err
	}
	raw, _ := json.Marshal(node)
	m.kernMu.Lock()
	defer m.kernMu.Unlock()
	port := m.localPorts[slot.ID]
	if !m.kernel.HasSlot(slot.ID) {
		var err error
		port, err = m.freeLocalPort()
		if err != nil {
			return 0, err
		}
		if err := m.kernel.AddSlot(slot.ID, port, node); err != nil {
			return 0, fmt.Errorf("local proxy slot: %w", err)
		}
		m.localPorts[slot.ID] = port
	} else if m.localTargets[slot.ID] != string(raw) {
		if err := m.kernel.RetargetSlot(slot.ID, node); err != nil {
			return 0, err
		}
	}
	m.localTargets[slot.ID] = string(raw)
	return port, nil
}
