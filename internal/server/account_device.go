package server

import (
	"fmt"
	"os"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

// deviceForLogin resolves the account's existing device before replacing its
// credentials. New-account defaults must never run for a re-login or reload.
// There is deliberately no caller-supplied platform selector yet.
func (h *Handler) deviceForLogin(uid, path string) (auth.Credentials, error) {
	if h.cfg.Pool != nil {
		if existing := h.cfg.Pool.AuthByUID(uid); existing != nil {
			profile := existing.Device()
			c := existing.Snapshot()
			c.DeviceProfile = profile
			return c, nil
		}
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		existing, err := auth.Parse(raw)
		if err != nil || existing.UID != uid {
			return auth.Credentials{}, fmt.Errorf("无法确认已有账号设备档案，未覆盖授权文件")
		}
		existing.FilePath = path
		profile := existing.Device()
		c := existing.Snapshot()
		c.DeviceProfile = profile
		return c, nil
	}
	if !os.IsNotExist(err) {
		return auth.Credentials{}, fmt.Errorf("无法读取已有账号设备档案")
	}
	id, profile, err := device.NewMac()
	if err != nil {
		return auth.Credentials{}, fmt.Errorf("创建设备档案失败")
	}
	return auth.Credentials{UID: uid, DeviceID: id, DeviceProfile: profile}, nil
}
