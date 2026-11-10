package device

import (
	"encoding/json"
	"testing"
)

func TestNewMacRecordsAreIndependentAndRoundTrip(t *testing.T) {
	seen, hardware := map[string]bool{}, map[int]bool{}
	for range 128 {
		id, p, err := NewMac()
		if err != nil || p.Validate() != nil || !p.IsMac() || len(id) != 36 || seen[p.MachineID] {
			t.Fatalf("invalid/repeated Mac record: %v", err)
		}
		if p.CPUModel != "Apple M2" || p.CPUCores != 8 || p.Arch != "arm64" || p.OSVersion != "24.6.0" {
			t.Fatal("inconsistent Mac hardware")
		}
		switch p.MemoryGiB {
		case 8, 16, 24:
		default:
			t.Fatal("unsupported M2 memory")
		}
		seen[p.MachineID], hardware[p.MemoryGiB] = true, true
		raw, _ := json.Marshal(p)
		var reloaded Profile
		if err := json.Unmarshal(raw, &reloaded); err != nil || reloaded != p {
			t.Fatal("stored device changed after reload")
		}
	}
	if len(hardware) < 2 {
		t.Fatal("new devices all received one hardware configuration")
	}
}

func TestDeviceRecordValidation(t *testing.T) {
	_, p, _ := NewMac()
	p.CPUModel = "Apple M2\r\nInjected: yes"
	if p.Validate() == nil {
		t.Fatal("control characters accepted")
	}
	p = LegacyWindows("legacy-user", "legacy-identity")
	if p.Validate() != nil || p.IsMac() {
		t.Fatal("legacy record is not a valid Windows device")
	}
}

func TestMacProfileForIdentityPreservesNewAccountDerivation(t *testing.T) {
	id, profile, err := NewMac()
	if err != nil {
		t.Fatal(err)
	}
	derived, err := MacProfileForIdentity(id)
	if err != nil || derived != profile || derived.Validate() != nil {
		t.Fatal("new and existing identity paths derive different Mac profiles")
	}
	for _, invalid := range []string{"", "short", "not-hex-00000000000000000000000000000"} {
		if _, err := MacProfileForIdentity(invalid); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}
