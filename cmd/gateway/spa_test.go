package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSPA(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)                                                //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644)               //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "assets", "worker-abc.mjs"), []byte("self.onmessage=1"), 0o644) //nolint:errcheck
	h := spa(dir)
	tests := []struct {
		path, body, contentType string
		code                    int
	}{
		{"/", "app", "text/html", 200},
		{"/drones/7", "app", "text/html", 200}, // a page path: the app handles it
		{"/assets/worker-abc.mjs", "self.onmessage", "text/javascript", 200},
		{"/assets/maplibre-gl-worker.mjs", "", "", 404}, // a missing script is not the page
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
		if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.body) || !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.contentType) {
			t.Errorf("%s: %d %q %q, want %d with %q as %q", tt.path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), tt.code, tt.body, tt.contentType)
		}
	}
}
