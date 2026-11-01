package upstream

import "workbuddy2api/internal/device"

type deviceProfile struct {
	osVersion string
	cpuCores  int
	memoryGiB int
}

// Hash-based selection is pseudorandom across accounts and reproducible after
// restart/import without storing new state. Token refresh, nickname changes,
// proxy changes and request order cannot change the selected device.
func deviceProfileForUID(uid string) deviceProfile {
	p := device.LegacyWindows(uid, "")
	return deviceProfile{p.OSVersion, p.CPUCores, p.MemoryGiB}
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
		case "machineId", "sessionId", "os", "arch", "osVersion", "cpuModel", "cpuCores", "memorySize", "timezone":
			if _, exists := base[k]; exists {
				continue
			}
		}
		out[k] = v
	}
	return out
}
