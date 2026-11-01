package upstream

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// BusinessCode reads provider codes without interpreting them as a new retry
// policy. Unknown codes remain observable and keep existing classification.
func BusinessCode(raw []byte) int {
	if len(raw) > 1<<20 || !bytes.Contains(raw, []byte(`"code"`)) {
		return 0
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return 0
	}
	var walk func(map[string]json.RawMessage, int) int
	walk = func(node map[string]json.RawMessage, depth int) int {
		if depth == 0 {
			return 0
		}
		if value := node["code"]; value != nil {
			s := string(value)
			if len(s) != 0 && s[0] == '"' {
				_ = json.Unmarshal(value, &s)
			}
			s = strings.TrimSpace(s)
			if n, err := strconv.ParseInt(s, 10, 32); err == nil && n >= 1000 {
				return int(n)
			}
		}
		for _, key := range []string{"data", "error", "response"} {
			var child map[string]json.RawMessage
			if json.Unmarshal(node[key], &child) == nil {
				if code := walk(child, depth-1); code != 0 {
					return code
				}
			}
		}
		return 0
	}
	return walk(root, 8)
}

type responseCode int

func (c *responseCode) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*c = 0
		return nil
	}
	s := string(raw)
	if len(s) != 0 && s[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return err
	}
	*c = responseCode(n)
	return nil
}
