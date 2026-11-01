package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorityMetadataRoundTrip(t *testing.T) {
	for _, nested := range []bool{false, true} {
		credential := map[string]any{"accessToken": "dummy-at", "refreshToken": "dummy-rt", "tokenType": "Bearer"}
		account := map[string]any{"uid": "test-user", "type": "personal", "oneidAccountId": "oid", "idp": "provider", "departmentFullName": "研发"}
		var doc map[string]any
		if nested {
			doc = map[string]any{"auth": credential, "account": account}
		} else {
			doc = credential
			for k, v := range account {
				doc[k] = v
			}
		}
		raw, _ := json.Marshal(doc)
		a, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := Authority{AccountType: "personal", OneIDAccountID: "oid", IDP: "provider", DepartmentFullName: "研发", TokenType: "Bearer"}
		if a.Snapshot().Authority != want {
			t.Fatal("metadata parse lost fields")
		}
		a.FilePath = filepath.Join(t.TempDir(), "workbuddy.json")
		if err := a.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		saved, err := os.ReadFile(a.FilePath)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Parse(saved)
		if err != nil {
			t.Fatal(err)
		}
		if b.Snapshot().Authority != want {
			t.Fatal("metadata save/reload lost fields")
		}
		exported := map[string]any{"accessToken": "dummy-at", "uid": "test-user"}
		b.Snapshot().AddAuthorityMetadata(exported, exported)
		raw, _ = json.Marshal(exported)
		c, err := Parse(raw)
		if err != nil || c.Snapshot().Authority != want {
			t.Fatal("flat export metadata lost")
		}
	}
}

func TestLegacyAuthWithoutAccountMetadata(t *testing.T) {
	for _, raw := range []string{`{"accessToken":"at"}`, `{"auth":{"accessToken":"at"}}`, `{"accessToken":"at","type":123,"idp":{},"tokenType":12}`} {
		if _, err := Parse([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
