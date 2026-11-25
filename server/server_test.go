package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/storage"
)

func setupTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "picbed-test-*")
	if err != nil {
		t.Fatalf("mktemp failed: %v", err)
	}

	cfg := &config.Config{
		Port:         8798,
		Host:         "127.0.0.1",
		PublicOrigin: "https://pic.example",
		StateDir:     tmpDir,
		AuthToken:    "test-pass-123",
	}

	db, err := storage.OpenDB(tmpDir)
	if err != nil {
		t.Fatalf("open db failed: %v", err)
	}

	cfg.DefaultConnectionID = "0123456789abcdef"
	cfg.Connections = []config.Connection{{ID: cfg.DefaultConnectionID, BotID: 123, BotToken: "123:fixture", ChatID: -100123}}
	srv, err := NewServer(cfg, db)
	if err != nil {
		t.Fatalf("new server failed: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(tmpDir)
	}
	return srv, cleanup
}

func TestIndexPage(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, marker := range []string{"<title>Pic TG</title>", `id="uploadBtn"`, `id="queue"`} {
		if !strings.Contains(body, marker) {
			t.Errorf("index missing %q", marker)
		}
	}
}

func TestStatusUnauthenticatedNoLeak(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var res struct {
		Code int `json:"code"`
		Data map[string]any
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse failed: %v", err)
	}

	if res.Data["has_auth"] != true {
		t.Fatalf("expected has_auth true")
	}
	// Token object must NOT be exposed
	if _, exists := res.Data["token"]; exists {
		t.Fatalf("token object must not be exposed to unauthenticated callers")
	}
}

func TestFaviconEndpoint(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("expected image/svg+xml, got %s", w.Header().Get("Content-Type"))
	}
}

func TestNonExistentImage404(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/images/telegram/2026/11/02/abcdef123456.png", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func pngFixture(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestUploadFormTokenNoDiskSpill(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	raw := pngFixture(t)
	large := append(append([]byte(nil), raw...), make([]byte, 10*1024*1024)...)
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(raw))
	processed, _, _, _, err := prepareUpload(context.Background(), large, "png", cfg)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(processed)
	key := fmt.Sprintf("telegram/0123456789abcdef/%s/%x.png", time.Now().UTC().Format("2006/01/02"), sum[:6])
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, ConnectionID: "0123456789abcdef", FID: "fixture", FileID: "fixture", Size: int64(len(raw)), Width: 4, Height: 3, Format: "png", Mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writer.WriteField("token", "test-pass-123") // Legacy clients authenticate before the image bytes.
	part, err := writer.CreateFormFile("file", "large.png")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(raw)
	// Stay within the TG limit and verify form-token auth keeps the complete
	// multipart file in memory. Re-encoding removes the padded trailing data.
	part.Write(make([]byte, 10*1024*1024))
	writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	srv.handleUpload(w, req)
	if w.Code != 200 {
		t.Fatalf("upload failed: %d %s", w.Code, w.Body.String())
	}
	inMemory, _, err := req.FormFile("file")
	if err != nil {
		t.Fatalf("multipart depended on a deleted temporary file: %v", err)
	}
	if _, onDisk := inMemory.(*os.File); onDisk {
		t.Fatal("multipart spilled image to disk")
	}
	inMemory.Close()
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload wrote temporary files: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.StateDir, "thumbnails")); !os.IsNotExist(err) {
		t.Fatalf("thumbnail cache unexpectedly exists: %v", err)
	}
}

func TestMultipartAuthDoesNotParseImplicitly(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	req := httptest.NewRequest("GET", "/api/status", strings.NewReader("not a multipart body"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	if srv.checkAuth(req) {
		t.Fatal("unexpected auth")
	}
	if req.MultipartForm != nil {
		t.Fatal("auth parsed multipart implicitly")
	}
}

func TestRejectedUploadAndRetiredCompressor(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("file", "fake.png")
	part.Write([]byte("not an image"))
	writer.Close()
	req := httptest.NewRequest("POST", "/api/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("invalid PNG accepted: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", "/api/admin/compress-stock", nil)
	req.Header.Set("Authorization", "Bearer test-pass-123")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("legacy compressor still available: %d", w.Code)
	}
}

func TestPasswordSaveFailureKeepsOldPassword(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	srv.cfg.Path = srv.cfg.StateDir // rename cannot replace a directory
	req := httptest.NewRequest("POST", "/api/admin/password", strings.NewReader(`{"old_password":"test-pass-123","new_password":"replacement"}`))
	req.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 500 || srv.authToken() != "test-pass-123" {
		t.Fatalf("password changed after failed save: %d", w.Code)
	}
}

func TestConcurrentAuthentication(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	srv.cfg.Path = filepath.Join(srv.cfg.StateDir, "config.json")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				req := httptest.NewRequest("POST", "/api/auth/verify", strings.NewReader(`{"token":"incorrect"}`))
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
			}
		}()
	}
	wg.Wait()
}

func TestCORSRejectsPrefixLookalikes(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	for _, origin := range []string{"http://localhost.attacker.invalid", "http://127.0.0.1.attacker.invalid", "http://pic.example", "http://localhost:3000"} {
		req := httptest.NewRequest("OPTIONS", "/api/status", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("allowed %s", origin)
		}
	}
	req := httptest.NewRequest("OPTIONS", "/api/status", nil)
	req.Header.Set("Origin", "https://pic.example")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://pic.example" || w.Header().Get("Vary") != "Origin" {
		t.Fatalf("configured public_origin rejected or missing Vary: %v", w.Header())
	}
}

