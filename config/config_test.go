package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicConfigAndPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != path {
		t.Fatal("config save path lost")
	}
	if !cfg.Initialized || cfg.AuthToken == "" || cfg.APIKey == "" || cfg.AuthToken == cfg.APIKey {
		t.Fatalf("first-run config did not generate distinct credentials: init=%v", cfg.Initialized)
	}
	cfg.AuthToken = "new-secret"
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.AuthToken != cfg.AuthToken || loaded.Initialized {
		t.Fatalf("reload failed: %v init=%v", err, loaded.Initialized)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Fatalf("credential config mode: %o", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary config leaked: %v", entries)
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() *Config {
		c := DefaultConfig()
		c.AuthToken = "login-pass"
		c.APIKey = "pic_api_key"
		return c
	}
	for name, mutate := range map[string]func(*Config){
		"bad_port":         func(c *Config) { c.Port = 70000 },
		"bad_origin":       func(c *Config) { c.PublicOrigin = "ftp://pic.example" },
		"bad_proxy_header": func(c *Config) { c.TrustProxyHeader = "X-Spoofed" },
		"same_token_key":   func(c *Config) { c.APIKey = c.AuthToken },
	} {
		t.Run(name, func(t *testing.T) {
			c := base()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
	for _, hdr := range []string{"", "CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For"} {
		c := base()
		c.TrustProxyHeader = hdr
		if err := c.Validate(); err != nil {
			t.Fatalf("valid trust_proxy_header %q rejected: %v", hdr, err)
		}
	}
}

func TestStorageCredentialsSurvivePasswordSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := DefaultConfig()
	cfg.DefaultConnectionID = "0123456789abcdef"
	cfg.Connections = []Connection{{ID: cfg.DefaultConnectionID, BotID: 123, BotToken: "123:fixture", ChatID: -100123}}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got.AuthToken = "new-password"
	if err := SaveConfig(path, got); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || reloaded.Connections[0].BotToken != cfg.Connections[0].BotToken || reloaded.DefaultConnectionID != cfg.DefaultConnectionID {
		t.Fatal("storage credentials lost", err)
	}
}

func TestIndependentAPIKeyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"auth_token":"unchanged-login-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey == "" || cfg.APIKey == cfg.AuthToken || cfg.AuthToken != "unchanged-login-password" {
		t.Fatal("migration changed password or reused key")
	}
	again, err := LoadConfig(path)
	if err != nil || again.APIKey != cfg.APIKey {
		t.Fatal("key not stable", err)
	}
	cfg.AuthToken = cfg.APIKey
	if err := SaveConfig(path, cfg); err == nil {
		t.Fatal("same password and API key accepted")
	}
}
