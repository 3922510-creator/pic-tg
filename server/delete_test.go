package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"github.com/3922510-creator/pic-tg/storage"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeleteUnavailableConnectionAndDatabaseFailure(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	key := "telegram/0123456789abcdef/2026/11/02/abcdef123456.png"
	obj := &storage.ObjectRecord{Key: key, FID: "f", FileID: "f", ConnectionID: "missing", Backend: "telegram"}
	if err := srv.db.SaveObject(obj); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.PublicToken(key); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.Exec(`CREATE TRIGGER block_delete BEFORE DELETE ON objects BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := srv.deleteStored(context.Background(), key); err == nil {
		t.Fatal("local DB failure reported success")
	}
	if got, err := srv.db.GetObject(key); err != nil || got == nil {
		t.Fatal("failed DB deletion lost index", err)
	}
	if _, err := srv.db.Exec(`DROP TRIGGER block_delete`); err != nil {
		t.Fatal(err)
	}
	if err := srv.deleteStored(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.db.GetObject(key); err != nil || got != nil {
		t.Fatal("missing connection blocked local revocation", err)
	}
}

// A stalled Telegram must not cost 30 seconds per image or leave later links
// active when the request disconnects. All state here is a disposable test DB.
func TestBatchDeleteBudgetConcurrencyAndCancellation(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	var active, peak, calls atomic.Int32
	s.clients[s.cfg.DefaultConnectionID].HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		calls.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("unbounded remote delete")
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	var keys, paths []string
	for i := 0; i < 9; i++ {
		key := fmt.Sprintf("telegram/0123456789abcdef/2026/11/02/%012x.png", i)
		if err := s.db.SaveObject(&storage.ObjectRecord{Key: key, FID: "fixture", FileID: "fixture", ConnectionID: s.cfg.DefaultConnectionID, Backend: "telegram", TGChatID: -100123, TGMessageID: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
		token, err := s.db.PublicToken(key)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
		paths = append(paths, "/i/"+token, "/images/"+key, "/thumb/"+key)
	}
	body, _ := json.Marshal(map[string]any{"keys": keys})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest("DELETE", "/api/objects", bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(w, r)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatal("batch outlived its budget", elapsed)
	}
	var result struct {
		Data struct {
			DeletedKeys []string `json:"deleted_keys"`
			Errors      []string `json:"errors"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Data.DeletedKeys) != len(keys) || len(result.Data.Errors) != 0 || peak.Load() != 3 || calls.Load() > 3 || active.Load() != 0 {
		t.Fatalf("batch failed: %d %s peak=%d calls=%d active=%d", w.Code, w.Body.String(), peak.Load(), calls.Load(), active.Load())
	}
	for _, path := range paths {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("If-None-Match", `"old"`)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("batch left link active", path, w.Code)
		}
	}
}

func TestBatchDeleteReportsPartialDatabaseFailure(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	var keys []string
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("telegram/0123456789abcdef/2026/11/02/%012x.png", i)
		keys = append(keys, key)
		if err := s.db.SaveObject(&storage.ObjectRecord{Key: key, FID: "f", FileID: "f", ConnectionID: "missing"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER deny_one BEFORE DELETE ON objects WHEN OLD.key = '` + keys[1] + `' BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	deleted, errs := s.deleteBatch(context.Background(), keys)
	if len(deleted) != 2 || deleted[0] != keys[0] || deleted[1] != keys[2] || len(errs) != 1 {
		t.Fatal(deleted, errs)
	}
	kept, err := s.db.GetObject(keys[1])
	if err != nil || kept == nil {
		t.Fatal("DB error lost retained object", err)
	}
}