func TestStaticAssetsServedWithVersionHash(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/assets/app.css?v=") || !strings.Contains(w.Body.String(), "/assets/app.js?v=") {
		t.Fatalf("index missing hashed asset links: %d", w.Code)
	}
	for path, wantType := range map[string]string{"/assets/app.css": "text/css", "/assets/app.js": "application/javascript"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), wantType) || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("asset %s failed: %d %v", path, w.Code, w.Header())
		}
	}
}

func TestPasswordSameAsAPIKeyReturns400(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	srv.cfg.APIKey = "pic_api_fixed"
	srv.cfg.Path = filepath.Join(srv.cfg.StateDir, "config.json")
	req := httptest.NewRequest("POST", "/api/admin/password", strings.NewReader(`{"old_password":"test-pass-123","new_password":"pic_api_fixed"}`))
	req.Header.Set("Authorization", "Bearer test-pass-123")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 400 || srv.authToken() != "test-pass-123" {
		t.Fatalf("expected 400 on password==api_key conflict: %d %s", w.Code, w.Body.String())
	}
}

func TestConcurrentOriginDownloadCoalescing(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	raw := pngFixture(t)
	key := "telegram/0123456789abcdef/2026/11/02/123456789abc.png"
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, ConnectionID: "0123456789abcdef", FID: "fixture", FileID: "fixture", Size: int64(len(raw)), Mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	downloads := 0
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/image.png"}}`))
			return
		}
		mu.Lock()
		downloads++
		mu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Write(raw)
	}))
	defer api.Close()
	srv.clients["0123456789abcdef"].APIBase = api.URL
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/images/"+key, nil))
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw) {
				errs <- fmt.Errorf("status=%d len=%d", w.Code, w.Body.Len())
			}
		}()
	}
	<-started
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if downloads != 1 {
		t.Fatalf("expected 1 coalesced upstream download, got %d", downloads)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestThumbnailGeneratedWithoutDiskCache(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	raw := pngFixture(t)
	key := "telegram/0123456789abcdef/2026/11/02/123456789abc.png"
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, ConnectionID: "0123456789abcdef", FID: "fixture", FileID: "fixture", Size: int64(len(raw)), Mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	downloads := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/image.png"}}`))
		} else {
			downloads++
			w.Write(raw)
		}
	}))
	defer api.Close()
	srv.clients["0123456789abcdef"].APIBase = api.URL
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/thumb/"+key, nil))
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("thumbnail failed: %d %s", w.Code, w.Body.String())
		}
		if _, _, err := image.DecodeConfig(w.Body); err != nil {
			t.Fatal(err)
		}
	}
	if downloads != 2 {
		t.Fatalf("unexpected local caching: %d downloads", downloads)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.StateDir, "thumbnails")); !os.IsNotExist(err) {
		t.Fatalf("disk thumbnail cache created: %v", err)
	}
	entries, _ := os.ReadDir(srv.cfg.StateDir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".png") || strings.HasSuffix(entry.Name(), ".jpg") {
			t.Fatalf("image written to disk: %s", entry.Name())
		}
	}
}

func TestPasswordConcurrentReaders(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	srv.cfg.Path = filepath.Join(srv.cfg.StateDir, "config.json")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				req := httptest.NewRequest("GET", "/api/objects", nil)
				req.Header.Set("Authorization", "Bearer test-pass-123")
				if !srv.checkAuth(req) {
					t.Error("unchanged token rejected")
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/admin/password", strings.NewReader(`{"old_password":"test-pass-123","new_password":"test-pass-123"}`))
		req.Header.Set("Authorization", "Bearer test-pass-123")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("password save failed: %d", w.Code)
		}
	}
	wg.Wait()
}

func TestImageFailuresAreNotCached(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	for _, prefix := range []string{"/thumb/", "/images/", "/direct/"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", prefix+"telegram/2026/11/02/123456789abc.png", nil))
		if w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("transient image failure cached for %s: %d %s", prefix, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	// Queue cancellation applies to a still-indexed image, not a revoked URL.
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: "telegram/2026/11/02/123456789abc.png", FID: "fixture", FileID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	srv.thumbnailSlots <- struct{}{}
	srv.thumbnailSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/thumb/telegram/2026/11/02/123456789abc.png", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("thumbnail queue ignored cancellation: %d", w.Code)
	}
}

func TestUploadPreservesBrowserSourceSize(t *testing.T) {
	for _, sourceSize := range []string{"3044726", "", "-1", "999999999999"} {
		t.Run("source-size-"+sourceSize, func(t *testing.T) {
			srv, cleanup := setupTestServer(t)
			defer cleanup()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseMultipartForm(1 << 20)
				f, _, _ := r.FormFile("document")
				defer f.Close()
				raw, _ := io.ReadAll(f)
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1, "chat": map[string]any{"id": -100123}, "document": map[string]any{"file_id": "test", "file_size": len(raw)}}})
			}))
			defer api.Close()
			srv.clients["0123456789abcdef"].APIBase = api.URL
			imageData := pngFixture(t)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, _ := writer.CreateFormFile("file", "source.png")
			part.Write(imageData)
			if sourceSize != "" {
				writer.WriteField("orig_size", sourceSize)
			}
			writer.Close()
			req := httptest.NewRequest("POST", "/api/upload", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer test-pass-123")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("upload: %d %s", w.Code, w.Body.String())
			}
			var result struct {
				Data struct {
					Key      string `json:"key"`
					Original int64  `json:"orig_size"`
					Size     int64  `json:"size"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			expected := int64(len(imageData))
			if sourceSize == "3044726" {
				expected = 3044726
			}
			stored, err := srv.db.GetObject(result.Data.Key)
			if err != nil {
				t.Fatal(err)
			}
			if result.Data.Original != expected || stored.OrigSize != expected || result.Data.Size != int64(len(imageData)) {
				t.Fatalf("source/output sizes lost: %+v stored=%+v", result.Data, stored)
			}
		})
	}
}
