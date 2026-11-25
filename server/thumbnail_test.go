package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"github.com/3922510-creator/pic-tg/storage"
	"strings"
	"testing"
)

func TestThumbnailVersionAvoidsOldImmutablePreviewCache(t *testing.T) {
	s, done := setupTestServer(t)
	defer done()
	raw := pngFixture(t)
	key := "telegram/0123456789abcdef/2026/11/02/123456789abc.png"
	if err := s.db.SaveObject(&storage.ObjectRecord{Key: key, FID: "fixture", FileID: "fixture", ConnectionID: s.cfg.DefaultConnectionID, Size: int64(len(raw)), Mime: "image/png"}); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/image.png"}}`))
		} else {
			w.Write(raw)
		}
	}))
	defer api.Close()
	s.clients[s.cfg.DefaultConnectionID].APIBase = api.URL
	w := keyRequest(s, "GET", "/api/objects", "test-pass-123", "")
	var res struct {
		Data struct {
			List []struct {
				ThumbURL string `json:"thumb_url"`
			} `json:"list"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Data.List) != 1 {
		t.Fatal(w.Body.String())
	}
	u, err := url.Parse(res.Data.List[0].ThumbURL)
	if err != nil || u.Query().Get("v") != "2" {
		t.Fatal("unversioned thumbnail", u, err)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", u.RequestURI(), nil))
	etag := w.Header().Get("ETag")
	if w.Code != 200 || !strings.HasPrefix(etag, `"thumb-v2-`) {
		t.Fatal(w.Code, etag)
	}
	r := httptest.NewRequest("GET", u.RequestURI(), nil)
	r.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 304 {
		t.Fatal("new version lost conditional caching", w.Code)
	}
}
