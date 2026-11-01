// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegalNoticesDoNotRequireFrontendOrAPIAuthentication(t *testing.T) {
	h := NewHandler(Config{APIKey: "fixture-admin", FrontendPassword: "fixture-password"})
	for _, path := range []string{"/legal", "/legal/license", "/legal/notice", "/legal/third-party"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Link"), `rel="license"`) {
			t.Fatalf("legal route inaccessible: %s status=%d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/config", nil))
	if w.Code != 401 {
		t.Fatalf("admin authentication changed: status=%d", w.Code)
	}
}
