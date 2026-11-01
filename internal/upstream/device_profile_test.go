package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

func TestAccountDeviceStableAcrossCredentialAndClientChanges(t *testing.T) {
	first := &auth.Auth{UID: "device-account", AccessToken: "old", Nickname: "before"}
	// A separate object/client models reload/import, rather than cached state.
	reloaded := &auth.Auth{UID: first.UID, AccessToken: "new", Nickname: "after"}
	before, after := desktopFingerprint(first), desktopFingerprint(reloaded)
	for _, key := range []string{"os", "arch", "osVersion", "cpuCores", "memorySize", "machineId", "sessionId", "timezone"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("device field %s changed: %v -> %v", key, before[key], after[key])
		}
	}
	if before["username"] == after["username"] {
		t.Fatal("account nickname should still reflect the current credentials")
	}
	mp := mpEventBase(reloaded)
	major := "11"
	if before["osVersion"] == "10.0.19045" {
		major = "10"
	}
	if mp["osVersion"] != major || mp["arch"] != before["arch"] {
		t.Fatalf("desktop and mini-program disagree about the device: %v / %v", before, mp)
	}
}

func TestAccountDevicePopulationAllowsSharedHardware(t *testing.T) {
	counts := map[deviceProfile]int{}
	for i := 0; i < 256; i++ {
		uid := fmt.Sprintf("fixture-account-%d", i)
		profile := deviceProfileForUID(uid)
		counts[profile]++
		if profile.cpuCores < 8 || profile.cpuCores > 24 || profile.memoryGiB < 16 || profile.memoryGiB > 64 {
			t.Fatalf("unexpected hardware: %+v", profile)
		}
	}
	if len(counts) < 2 {
		t.Fatal("all accounts received the same hardware")
	}
	shared := false
	for _, n := range counts {
		shared = shared || n > 1
	}
	if !shared {
		t.Fatal("hardware profiles must allow sharing across accounts")
	}
}

func TestMixedMacAndWindowsDevicesDoNotSharePlatformFields(t *testing.T) {
	id, profile, err := device.NewMac()
	if err != nil {
		t.Fatal(err)
	}
	mac := &auth.Auth{UID: "mac-account", AccessToken: "fake-at", DeviceID: id, DeviceProfile: profile}
	windows := &auth.Auth{UID: "old-windows-account", AccessToken: "fake-at"}
	before := desktopFingerprint(windows)
	c := &Client{ClientSettings: DefaultClientSettings()}
	for _, a := range []*auth.Auth{mac, windows, mac, windows} {
		fp := desktopFingerprint(a)
		req, _ := http.NewRequest("POST", "https://example.test/v2/chat/completions", strings.NewReader("{}"))
		c.chatHeaders(req, a)
		if a == mac {
			if fp["os"] != "darwin" || fp["arch"] != "arm64" || fp["cpuModel"] != "Apple M2" || fp["machineId"] != profile.MachineID || fp["sessionId"] != "" {
				t.Fatal("incorrect Mac telemetry")
			}
			if req.Header.Get("X-Stainless-OS") != "MacOS" || req.Header.Get("X-Stainless-Arch") != "arm64" || !strings.Contains(browserUserAgent(a), "Macintosh") {
				t.Fatal("incorrect Mac request profile")
			}
		} else {
			for _, key := range []string{"os", "arch", "osVersion", "cpuCores", "memorySize", "machineId", "sessionId", "timezone"} {
				if fp[key] != before[key] {
					t.Fatalf("legacy Windows field %s changed", key)
				}
			}
			if req.Header.Get("X-Stainless-OS") != "" || browserUserAgent(a) != webUA {
				t.Fatal("Mac-only headers leaked to Windows account")
			}
		}
	}
}

