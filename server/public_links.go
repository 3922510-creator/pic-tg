package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/3922510-creator/pic-tg/storage"
)

var publicTokenPattern = regexp.MustCompile(`^[a-f0-9]{32}\.(png|jpg|jpeg|gif|webp|avif)$`)

func (s *Server) formatPublicURL(token string) string {
	return strings.TrimRight(s.cfg.PublicOrigin, "/") + "/i/" + token
}

func (s *Server) publicURL(key string) (string, error) {
	token, err := s.db.PublicToken(key)
	if err != nil {
		return "", err
	}
	return s.formatPublicURL(token), nil
}

func (s *Server) handlePublicImage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/i/")
	if !publicTokenPattern.MatchString(token) {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	key, err := s.db.KeyForPublicToken(token)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	s.serveProxyImage(w, r, key)
}

// Upgrade browser-local history without listing or importing other devices' uploads.
func (s *Server) handleShareLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.checkDataAuth(r) {
		s.jsonResp(w, http.StatusUnauthorized, "未授权", nil)
		return
	}
	var input struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil || len(input.Keys) > 200 {
		s.jsonResp(w, http.StatusBadRequest, "链接请求无效", nil)
		return
	}
	links := make(map[string]string)
	for _, key := range input.Keys {
		if !storage.KeyPattern.MatchString(key) {
			continue
		}
		obj, err := s.db.GetObject(key)
		if err != nil {
			s.jsonResp(w, 500, "链接读取失败", nil)
			return
		}
		if obj == nil {
			continue
		}
		link, err := s.publicURL(key)
		if err != nil {
			s.jsonResp(w, 500, "链接生成失败", nil)
			return
		}
		links[key] = link
	}
	s.jsonResp(w, 200, "success", links)
}
