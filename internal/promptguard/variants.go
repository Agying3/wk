package promptguard

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
)

type Variant struct {
	ID          string `json:"id"`
	Replacement string `json:"replacement"`
	Weight      int    `json:"weight"`
}

type promptSession struct {
	Choices  map[string]string `json:"choices"`
	LastSeen time.Time         `json:"last_seen"`
}

const promptSessionLimit = 10000
const promptSessionTTL = 30 * 24 * time.Hour

func validateVariants(r *Rule) error {
	if r.WeightRefreshHours < 0 || r.WeightRefreshHours > 8760 {
		return errors.New("权重刷新间隔须为 0（关闭）或 1–8760 小时")
	}
	if len(r.Variants) > 32 {
		return errors.New("每条规则最多 32 个替换候选")
	}
	ids, positive := map[string]bool{}, false
	for i := range r.Variants {
		v := &r.Variants[i]
		if len(v.Replacement) > 1<<20 || v.Weight < 0 || v.Weight > 1000000 {
			return errors.New("每个候选最多 1 MiB，权重须为 0–1000000 的整数")
		}
		if v.ID == "" {
			var b [12]byte
			if _, err := rand.Read(b[:]); err != nil {
				return err
			}
			v.ID = hex.EncodeToString(b[:])
		}
		if len(v.ID) > 100 || ids[v.ID] {
			return errors.New("候选 ID 重复或超过 100 字符")
		}
		ids[v.ID] = true
		positive = positive || v.Weight > 0
	}
	if len(r.Variants) > 0 && !positive {
		return errors.New("至少一个替换候选的权重须大于 0")
	}
	return nil
}

// Domain-separated keyed randomness keeps epoch weights stable across restarts
// without exposing the seed or depending on a timer firing at the right time.
func keyedUnit(seed []byte, parts ...any) float64 {
	mac := hmac.New(sha256.New, seed)
	for _, part := range parts {
		fmt.Fprintf(mac, "%q\x00", fmt.Sprint(part))
	}
	return float64(binary.BigEndian.Uint64(mac.Sum(nil))>>11) / (1 << 53)
}

// Manual mode preserves exact relative weights. Automatic mode perturbs base
// weights, then projects onto a capped simplex: sum(p)=1 and each eligible
// candidate receives between 0.5/n and 1.5/n. Weight zero excludes new sessions.
func variantWeights(r Rule, seed []byte, now time.Time) []float64 {
	w := make([]float64, len(r.Variants))
	n, sum := 0, 0.0
	maxWeight := 1
	for _, v := range r.Variants {
		if v.Weight > maxWeight {
			maxWeight = v.Weight
		}
	}
	for i, v := range r.Variants {
		if v.Weight <= 0 {
			continue
		}
		w[i] = float64(v.Weight)
		if r.WeightRefreshHours > 0 {
			epoch := now.Unix() / (int64(r.WeightRefreshHours) * 3600)
			// Limit the base preference ratio to two before randomization, so
			// extreme manual weights cannot pin every epoch to the same cap.
			w[i] = math.Max(0.5, w[i]/float64(maxWeight)) * (0.5 + keyedUnit(seed, "weight", r.ID, v.ID, epoch))
		}
		sum += w[i]
		n++
	}
	if n == 0 {
		return w
	}
	if r.WeightRefreshHours == 0 {
		for i := range w {
			w[i] /= sum
		}
		return w
	}
	floor, ceiling := 0.5/float64(n), 1.5/float64(n)
	lo, hi := 0.0, 6.0 // every eligible jittered score is >= 0.25
	for range 80 {
		mid, total := (lo+hi)/2, 0.0
		for _, value := range w {
			if value > 0 {
				total += math.Min(ceiling, math.Max(floor, mid*value))
			}
		}
		if total < 1 {
			lo = mid
		} else {
			hi = mid
		}
	}
	for i, value := range w {
		if value > 0 {
			w[i] = math.Min(ceiling, math.Max(floor, (lo+hi)/2*value))
		}
	}
	return w
}

func pickVariant(r Rule, weights []float64, unit float64) Variant {
	last := Variant{}
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		last = r.Variants[i]
		unit -= w
		if unit < 0 {
			return last
		}
	}
	return last // floating-point roundoff at the end of the CDF
}

func (m *Manager) ApplySession(doc map[string]any, session string) error {
	return m.applySessionAt(doc, session, time.Now().UTC())
}

func (m *Manager) applySessionAt(doc map[string]any, session string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("prompt guard closed")
	}
	key := ""
	if session != "" {
		mac := hmac.New(sha256.New, m.state.SelectionSeed)
		mac.Write([]byte("prompt-session\x00" + session))
		key = hex.EncodeToString(mac.Sum(nil))
	}
	old, exists := m.state.PromptSessions[key]
	entry := copyJSON(old)
	if entry.Choices == nil || now.Sub(entry.LastSeen) > promptSessionTTL {
		entry = promptSession{Choices: map[string]string{}}
	}
	selected := map[string]string{}
	used, changed := false, false
	err := applyToDocument(doc, m.state.Config.Rules, func(r Rule) (string, error) {
		if len(r.Variants) == 0 {
			return r.Replacement, nil
		}
		used = true
		if text, ok := selected[r.ID]; ok {
			return text, nil
		}
		for _, v := range r.Variants {
			if key != "" && entry.Choices[r.ID] == v.ID {
				selected[r.ID] = v.Replacement
				return v.Replacement, nil
			}
		}
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		unit := float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
		v := pickVariant(r, variantWeights(r, m.state.SelectionSeed, now), unit)
		if v.ID == "" {
			return "", errors.New("no eligible prompt variant")
		}
		selected[r.ID] = v.Replacement
		entry.Choices[r.ID] = v.ID
		changed = true
		return v.Replacement, nil
	})
	if err != nil || !used || key == "" {
		return err
	}
	if !changed && now.Sub(old.LastSeen) < time.Hour {
		return nil
	}
	if !exists && len(m.state.PromptSessions) >= promptSessionLimit {
		for k, v := range m.state.PromptSessions {
			if now.Sub(v.LastSeen) > promptSessionTTL {
				delete(m.state.PromptSessions, k)
			}
		}
		if len(m.state.PromptSessions) >= promptSessionLimit {
			return errors.New("prompt session capacity reached; existing sessions are preserved")
		}
	}
	entry.LastSeen = now
	m.state.PromptSessions[key] = entry
	if err := m.saveLocked(); err != nil {
		if exists {
			m.state.PromptSessions[key] = old
		} else {
			delete(m.state.PromptSessions, key)
		}
		return err
	}
	return nil
}

// Diagnostic rewrites use a deterministic choice per case, revision and job
// start time. They neither consume business-session capacity nor change choice
// while a job crosses an epoch boundary. Passing one combination does not
// validate every candidate combination.
func (m *Manager) ApplyDiagnostic(doc map[string]any, caseID string, at time.Time) error {
	m.mu.Lock()
	rules := copyJSON(m.state.Config.Rules)
	seed := append([]byte(nil), m.state.SelectionSeed...)
	revision := m.state.Config.Revision
	m.mu.Unlock()
	return applyToDocument(doc, rules, func(r Rule) (string, error) {
		if len(r.Variants) == 0 {
			return r.Replacement, nil
		}
		v := pickVariant(r, variantWeights(r, seed, at), keyedUnit(seed, "diagnostic", caseID, revision, r.ID))
		return v.Replacement, nil
	})
}
