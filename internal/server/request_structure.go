package server

// A temporary, bounded companion to error captures. Only fixed-schema counts,
// JSON types, hashes and whitelisted protocol controls survive the request.
// Never retain message text, tool names/schemas, IDs or arbitrary JSON keys.
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const captureStructureLimit = 64
const captureStructureBytes = 32 << 10

func structureHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func structureString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func structureKind(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "missing"
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "invalid"
	}
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func structureControl(raw json.RawMessage) map[string]any {
	v := map[string]any{"kind": structureKind(raw)}
	if len(raw) == 0 {
		return v
	}
	v["sha256"] = structureHash(raw)
	var scalar any
	_ = json.Unmarshal(raw, &scalar)
	switch n := scalar.(type) {
	case bool:
		v["value"] = n
	case float64:
		if n >= 0 && n <= 1e9 {
			v["value"] = n
		}
	case string:
		switch n {
		case "enabled", "disabled", "none", "minimal", "low", "medium", "high", "xhigh", "auto", "required":
			v["value"] = n
		}
	}
	return v
}

func structureBlob(b *captureBlob) map[string]any {
	if b == nil {
		return nil
	}
	out := map[string]any{"bytes": b.Bytes, "sha256": b.SHA256, "complete": b.Complete, "truncated": b.Truncated}
	if b.Truncated || !b.Complete || len(b.raw) == 0 {
		out["analyzed"] = false
		return out
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b.raw, &doc) != nil || doc == nil {
		out["analyzed"] = false
		return out
	}
	out["analyzed"] = true
	controls := map[string]any{}
	for _, key := range []string{"stream", "max_tokens", "max_completion_tokens", "max_output_tokens", "reasoning_effort", "reasoning_summary", "parallel_tool_calls", "tool_choice"} {
		controls[key] = structureControl(doc[key])
	}
	var thinking map[string]json.RawMessage
	_ = json.Unmarshal(doc["thinking"], &thinking)
	controls["thinking"] = map[string]any{"kind": structureKind(doc["thinking"]), "type": structureControl(thinking["type"])}
	out["controls"] = controls
	var tools []json.RawMessage
	_ = json.Unmarshal(doc["tools"], &tools)
	out["tools"] = map[string]any{"count": len(tools), "bytes": len(doc["tools"]), "sha256": structureHash(doc["tools"])}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(doc["messages"], &messages) != nil {
		out["messages_kind"] = structureKind(doc["messages"])
		return out
	}
	roles := map[string]int{}
	reasoningContent, reasoning := map[string]int{}, map[string]int{}
	pending, calls, results := map[string]bool{}, map[string]bool{}, map[string]bool{}
	callCount, resultCount, duplicateCalls, duplicateResults, orphanResults, missingIDs, contentBytes := 0, 0, 0, 0, 0, 0, 0
	for _, message := range messages {
		role := structureString(message["role"])
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			role = "other"
		}
		roles[role]++
		contentBytes += len(message["content"])
		if role == "assistant" {
			for key, target := range map[string]map[string]int{"reasoning_content": reasoningContent, "reasoning": reasoning} {
				kind := structureKind(message[key])
				if kind == "string" && structureString(message[key]) == "" {
					kind = "empty_string"
				}
				target[kind]++
			}
			var toolCalls []map[string]json.RawMessage
			_ = json.Unmarshal(message["tool_calls"], &toolCalls)
			for _, call := range toolCalls {
				callCount++
				id := structureString(call["id"])
				if id == "" {
					missingIDs++
					continue
				}
				if calls[id] {
					duplicateCalls++
				}
				calls[id], pending[id] = true, true
			}
		}
		if role == "tool" {
			resultCount++
			id := structureString(message["tool_call_id"])
			if id == "" {
				missingIDs++
			}
			if results[id] && id != "" {
				duplicateResults++
			}
			results[id] = true
			if id == "" || !pending[id] {
				orphanResults++
			} else {
				delete(pending, id)
			}
		}
	}
	out["messages"] = map[string]any{"count": len(messages), "bytes": len(doc["messages"]), "sha256": structureHash(doc["messages"]), "roles": roles, "content_json_bytes": contentBytes, "assistant_reasoning_content": reasoningContent, "assistant_reasoning": reasoning}
	out["tool_history"] = map[string]any{"calls": callCount, "results": resultCount, "duplicate_call_ids": duplicateCalls, "duplicate_result_ids": duplicateResults, "orphan_results": orphanResults, "unresolved_calls": len(pending), "missing_ids": missingIDs}
	return out
}

func (s *captureSession) structure() map[string]any {
	attempts := make([]map[string]any, 0, len(s.record.Attempts))
	for _, a := range s.record.Attempts {
		attempts = append(attempts, map[string]any{"account": a.Account, "proxy": a.Proxy, "dispatched": a.Dispatched, "http": a.HTTP, "business_code": a.BusinessCode, "transport_error": a.TransportError, "request": structureBlob(a.Request)})
	}
	return map[string]any{
		"id": s.record.ID, "created_at": s.record.Created, "expires_at": time.Now().Add(captureRetention),
		"request_id_sha256": structureHash([]byte(s.record.RequestID)), "model_sha256": structureHash([]byte(s.record.Model)),
		"http": s.record.HTTP, "internal_status": s.record.InternalStatus, "read_incomplete": s.record.ReadIncomplete,
		"incoming": structureBlob(s.record.Incoming), "normalized_chat": structureBlob(s.record.Normalized),
		"attempts": attempts, "attempts_omitted": s.record.AttemptsOmitted,
	}
}
