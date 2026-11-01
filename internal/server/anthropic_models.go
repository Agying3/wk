package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Both APIs share the models path. A version header also supports Anthropic
// clients configured with Bearer authentication instead of x-api-key.
func isAnthropicModelsRequest(r *http.Request) bool {
	return r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != ""
}

func (h *Handler) withModelsAuth(next http.HandlerFunc) http.HandlerFunc {
	openai, anthropic := h.withAuth(next), h.withAnthropicAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if isAnthropicModelsRequest(r) {
			anthropic(w, r)
		} else {
			openai(w, r)
		}
	}
}

func anthropicModelInfo(model map[string]any) map[string]any {
	created, _ := numberInt(model["created"])
	out := map[string]any{
		"type": "model", "id": model["id"], "display_name": model["id"],
		"created_at":   time.Unix(int64(created), 0).UTC().Format(time.RFC3339),
		"capabilities": nil,
	}
	if n, ok := numberInt(model["context_length"]); ok && n > 0 {
		out["max_input_tokens"] = n
	}
	if n, ok := numberInt(model["max_output_tokens"]); ok && n > 0 {
		out["max_tokens"] = n
	}
	return out
}

func (h *Handler) model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model")
	for _, item := range h.modelsForRequest(r) {
		if item["id"] == id {
			if isAnthropicModelsRequest(r) {
				item = anthropicModelInfo(item)
			}
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
	if isAnthropicModelsRequest(r) {
		anthropicError(w, http.StatusNotFound, "not_found_error", "model not found")
	} else {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model not found")
	}
}

func writeAnthropicModels(w http.ResponseWriter, r *http.Request, models []map[string]any) {
	page, err := anthropicModelsPage(r, models)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func anthropicModelsPage(r *http.Request, models []map[string]any) (map[string]any, error) {
	query := r.URL.Query()
	limit := 20
	if raw, present := query["limit"]; present {
		var err error
		if len(raw) != 1 {
			return nil, fmt.Errorf("limit must be an integer from 1 to 1000")
		}
		limit, err = strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > 1000 {
			return nil, fmt.Errorf("limit must be an integer from 1 to 1000")
		}
	}
	after, before := query.Get("after_id"), query.Get("before_id")
	if after != "" && before != "" {
		return nil, fmt.Errorf("after_id and before_id are mutually exclusive")
	}
	// Sort a copy, leaving OpenAI output and shared static metadata untouched.
	ordered := append([]map[string]any(nil), models...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, _ := numberInt(ordered[i]["created"])
		b, _ := numberInt(ordered[j]["created"])
		if a != b {
			return a > b
		}
		return fmt.Sprint(ordered[i]["id"]) < fmt.Sprint(ordered[j]["id"])
	})
	start, end := 0, len(ordered)
	cursor := after
	if before != "" {
		cursor = before
	}
	if cursor != "" {
		index := -1
		for i, item := range ordered {
			if item["id"] == cursor {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("pagination cursor not found")
		}
		if after != "" {
			start = index + 1
		} else {
			end = index
		}
	}
	more := false
	if before != "" {
		if end-start > limit {
			start = end - limit
			more = true
		}
	} else if end-start > limit {
		end = start + limit
		more = true
	}
	data := make([]map[string]any, 0, end-start)
	for _, item := range ordered[start:end] {
		data = append(data, anthropicModelInfo(item))
	}
	var first, last any
	if len(data) > 0 {
		first, last = data[0]["id"], data[len(data)-1]["id"]
	}
	return map[string]any{"data": data, "has_more": more, "first_id": first, "last_id": last}, nil
}
