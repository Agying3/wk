// Package device owns immutable account device records. New registrations use
// macOS; Windows is retained only for legacy records and explicit imports.
package device

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type Profile struct {
	Version      int    `json:"version"`
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
	OSVersion    string `json:"os_version"`
	CPUModel     string `json:"cpu_model,omitempty"`
	CPUCores     int    `json:"cpu_cores"`
	MemoryGiB    int    `json:"memory_gib"`
	Timezone     string `json:"timezone"`
	MachineID    string `json:"machine_id"`
	SessionID    string `json:"session_id"`
	CLISessionID string `json:"cli_session_id,omitempty"`
	WebMachineID string `json:"web_machine_id"`
	MPMachineID  string `json:"mp_machine_id"`
}

func (p Profile) IsMac() bool { return p.Platform == "macos" }

func (p Profile) Validate() error {
	if p.Version != 1 || (p.Platform != "macos" && p.Platform != "windows") ||
		p.CPUCores < 1 || p.CPUCores > 256 || p.MemoryGiB < 1 || p.MemoryGiB > 4096 ||
		p.Timezone != "Asia/Shanghai" || p.OSVersion == "" {
		return fmt.Errorf("invalid device profile")
	}
	for _, value := range []string{p.Arch, p.OSVersion, p.CPUModel, p.MachineID, p.SessionID, p.CLISessionID, p.WebMachineID, p.MPMachineID} {
		if len(value) > 128 || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
			return fmt.Errorf("invalid device profile field")
		}
	}
	if p.IsMac() {
		if p.Arch != "arm64" || !validUUID(p.MachineID) || p.SessionID != "" || !validUUID(p.CLISessionID) || !validHex(p.WebMachineID, 32) {
			return fmt.Errorf("invalid macOS device profile")
		}
	} else if p.Arch != "x64" || !validHex(p.MachineID, 36) || !validHex(p.SessionID, 36) || !validHex(p.WebMachineID, 36) {
		return fmt.Errorf("invalid Windows device profile")
	}
	if !validHex(p.MPMachineID, 36) {
		return fmt.Errorf("invalid mini-program identity")
	}
	return nil
}

func validHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validUUID(s string) bool {
	return len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' && validHex(strings.ReplaceAll(s, "-", ""), 32)
}

// NewMac allocates once. M2 has an 8-core CPU and supports 8/16/24 GB unified
// memory (Apple MacBook Air M2 2022 specifications). Account records, rather
// than catalogue positions or software defaults, own the selected values.
func NewMac() (string, Profile, error) {
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", Profile{}, err
	}
	id := hex.EncodeToString(random[:])
	p := Profile{Version: 1, Platform: "macos", Arch: "arm64", OSVersion: "24.6.0", CPUModel: "Apple M2", CPUCores: 8,
		MemoryGiB: [...]int{8, 16, 24}[int(random[0])%3], Timezone: "Asia/Shanghai",
		MachineID: strings.ToUpper(uuid(id, "mac-machine")), CLISessionID: uuid(id, "mac-cli-session"),
		WebMachineID: digest(id, "mac-web", 16), MPMachineID: LegacyID(id, "mpmachine")}
	return id, p, nil
}

func uuid(id, purpose string) string {
	sum := sha256.Sum256([]byte(purpose + ":" + id))
	sum[6], sum[8] = sum[6]&15|64, sum[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
func digest(id, purpose string, size int) string {
	sum := sha256.Sum256([]byte(purpose + ":" + id))
	return hex.EncodeToString(sum[:size])
}
func LegacyID(identity, purpose string) string { return digest(identity, purpose, 18) }

// These twelve entries and their hash rule are the historical Windows
// catalogue. Keep them byte-for-byte stable for records without metadata.
var legacyHardware = [...][3]int{
	{19045, 8, 16}, {19045, 12, 16}, {19045, 16, 32}, {22631, 8, 16},
	{22631, 12, 24}, {22631, 16, 32}, {22631, 20, 32}, {26100, 8, 16},
	{26100, 12, 16}, {26100, 16, 32}, {26100, 20, 32}, {26100, 24, 64},
}

func LegacyWindows(uid, identity string) Profile {
	if identity == "" {
		identity = uid
	}
	sum := sha256.Sum256([]byte("device-profile-v1:" + uid))
	hardware := legacyHardware[(int(sum[0])<<8|int(sum[1]))%len(legacyHardware)]
	return Profile{Version: 1, Platform: "windows", Arch: "x64", OSVersion: fmt.Sprintf("10.0.%d", hardware[0]),
		CPUCores: hardware[1], MemoryGiB: hardware[2], Timezone: "Asia/Shanghai", MachineID: LegacyID(identity, "machine"),
		SessionID: LegacyID(identity, "session"), WebMachineID: LegacyID(identity, "webmachine"), MPMachineID: LegacyID(identity, "mpmachine")}
}
