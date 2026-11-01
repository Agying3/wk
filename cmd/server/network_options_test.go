package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNetworkOptionsDefaultsAndExplicitOptOut(t *testing.T) {
	c := Default()
	if !c.Upstream.TLSFingerprint || !c.Upstream.SharedConversationTrace {
		t.Fatal("requested network capabilities are not enabled by default")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":{"tls_fingerprint":false,"shared_conversation_trace":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Upstream.TLSFingerprint || c.Upstream.SharedConversationTrace {
		t.Fatalf("file opt-out lost: %v", err)
	}
	t.Setenv("WB2A_TLS_FINGERPRINT", "true")
	t.Setenv("WB2A_SHARED_CONVERSATION_TRACE", "true")
	c, err = Load(path)
	if err != nil || !c.Upstream.TLSFingerprint || !c.Upstream.SharedConversationTrace {
		t.Fatalf("environment did not override file: %v", err)
	}
}
