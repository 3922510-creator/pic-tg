package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/storage"
	"github.com/3922510-creator/pic-tg/telegram"
	"strings"
	"sync"
	"testing"
)

func settingsFixture(t *testing.T) (*Server, func()) {
	s, done := setupTestServer(t)
	s.cfg.Path = filepath.Join(s.cfg.StateDir, "config.json")
	if e := config.SaveConfig(s.cfg.Path, s.cfg); e != nil {
		t.Fatal(e)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		bot := int64(123)
		if strings.Contains(r.URL.Path, "bot456:") {
			bot = 456
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": bot, "username": "fixture", "is_bot": true}})
		case strings.HasSuffix(r.URL.Path, "/getChat"):
			var b struct {
				ChatID int64 `json:"chat_id"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"id": b.ChatID, "title": "Fixture channel", "type": "channel"}})
		case strings.HasSuffix(r.URL.Path, "/getChatMember"):
			w.Write([]byte(`{"ok":true,"result":{"status":"administrator","can_post_messages":true,"can_delete_messages":true}}`))
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			w.Write([]byte(`{"ok":true,"result":{"url":""}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			w.Write([]byte(`{"ok":true,"result":[{"channel_post":{"chat":{"id":-100123,"title":"Fixture channel","type":"channel"}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/file.png"}}`))
		case strings.Contains(r.URL.Path, "/file/bot123:"):
			w.Write([]byte("old-bot-file"))
		case strings.Contains(r.URL.Path, "/file/bot456:"):
			w.Write([]byte("new-bot-file"))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	s.factory = func(token string, id int64) (*telegram.Client, error) {
		c, e := telegram.New(token, id)
		if e == nil {
			c.APIBase = api.URL
		}
		return c, e
	}
	for _, c := range s.clients {
		c.APIBase = api.URL
	}
	return s, func() { api.Close(); done() }
}
func settingsRequest(s *Server, action string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	method := "POST"
	url := "/api/admin/telegram/" + action
	if action == "" {
		method = "GET"
		url = "/api/admin/telegram"
	}
	r := httptest.NewRequest(method, url, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer test-pass-123")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestSettingsHotSwitchAndOldSource(t *testing.T) {
	s, done := settingsFixture(t)
	defer done()
	oldID := s.cfg.DefaultConnectionID
	old := &storage.ObjectRecord{Key: "telegram/2026/11/02/abcdefabcdef.png", FID: "old", Size: 12, Mime: "image/png", Backend: "telegram", ConnectionID: oldID}
	if e := s.db.SaveObject(old); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"verify", "discover"} {
		w := settingsRequest(s, action, map[string]any{"connection_id": oldID})
		if w.Code != 200 || strings.Contains(w.Body.String(), "123:fixture") {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	w := settingsRequest(s, "save", map[string]any{"token": "456:other", "chat_id": -100456, "revision": 0})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := s.cfg.DefaultConnectionID
	if id == oldID || len(s.cfg.Connections) != 2 {
		t.Fatal("switch lost history")
	}
	if strings.Contains(w.Body.String(), "456:other") {
		t.Fatal("token leaked")
	}
	raw, _, e := s.downloadStored(context.Background(), old.Key)
	if e != nil || string(raw) != "old-bot-file" {
		t.Fatal("old bot source lost", string(raw), e)
	}
	loaded, e := config.LoadConfig(s.cfg.Path)
	if e != nil || loaded.DefaultConnectionID != id || len(loaded.Connections) != 2 {
		t.Fatal("not persisted", e)
	}
	if _, e := NewServer(loaded, s.db); e != nil {
		t.Fatal("restart failed", e)
	}
	w = settingsRequest(s, "save", map[string]any{"connection_id": oldID, "chat_id": -100123, "revision": 1})
	if w.Code != 200 || s.cfg.DefaultConnectionID != oldID || len(s.cfg.Connections) != 2 {
		t.Fatal("switch-back failed", w.Body.String())
	}
}
func TestSettingsFailureAndRevision(t *testing.T) {
	s, done := settingsFixture(t)
	defer done()
	oldID := s.cfg.DefaultConnectionID
	w := settingsRequest(s, "save", map[string]any{"token": "456:other", "chat_id": -100456, "revision": 9})
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	s.cfg.Path = filepath.Join(s.cfg.StateDir, "missing", "..", "config.json", "child")
	w = settingsRequest(s, "save", map[string]any{"token": "456:other", "chat_id": -100456, "revision": 0})
	if w.Code != 500 || s.cfg.DefaultConnectionID != oldID || s.cfg.Revision != 0 || len(s.clients) != 1 {
		t.Fatal("failed persistence published config", w.Code, w.Body.String())
	}
}
func TestSettingsTokenRotation(t *testing.T) {
	s, done := settingsFixture(t)
	defer done()
	id := s.cfg.DefaultConnectionID
	for _, b := range []map[string]any{{"connection_id": id, "chat_id": -100999, "revision": 0}, {"token": "123:rotated", "chat_id": -100123, "revision": 1}} {
		if w := settingsRequest(s, "save", b); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	for _, p := range s.cfg.Connections {
		if p.BotToken != "123:rotated" {
			t.Fatal("same-bot old channel token not rotated")
		}
	}
	if len(s.cfg.Connections) != 2 || s.cfg.DefaultConnectionID != id {
		t.Fatal("rotation duplicated connection")
	}
	w := settingsRequest(s, "", nil)
	if strings.Contains(w.Body.String(), "rotated") || strings.Contains(w.Body.String(), "bot_token") {
		t.Fatal("token leaked")
	}
	r := httptest.NewRequest("GET", "/api/admin/telegram", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("settings exposed")
	}
}
func TestSettingsConcurrentSave(t *testing.T) {
	s, done := settingsFixture(t)
	defer done()
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- settingsRequest(s, "save", map[string]any{"connection_id": "0123456789abcdef", "chat_id": -100123, "revision": 0}).Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
}
func TestStartupRejectsOrphanedSources(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	s.db.SaveObject(&storage.ObjectRecord{Key: "telegram/2026/11/02/abcdefabcdef.png", FID: "orphan", ConnectionID: "missing"})
	if _, e := NewServer(s.cfg, s.db); e == nil {
		t.Fatal("orphaned source accepted")
	}
}
