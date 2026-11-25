package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"github.com/3922510-creator/pic-tg/config"
	"github.com/3922510-creator/pic-tg/telegram"
	"strings"
	"time"
)

type connectionView struct {
	ID          string `json:"id"`
	BotID       int64  `json:"bot_id"`
	BotUsername string `json:"bot_username"`
	ChatID      int64  `json:"chat_id"`
	ChatTitle   string `json:"chat_title"`
	TokenSet    bool   `json:"token_set"`
}

func (s *Server) settingsViewLocked() map[string]any {
	list := make([]connectionView, 0, len(s.cfg.Connections))
	for _, p := range s.cfg.Connections {
		list = append(list, connectionView{p.ID, p.BotID, p.BotUsername, p.ChatID, p.ChatTitle, p.BotToken != ""})
	}
	return map[string]any{"connections": list, "active_id": s.cfg.DefaultConnectionID, "revision": s.cfg.Revision}
}
func (s *Server) handleTelegramList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.checkAuth(r) {
		s.jsonResp(w, 401, "未授权", nil)
		return
	}
	s.mu.Lock()
	v := s.settingsViewLocked()
	s.mu.Unlock()
	s.jsonResp(w, 200, "success", v)
}
func (s *Server) handleTelegramAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.checkAuth(r) {
		s.jsonResp(w, 401, "未授权", nil)
		return
	}
	var body struct {
		Token        string `json:"token"`
		ConnectionID string `json:"connection_id"`
		ChatID       int64  `json:"chat_id"`
		Revision     int64  `json:"revision"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); e != nil {
		s.jsonResp(w, 400, "请求格式无效", nil)
		return
	}
	action := r.PathValue("action")
	if action != "verify" && action != "discover" && action != "save" {
		s.jsonResp(w, 404, "未知操作", nil)
		return
	}
	s.mu.Lock()
	startRevision := s.cfg.Revision
	startAuth := s.cfg.AuthToken
	token := strings.TrimSpace(body.Token)
	if token == "" {
		for _, p := range s.cfg.Connections {
			if p.ID == body.ConnectionID {
				token = p.BotToken
				break
			}
		}
	}
	s.mu.Unlock()
	chatID := body.ChatID
	if action != "save" && chatID == 0 {
		chatID = -1
	}
	client, e := s.factory(token, chatID)
	if e != nil {
		s.jsonResp(w, 400, "请填写有效 Bot Token 和负数频道 ID", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	bot, e := client.GetMe(ctx)
	if e != nil {
		s.jsonResp(w, 400, e.Error(), nil)
		return
	}
	if action == "verify" {
		s.jsonResp(w, 200, "机器人验证成功", bot)
		return
	}
	if action == "discover" {
		channels, e := client.DiscoverChannels(ctx)
		if e != nil {
			s.jsonResp(w, 400, e.Error(), nil)
			return
		}
		s.jsonResp(w, 200, "success", map[string]any{"channels": channels})
		return
	}
	if body.Revision != startRevision {
		s.jsonResp(w, 409, "配置已更新，请刷新后重试", nil)
		return
	}
	chat, e := client.GetChat(ctx)
	if e != nil {
		s.jsonResp(w, 400, e.Error(), nil)
		return
	}
	if ok, detail := client.Probe(ctx); !ok {
		s.jsonResp(w, 400, detail, nil)
		return
	}
	// Serialize config writes with password changes; publish only after atomic persistence.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Revision != startRevision || s.cfg.AuthToken != startAuth {
		s.jsonResp(w, 409, "配置或登录状态已变更，请刷新后重试", nil)
		return
	}
	next := *s.cfg
	next.Connections = append([]config.Connection(nil), s.cfg.Connections...)
	id := ""
	for _, p := range next.Connections {
		if p.BotID == bot.ID && p.ChatID == chat.ID {
			id = p.ID
			break
		}
	}
	if id == "" {
		var buf [8]byte
		if _, e := rand.Read(buf[:]); e != nil {
			s.jsonResp(w, 500, "生成接入 ID 失败", nil)
			return
		}
		id = hex.EncodeToString(buf[:])
		next.Connections = append(next.Connections, config.Connection{ID: id, BotID: bot.ID, ChatID: chat.ID})
	}
	clients := make(map[string]*telegram.Client, len(s.clients)+1)
	for k, v := range s.clients {
		clients[k] = v
	}
	for i := range next.Connections {
		p := &next.Connections[i]
		if p.BotID == bot.ID {
			p.BotToken = token
			p.BotUsername = bot.Username
			c, e := s.factory(token, p.ChatID)
			if e != nil {
				s.jsonResp(w, 400, "TG 配置无效", nil)
				return
			}
			clients[p.ID] = c
		}
		if p.ID == id {
			p.ChatTitle = chat.Title
		}
	}
	next.DefaultConnectionID = id
	next.Revision++
	if e := config.SaveConfig(next.Path, &next); e != nil {
		s.jsonResp(w, 500, "保存配置失败，当前接入未改变", nil)
		return
	}
	// Keep the cfg pointer stable: common immutable fields are read outside the lock.
	s.cfg.Connections = next.Connections
	s.cfg.DefaultConnectionID = id
	s.cfg.Revision = next.Revision
	s.clients = clients
	s.jsonResp(w, 200, "TG 接入已生效，无需重启；旧图片保留原接入", s.settingsViewLocked())
}
