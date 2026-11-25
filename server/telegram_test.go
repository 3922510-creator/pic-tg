package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"github.com/3922510-creator/pic-tg/storage"
	"github.com/3922510-creator/pic-tg/telegram"
	"strings"
	"testing"
)

func TestTelegramCompleteStoragePath(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	var uploaded []byte
	sends := 0
	denyDelete := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendDocument"):
			sends++
			r.ParseMultipartForm(1024 * 1024)
			f, _, _ := r.FormFile("document")
			defer f.Close()
			uploaded, _ = io.ReadAll(f)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 123, "chat": map[string]any{"id": -100123}, "document": map[string]any{"file_id": "tg-file", "file_unique_id": "tg-unique", "file_size": len(uploaded)}}})
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "documents/image.png", "file_size": len(uploaded)}})
		case strings.Contains(r.URL.Path, "/file/bot"):
			w.Write(uploaded)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			if denyDelete {
				w.WriteHeader(400)
				w.Write([]byte(`{"ok":false,"description":"message cannot be deleted"}`))
			} else {
				w.Write([]byte(`{"ok":true,"result":true}`))
			}
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer api.Close()
	srv.clients["0123456789abcdef"].APIBase = api.URL
	upload := func() string {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, _ := form.CreateFormFile("file", "test.png")
		part.Write(pngFixture(t))
		form.WriteField("link_mode", "direct")
		form.Close()
		req := httptest.NewRequest("POST", "/api/upload", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.Header.Set("Authorization", "Bearer test-pass-123")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("upload: %s", w.Body.String())
		}
		var response struct {
			Data struct {
				Key     string `json:"key"`
				URL     string `json:"url"`
				Backend string `json:"backend"`
			}
		}
		json.Unmarshal(w.Body.Bytes(), &response)
		if response.Data.Backend != "telegram" || !strings.HasPrefix(response.Data.Key, "telegram/") || !strings.Contains(response.Data.URL, "/i/") {
			t.Fatal("incorrect provider")
		}
		return response.Data.Key
	}
	key := upload()
	if upload() != key || sends != 1 {
		t.Fatal("TG upload not deduplicated")
	}
	obj, err := srv.db.GetObject(key)
	if err != nil || obj.Backend != "telegram" || obj.TGMessageID != 123 || obj.TGChatID != -100123 {
		t.Fatal(obj, err)
	}
	for _, prefix := range []string{"/images/", "/direct/", "/thumb/"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", prefix+key, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/") || w.Header().Get("Location") != "" {
			t.Fatal(prefix, w.Code, w.Body.String())
		}
		if prefix != "/thumb/" && !bytes.Equal(w.Body.Bytes(), uploaded) {
			t.Fatal("image bytes changed")
		}
	}
	oldKey := "telegram/2026/11/02/abcdefabcdef.png"
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: oldKey, FID: "old", FileID: "old"}); err != nil {
		t.Fatal(err)
	}
	old, _ := srv.db.GetObject(oldKey)
	if old.Backend != "telegram" {
		t.Fatal("legacy row provider lost")
	}
	list := httptest.NewRequest("GET", "/api/objects", nil)
	list.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, list)
	if !strings.Contains(w.Body.String(), `"backend":"telegram"`) {
		t.Fatal("mixed gallery missing providers")
	}
	remove := func() int {
		req := httptest.NewRequest("DELETE", "/api/objects", strings.NewReader(`{"key":"`+key+`"}`))
		req.Header.Set("Authorization", "Bearer test-pass-123")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w.Code
	}
	saved, err := srv.db.GetObject(key)
	if err != nil || saved == nil {
		t.Fatal("fixture missing", err)
	}
	publicURL, err := srv.publicURL(key)
	if err != nil {
		t.Fatal(err)
	}
	etags := make(map[string]string)
	for _, path := range []string{publicURL, "/images/" + key, "/direct/" + key, "/thumb/" + key} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		etags[path] = w.Header().Get("ETag")
	}
	if remove() != 200 {
		t.Fatal("remote rejection must not prevent local deletion")
	}
	if kept, _ := srv.db.GetObject(key); kept != nil {
		t.Fatal("index retained after remote failure")
	}
	for path, etag := range etags {
		for _, conditional := range []bool{false, true} {
			r := httptest.NewRequest("GET", path, nil)
			if conditional {
				r.Header.Set("If-None-Match", etag)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("deleted link still usable", path, w.Code, w.Header())
			}
		}
	}
	if remove() != 200 {
		t.Fatal("repeated deletion must be idempotent")
	}
	if err := srv.db.SaveObject(saved); err != nil {
		t.Fatal(err)
	}
	denyDelete = false
	if remove() != 200 {
		t.Fatal("delete failed")
	}
	if kept, _ := srv.db.GetObject(key); kept != nil {
		t.Fatal("index retained after success")
	}
}

func TestTelegramUploadSizeGate(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	if srv.uploadLimit() != telegram.MaxFileSize {
		t.Fatal("TG size limit not selected")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "large.png")
	part.Write(make([]byte, telegram.MaxFileSize+1))
	form.Close()
	req := httptest.NewRequest("POST", "/api/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "20MB") {
		t.Fatal("oversized file accepted", w.Code, w.Body.String())
	}
}
