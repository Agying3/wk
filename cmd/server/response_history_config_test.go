package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/responsehistory"
)

func TestResponseHistoryConfigLoading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, tc := range []struct {
		json string
		want responsehistory.Policy
		bad  bool
	}{
		{`{}`, responsehistory.Default(), false},
		{`{"response_history":{"max_entries":100000,"max_storage_mb":2048,"ttl_seconds":604800}}`, responsehistory.Policy{TTLSeconds: 604800, MaxEntries: 100000, MaxStorageMB: 2048, MaxEntryMB: 32}, false},
		{`{"response_history":{"max_entries":2000}}`, responsehistory.Policy{TTLSeconds: 3600, MaxEntries: 2000, MaxStorageMB: 64, MaxEntryMB: 32}, false},
		{`{"response_history":{"max_entry_mb":128,"max_storage_mb":64}}`, responsehistory.Policy{TTLSeconds: 3600, MaxEntries: 1024, MaxStorageMB: 64, MaxEntryMB: 128}, false},
		{`{"response_history":{"ttl_seconds":0}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_entries":-1}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_storage_mb":0}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_entry_mb":0}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_entry_mb":-1}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_entry_mb":1000000001}}`, responsehistory.Policy{}, true},
		{`{"response_history":{"max_entry_mb":1.5}}`, responsehistory.Policy{}, true},
	} {
		if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.bad {
			if err == nil {
				t.Fatalf("accepted %s", tc.json)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ResponseHistory != tc.want {
			t.Fatalf("%s: %+v want %+v", tc.json, cfg.ResponseHistory, tc.want)
		}
	}
}
