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

func TestClientSettingsDefaultsFileAndEnvironment(t *testing.T) {
	c := Default()
	if c.Upstream.HeaderProfile != "desktop" || c.Upstream.ClientVersionCN != "5.9.3" || c.Upstream.ClientVersionGlobal != "5.9.4" || c.Upstream.CLIVersion != "2.165.0" || c.Upstream.ModelOptimization || c.Upstream.RefreshIdentityCheck != "best_effort" {
		t.Fatal("client settings defaults")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":{"header_profile":"legacy","client_version_cn":"6.0.0","refresh_identity_check":"off","model_optimization":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Upstream.HeaderProfile != "legacy" || c.Upstream.ClientVersionCN != "6.0.0" || !c.Upstream.ModelOptimization || c.Upstream.RefreshIdentityCheck != "off" {
		t.Fatalf("file settings: %v", err)
	}
	t.Setenv("WB2A_CLIENT_VERSION_CN", "6.0.1")
	t.Setenv("WB2A_CLIENT_VERSION_GLOBAL", "6.0.2")
	t.Setenv("WB2A_CLI_VERSION", "3.0.0")
	t.Setenv("WB2A_HEADER_PROFILE", "desktop")
	t.Setenv("WB2A_MODEL_OPTIMIZATION", "false")
	t.Setenv("WB2A_REFRESH_IDENTITY_CHECK", "strict")
	c, err = Load(path)
	if err != nil || c.Upstream.HeaderProfile != "desktop" || c.Upstream.ClientVersionCN != "6.0.1" || c.Upstream.ClientVersionGlobal != "6.0.2" || c.Upstream.CLIVersion != "3.0.0" || c.Upstream.ModelOptimization || c.Upstream.RefreshIdentityCheck != "strict" {
		t.Fatalf("environment settings: %v", err)
	}
	t.Setenv("WB2A_CLI_VERSION", "bad\r\nX-Header: value")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid version accepted")
	}
}
