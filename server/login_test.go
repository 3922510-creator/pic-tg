package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestLoginStatusDoesNotProbeOrLeakCloudAccount(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	for _, token := range []string{"", "invalid", "test-pass-123"} {
		req := httptest.NewRequest("GET", "/api/status?auth_only=1", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		var response struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || response.Data["has_auth"] != true || response.Data["authenticated"] != (token == "test-pass-123") || len(response.Data) != 2 {
			t.Fatalf("invalid auth-only status: %s", w.Body.String())
		}
	}
	srv.cfg.AuthToken = ""
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/status?auth_only=1", nil))
	var response struct{ Data map[string]any }
	json.Unmarshal(w.Body.Bytes(), &response)
	if response.Data["has_auth"] != false || response.Data["authenticated"] != true {
		t.Fatal("explicit public mode blocked")
	}
}
