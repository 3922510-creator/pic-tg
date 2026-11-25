package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocumentRoundTripAndRealDeleteResult(t *testing.T) {
	raw := []byte("original document bytes")
	deleted := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendDocument"):
			if err := r.ParseMultipartForm(1024 * 1024); err != nil {
				t.Error(err)
			}
			f, h, err := r.FormFile("document")
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			var b bytes.Buffer
			b.ReadFrom(f)
			if !bytes.Equal(b.Bytes(), raw) || r.FormValue("chat_id") != "-100123" || r.FormValue("disable_content_type_detection") != "true" || h.Header.Get("Content-Type") != "image/png" {
				t.Error("document changed")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 42, "chat": map[string]any{"id": -100123}, "document": map[string]any{"file_id": "file-id", "file_unique_id": "unique", "file_size": len(raw)}}})
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "documents/image.png", "file_size": len(raw)}})
		case strings.Contains(r.URL.Path, "/file/bot"):
			w.Write(raw)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			deleted = true
			w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/getChatMember"):
			w.Write([]byte(`{"ok":true,"result":{"status":"administrator","can_post_messages":true,"can_delete_messages":true}}`))
		default:
			t.Error("unexpected API")
			http.Error(w, "unexpected", 500)
		}
	}))
	defer ts.Close()
	c, err := New("123:fixture", -100123)
	if err != nil {
		t.Fatal(err)
	}
	c.APIBase = ts.URL
	m, err := c.Upload(context.Background(), "image.png", "image/png", raw)
	if err != nil || m.ID != 42 {
		t.Fatal(m, err)
	}
	got, err := c.Download(context.Background(), m.Document.FileID)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal(err)
	}
	if ok, detail := c.Probe(context.Background()); !ok {
		t.Fatal(detail)
	}
	if err := c.Delete(context.Background(), -100123, 42); err != nil || !deleted {
		t.Fatal(err)
	}
	if _, err := c.Upload(context.Background(), "large.png", "image/png", make([]byte, MaxFileSize+1)); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestUnsafeFilePathsLimitsAndSecretRedaction(t *testing.T) {
	mode := "path"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "path":
			w.Write([]byte(`{"ok":true,"result":{"file_path":"https://evil.example/image.png"}}`))
		case "size":
			w.Write([]byte(`{"ok":true,"result":{"file_path":"documents/a.png","file_size":20971521}}`))
		case "error":
			w.WriteHeader(400)
			w.Write([]byte(`{"ok":false,"description":"invalid token 123:secret"}`))
		case "delete":
			w.Write([]byte(`{"ok":true,"result":false}`))
		}
	}))
	defer ts.Close()
	c, _ := New("123:secret", -100123)
	c.APIBase = ts.URL
	for _, m := range []string{"path", "size", "error"} {
		mode = m
		_, err := c.Download(context.Background(), "id")
		if err == nil || strings.Contains(err.Error(), "123:secret") {
			t.Fatalf("unsafe result: %v", err)
		}
	}
	mode = "delete"
	if err := c.Delete(context.Background(), -100123, 42); err == nil {
		t.Fatal("false delete reported success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Download(ctx, "id"); err != context.Canceled {
		t.Fatal(err)
	}
}
