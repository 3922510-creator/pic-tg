package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoveryPreservesWebhookAndUpdates(t *testing.T) {
	for _, hook := range []string{"https://example.test/webhook", ""} {
		t.Run(hook, func(t *testing.T) {
			updates := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"url": hook}})
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
					t.Errorf("unexpected method %s", r.URL.Path)
				}
				updates++
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if _, ok := body["offset"]; ok {
					t.Error("updates acknowledged")
				}
				if _, ok := body["allowed_updates"]; ok {
					t.Error("filter modified")
				}
				w.Write([]byte(`{"ok":true,"result":[{"channel_post":{"chat":{"id":-100123,"title":"A","type":"channel"}}},{"my_chat_member":{"chat":{"id":-100123,"title":"A","type":"channel"}}}]}`))
			}))
			defer api.Close()
			c, _ := New("123:fixture", -100123)
			c.APIBase = api.URL
			list, e := c.DiscoverChannels(context.Background())
			if hook != "" {
				if e == nil || updates != 0 {
					t.Fatal("webhook not preserved")
				}
			} else if e != nil || len(list) != 1 || updates != 1 {
				t.Fatal(list, e, updates)
			}
		})
	}
}
func TestProbeRequiresDeletePermission(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":{"status":"administrator","can_post_messages":true,"can_delete_messages":false}}`))
	}))
	defer api.Close()
	c, _ := New("123:fixture", -100123)
	c.APIBase = api.URL
	if ok, _ := c.Probe(context.Background()); ok {
		t.Fatal("missing delete permission accepted")
	}
}
