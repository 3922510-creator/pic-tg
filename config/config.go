package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var connectionIDPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

type Connection struct {
	ID          string `json:"id"`
	BotID       int64  `json:"bot_id"`
	BotUsername string `json:"bot_username"`
	BotToken    string `json:"bot_token"`
	ChatID      int64  `json:"chat_id"`
	ChatTitle   string `json:"chat_title"`
}

type Config struct {
	Path                string       `json:"-"`
	Initialized         bool         `json:"-"`
	Port                int          `json:"port"`
	Host                string       `json:"host"`
	PublicOrigin        string       `json:"public_origin"`
	StateDir            string       `json:"state_dir"`
	AuthToken           string       `json:"auth_token"`
	APIKey              string       `json:"api_key"`
	TrustProxyHeader    string       `json:"trust_proxy_header,omitempty"`
	AllowedOrigins      []string     `json:"allowed_origins,omitempty"`
	Revision            int64        `json:"revision"`
	DefaultConnectionID string       `json:"default_connection_id"`
	Connections         []Connection `json:"telegram_connections"`
}

func DefaultConfig() *Config {
	return &Config{
		Port:         8798,
		Host:         "127.0.0.1",
		PublicOrigin: "http://127.0.0.1:8798",
	}
}

// NormalizeTrustProxyHeader validates and canonicalizes an optional reverse-proxy IP header.
func NormalizeTrustProxyHeader(header string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "":
		return "", nil
	case "cf-connecting-ip":
		return "CF-Connecting-IP", nil
	case "x-real-ip":
		return "X-Real-IP", nil
	case "x-forwarded-for":
		return "X-Forwarded-For", nil
	default:
		return "", errors.New("trust_proxy_header 仅支持留空、CF-Connecting-IP、X-Real-IP 或 X-Forwarded-For")
	}
}

func validateOrigin(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("public_origin 必须是有效的 http/https 地址")
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port 必须在 1 到 65535 之间")
	}
	if err := validateOrigin(c.PublicOrigin); err != nil {
		return err
	}
	normHeader, err := NormalizeTrustProxyHeader(c.TrustProxyHeader)
	if err != nil {
		return err
	}
	c.TrustProxyHeader = normHeader
	for _, origin := range c.AllowedOrigins {
		if err := validateOrigin(origin); err != nil {
			return errors.New("allowed_origins 包含无效地址")
		}
	}
	if c.APIKey != "" && c.APIKey == c.AuthToken {
		return errors.New("API Key 不能与登录口令相同")
	}
	seen := map[string]bool{}
	found := c.DefaultConnectionID == "" && len(c.Connections) == 0
	for _, p := range c.Connections {
		if !connectionIDPattern.MatchString(p.ID) || seen[p.ID] || p.BotToken == "" || p.ChatID >= 0 || p.BotID <= 0 {
			return errors.New("TG 接入配置无效或 ID 重复")
		}
		seen[p.ID] = true
		if p.ID == c.DefaultConnectionID {
			found = true
		}
	}
	if !found {
		return errors.New("默认 TG 接入配置不存在")
	}
	return nil
}

func LoadConfig(path string) (*Config, error) {
	c := DefaultConfig()
	c.Path = path
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			authToken, e := NewAuthToken()
			if e != nil {
				return nil, e
			}
			key, e := NewAPIKey()
			if e != nil {
				return nil, e
			}
			c.AuthToken = authToken
			c.APIKey = key
			c.Initialized = true
			return c, SaveConfig(path, c)
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.APIKey == "" {
		key, e := NewAPIKey()
		if e != nil {
			return nil, e
		}
		c.APIKey = key
		if e := SaveConfig(path, c); e != nil {
			return nil, e
		}
	}
	return c, nil
}

func SaveConfig(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func NewAuthToken() (string, error) {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}

func NewAPIKey() (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return "pic_api_" + hex.EncodeToString(b[:]), nil
}
