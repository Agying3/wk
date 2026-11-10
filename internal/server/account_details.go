package server

import (
	"net/http"
	"net/url"
	"strings"

	"workbuddy2api/internal/device"
	"workbuddy2api/internal/pool"
)

// The list omits zero-valued optional fields to stay compact. A detail view
// must distinguish a known zero from an unavailable value.
type accountDetailsStatus struct {
	pool.Status
	SuccessCount        int64 `json:"success_count"`
	ErrTotal            int64 `json:"err_total"`
	RiskStrikes         int   `json:"risk_strikes"`
	CoolRemaining       int64 `json:"cool_remaining_sec"`
	CapacitySize        int64 `json:"capacity_size"`
	CapacityRemain      int64 `json:"capacity_remain"`
	CapacityUsed        int64 `json:"capacity_used"`
	CycleCapacitySize   int64 `json:"cycle_capacity_size"`
	CycleCapacityRemain int64 `json:"cycle_capacity_remain"`
	CycleCapacityUsed   int64 `json:"cycle_capacity_used"`
}

// A separate admin-only projection keeps credentials and detailed device
// metadata out of the lightweight /status response. Reading never initializes
// device identities or saves a legacy device profile.
type accountDeviceDetails struct {
	ProfileSource   string `json:"profile_source"`
	IdentityPresent bool   `json:"identity_present"`
	Profile         struct {
		Version   int    `json:"version"`
		Platform  string `json:"platform"`
		Arch      string `json:"arch"`
		OSVersion string `json:"os_version"`
		CPUModel  string `json:"cpu_model"`
		CPUCores  int    `json:"cpu_cores"`
		MemoryGiB int    `json:"memory_gib"`
		Timezone  string `json:"timezone"`
	} `json:"profile"`
	Identifiers struct {
		DeviceIdentity string `json:"device_identity"`
		MachineID      string `json:"machine_id"`
		SessionID      string `json:"session_id"`
		CLISessionID   string `json:"cli_session_id"`
		WebMachineID   string `json:"web_machine_id"`
		MPMachineID    string `json:"mp_machine_id"`
	} `json:"identifiers"`
}

func maskAccountDeviceID(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 8 {
		return "已设置"
	}
	return value[:4] + "…" + value[len(value)-4:]
}

func accountDisplayDomain(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

func (h *Handler) accountDetails(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.cfg.Pool == nil {
		writeJSON(w, 503, map[string]string{"error": "账号池未启用"})
		return
	}
	uid := r.PathValue("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if h.cfg.Pool.SharedError() != nil {
		writeJSON(w, 503, map[string]string{"error": "账号池存储不可用"})
		return
	}
	if a == nil {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	c := a.Snapshot()
	status, ok := h.cfg.Pool.Status(uid)
	if h.cfg.Pool.SharedError() != nil {
		writeJSON(w, 503, map[string]string{"error": "账号池存储不可用"})
		return
	}
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "账号已不在账号池中，请刷新列表"})
		return
	}
	p := c.DeviceProfile
	view := accountDeviceDetails{ProfileSource: "stored", IdentityPresent: c.DeviceID != ""}
	if p.Version == 0 {
		view.ProfileSource = "legacy_derived"
		p = device.LegacyWindows(c.UID, c.DeviceID)
	}
	view.Profile.Version, view.Profile.Platform, view.Profile.Arch = p.Version, p.Platform, p.Arch
	view.Profile.OSVersion, view.Profile.CPUModel = p.OSVersion, p.CPUModel
	view.Profile.CPUCores, view.Profile.MemoryGiB, view.Profile.Timezone = p.CPUCores, p.MemoryGiB, p.Timezone
	view.Identifiers.DeviceIdentity = maskAccountDeviceID(c.DeviceID)
	view.Identifiers.MachineID = maskAccountDeviceID(p.MachineID)
	view.Identifiers.SessionID = maskAccountDeviceID(p.SessionID)
	view.Identifiers.CLISessionID = maskAccountDeviceID(p.CLISessionID)
	view.Identifiers.WebMachineID = maskAccountDeviceID(p.WebMachineID)
	view.Identifiers.MPMachineID = maskAccountDeviceID(p.MPMachineID)
	groups := []string{"default"}
	if h.cfg.Groups != nil {
		groups = h.cfg.Groups.AccountGroups(uid)
	}
	statusView := accountDetailsStatus{Status: status, SuccessCount: status.SuccessCount, ErrTotal: status.ErrTotal, RiskStrikes: status.RiskStrikes, CoolRemaining: status.CoolRemaining,
		CapacitySize: status.CapacitySize, CapacityRemain: status.CapacityRemain, CapacityUsed: status.CapacityUsed,
		CycleCapacitySize: status.CycleCapacitySize, CycleCapacityRemain: status.CycleCapacityRemain, CycleCapacityUsed: status.CycleCapacityUsed}
	writeJSON(w, 200, map[string]any{
		"uid": c.UID, "nickname": c.Nickname, "region": a.Region(),
		"domain": accountDisplayDomain(c.Domain), "enterprise_id": c.EnterpriseID,
		"account_type": c.AccountType, "oneid_account_id": c.OneIDAccountID,
		"idp": c.IDP, "department_full_name": c.DepartmentFullName, "token_type": c.TokenType,
		"groups": groups, "status": statusView,
		"credential": map[string]any{"has_access_token": c.AccessToken != "", "has_refresh_token": c.RefreshToken != "", "expires_at": c.ExpiresAt, "import_disabled": c.ImportDisabled},
		"device":     view,
	})
}
