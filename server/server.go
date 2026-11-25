package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/imageutil"
	"github.com/3922510-creator/pic-tg/storage"
	"github.com/3922510-creator/pic-tg/telegram"
)

//go:embed static/*
var staticFS embed.FS

type RateRecord struct {
	Failures    int
	LockedUntil time.Time
	LastSeen    time.Time
}

type embeddedAsset struct {
	data        []byte
	etag        string
	hash        string
	contentType string
}

type Server struct {
	cfg            *config.Config
	db             *storage.DB
	clients        map[string]*telegram.Client
	factory        func(string, int64) (*telegram.Client, error)
	tgUploads      chan struct{}
	deleteSlots    chan struct{}
	allowedOrigins map[string]bool

	mu          sync.Mutex
	authRates   map[string]*RateRecord
	authPruneAt time.Time

	flightMu sync.Mutex
	flights  map[string]*downloadFlight

	indexHTML      []byte
	indexETag      string
	assets         map[string]embeddedAsset
	uploadSlots    chan struct{}
	thumbnailSlots chan struct{}
}

func NewServer(cfg *config.Config, db *storage.DB) (*Server, error) {
	return newServer(cfg, db, telegram.New)
}

func canonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

func loadAsset(path, contentType string) (embeddedAsset, error) {
	data, err := staticFS.ReadFile(path)
	if err != nil {
		return embeddedAsset{}, err
	}
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:8])
	return embeddedAsset{
		data:        data,
		hash:        hash,
		etag:        fmt.Sprintf(`"%s"`, hash),
		contentType: contentType,
	}, nil
}

func newServer(cfg *config.Config, db *storage.DB, factory func(string, int64) (*telegram.Client, error)) (*Server, error) {
	if norm, err := config.NormalizeTrustProxyHeader(cfg.TrustProxyHeader); err != nil {
		return nil, err
	} else {
		cfg.TrustProxyHeader = norm
	}

	clients := map[string]*telegram.Client{}
	for _, p := range cfg.Connections {
		c, e := factory(p.BotToken, p.ChatID)
		if e != nil {
			return nil, e
		}
		clients[p.ID] = c
	}
	rows, e := db.Query("SELECT DISTINCT connection_id,backend FROM objects")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id, backend string
		if e := rows.Scan(&id, &backend); e != nil {
			return nil, e
		}
		if backend != "telegram" || clients[id] == nil {
			return nil, errors.New("图片存在未迁移或缺失的 TG 接入配置")
		}
	}
	if e := rows.Err(); e != nil {
		return nil, e
	}

	cssAsset, err := loadAsset("static/app.css", "text/css; charset=utf-8")
	if err != nil {
		return nil, fmt.Errorf("missing embedded app.css: %w", err)
	}
	jsAsset, err := loadAsset("static/app.js", "application/javascript; charset=utf-8")
	if err != nil {
		return nil, fmt.Errorf("missing embedded app.js: %w", err)
	}
	rawIndex, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		return nil, fmt.Errorf("missing embedded index.html: %w", err)
	}
	renderedIndex := strings.ReplaceAll(string(rawIndex), `href="/assets/app.css"`, `href="/assets/app.css?v=`+cssAsset.hash+`"`)
	renderedIndex = strings.ReplaceAll(renderedIndex, `src="/assets/app.js"`, `src="/assets/app.js?v=`+jsAsset.hash+`"`)
	indexBytes := []byte(renderedIndex)
	indexSum := sha256.Sum256(indexBytes)
	indexETag := fmt.Sprintf(`"%s"`, hex.EncodeToString(indexSum[:12]))

	allowedOrigins := make(map[string]bool)
	if origin, ok := canonicalOrigin(cfg.PublicOrigin); ok {
		allowedOrigins[origin] = true
	}
	for _, raw := range cfg.AllowedOrigins {
		if origin, ok := canonicalOrigin(raw); ok {
			allowedOrigins[origin] = true
		}
	}

	lookupImageTools()

	return &Server{
		cfg:            cfg,
		db:             db,
		clients:        clients,
		factory:        factory,
		tgUploads:      make(chan struct{}, 1),
		deleteSlots:    make(chan struct{}, 3),
		allowedOrigins: allowedOrigins,
		authRates:      make(map[string]*RateRecord),
		flights:        make(map[string]*downloadFlight),
		indexHTML:      indexBytes,
		indexETag:      indexETag,
		assets: map[string]embeddedAsset{
			"app.css": cssAsset,
			"app.js":  jsAsset,
		},
		uploadSlots:    make(chan struct{}, 3),
		thumbnailSlots: make(chan struct{}, 2),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Web UI, static assets & Favicon
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /index.html", s.handleIndex)
	mux.HandleFunc("GET /assets/{name}", s.handleAsset)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)

	// Image endpoints
	mux.HandleFunc("GET /i/", s.handlePublicImage)
	mux.HandleFunc("POST /api/share-links", s.handleShareLinks)
	mux.HandleFunc("GET /images/", s.handleProxyImage)
	mux.HandleFunc("GET /thumb/", s.handleThumbnailImage)
	mux.HandleFunc("GET /direct/", s.handleDirectImage)

	// API endpoints
	mux.Handle("POST /api/upload", http.TimeoutHandler(http.HandlerFunc(s.handleUpload), 240*time.Second, `{"code":503,"msg":"图片存储超时，请先刷新图库确认是否成功，再重试"}`))
	mux.HandleFunc("GET /api/objects", s.handleListObjects)
	mux.HandleFunc("DELETE /api/objects", s.handleDeleteObject)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/auth/verify", s.handleAuthVerify)
	mux.HandleFunc("POST /api/admin/password", s.handleChangePassword)
	mux.HandleFunc("GET /api/admin/api-key", s.handleAPIKey)
	mux.HandleFunc("POST /api/admin/api-key", s.handleAPIKey)
	mux.HandleFunc("GET /api/download/{key...}", s.handleAPIDownload)
	mux.HandleFunc("GET /api/admin/telegram", s.handleTelegramList)
	mux.HandleFunc("POST /api/admin/telegram/{action}", s.handleTelegramAction)

	return s.corsMiddleware(s.authRateMiddleware(mux))
}

