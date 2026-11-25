package server

import (
	"encoding/json"
	"net/http/httptest"
	"github.com/3922510-creator/pic-tg/storage"
	"strings"
	"testing"
)

func TestShareLinkAuthAndMissingTokens(t *testing.T) {
	srv, cleanup := setupTestServer(t)
	defer cleanup()
	key := "telegram/0123456789abcdef/2026/11/02/abcdef123456.png"
	if err := srv.db.SaveObject(&storage.ObjectRecord{Key: key, FID: "f", FileID: "f"}); err != nil {
		t.Fatal(err)
	}
	body := `{"keys":["` + key + `","telegram/2026/11/02/eeeeeeeeeeee.png"]}`
	for _, authorized := range []bool{false, true} {
		r := httptest.NewRequest("POST", "/api/share-links", strings.NewReader(body))
		if authorized {
			r.Header.Set("Authorization", "Bearer "+srv.cfg.AuthToken)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if !authorized {
			if w.Code != 401 {
				t.Fatal(w.Code)
			}
			continue
		}
		var result struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || len(result.Data) != 1 || !strings.Contains(result.Data[key], "/i/") || strings.Contains(result.Data[key], "telegram") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/i/not-valid.png", "/i/00000000000000000000000000000000.png"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}
