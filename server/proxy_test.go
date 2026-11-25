package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"github.com/3922510-creator/pic-tg/storage"
	"strings"
	"testing"
)

func TestProxyAndLegacyDirectServeImageWithoutRedirect(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	key := "telegram/0123456789abcdef/2026/11/02/abcdef123456.png"
	raw := pngFixture(t)
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, ConnectionID: "0123456789abcdef", FID: "fixture", FileID: "fixture", Size: int64(len(raw)), Width: 4, Height: 3, Format: "png", Mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/image.png"}}`))
		} else {
			w.Write(raw)
		}
	}))
	defer api.Close()
	srv.clients["0123456789abcdef"].APIBase = api.URL
	publicURL, err := srv.publicURL(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, imageURL := range []string{"/images/" + key, "/direct/" + key, publicURL} {
		req := httptest.NewRequest("GET", imageURL, nil)
		req.Header.Set("Referer", "https://forum.example/thread")
		recorder := httptest.NewRecorder()
		srv.Handler().ServeHTTP(recorder, req)
		if recorder.Code != 200 || !bytes.Equal(recorder.Body.Bytes(), raw) || recorder.Header().Get("Location") != "" || recorder.Header().Get("Content-Type") != "image/png" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("not a direct image response: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
		}
		before := calls
		conditional := httptest.NewRequest("GET", imageURL, nil)
		conditional.Header.Set("If-None-Match", recorder.Header().Get("ETag"))
		notModified := httptest.NewRecorder()
		srv.Handler().ServeHTTP(notModified, conditional)
		if notModified.Code != 304 || calls != before {
			t.Fatal("conditional request downloaded image again")
		}
		missing := httptest.NewRecorder()
		srv.Handler().ServeHTTP(missing, httptest.NewRequest("GET", imageURL[:strings.LastIndex(imageURL, "/")+1]+"00000000000000000000000000000000.png", nil))
		if missing.Code != 404 || missing.Header().Get("Cache-Control") != "no-store" || missing.Header().Get("Location") != "" {
			t.Fatal("failure cached or redirected")
		}
	}
}
