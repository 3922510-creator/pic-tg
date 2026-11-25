package server

import (
	"crypto/subtle"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/storage"
)

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Only data endpoints accept the API key. Admin/login checks remain password-only.
func (s *Server) checkDataAuth(r *http.Request) bool {
	s.mu.Lock()
	key, password := s.cfg.APIKey, s.cfg.AuthToken
	s.mu.Unlock()
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Header.Get("X-API-Key") != "" {
		presented = r.Header.Get("X-API-Key")
	}
	if key != "" && constantTimeEqual(presented, key) {
		return true
	}
	// An API key, even accidentally presented via a legacy field, must not gain
	// privileges through passwordless mode. Such mode is otherwise kept intact.
	if password == "" && key != "" {
		return r.Header.Get("Authorization") == "" && r.Header.Get("X-API-Key") == ""
	}
	return s.checkAuth(r)
}

func (s *Server) handleAPIKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.checkAuth(r) {
		s.jsonResp(w, 401, "仅网页登录口令可管理 API Key", nil)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Recheck after acquiring the config write lock, so a concurrent password
	// change cannot leave a formerly authorized request able to rotate the key.
	if !s.checkAuthLocked(r) {
		s.jsonResp(w, 401, "登录状态已改变，请重新登录", nil)
		return
	}
	if r.Method == http.MethodPost {
		key, e := config.NewAPIKey()
		if e != nil {
			s.jsonResp(w, 500, "生成 API Key 失败", nil)
			return
		}
		next := *s.cfg
		next.APIKey = key
		next.Revision++
		if next.Path == "" {
			s.jsonResp(w, 500, "未配置凭据保存路径", nil)
			return
		}
		if e := config.SaveConfig(next.Path, &next); e != nil {
			s.jsonResp(w, 500, "保存失败，旧 API Key 仍然有效", nil)
			return
		}
		s.cfg.APIKey = key
		s.cfg.Revision = next.Revision
	}
	s.jsonResp(w, 200, "success", map[string]any{"api_key": s.cfg.APIKey})
}

func (s *Server) handleAPIDownload(w http.ResponseWriter, r *http.Request) {
	if !s.checkDataAuth(r) {
		s.jsonResp(w, 401, "未授权", nil)
		return
	}
	key := r.PathValue("key")
	if !storage.KeyPattern.MatchString(key) {
		s.jsonResp(w, 400, "无效图片路径", nil)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	data, mimeType, e := s.downloadStored(r.Context(), key)
	if e != nil {
		s.jsonResp(w, 404, "图片不存在或下载失败", nil)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(key)}))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}