func (s *Server) getClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Only trust an explicitly configured forwarding header from a local loopback reverse proxy.
	if s.cfg.TrustProxyHeader != "" {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			switch s.cfg.TrustProxyHeader {
			case "CF-Connecting-IP":
				if cfIP := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); cfIP != nil {
					return cfIP.String()
				}
			case "X-Real-IP":
				if realIP := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); realIP != nil {
					return realIP.String()
				}
			case "X-Forwarded-For":
				parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
				// Walk right-to-left so client-prepended entries cannot spoof the rate-limit key,
				// skipping internal/loopback proxy hops when a public client IP is present.
				var fallback string
				for i := len(parts) - 1; i >= 0; i-- {
					candidate := net.ParseIP(strings.TrimSpace(parts[i]))
					if candidate == nil || candidate.IsLoopback() {
						continue
					}
					if !candidate.IsPrivate() && !candidate.IsLinkLocalUnicast() {
						return candidate.String()
					}
					if fallback == "" {
						fallback = candidate.String()
					}
				}
				if fallback != "" {
					return fallback
				}
			}
		}
	}
	return host
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin != "" {
			if canonical, ok := canonicalOrigin(origin); ok && s.allowedOrigins[canonical] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Auth-Token, X-Upload-Token, X-API-Key")
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/api/upload" {
			r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) jsonResp(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": code,
		"msg":  msg,
		"data": data,
	})
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="6" fill="#3b82f6"/><path d="M7 23l5-6 4 4 6-8 3 4v6H7z" fill="#ffffff"/><circle cx="11" cy="11" r="2.5" fill="#ffffff"/></svg>`
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("ETag", s.indexETag)
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if match := r.Header.Get("If-None-Match"); match == s.indexETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.indexHTML)
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.assets[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("v") == asset.hash {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	}
	if r.Header.Get("If-None-Match") == asset.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(asset.data)
}

func (s *Server) handleProxyImage(w http.ResponseWriter, r *http.Request) {
	s.serveProxyImage(w, r, strings.TrimPrefix(r.URL.Path, "/images/"))
}

func (s *Server) serveProxyImage(w http.ResponseWriter, r *http.Request, key string) {
	if !storage.KeyPattern.MatchString(key) {
		http.Error(w, "文件未找到 (路径格式无效)", http.StatusNotFound)
		return
	}

	if r.URL.Query().Get("thumb") == "1" {
		s.serveThumbnail(w, r, key)
		return
	}

	obj, ok := s.lookupIndexedObject(w, r, key)
	if !ok {
		return
	}
	sum := sha256.Sum256([]byte(key))
	etag := fmt.Sprintf(`"%x"`, sum[:8])
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	data, mimeType, err := s.downloadObject(r.Context(), obj)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "图片未找到或获取失败", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleThumbnailImage(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/thumb/")
	if !storage.KeyPattern.MatchString(key) {
		http.Error(w, "文件未找到", http.StatusNotFound)
		return
	}
	s.serveThumbnail(w, r, key)
}

// Check revocation before conditional responses: deleted links must not return 304.
func (s *Server) lookupIndexedObject(w http.ResponseWriter, r *http.Request, key string) (*storage.ObjectRecord, bool) {
	obj, err := s.db.GetObject(key)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "图片索引读取失败", http.StatusServiceUnavailable)
		return nil, false
	}
	if obj == nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return nil, false
	}
	return obj, true
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request, key string) {
	obj, ok := s.lookupIndexedObject(w, r, key)
	if !ok {
		return
	}
	sum := sha256.Sum256([]byte(key))
	etag := fmt.Sprintf(`"thumb-v2-%x"`, sum[:8])
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Bound concurrent decode memory now that there is no disk thumbnail cache.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	select {
	case s.thumbnailSlots <- struct{}{}:
		defer func() { <-s.thumbnailSlots }()
	case <-ctx.Done():
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "预览请求繁忙，请稍后重试", http.StatusServiceUnavailable)
		return
	}
	// Images are never written to local storage. Only browsers cache thumbnails.
	data, sourceMime, err := s.downloadObject(r.Context(), obj)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "原图未找到", http.StatusNotFound)
		return
	}

	thumbBytes, thumbMime, err := imageutil.GenerateThumbnail(data, 320)
	if err != nil {
		// Fallback to original image if resizing fails
		w.Header().Set("Content-Type", sourceMime)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", thumbMime)
	w.Header().Set("Content-Length", strconv.Itoa(len(thumbBytes)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(thumbBytes)
}

func (s *Server) handleDirectImage(w http.ResponseWriter, r *http.Request) {
	// Compatibility for shared links: serve bytes directly, never redirect.
	s.serveProxyImage(w, r, strings.TrimPrefix(r.URL.Path, "/direct/"))
}

func (s *Server) authToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.AuthToken
}

func (s *Server) checkAuthLocked(r *http.Request) bool {
	token, apiKey := s.cfg.AuthToken, s.cfg.APIKey
	auth := r.Header.Get("Authorization")
	if apiKey != "" && ((strings.HasPrefix(auth, "Bearer ") && constantTimeEqual(strings.TrimPrefix(auth, "Bearer "), apiKey)) || constantTimeEqual(r.Header.Get("X-API-Key"), apiKey)) {
		return false
	}
	if token == "" {
		return true
	}
	if strings.HasPrefix(auth, "Bearer ") && constantTimeEqual(strings.TrimPrefix(auth, "Bearer "), token) {
		return true
	}
	if constantTimeEqual(r.Header.Get("X-Auth-Token"), token) || constantTimeEqual(r.Header.Get("X-Upload-Token"), token) {
		return true
	}
	// Do not implicitly parse multipart here: that can spill images to disk.
	// Uploads explicitly parse bounded, memory-only forms before authentication.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		_ = r.ParseForm()
	}
	if constantTimeEqual(r.Form.Get("token"), token) || constantTimeEqual(r.Form.Get("auth_token"), token) {
		return true
	}
	return false
}

func (s *Server) checkAuth(r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkAuthLocked(r)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	hasAuth := s.authToken() != ""
	isAuthed := s.checkAuth(r)

	w.Header().Set("Cache-Control", "no-store")
	// Login bootstrapping must not wait for a remote probe or expose account state.
	if r.URL.Query().Get("auth_only") == "1" {
		s.jsonResp(w, http.StatusOK, "success", map[string]any{"has_auth": hasAuth, "authenticated": !hasAuth || isAuthed})
		return
	}
	if hasAuth && !isAuthed {
		s.jsonResp(w, http.StatusOK, "success", map[string]any{
			"has_auth":      true,
			"authenticated": false,
			"public_origin": s.cfg.PublicOrigin,
			"alive":         true,
		})
		return
	}

	c, _ := s.activeConnection()
	online, detail := false, "请在设置中接入 Telegram 频道"
	if c != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		online, detail = c.Probe(ctx)
		cancel()
	}
	s.jsonResp(w, http.StatusOK, "success", map[string]any{
		"public_origin": s.cfg.PublicOrigin,
		"has_auth":      hasAuth,
		"authenticated": true,
		"storage": map[string]any{
			"backend":       "telegram",
			"ok":            online,
			"detail":        detail,
			"max_file_size": s.uploadLimit(),
		},
	})
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authAttemptAllowed(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonResp(w, http.StatusBadRequest, "无效请求格式", nil)
		return
	}
	token := s.authToken()
	valid := token == "" || constantTimeEqual(body.Token, token)
	if !s.recordAuthResult(w, r, valid) {
		return
	}
	if !valid {
		s.jsonResp(w, http.StatusUnauthorized, "口令错误", nil)
		return
	}
	s.jsonResp(w, http.StatusOK, "验证通过", nil)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.checkAuth(r) {
		s.jsonResp(w, http.StatusUnauthorized, "当前口令验证失败，无权修改", nil)
		return
	}

	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonResp(w, http.StatusBadRequest, "无效请求格式", nil)
		return
	}

	if body.NewPassword == "" {
		s.jsonResp(w, http.StatusBadRequest, "新口令不能为空", nil)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.AuthToken != "" && !constantTimeEqual(body.OldPassword, s.cfg.AuthToken) {
		s.jsonResp(w, http.StatusUnauthorized, "旧口令不正确", nil)
		return
	}

	next := *s.cfg
	next.AuthToken = body.NewPassword
	if err := next.Validate(); err != nil {
		s.jsonResp(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if next.Path == "" {
		s.jsonResp(w, http.StatusInternalServerError, "未配置凭据保存路径", nil)
		return
	}
	if err := config.SaveConfig(next.Path, &next); err != nil {
		s.jsonResp(w, http.StatusInternalServerError, "保存配置文件失败", nil)
		return
	}
	// Publish the new password only after the atomic save succeeds.
	s.cfg.AuthToken = body.NewPassword

	s.jsonResp(w, http.StatusOK, "口令修改成功", map[string]string{
		"token": body.NewPassword,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, s.uploadLimit()+1024*1024)
	reader, authorized := s.authorizeUpload(w, r)
	if !authorized {
		return
	}
	// Only authenticated uploads consume processing slots. Do not build an
	// unbounded queue of request bodies while the three workers are busy.
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		s.jsonResp(w, http.StatusServiceUnavailable, "上传繁忙，请稍后重试", nil)
		return
	}
	if err := parseUploadForm(r, reader); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.jsonResp(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("上传请求过大，当前单图最大 %dMB", s.uploadLimit()/(1024*1024)), nil)
		} else {
			s.jsonResp(w, http.StatusBadRequest, "解析上传表单失败", nil)
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !s.checkDataAuth(r) {
		s.jsonResp(w, http.StatusUnauthorized, "未授权访问", nil)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		s.jsonResp(w, http.StatusBadRequest, "请选择要上传的文件 (参数名 file)", nil)
		return
	}
	defer file.Close()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		s.jsonResp(w, http.StatusBadRequest, "每个上传请求只能包含一张图片", nil)
		return
	}

	if header.Size > s.uploadLimit() {
		s.jsonResp(w, http.StatusBadRequest, fmt.Sprintf("文件大小超出 %dMB 限制", s.uploadLimit()/(1024*1024)), nil)
		return
	}

	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowedExts := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	}
	if !allowedExts[ext] {
		s.jsonResp(w, http.StatusBadRequest, "仅支持 PNG/JPG/JPEG/GIF/WebP 格式图片；AVIF 请通过网页上传自动转 PNG", nil)
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		s.jsonResp(w, http.StatusInternalServerError, "读取上传数据失败", nil)
		return
	}

	imageConfig, decodedFormat, decodeErr := image.DecodeConfig(bytes.NewReader(data))
	if decodeErr != nil {
		s.jsonResp(w, http.StatusBadRequest, "无法识别图片内容，请上传 PNG/JPG/GIF/WebP 格式图片", nil)
		return
	}
	if int64(imageConfig.Width)*int64(imageConfig.Height) > imageutil.MaxImagePixels {
		s.jsonResp(w, http.StatusBadRequest, "图片超过 5000 万像素，请先手动裁剪", nil)
		return
	}
	expectedFormat := strings.TrimPrefix(ext, ".")
	if expectedFormat == "jpg" {
		expectedFormat = "jpeg"
	}
	if decodedFormat != expectedFormat {
		s.jsonResp(w, http.StatusBadRequest, "图片扩展名与实际格式不一致", nil)
		return
	}

	// The browser knows the source size before local processing. API clients
	// without this optional field keep the actual uploaded file size.
	origSize := int64(len(data))
	if n, err := strconv.ParseInt(r.FormValue("orig_size"), 10, 64); err == nil && n > 0 && n <= 50*1024*1024 {
		origSize = n
	}

	// Shared by browser and API uploads; never pre-strip orientation metadata.
	uploadData, width, height, processing, err := prepareUpload(r.Context(), data, decodedFormat, imageConfig)
	if err != nil {
		s.jsonResp(w, http.StatusUnprocessableEntity, "服务端图片处理失败，请重试或更换图片", nil)
		return
	}
	format := strings.TrimPrefix(ext, ".")
	if format == "jpeg" {
		format = "jpg"
	}

	h := sha256.Sum256(uploadData)
	hashPrefix := hex.EncodeToString(h[:])[:12]
	now := time.Now().UTC()
	_, connectionID := s.activeConnection()
	if connectionID == "" {
		s.jsonResp(w, 503, "请先在设置中接入 TG", nil)
		return
	}
	key := fmt.Sprintf("telegram/%s/%s/%s%s", connectionID, now.Format("2006/01/02"), hashPrefix, ext)

	ctx, cancel := context.WithTimeout(r.Context(), 220*time.Second)
	defer cancel()
	obj, err := s.uploadStored(ctx, key, uploadData, origSize, width, height, format)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.jsonResp(w, http.StatusGatewayTimeout, "图片存储超时或连接中断，请先刷新图库确认是否成功，再重试", nil)
			return
		}
		s.jsonResp(w, http.StatusInternalServerError, fmt.Sprintf("图片存储失败: %v", err), nil)
		return
	}

	origin := strings.TrimRight(s.cfg.PublicOrigin, "/")
	proxyURL, err := s.publicURL(obj.Key)
	if err != nil {
		s.jsonResp(w, http.StatusInternalServerError, "分享链接生成失败，请刷新图库重试", nil)
		return
	}
	directURL := proxyURL
	thumbURL := fmt.Sprintf("%s/thumb/%s?v=2", origin, obj.Key)

	s.jsonResp(w, http.StatusOK, "success", map[string]any{
		"url":        proxyURL,
		"proxy_url":  proxyURL,
		"direct_url": directURL,
		"thumb_url":  thumbURL,
		"key":        obj.Key,
		"size":       obj.Size,
		"orig_size":  obj.OrigSize,
		"processing": processing,
		"backend":    obj.Backend,
		"width":      obj.Width,
		"height":     obj.Height,
		"format":     obj.Format,
		"mime":       obj.Mime,
		"created_at": obj.CreatedAt,
	})
}

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	if !s.checkDataAuth(r) {
		s.jsonResp(w, http.StatusUnauthorized, "未授权", nil)
		return
	}

	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	tzOffset, _ := strconv.Atoi(q.Get("tz_offset"))
	includeSummary := q.Get("summary") == "1" || q.Get("summary") == "true"

	res, err := s.db.ListObjectsPage(storage.ListOptions{
		Limit:           limit,
		Offset:          offset,
		Query:           strings.TrimSpace(q.Get("q")),
		Sort:            strings.TrimSpace(q.Get("sort")),
		Date:            strings.TrimSpace(q.Get("date")),
		TZOffsetMinutes: tzOffset,
		IncludeSummary:  includeSummary,
	})
	if err != nil {
		s.jsonResp(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}

	origin := strings.TrimRight(s.cfg.PublicOrigin, "/")
	type Item struct {
		Key       string `json:"key"`
		ProxyURL  string `json:"proxy_url"`
		DirectURL string `json:"direct_url"`
		ThumbURL  string `json:"thumb_url"`
		Size      int64  `json:"size"`
		OrigSize  int64  `json:"orig_size"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Format    string `json:"format"`
		Mime      string `json:"mime"`
		CreatedAt string `json:"created_at"`
		Backend   string `json:"backend"`
	}

	items := make([]Item, 0, len(res.Items))
	for _, o := range res.Items {
		var publicURL string
		if o.PublicToken != "" {
			publicURL = s.formatPublicURL(o.PublicToken)
		} else {
			publicURL, err = s.publicURL(o.Key)
			if err != nil {
				s.jsonResp(w, http.StatusInternalServerError, "分享链接生成失败", nil)
				return
			}
		}
		origSize := o.OrigSize
		if origSize <= 0 {
			origSize = o.Size
		}
		items = append(items, Item{
			Key:       o.Key,
			ProxyURL:  publicURL,
			DirectURL: publicURL,
			ThumbURL:  fmt.Sprintf("%s/thumb/%s?v=2", origin, o.Key),
			Size:      o.Size,
			OrigSize:  origSize,
			Width:     o.Width,
			Height:    o.Height,
			Format:    o.Format,
			Mime:      o.Mime,
			CreatedAt: o.CreatedAt,
			Backend:   o.Backend,
		})
	}

	nextOffset := offset + len(items)
	payload := map[string]any{
		"list":        items,
		"total":       res.Total,
		"total_size":  res.TotalSize,
		"has_more":    nextOffset < res.Total,
		"next_offset": nextOffset,
	}
	if res.Summary != nil {
		payload["summary"] = res.Summary
	}

	s.jsonResp(w, http.StatusOK, "success", payload)
}