func TestMacWebReportKeepsStoredBrowserIdentity(t *testing.T) {
	id, profile, _ := device.NewMac()
	a := &auth.Auth{UID: "mac-web", AccessToken: "fake-at", DeviceID: id, DeviceProfile: profile}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0]["os"] != "macOS" || events[0]["machineId"] != profile.WebMachineID || events[0]["userAgent"] != macBrowserUA || r.Header.Get("User-Agent") != macBrowserUA {
			t.Fatal("Mac browser and event disagree")
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.ReportWebEvent(a, "fixture_event", "https://www.workbuddy.cn/profile", "fixture", "fixture"); err != nil {
		t.Fatal(err)
	}
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEventIDsAreIndependentUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1024; i++ {
		id := NewEventID()
		if !uuidV4Pattern.MatchString(id) || seen[id] {
			t.Fatalf("invalid or repeated event ID: %q", id)
		}
		seen[id] = true
	}
}

func TestReportProtectsStableDeviceAndPreservesEventLinks(t *testing.T) {
	a := &auth.Auth{UID: "report-account", AccessToken: "fixture-token"}
	profile := desktopFingerprint(a)
	var requestIDs []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		var events []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0]["eventCode"] != "fixture_event" || events[0]["conversationId"] != "fixture-conversation" {
			t.Fatalf("business event fields changed: %v", events)
		}
		ev := events[0]
		if ev["machineId"] != profile["machineId"] || ev["osVersion"] != profile["osVersion"] || ev["cpuCores"] != float64(profile["cpuCores"].(int)) {
			t.Fatalf("business event overwrote the account device: %v", ev)
		}
		id := r.Header.Get("X-Request-ID")
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("request ID is not opaque UUIDv4: %q", id)
		}
		requestIDs = append(requestIDs, id)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	for i := 0; i < 2; i++ {
		if err := c.ReportDesktopEvent(a, DesktopEvent{
			"eventCode": "fixture_event", "conversationId": "fixture-conversation",
			"machineId": "different", "osVersion": "different", "cpuCores": 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if requestIDs[0] == requestIDs[1] {
		t.Fatal("separate report requests shared a request ID")
	}
	// A returned map is a fresh value, not mutable shared device state.
	profile["osVersion"] = "mutated"
	if desktopFingerprint(a)["osVersion"] == "mutated" {
		t.Fatal("mutating one event changed subsequent device reports")
	}
}

func TestSchoolEventIDsPreserveChainRelationships(t *testing.T) {
	for _, ev := range []map[string]any{
		SchoolChatTimesEvents("fixture-conversation"),
		SchoolExpertUseEvents("fixture-expert", "fixture-name", "fixture-conversation")[3],
	} {
		id, _ := ev["traceId"].(string)
		if !uuidV4Pattern.MatchString(id) || ev["rootRequestId"] != id || ev["codebuddy.conversation_request_id"] != id {
			t.Fatalf("broken request ID relationship: %v", ev)
		}
		if ev["conversationId"] != "fixture-conversation" || ev["codebuddy.session_id"] != "fixture-conversation" {
			t.Fatalf("broken conversation relationship: %v", ev)
		}
	}
}

func TestChatErrorLogOmitsBodyButPreservesClientError(t *testing.T) {
	const raw = `{"code":11140,"msg":"request illegal","echo":"private-prompt-and-fixture-token"}`
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusForbidden, raw), nil
	})
	_, status, body, _, err := c.ChatStreamContextWithHeaders(context.Background(), &auth.Auth{UID: "fixture-account"}, []byte(`{"model":"fixture-model","messages":[]}`))
	if err != nil || status != http.StatusForbidden || string(body) != raw || Classify(status, string(body)) != ErrRiskFlag {
		t.Fatalf("client error or classification changed: status=%d body=%s err=%v", status, body, err)
	}
	if strings.Contains(logs.String(), "private-prompt") || strings.Contains(logs.String(), "fixture-token") || !strings.Contains(logs.String(), "code=11140") {
		t.Fatalf("unsafe or insufficient error log: %s", logs.String())
	}
}
