package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/storage"
	"strings"
	"testing"
)

func keyRequest(s *Server, method, path, key string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestAPIKeyScopeAndRotation(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	s.cfg.APIKey = "pic_api_fixture"
	s.cfg.Path = filepath.Join(s.cfg.StateDir, "config.json")
	if e := config.SaveConfig(s.cfg.Path, s.cfg); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ method, path, body string }{{"GET", "/api/admin/telegram", ""}, {"GET", "/api/admin/api-key", ""}, {"POST", "/api/admin/api-key", ""}, {"POST", "/api/admin/password", `{"old_password":"test-pass-123","new_password":"bad"}`}, {"POST", "/api/admin/telegram/save", `{}`}} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			// Scope checks are independent attempts; cross-route locking has its
			// own regression and must not hide an authorization failure here.
			scoped, cleanup := setupTestServer(t)
			defer cleanup()
			scoped.cfg.APIKey = "pic_api_fixture"
			if w := keyRequest(scoped, tc.method, tc.path, "pic_api_fixture", tc.body); w.Code != 401 {
				t.Fatalf("API key accessed admin %s: %d", tc.path, w.Code)
			}
		})
	}
	if w := keyRequest(s, "POST", "/api/auth/verify", "", `{"token":"pic_api_fixture"}`); w.Code != 401 {
		t.Fatal("API key logged in")
	}
	for _, key := range []string{"pic_api_fixture", "test-pass-123"} {
		if w := keyRequest(s, "GET", "/api/objects", key, ""); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := keyRequest(s, "GET", "/api/objects", "wrong", ""); w.Code != 401 {
		t.Fatal("bad key accepted")
	}
	r := httptest.NewRequest("GET", "/api/objects", nil)
	r.Header.Set("X-API-Key", "pic_api_fixture")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("X-API-Key rejected")
	}
	w = keyRequest(s, "GET", "/api/admin/api-key", "test-pass-123", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "pic_api_fixture") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Body.String())
	}
	// X-Auth-Token header must also be accepted by admin routes consistently.
	rx := httptest.NewRequest("GET", "/api/admin/api-key", nil)
	rx.Header.Set("X-Auth-Token", "test-pass-123")
	wx := httptest.NewRecorder()
	s.Handler().ServeHTTP(wx, rx)
	if wx.Code != 200 || !strings.Contains(wx.Body.String(), "pic_api_fixture") {
		t.Fatalf("X-Auth-Token rejected on /api/admin/api-key: %d %s", wx.Code, wx.Body.String())
	}
	w = keyRequest(s, "POST", "/api/admin/api-key", "test-pass-123", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if s.cfg.APIKey == "pic_api_fixture" || s.cfg.AuthToken != "test-pass-123" || s.cfg.APIKey == s.cfg.AuthToken {
		t.Fatal("rotation changed password or reused key")
	}
	if w := keyRequest(s, "GET", "/api/objects", "pic_api_fixture", ""); w.Code != 401 {
		t.Fatal("old key still accepted")
	}
	got, e := config.LoadConfig(s.cfg.Path)
	if e != nil || got.APIKey != s.cfg.APIKey || got.AuthToken != "test-pass-123" {
		t.Fatal("key not durable", e)
	}
}
func TestAPIKeySaveFailureKeepsOldKey(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	s.cfg.APIKey = "pic_api_fixture"
	s.cfg.Path = filepath.Join(s.cfg.StateDir, "not-a-directory", "config.json") // parent is created; make the target itself a directory to force rename failure.
	s.cfg.Path = s.cfg.StateDir
	w := keyRequest(s, "POST", "/api/admin/api-key", "test-pass-123", "")
	if w.Code != 500 || s.cfg.APIKey != "pic_api_fixture" || s.cfg.AuthToken != "test-pass-123" {
		t.Fatal("failed save published change", w.Code, w.Body.String())
	}
}
func TestAuthenticatedDownloadAndKeyDelete(t *testing.T) {
	s, done := settingsFixture(t)
	defer done()
	s.cfg.APIKey = "pic_api_fixture"
	obj := &storage.ObjectRecord{Key: "telegram/2026/11/02/abcdefabcdef.png", FID: "old", Size: 12, Backend: "telegram", ConnectionID: s.cfg.DefaultConnectionID, Mime: "image/png"}
	if e := s.db.SaveObject(obj); e != nil {
		t.Fatal(e)
	}
	if w := keyRequest(s, "GET", "/api/download/"+obj.Key, "wrong", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := keyRequest(s, "GET", "/api/download/"+obj.Key, "pic_api_fixture", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte("old-bot-file")) || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	w = keyRequest(s, "GET", "/api/download/invalid", "pic_api_fixture", "")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	// Missing object is rejected by the storage path, not by API-key authentication.
	w = keyRequest(s, "DELETE", "/api/objects", "pic_api_fixture", `{"key":"telegram/2026/11/02/000000000000.png"}`)
	if w.Code == 401 {
		t.Fatal("API key cannot delete")
	}
}
func TestAPIKeyNeverLeakedInStatus(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	s.cfg.APIKey = "pic_api_fixture"
	for _, key := range []string{"", "pic_api_fixture", "test-pass-123"} {
		w := keyRequest(s, "GET", "/api/status?auth_only=1", key, "")
		var body map[string]any
		json.Unmarshal(w.Body.Bytes(), &body)
		if strings.Contains(w.Body.String(), s.cfg.APIKey) {
			t.Fatal("key leaked in status")
		}
	}
}
