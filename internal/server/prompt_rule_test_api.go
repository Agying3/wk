package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/lifecycle"
	"workbuddy2api/internal/promptguard"
)

// Keep manual tests bounded even when multiple administrator tabs are open.
var promptRuleTestSlot = make(chan struct{}, 1)

func (h *Handler) promptRuleTestAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "不支持的操作"})
		return
	}
	if h.cfg.PromptGuard == nil {
		writeJSON(w, 503, map[string]any{"error": "提示词管理未启用"})
		return
	}
	var input struct {
		Revision     uint64 `json:"revision"`
		RuleID       string `json:"rule_id"`
		VariantID    string `json:"variant_id"`
		Text         string `json:"text"`
		Upstream     bool   `json:"upstream"`
		Model        string `json:"model"`
		AccountScope struct {
			Kind  string `json:"kind"`
			Group string `json:"group,omitempty"`
		} `json:"account_scope"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil {
		writeJSON(w, 400, map[string]any{"error": "无效或过大的测试请求"})
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeJSON(w, 400, map[string]any{"error": "请求体必须为单个 JSON 对象"})
		return
	}
	config := h.cfg.PromptGuard.Config()
	if input.Revision != config.Revision {
		writeJSON(w, 409, map[string]any{"error": "规则已变化，请关闭测试并重新打开"})
		return
	}
	var rule *promptguard.Rule
	for i := range config.Rules {
		if config.Rules[i].ID == input.RuleID {
			rule = &config.Rules[i]
			break
		}
	}
	if rule == nil {
		writeJSON(w, 404, map[string]any{"error": "规则不存在"})
		return
	}
	preview, err := promptguard.PreviewRule(*rule, input.VariantID, input.Text)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	result := map[string]any{"preview": preview, "revision": config.Revision, "trace": []promptguard.Trace{}}
	if !input.Upstream {
		writeJSON(w, 200, result)
		return
	}
	if strings.TrimSpace(input.Model) == "" || len(input.Model) > 200 || preview.Matches == 0 || preview.Output == input.Text {
		writeJSON(w, 400, map[string]any{"error": "请选择模型，并确保规则命中且产生实际文本变化"})
		return
	}
	scope, err := h.manualPromptScope(input.AccountScope.Kind, input.AccountScope.Group)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	done, admitted := lifecycle.Default.Enter()
	if !admitted {
		writeJSON(w, 503, map[string]any{"error": "服务正在排空，请稍后重试"})
		return
	}
	defer done()
	select {
	case promptRuleTestSlot <- struct{}{}:
		defer func() { <-promptRuleTestSlot }()
	default:
		writeJSON(w, 409, map[string]any{"error": "已有规则测试正在运行，请稍后重试"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 145*time.Second)
	defer cancel()
	session := h.promptSession(ctx, promptguard.Case{Model: input.Model, AccountScope: &scope})
	if session.Close != nil {
		defer session.Close()
	}
	result["selection"] = session.Selection
	trace := []promptguard.Trace{}
	for _, step := range []struct{ name, text string }{{"control", "You are a helpful assistant."}, {"original", input.Text}, {"replacement", preview.Output}} {
		if ctx.Err() != nil {
			result["cancelled"] = true
			break
		}
		if h.cfg.PromptGuard.Config().Revision != config.Revision {
			result["stale"] = true
			break
		}
		started := time.Now()
		probe := session.Probe(ctx, []promptguard.Message{{Role: "system", Content: step.text}}, false)
		trace = append(trace, promptguard.Trace{Step: step.name, Characters: len([]rune(step.text)), DurationMS: time.Since(started).Milliseconds(), ProbeResult: probe})
		if step.name == "control" && probe.Outcome != "pass" || probe.Outcome == "inconclusive" {
			break
		}
		if step.name != "replacement" {
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	result["trace"] = trace
	result["stale"] = h.cfg.PromptGuard.Config().Revision != config.Revision
	result["cancelled"] = ctx.Err() != nil
	writeJSON(w, 200, result)
}
