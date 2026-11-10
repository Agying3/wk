package lifecycle

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func TestControlRequiresTwoStageDrainAndCommit(t *testing.T) {
	Default.Resume()
	defer Default.Resume()
	var foreground Gate
	finish, _ := foreground.Enter()
	commits, stops := 0, 0
	h := Control("release-a", &foreground, func() bool { return true }, func() error { commits++; return nil }, func() { stops++ })
	request := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	request("/pause?release=wrong", 409)
	request("/pause?release=release-a", 200)
	request("/quiesce?release=release-a", 409)
	finish()
	request("/quiesce?release=release-a", 200)
	background := Default.Track()
	request("/stop?release=release-a", 409)
	background()
	request("/stop?release=release-a", 200)
	request("/resume?release=release-a", 409)
	if commits != 1 || stops != 1 {
		t.Fatal("commit/stop count", commits, stops)
	}
}

func TestOverlapPromotionKeepsBackgroundPausedUntilOwnerCallbackSucceeds(t *testing.T) {
	Default.Pause()
	defer Default.Resume()
	var foreground Gate
	owner, management, fail := false, false, true
	h := ControlWithOptions("b", &foreground, func() bool { return true }, func() error { return nil }, func() {}, ControlOptions{
		Status: func() map[string]any {
			return map[string]any{"mode": "overlap", "overlap_protocol": 1, "coordination_domain": "shared-test", "background_owner": owner}
		},
		ActivateBackground: func() error {
			if fail {
				return errors.New("lease unavailable")
			}
			owner = true
			return nil
		},
		ActivateManagement: func() error { management = true; return nil },
	})
	request := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	request("/resume?release=b", 200)
	if paused, _ := Default.Status(); !paused {
		t.Fatal("standby resume bypassed owner acquisition")
	}
	request("/activate-background?release=b", 409)
	if paused, _ := Default.Status(); !paused {
		t.Fatal("failed promotion resumed dispatch")
	}
	fail = false
	request("/activate-management?release=b", 200)
	request("/activate-background?release=b", 200)
	if paused, _ := Default.Status(); paused || !owner || !management {
		t.Fatal("successful promotion did not enable owner")
	}
	finish, _ := foreground.Enter()
	defer finish()
	request("/pause-background?release=b", 200)
	if paused, active := foreground.Status(); paused || active != 1 {
		t.Fatal("background pause interrupted foreground")
	}
	if paused, _ := Default.Status(); !paused {
		t.Fatal("background pause did not close dispatch")
	}
}

func TestOverlapControlsRequireExplicitOptions(t *testing.T) {
	var foreground Gate
	h := Control("a", &foreground, func() bool { return true }, func() error { return nil }, func() {})
	for _, action := range []string{"pause-background", "activate-background", "activate-management"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/"+action+"?release=a", nil))
		if w.Code != 409 {
			t.Fatalf("%s enabled without coordination: %d", action, w.Code)
		}
	}
}
