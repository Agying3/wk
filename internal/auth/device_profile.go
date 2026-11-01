package auth

import "workbuddy2api/internal/device"

// Device returns a value copy. Bare legacy credentials keep the old Windows
// rules; merely loading or reporting them never allocates a new Mac device.
func (a *Auth) Device() device.Profile {
	c := a.Snapshot()
	if c.DeviceProfile.Version != 0 {
		return c.DeviceProfile
	}
	identity := a.DeviceIdentity()
	// A concurrent credential save may have frozen a legacy profile while
	// identity initialization was waiting for the file lock.
	if current := a.Snapshot().DeviceProfile; current.Version != 0 {
		return current
	}
	return device.LegacyWindows(c.UID, identity)
}

// AddDeviceMetadata writes only local non-secret metadata. Callers preparing
// credential exports must resolve legacy records first so imports keep them.
func (c Credentials) AddDeviceMetadata(doc map[string]any) {
	if c.DeviceID != "" {
		doc["_device_identity"] = c.DeviceID
	}
	if c.DeviceProfile.Version != 0 {
		doc["_device_profile"] = c.DeviceProfile
	}
}
