package promptguard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

// The nonce stays in the encrypted server state, never in writable rule fields.
// It changes only this rule's random stream and survives config edits/restarts.
type weightRefresh struct {
	Nonce []byte    `json:"nonce"`
	At    time.Time `json:"at"`
}

var ErrWeightRefreshConflict = errors.New("配置已被更新，请刷新后重试")
var ErrWeightRefreshStorage = errors.New("权重保存失败，刷新未生效")

func refreshedVariantWeights(r Rule, seed []byte, refresh weightRefresh, at time.Time) []float64 {
	if len(refresh.Nonce) > 0 {
		mac := hmac.New(sha256.New, seed)
		mac.Write([]byte("manual-weight-refresh\x00"))
		mac.Write([]byte(r.ID))
		mac.Write([]byte{0})
		mac.Write(refresh.Nonce)
		seed = mac.Sum(nil)
	}
	return variantWeights(r, seed, at)
}

// RefreshWeights changes probabilities for new sessions only. It shares the
// automatic bounded random algorithm and does not reset its scheduled epoch.
func (m *Manager) RefreshWeights(ruleID string, revision uint64) error {
	return m.refreshWeightsAt(ruleID, revision, time.Now().UTC())
}

func (m *Manager) refreshWeightsAt(ruleID string, revision uint64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("提示词管理已关闭")
	}
	if revision != m.state.Config.Revision {
		return ErrWeightRefreshConflict
	}
	found := false
	for _, r := range m.state.Config.Rules {
		if r.ID != ruleID {
			continue
		}
		positive := 0
		for _, v := range r.Variants {
			if v.Weight > 0 {
				positive++
			}
		}
		if r.WeightRefreshHours <= 0 || positive < 2 {
			return errors.New("请先开启自动刷新，并设置至少两个正权重候选")
		}
		found = true
		break
	}
	if !found {
		return errors.New("替换规则不存在")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	old := copyJSON(m.state)
	if m.state.WeightRefreshes == nil {
		m.state.WeightRefreshes = map[string]weightRefresh{}
	}
	m.state.WeightRefreshes[ruleID] = weightRefresh{Nonce: nonce, At: at}
	m.state.Config.Revision++
	for _, item := range m.state.Cases {
		item.Resolved = false
	}
	if err := m.saveLocked(); err != nil {
		m.state = old
		m.resetBindingsLocked()
		return fmt.Errorf("%w: %v", ErrWeightRefreshStorage, err)
	}
	return nil
}
