package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStructureProjectionDoesNotRetainTextOrArbitraryKeys(t *testing.T) {
	raw := []byte(`{"model":"private-model","stream":true,"max_tokens":32768,"reasoning_effort":"high","thinking":{"type":"enabled","secret":"private-thinking"},"tools":[{"type":"function","function":{"name":"private-tool","description":"private-description","parameters":{"private-schema-key":"private-schema"}}}],"messages":[{"role":"system","content":"private-system"},{"role":"assistant","content":"private-answer","reasoning_content":"private-reasoning","reasoning":" ","tool_calls":[{"id":"private-call","function":{"name":"private-tool","arguments":"private-arguments"}}]},{"role":"tool","tool_call_id":"private-call","content":"private-result"},{"role":"tool","tool_call_id":"private-orphan","content":"private-orphan-result"},{"role":"private-role","content":"private-content"}]}`)
	s := &captureSession{remaining: captureRecordBudget}
	got := structureBlob(s.blob(raw, true))
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("projection retained arbitrary request text")
	}
	messages := got["messages"].(map[string]any)
	if messages["count"] != 5 || messages["roles"].(map[string]int)["other"] != 1 {
		t.Fatal("incorrect message counts")
	}
	history := got["tool_history"].(map[string]any)
	if history["calls"] != 1 || history["results"] != 2 || history["orphan_results"] != 1 || history["unresolved_calls"] != 0 {
		t.Fatalf("incorrect pairing: %+v", history)
	}
	controls := got["controls"].(map[string]any)
	if controls["max_tokens"].(map[string]any)["value"] != float64(32768) || controls["reasoning_effort"].(map[string]any)["value"] != "high" {
		t.Fatal("missing whitelisted controls")
	}
	if got["sha256"] != structureHash(raw) {
		t.Fatal("hash does not cover original body")
	}
	large := s.blob([]byte(strings.Repeat("s", captureBodyLimit+1)), true)
	if structureBlob(large)["analyzed"] != false {
		t.Fatal("oversized body must not be parsed")
	}
}

func TestStructureSamplingSuccessIsOptInAndBounded(t *testing.T) {
	m := &captureManager{}
	t.Cleanup(m.clear)
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	s := m.begin(request)
	s.finish()
	if len(m.structures) != 0 {
		t.Fatal("success sampling enabled by default")
	}
	m.mu.Lock()
	m.structureUntil = time.Now().Add(time.Minute)
	m.mu.Unlock()
	for i := 0; i < captureStructureLimit+3; i++ {
		s = m.begin(request)
		s.record.Incoming = s.blob([]byte(`{"messages":[{"role":"user","content":"private-prompt"}]}`), true)
		s.finish()
	}
	if len(m.structures) != captureStructureLimit || len(m.records) != 0 || m.bytes != 0 {
		t.Fatal("structure sampling affects error capture budget")
	}
	for _, row := range m.structures {
		if len(row.data) > captureStructureBytes || strings.Contains(string(row.data), "private-prompt") {
			t.Fatal("unsafe structure retention")
		}
	}
	m.mu.Lock()
	m.structureUntil = time.Now().Add(-time.Second)
	m.mu.Unlock()
	s = m.begin(request)
	s.finish()
	if len(m.structures) != captureStructureLimit {
		t.Fatal("expired sample window accepted a request")
	}
	m.mu.Lock()
	backing := m.structures
	m.expireLocked(time.Now().Add(captureRetention + time.Minute))
	if m.structures != nil || backing[0] != nil {
		t.Fatal("expired structures retain backing references")
	}
	m.mu.Unlock()
}

func TestStructureSamplingAPIAndCancellation(t *testing.T) {
	h := captureTestHandler(t, func(string) (int, string, bool) { return 200, routingSSE, true }, 1)
	for _, invalid := range []string{`{"structure_sample_seconds":-1}`, `{"structure_sample_seconds":1201}`} {
		if w := captureTestCall(t, h, "POST", "/admin/request-capture", invalid); w.Code != 400 {
			t.Fatal("accepted invalid deadline")
		}
	}
	w := captureTestCall(t, h, "POST", "/admin/request-capture", `{"enabled":false,"structure_sample_seconds":60}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	request := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"private-test-content"}],"stream":true}`
	if w := captureTestCall(t, h, "POST", "/v1/chat/completions", request); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = captureTestCall(t, h, "GET", "/admin/request-capture", "")
	var got struct {
		Structures []struct {
			Incoming map[string]any `json:"incoming"`
			Attempts []struct {
				Request map[string]any `json:"request"`
			} `json:"attempts"`
		} `json:"structures"`
		Records  []any `json:"records"`
		Sampling bool  `json:"structure_sampling"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Sampling || len(got.Structures) != 1 || len(got.Records) != 0 || len(got.Structures[0].Attempts) != 1 || got.Structures[0].Attempts[0].Request["analyzed"] != true {
		t.Fatal("missing incoming/actual upstream structure while error capture disabled")
	}
	if strings.Contains(w.Body.String(), "private-test-content") || strings.Contains(w.Body.String(), "private-upstream-token") {
		t.Fatal("API retained content or credentials")
	}
	// Configuration changes discard already-started samples just like error captures.
	s := h.requestCapture.begin(httptest.NewRequest("POST", "/v1/chat/completions", nil))
	captureTestCall(t, h, "POST", "/admin/request-capture", `{"structure_sample_seconds":0}`)
	s.finish()
	if len(h.requestCapture.structures) != 1 {
		t.Fatal("disabled sampling retained in-flight request")
	}
	captureTestCall(t, h, "DELETE", "/admin/request-capture", "")
	if len(h.requestCapture.structures) != 0 || !h.requestCapture.structureUntil.IsZero() {
		t.Fatal("clear did not remove structures and deadline")
	}
}
