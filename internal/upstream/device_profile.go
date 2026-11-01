package upstream

import "crypto/sha256"

type deviceProfile struct {
	osVersion string
	cpuCores  int
	memoryGiB int
}

// v1 is an immutable catalogue: changing its order, contents or size would
// change existing accounts' devices. Accounts may share a hardware profile.
var deviceProfilesV1 = [...]deviceProfile{
	{"10.0.19045", 8, 16},
	{"10.0.19045", 12, 16},
	{"10.0.19045", 16, 32},
	{"10.0.22631", 8, 16},
	{"10.0.22631", 12, 24},
	{"10.0.22631", 16, 32},
	{"10.0.22631", 20, 32},
	{"10.0.26100", 8, 16},
	{"10.0.26100", 12, 16},
	{"10.0.26100", 16, 32},
	{"10.0.26100", 20, 32},
	{"10.0.26100", 24, 64},
}

// Hash-based selection is pseudorandom across accounts and reproducible after
// restart/import without storing new state. Token refresh, nickname changes,
// proxy changes and request order cannot change the selected device.
func deviceProfileForUID(uid string) deviceProfile {
	sum := sha256.Sum256([]byte("device-profile-v1:" + uid))
	index := (int(sum[0])<<8 | int(sum[1])) % len(deviceProfilesV1)
	return deviceProfilesV1[index]
}

func (d deviceProfile) windowsMajor() string {
	if d.osVersion == "10.0.19045" {
		return "10"
	}
	return "11"
}

// Business event properties cannot replace the account's stable device.
// Other properties retain the existing event-over-common precedence.
func mergeReportEvent(base, event map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(event))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range event {
		switch k {
		case "machineId", "sessionId", "os", "arch", "osVersion", "cpuCores", "memorySize", "timezone":
			if _, exists := base[k]; exists {
				continue
			}
		}
		out[k] = v
	}
	return out
}
