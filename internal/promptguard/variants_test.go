package promptguard

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"workbuddy2api/internal/session"
)

func variantDocument() map[string]any {
	return map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "original original"},
		map[string]any{"role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "original"}}},
		map[string]any{"role": "user", "content": "original"},
	}}
}

func variantRule() Rule {
	return Rule{ID: "rule", Enabled: true, Match: "original", WeightRefreshHours: 6,
		Variants: []Variant{{ID: "a", Replacement: "alpha", Weight: 1}, {ID: "b", Replacement: "beta", Weight: 1}, {ID: "c", Replacement: "gamma", Weight: 1}}}
}

func TestVariantWeightsBoundedRandomAndManual(t *testing.T) {
	r := variantRule()
	r.Variants[0].Weight = 1000000
	r.Variants = append(r.Variants, Variant{ID: "zero", Weight: 0})
	seed := []byte("test-seed")
	changed := false
	first := variantWeights(r, seed, time.Unix(0, 0))
	for epoch := range 1000 {
		at := time.Unix(int64(epoch)*6*3600, 0)
		w := variantWeights(r, seed, at)
		if !reflect.DeepEqual(w, variantWeights(r, seed, at.Add(time.Hour))) {
			t.Fatal("weights changed inside epoch")
		}
		sum := 0.0
		for i, value := range w {
			if i == 3 {
				if value != 0 {
					t.Fatal("zero weight activated")
				}
				continue
			}
			if value < 0.5/3-1e-12 || value > 1.5/3+1e-12 {
				t.Fatal("unbounded", w)
			}
			sum += value
		}
		if math.Abs(sum-1) > 1e-12 {
			t.Fatal("not normalized", w)
		}
		changed = changed || !reflect.DeepEqual(first, w)
	}
	if !changed {
		t.Fatal("extreme base weights removed randomness")
	}
	r.WeightRefreshHours = 0
	r.Variants[0].Weight, r.Variants[1].Weight, r.Variants[2].Weight = 1, 3, 0
	w := variantWeights(r, seed, time.Now())
	if !reflect.DeepEqual(w, []float64{0.25, 0.75, 0, 0}) {
		t.Fatal(w)
	}
	counts := map[string]int{}
	for i := range 10000 {
		counts[pickVariant(r, w, (float64(i)+0.5)/10000).ID]++
	}
	if counts["a"] != 2500 || counts["b"] != 7500 || counts["c"] != 0 {
		t.Fatal(counts)
	}
}

func TestVariantSessionPersistsAcrossEpochRestartAndWeightEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, err := Open(path, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	c := m.Config()
	c.Rules = []Rule{variantRule()}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(6*3600*100, 0).UTC()
	doc := variantDocument()
	if err := m.applySessionAt(doc, "tenant/session", at); err != nil {
		t.Fatal(err)
	}
	want := Text(Extract(doc))
	parts := strings.Fields(want)
	if len(parts) != 3 || parts[0] != parts[1] || parts[0] != parts[2] {
		t.Fatal("mixed candidates within request", want)
	}
	if doc["messages"].([]any)[2].(map[string]any)["content"] != "original" {
		t.Fatal("user rewritten")
	}
	m.Close()
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "tenant/session") || strings.Contains(string(b), "alpha") {
		t.Fatal("plaintext persisted")
	}
	m, err = Open(path, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c = m.Config()
	for i := range c.Rules[0].Variants {
		c.Rules[0].Variants[i].Weight = 0
	}
	c.Rules[0].Variants = append(c.Rules[0].Variants, Variant{ID: "new", Replacement: "new-only", Weight: 10})
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := variantDocument()
			if err := m.applySessionAt(d, "tenant/session", at.Add(24*time.Hour)); err != nil {
				t.Error(err)
				return
			}
			if Text(Extract(d)) != want {
				t.Errorf("session changed: %s", Text(Extract(d)))
			}
		}(i)
	}
	wg.Wait()
	doc = variantDocument()
	if err := m.applySessionAt(doc, "different-session", at.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if Text(Extract(doc)) != "new-only new-only\nnew-only" {
		t.Fatal("new session ignored weights", doc)
	}
	doc = variantDocument()
	if err := m.applySessionAt(doc, "tenant/session", at.Add(32*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if Text(Extract(doc)) != "new-only new-only\nnew-only" {
		t.Fatal("idle session did not expire")
	}
}

func TestVariantEmptyDeletionChainingAndCandidateRemoval(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Rules = []Rule{{ID: "delete", Match: "original", Enabled: true, RequireEndBoundary: true,
		Variants: []Variant{{ID: "empty", Replacement: "", Weight: 1}}}}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	doc := variantDocument()
	if err := m.ApplySession(doc, "stable"); err != nil {
		t.Fatal(err)
	}
	if got := Text(Extract(doc)); got != " \n" {
		t.Fatal("empty replacement not applied", got)
	}
	c = m.Config()
	c.Rules[0].Variants = []Variant{{ID: "removed-and-replaced", Replacement: "next", Weight: 1}}
	c.Rules = append(c.Rules, Rule{ID: "chain", Match: "next", Replacement: "final", Enabled: true})
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	doc = variantDocument()
	if err := m.ApplySession(doc, "stable"); err != nil {
		t.Fatal(err)
	}
	if got := Text(Extract(doc)); got != "final final\nfinal" {
		t.Fatal("removed candidate or chaining broken", got)
	}
	c = m.Config()
	c.Rules = c.Rules[1:]
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.state.PromptSessions {
		if _, exists := s.Value["delete"]; exists {
			t.Fatal("deleted rule retained in session map")
		}
	}
}

func TestVariantConcurrentFirstRequestPinnedAndSaveFailureRollsBack(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Rules = []Rule{variantRule()}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 40)
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := variantDocument()
			if err := m.ApplySession(d, "same"); err != nil {
				t.Error(err)
			}
			results <- Text(Extract(d))
		}()
	}
	wg.Wait()
	close(results)
	want := ""
	for got := range results {
		if want == "" {
			want = got
		}
		if want != got {
			t.Fatal("concurrent first allocation diverged")
		}
	}
	if len(m.state.PromptSessions) != 1 {
		t.Fatal("extra bindings")
	}
	// Force persistence to fail before serialization/encryption; no fallback to
	// per-request random choice may be returned as a successful request.
	m.state.Cases["invalid"] = &Case{Original: []Message{{Content: make(chan int)}}}
	m.path = filepath.Join(t.TempDir(), "cannot-save.enc")
	if err := m.ApplySession(variantDocument(), "new"); err == nil {
		t.Fatal("save failure hidden")
	}
	if len(m.state.PromptSessions) != 1 {
		t.Fatal("failed allocation retained")
	}
	m.path = ""
	delete(m.state.Cases, "invalid")
}

