package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// This trace describes existing balance reads only. It never retains response
// bodies, string values, request headers or credentials, and never sends a read.
type balanceTraceKey struct{}
type balanceTrace struct {
	Reads     []balanceRead    `json:"reads"`
	Failures  []balanceFailure `json:"failures"`
	Truncated bool             `json:"truncated,omitempty"`
}
type balanceRead struct {
	Stage       string            `json:"stage"`
	HTTP        int               `json:"http,omitempty"`
	Bytes       int               `json:"bytes"`
	SHA256      string            `json:"sha256"`
	Code        *int64            `json:"code,omitempty"`
	JSON        bool              `json:"json"`
	Schema      map[string]string `json:"schema,omitempty"`
	SchemaLimit bool              `json:"schema_limited,omitempty"`
}
type balanceFailure struct {
	Stage    string `json:"stage"`
	Reason   string `json:"reason"`
	Kind     string `json:"kind"`
	Field    string `json:"field,omitempty"`
	Expected string `json:"expected,omitempty"`
}

var balanceSchemaFields = []string{
	"code", "data", "Response", "Data", "Accounts", "Packages", "TotalCount",
	"CapacityType", "CapacitySize", "CapacityRemain", "CapacityUsed",
	"CycleCapacitySize", "CycleCapacityRemain", "CycleCapacityUsed",
	"CycleCapacitySizePrecise", "CycleCapacityRemainPrecise",
	"CycleTotalCapacity", "CycleRemainCapacity", "SlicePeriodUsageDetails",
	"SlicePeriodCapacitySizePrecise", "SlicePeriodCapacityRemainPrecise",
	"DeductionEndTime", "PackageCode", "SubscriptionPackageCode", "IsPaidUser",
}

func balanceStage(path string) string {
	switch path {
	case resourceSummaryAPI:
		return "summary"
	case resourceFreeAPI:
		return "free"
	case resourcePaidAPI:
		return "paid"
	case "/v2/billing/meter/get-user-resource":
		return "legacy"
	}
	return ""
}

func balanceTraceFrom(ctx context.Context) *balanceTrace {
	t, _ := ctx.Value(balanceTraceKey{}).(*balanceTrace)
	return t
}

func observeBalanceRead(req *http.Request, resp *http.Response, raw []byte, readErr error) {
	t := balanceTraceFrom(req.Context())
	stage := balanceStage(req.URL.Path)
	if t == nil || stage == "" {
		return
	}
	if len(t.Reads) >= 24 {
		t.Truncated = true
		return
	}
	sum := sha256.Sum256(raw)
	r := balanceRead{Stage: stage, Bytes: len(raw), SHA256: hex.EncodeToString(sum[:])}
	if resp != nil {
		r.HTTP = resp.StatusCode
	}
	if len(raw) > 1<<20 {
		r.SchemaLimit = true
	} else if json.Valid(raw) {
		r.JSON = true
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&value) == nil {
			r.Schema = map[string]string{}
			balanceSchema(value, "$", 0, &r)
			if obj, ok := value.(map[string]any); ok {
				if code, ok := obj["code"].(json.Number); ok {
					if n, err := code.Int64(); err == nil {
						r.Code = &n
					}
				}
			}
		}
	}
	t.Reads = append(t.Reads, r)
	if readErr != nil {
		noteBalanceFailure(req.Context(), stage, "read", readErr)
	}
}

func balanceSchema(value any, path string, depth int, r *balanceRead) {
	if depth > 8 || len(r.Schema) >= 96 {
		r.SchemaLimit = true
		return
	}
	switch v := value.(type) {
	case nil:
		r.Schema[path] = "null"
	case map[string]any:
		r.Schema[path] = "object"
		for _, field := range balanceSchemaFields {
			if child, ok := v[field]; ok {
				balanceSchema(child, path+"."+field, depth+1, r)
			}
		}
	case []any:
		r.Schema[path] = fmt.Sprintf("array[%d]", len(v))
		for i := 0; i < len(v) && i < 3; i++ {
			balanceSchema(v[i], fmt.Sprintf("%s[%d]", path, i), depth+1, r)
		}
		if len(v) > 3 {
			r.SchemaLimit = true
		}
	case json.Number:
		r.Schema[path] = "number"
	case string:
		r.Schema[path] = "string"
		if v == "" {
			r.Schema[path] = "string_empty"
		} else if len(v) <= 128 {
			if _, err := strconv.ParseFloat(v, 64); err == nil {
				r.Schema[path] = "string_numeric"
			}
		}
	case bool:
		r.Schema[path] = "boolean"
	}
}

func noteBalanceFailure(ctx context.Context, stage, reason string, err error) {
	t := balanceTraceFrom(ctx)
	if t == nil {
		return
	}
	if len(t.Failures) >= 24 {
		t.Truncated = true
		return
	}
	f := balanceFailure{Stage: stage, Reason: reason, Kind: "invalid_response"}
	var ue *Error
	var te *json.UnmarshalTypeError
	var se *json.SyntaxError
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		f.Kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		f.Kind = "timeout"
	case errors.Is(err, ErrNoProxyNode):
		f.Kind = "proxy"
	case errors.As(err, &ue):
		f.Kind = ue.Kind.String()
	case errors.As(err, &te):
		f.Kind = "json_type"
		f.Expected = te.Type.Kind().String()
		// Field names in a JSON type error are untrusted too. Emit a path
		// only when every component belongs to the public billing schema.
		valid := true
		for _, part := range strings.Split(te.Field, ".") {
			found := false
			for _, field := range balanceSchemaFields {
				found = found || part == field
			}
			valid = valid && found
		}
		if valid {
			f.Field = te.Field
		}
	case errors.As(err, &se):
		f.Kind = "json_syntax"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &ne):
		f.Kind = "network"
	}
	t.Failures = append(t.Failures, f)
}

func (t *balanceTrace) logFailure(uid string) {
	// Hash the identifier rather than trusting an imported UID as log text.
	id := sha256.Sum256([]byte(uid))
	b, err := json.Marshal(t)
	if err == nil {
		log.Printf("balance_diagnostic uid_sha256=%x trace=%s", id[:], b)
	}
}