func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	if !s.checkDataAuth(r) {
		s.jsonResp(w, http.StatusUnauthorized, "未授权", nil)
		return
	}

	var keys []string
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Key  string   `json:"key"`
			Keys []string `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if body.Key != "" {
				keys = append(keys, body.Key)
			}
			keys = append(keys, body.Keys...)
		}
	}
	if len(keys) == 0 {
		for _, k := range r.URL.Query()["key"] {
			if k != "" {
				keys = append(keys, k)
			}
		}
	}

	if len(keys) == 0 {
		s.jsonResp(w, http.StatusBadRequest, "无效的 key 参数", nil)
		return
	}

	seen := make(map[string]bool)
	var validKeys, errs []string
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if !storage.KeyPattern.MatchString(key) {
			errs = append(errs, "无效图片路径: "+key)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		validKeys = append(validKeys, key)
	}
	deletedKeys, deleteErrors := s.deleteBatch(r.Context(), validKeys)
	errs = append(errs, deleteErrors...)

	if len(deletedKeys) == 0 && len(errs) > 0 {
		s.jsonResp(w, http.StatusInternalServerError, "删除失败: "+strings.Join(errs, "; "), nil)
		return
	}

	s.jsonResp(w, http.StatusOK, "已删除", map[string]any{
		"deleted":      len(deletedKeys),
		"deleted_keys": deletedKeys,
		"errors":       errs,
	})
}