func TestVariantValidationAndReadOnlyEffectiveFields(t *testing.T) {
	m := memory(t)
	for _, change := range []func(*Rule){
		func(r *Rule) {
			for i := range r.Variants {
				r.Variants[i].Weight = 0
			}
		},
		func(r *Rule) { r.Variants[0].Weight = -1 },
		func(r *Rule) { r.Variants[0].ID = r.Variants[1].ID },
		func(r *Rule) { r.WeightRefreshHours = -1 },
		func(r *Rule) { r.WeightRefreshHours = 8761 },
		func(r *Rule) { r.Variants[0].Replacement = strings.Repeat("x", 1<<20+1) },
	} {
		c := m.Config()
		r := variantRule()
		change(&r)
		c.Rules = []Rule{r}
		if err := m.Update(c); err == nil {
			t.Fatal("invalid variant accepted")
		}
	}
	c := m.Config()
	r := variantRule()
	r.Variants[0].ID = ""
	c.Rules = []Rule{r}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	c = m.Config()
	if c.Rules[0].Variants[0].ID == "" || len(c.Rules[0].EffectiveWeights) != 3 || c.Rules[0].NextWeightRefresh == nil {
		t.Fatal("missing config metadata")
	}
	c.Rules[0].EffectiveWeights = []float64{1, 0, 0}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	if len(m.state.Config.Rules[0].EffectiveWeights) != 0 {
		t.Fatal("client overwrote computed weights")
	}
}

func TestVariantExpiryCapacityAndDiagnosticIsolation(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Rules = []Rule{variantRule()}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	for i := range promptSessionLimit {
		m.state.PromptSessions[fmt.Sprint(i)] = promptSession{LastSeen: at}
	}
	if err := m.ApplySession(variantDocument(), "overflow"); err == nil {
		t.Fatal("capacity silently evicted active sessions")
	}
	for k, s := range m.state.PromptSessions {
		s.LastSeen = at.Add(-31 * 24 * time.Hour)
		m.state.PromptSessions[k] = s
	}
	if err := m.ApplySession(variantDocument(), "after-expiry"); err != nil {
		t.Fatal(err)
	}
	if len(m.state.PromptSessions) != 1 {
		t.Fatal("expired sessions not reclaimed")
	}
	doc1, doc2 := variantDocument(), variantDocument()
	if err := m.ApplyDiagnostic(doc1, "case", at); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyDiagnostic(doc2, "case", at); err != nil {
		t.Fatal(err)
	}
	b1, _ := json.Marshal(doc1)
	b2, _ := json.Marshal(doc2)
	if string(b1) != string(b2) || len(m.state.PromptSessions) != 1 {
		t.Fatal("diagnostic choice unstable or used business capacity")
	}
}

func TestVariantLegacyBindingsMigrateWithoutReselection(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "guard.enc")
			m, err := Open(path, "fixture-secret")
			if err != nil {
				t.Fatal(err)
			}
			c := m.Config()
			r := variantRule()
			r.WeightRefreshHours = 0
			r.Variants[0].Weight = 0
			c.Rules = []Rule{r}
			if err := m.Update(c); err != nil {
				t.Fatal(err)
			}
			doc := variantDocument()
			doc["messages"] = append(doc["messages"].([]any), map[string]any{"role": "user", "content": "task"})
			if explicit {
				doc["conversation_id"] = "old-conversation"
			}
			legacy := m.LegacySessionKey("tenant", doc, nil)
			m.state.PromptSessions[legacy] = promptSession{Value: map[string]string{"rule": "a"}, LastSeen: time.Now()}
			if err := m.saveLocked(); err != nil {
				t.Fatal(err)
			}
			m.Close()
			m, err = Open(path, "fixture-secret")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			id := session.NewResolver([]byte("fixed-new-shared-salt")).Resolve("tenant", doc, doc, nil)
			if err := m.ApplyIdentity(doc, id, legacy); err != nil {
				t.Fatal(err)
			}
			if Text(Extract(doc)) != "alpha alpha\nalpha" {
				t.Fatal("zero-weight legacy choice rerolled during migration")
			}
			if _, ok := m.state.PromptSessions[legacy]; ok || len(m.state.PromptSessions) != 1 {
				t.Fatal("migration duplicated binding capacity")
			}
		})
	}
}
