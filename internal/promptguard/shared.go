package promptguard

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"workbuddy2api/internal/coordination"
)

// guardMutex keeps the existing small state critical sections atomic across
// overlapping releases. No transaction is held during an upstream request.
type guardMutex struct {
	local   sync.Mutex
	manager *Manager
	shared  *coordination.Store
	tx      *coordination.Tx
	cancel  context.CancelFunc
	err     error
	fenced  error
	written bool
}

func (g *guardMutex) Lock() {
	g.local.Lock()
	if g.shared == nil {
		return
	}
	if g.fenced != nil {
		g.err = g.fenced
		return
	}
	g.written = false
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	g.cancel = cancel
	g.tx, g.err = g.shared.Begin(ctx, "promptguard/state")
	if g.err == nil {
		g.err = g.manager.loadSharedLocked(g.tx.Value())
	}
	if g.err != nil {
		g.fenced = g.err
		g.manager.storageError = "提示词共享状态不可用"
	}
}

func (g *guardMutex) Unlock() {
	if g.tx != nil {
		if g.err == nil && g.written {
			g.err = g.tx.CommitCurrent()
		} else {
			_ = g.tx.Rollback()
		}
		if g.err != nil {
			g.manager.storageError = "提示词共享状态提交失败"
		}
		g.tx = nil
	}
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
	if g.err != nil {
		g.fenced = g.err
	}
	g.local.Unlock()
}

// OpenShared imports the encrypted legacy file only for the confirmed owner.
// Relays require an existing authority and do not perform restart recovery.
func OpenShared(path, secret string, shared *coordination.Store, owner bool) (*Manager, error) {
	if shared == nil || path == "" || secret == "" {
		return nil, errors.New("prompt guard shared storage requires coordinator, path and secret")
	}
	m, err := open(path, secret, true)
	if err != nil {
		return nil, err
	}
	m.mu.shared = shared
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = shared.Update(ctx, "promptguard/state", func(raw json.RawMessage) (json.RawMessage, error) {
		if string(raw) != "null" {
			return raw, m.loadSharedLocked(raw)
		}
		if !owner {
			return nil, errors.New("prompt guard owner has not initialized shared state")
		}
		return m.sealSharedLocked()
	})
	if err != nil {
		return nil, err
	}
	if owner {
		if err = m.ActivateBackground(); err != nil {
			return nil, err
		}
	}
	if err = m.SharedError(); err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) SharedError() error {
	m.mu.local.Lock()
	defer m.mu.local.Unlock()
	return m.mu.err
}

func (m *Manager) loadSharedLocked(raw json.RawMessage) error {
	var sealed []byte
	if json.Unmarshal(raw, &sealed) != nil || len(sealed) < m.aead.NonceSize() {
		return errors.New("invalid encrypted shared prompt guard state")
	}
	n := m.aead.NonceSize()
	plain, err := m.aead.Open(nil, sealed[:n], sealed[n:], []byte("prompt-guard-v1"))
	if err != nil {
		return errors.New("cannot decrypt shared prompt guard state")
	}
	var next diskState
	if json.Unmarshal(plain, &next) != nil || next.Cases == nil || len(next.SelectionSeed) == 0 {
		return errors.New("invalid shared prompt guard state")
	}
	m.state = next
	m.resetBindingsLocked()
	return nil
}

func (m *Manager) sealSharedLocked() (json.RawMessage, error) {
	plain, err := json.Marshal(m.state)
	if err != nil {
		return nil, err
	}
	if len(plain) > 64<<20 {
		return nil, errors.New("prompt guard storage exceeds 64 MiB")
	}
	nonce := make([]byte, m.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return json.Marshal(m.aead.Seal(nonce, nonce, plain, []byte("prompt-guard-v1")))
}

func (m *Manager) saveSharedLocked() error {
	if m.mu.err != nil {
		return m.mu.err
	}
	if m.mu.tx == nil {
		return errors.New("prompt guard shared write outside transaction")
	}
	raw, err := m.sealSharedLocked()
	if err == nil {
		err = m.mu.tx.Write(raw)
	}
	if err != nil {
		m.mu.err = err
		m.storageError = "提示词共享状态写入失败"
		return err
	}
	m.mu.written = true
	m.storageError = ""
	return nil
}
