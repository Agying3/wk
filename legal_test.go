// SPDX-License-Identifier: AGPL-3.0-only
package workbuddy2api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegalHandlerPublicNoticesAndSource(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "source.tar.gz")
	data := []byte("release-source-fixture")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	h := LegalHandler(archive)
	for _, path := range []string{"/legal", "/legal/license", "/legal/notice", "/legal/third-party", "/legal/source"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s returned %d", path, w.Code)
		}
		if path == "/legal/source" && w.Body.String() != string(data) {
			t.Fatal("source response did not serve the fixed release archive")
		}
		if path == "/legal/license" && !strings.Contains(w.Body.String(), "GNU AFFERO GENERAL PUBLIC LICENSE") {
			t.Fatal("license text missing")
		}
	}
	for _, path := range []string{"/legal/config.json", "/legal/../../config.json", "/legal/source/config.json"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unexpected file exposure at %s", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("HEAD", "/legal/source", nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD source must have no response body")
	}
}

func TestLegalHandlerMissingSourceDoesNotPretendRepositoryIsSource(t *testing.T) {
	h := LegalHandler(filepath.Join(t.TempDir(), "missing.tar.gz"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/legal/source", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Location") != "" {
		t.Fatal("missing source must be explicit and must not redirect to a moving branch")
	}
}
