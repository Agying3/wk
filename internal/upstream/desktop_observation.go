package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// DesktopChatResult contains observations from exactly one completed request.
// Usage remains absent when the upstream did not provide it.
type DesktopChatResult struct {
	ConversationID string
	RequestID      string
	ModelID        string
	InputLength    int
	StartedAt      int64
	FinishedAt     int64
	FirstTokenAt   int64
	FinishReason   string
	ToolCalls      int
	Usage          map[string]any
}

func (r DesktopChatResult) Events() []DesktopEvent {
	return r.ObserveEvents(DesktopChatSequence(r.ConversationID, r.RequestID,
		"msg-"+r.RequestID[len(r.RequestID)-8:], r.ModelID, r.ModelID))
}

// ObserveEvents updates real chat metrics without changing task-specific
// business fields. Missing measurements must not inherit template counters.
func (r DesktopChatResult) ObserveEvents(events []DesktopEvent) []DesktopEvent {
	for _, ev := range events {
		code, _ := ev["eventCode"].(string)
		response := code == "chat_message_response" || code == "chat_request_response"
		if code == "chat_request_send" {
			ev["inputLength"] = r.InputLength
		}
		if code == "expert_actual_use" {
			ev["characterCount"] = r.InputLength
		}
		if response {
			for _, key := range []string{"inputToken", "outputToken", "totalToken", "cachedTokens", "cachedWriteTokens", "cachedMissTokens"} {
				delete(ev, key)
			}
			for field, aliases := range map[string][]string{
				"inputToken":        {"prompt_tokens", "input_tokens"},
				"outputToken":       {"completion_tokens", "output_tokens"},
				"totalToken":        {"total_tokens"},
				"cachedTokens":      {"prompt_cache_hit_tokens", "cache_read_input_tokens"},
				"cachedWriteTokens": {"cache_creation_input_tokens"},
				"cachedMissTokens":  {"prompt_cache_miss_tokens"},
			} {
				for _, alias := range aliases {
					if value, ok := r.Usage[alias]; ok && value != nil {
						ev[field] = value
						break
					}
				}
			}
			for field, paths := range map[string][][]string{
				"cachedTokens":      {{"prompt_tokens_details", "cached_tokens"}, {"input_tokens_details", "cached_tokens"}},
				"cachedWriteTokens": {{"input_tokens_details", "cache_creation_input_tokens"}, {"input_tokens_details", "cache_write_tokens"}},
			} {
				if _, exists := ev[field]; exists {
					continue
				}
				for _, path := range paths {
					if details, ok := r.Usage[path[0]].(map[string]any); ok {
						if value, known := details[path[1]]; known && value != nil {
							ev[field] = value
							break
						}
					}
				}
			}
			if r.FinishReason != "" {
				ev["finishReason"] = r.FinishReason
				ev["isSuccessful"] = r.FinishReason != "content_filter" && r.FinishReason != "error"
			}
			if code == "chat_message_response" {
				delete(ev, "firstTokenAt")
				if r.FirstTokenAt != 0 {
					ev["firstTokenAt"] = r.FirstTokenAt
				}
			}
			if code == "chat_request_response" {
				ev["toolCallCount"] = r.ToolCalls
			}
		}
		at := r.StartedAt
		if response || code == "chat_message_status" || code == "expert_actual_use" {
			at = r.FinishedAt
		}
		if at != 0 {
			ev["timestamp"] = at
			ev["presentAt"] = at
			ev["reportDelay"] = max(int64(0), time.Now().UnixMilli()-at)
		}
	}
	return events
}

func readDesktopObservation(body io.Reader, result *DesktopChatResult) error {
	reader := bufio.NewReader(body)
	tools := map[int]bool{}
	completed := false
	bytesRead := 0
	for {
		line, err := readBoundedLine(reader, maxSSELineBytes)
		if err != nil && err != io.EOF {
			return err
		}
		bytesRead += len(line)
		if bytesRead > maxAccumulatedBytes {
			return errAccumulatedTooLarge
		}
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				completed = true
				break
			}
			var event map[string]any
			if json.Unmarshal([]byte(payload), &event) == nil {
				if event["error"] != nil {
					return fmt.Errorf("desktop chat stream returned an error")
				}
				if id := ResponseID(event); result.RequestID == "" && desktopIDRegex.MatchString(id) {
					result.RequestID = id
				}
				if model, ok := event["model"].(string); ok && model != "" {
					result.ModelID = model
				}
				if usage, ok := event["usage"].(map[string]any); ok {
					result.Usage = MergeUsage(result.Usage, usage)
				}
				choices, _ := event["choices"].([]any)
				for _, choice := range choices {
					c, _ := choice.(map[string]any)
					if reason, ok := c["finish_reason"].(string); ok && reason != "" {
						result.FinishReason = reason
						completed = true
					}
					delta, _ := c["delta"].(map[string]any)
					if message, ok := c["message"].(map[string]any); ok {
						delta = message
					}
					calls, _ := delta["tool_calls"].([]any)
					for position, call := range calls {
						tc, _ := call.(map[string]any)
						index := position
						if explicit, ok := tc["index"].(float64); ok {
							index = int(explicit)
						}
						tools[index] = true
					}
					if result.FirstTokenAt == 0 && (contentText(delta["content"]) != "" || contentText(delta["reasoning_content"]) != "" || len(calls) > 0) {
						result.FirstTokenAt = time.Now().UnixMilli()
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	result.FinishedAt = time.Now().UnixMilli()
	result.ToolCalls = len(tools)
	if result.RequestID == "" {
		return fmt.Errorf("SSE 中未找到服务端 requestId")
	}
	if !completed {
		return fmt.Errorf("desktop chat ended before a completion event")
	}
	return nil
}
