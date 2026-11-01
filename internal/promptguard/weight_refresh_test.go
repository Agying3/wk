package promptguard

import (
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestImmediateWeightRefreshDurableScopedAndSticky(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, err := Open(path, "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	c := m.Config()
	r := variantRule()
	r.Variants = append(r.Variants, Variant{ID: "zero", Replacement: "excluded", Weight: 0})
	other := variantRule()
	other.ID = "other"
	other.Match = "different"
	c.Rules = []Rule{r, other}
	if err = m.Update(c); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	doc := variantDocument()
	if err = m.applySessionAt(doc, "stable", at); err != nil {
		t.Fatal(err)
	}
	want := Text(Extract(doc))
	bindings := copyJSON(m.state.PromptSessions)
	rules := copyJSON(m.state.Config.Rules)
	old := refreshedVariantWeights(r, m.state.SelectionSeed, m.state.WeightRefreshes[r.ID], at)
	otherWeights := refreshedVariantWeights(other, m.state.SelectionSeed, m.state.WeightRefreshes[other.ID], at)
	revision := m.Config().Revision
	m.state.Cases["verified"] = &Case{ID: "verified", Resolved: true, LastSeen: at}
	if err = m.refreshWeightsAt(r.ID, revision, at); err != nil {
		t.Fatal(err)
	}
	weights := refreshedVariantWeights(r, m.state.SelectionSeed, m.state.WeightRefreshes[r.ID], at)
	if reflect.DeepEqual(weights, old) || !reflect.DeepEqual(otherWeights, refreshedVariantWeights(other, m.state.SelectionSeed, m.state.WeightRefreshes[other.ID], at)) {
		t.Fatal("refresh did not affect only the selected rule")
	}
	if !reflect.DeepEqual(bindings, m.state.PromptSessions) || !reflect.DeepEqual(rules, m.state.Config.Rules) || m.Config().Revision != revision+1 || m.state.Cases["verified"].Resolved {
		t.Fatal("refresh changed binding/rule data or failed to advance validation revision")
	}
	if err = m.RefreshWeights(r.ID, revision); !errors.Is(err, ErrWeightRefreshConflict) {
		t.Fatal("stale refresh accepted", err)
	}
	if m.Config().Rules[0].LastWeightRefresh == nil || !m.Config().Rules[0].LastWeightRefresh.Equal(at) {
		t.Fatal("missing manual refresh timestamp")
	}
	m.Close()
	m, err = Open(path, "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if !reflect.DeepEqual(weights, refreshedVariantWeights(r, m.state.SelectionSeed, m.state.WeightRefreshes[r.ID], at)) {
		t.Fatal("restart lost refreshed weights")
	}
	doc = variantDocument()
	if err = m.applySessionAt(doc, "stable", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if Text(Extract(doc)) != want {
		t.Fatal("refresh rerolled an existing conversation")
	}
	// Normal config round trips cannot inject computed fields or reset refresh state.
	c = m.Config()
	c.Rules[0].LastWeightRefresh = nil
	if err = m.Update(c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(weights, refreshedVariantWeights(r, m.state.SelectionSeed, m.state.WeightRefreshes[r.ID], at)) {
		t.Fatal("config save reset manual random stream")
	}
	// The next wall-clock epoch continues to use the same bounded algorithm.
	for i := range 100 {
		w := refreshedVariantWeights(r, m.state.SelectionSeed, m.state.WeightRefreshes[r.ID], at.Add(time.Duration(i)*6*time.Hour))
		sum := 0.0
		for j, p := range w {
			if j == 3 {
				if p != 0 {
					t.Fatal("zero weight activated")
				}
				continue
			}
			if p < 0.5/3-1e-12 || p > 1.5/3+1e-12 {
				t.Fatal("refresh broke balance bounds", w)
			}
			sum += p
		}
		if math.Abs(sum-1) > 1e-12 {
			t.Fatal("probabilities do not sum to one")
		}
	}
}

func TestImmediateWeightRefreshRollbackAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.enc")
	m, err := Open(path, "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := m.Config()
	c.Rules = []Rule{variantRule()}
	if err = m.Update(c); err != nil {
		t.Fatal(err)
	}
	if err = m.RefreshWeights("missing", m.Config().Revision); err == nil {
		t.Fatal("missing rule accepted")
	}
	for range 2 {
		before := copyJSON(m.state)
		m.path = t.TempDir() // Atomic replacement of an existing directory must fail.
		if err = m.RefreshWeights("rule", m.Config().Revision); !errors.Is(err, ErrWeightRefreshStorage) {
			t.Fatal("persistence failure hidden", err)
		}
		if !reflect.DeepEqual(before, copyJSON(m.state)) {
			t.Fatal("failed refresh leaked nonce, revision or validation changes")
		}
		m.path = path
		if err = m.RefreshWeights("rule", m.Config().Revision); err != nil {
			t.Fatal("retry after failed refresh did not recover", err)
		}
	}
	if err = m.ApplySession(variantDocument(), "after-rollback"); err != nil {
		t.Fatal(err)
	}
	if len(m.state.PromptSessions) != 1 {
		t.Fatal("binding map detached from persistent state after rollback")
	}
	for _, manual := range []bool{true, false} {
		c = m.Config()
		if manual {
			c.Rules[0].WeightRefreshHours = 0
		} else {
			c.Rules[0].WeightRefreshHours = 6
			c.Rules[0].Variants[1].Weight = 0
			c.Rules[0].Variants[2].Weight = 0
		}
		if err = m.Update(c); err != nil {
			t.Fatal(err)
		}
		if err = m.RefreshWeights("rule", m.Config().Revision); err == nil {
			t.Fatal("non-random/futile refresh accepted")
		}
	}
	c = m.Config()
	c.Rules = nil
	if err = m.Update(c); err != nil {
		t.Fatal(err)
	}
	if len(m.state.WeightRefreshes) != 0 {
		t.Fatal("deleted rules retained unbounded refresh state")
	}
}

func TestImmediateWeightRefreshConcurrentRevision(t *testing.T) {
	m := memory(t)
	c := m.Config()
	c.Rules = []Rule{variantRule()}
	if err := m.Update(c); err != nil {
		t.Fatal(err)
	}
	revision := m.Config().Revision
	var wg sync.WaitGroup
	results := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- m.RefreshWeights("rule", revision) }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrWeightRefreshConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 || m.Config().Revision != revision+1 {
		t.Fatal("duplicate refresh passed revision guard", success)
	}
}
