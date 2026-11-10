package server

import (
	"encoding/json"
	"errors"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

// This is an explicit import policy, not a change to legacy disk loading or
// re-login. Only newly imported accounts missing a profile receive Mac defaults.
// Preview never calls this function or allocates identifiers.
func prepareImportedCredential(raw json.RawMessage) (*auth.Auth, []byte, error) {
	a, err := auth.Parse(raw)
	if err != nil {
		return nil, nil, errors.New("账号凭据无效")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, errors.New("账号凭据无效")
	}
	if a.DeviceProfile.Version == 0 {
		if a.DeviceID == "" {
			a.DeviceID, a.DeviceProfile, err = device.NewMac()
			doc["_device_identity"], _ = json.Marshal(a.DeviceID)
		} else {
			a.DeviceProfile, err = device.MacProfileForIdentity(a.DeviceID)
		}
		if err != nil {
			return nil, nil, errors.New("生成 Mac 设备档案失败，未导入")
		}
		doc["_device_profile"], _ = json.Marshal(a.DeviceProfile)
	}
	// Keep existing metadata verbatim, including fields unknown to this version.
	doc["_import_disabled"] = json.RawMessage("true")
	a.ImportDisabled = true
	prepared, err := json.Marshal(doc)
	if err != nil || len(prepared) > accountCredentialBytes {
		return nil, nil, errors.New("补全后的账号凭据超过 64 KiB，未导入")
	}
	return a, prepared, nil
}
