// Package telegram stores original image documents in a dedicated Telegram channel.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxFileSize = 20 * 1024 * 1024 // Hosted Bot API getFile download ceiling.

var botTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

type Client struct {
	token   string
	ChatID  int64
	APIBase string
	HTTP    *http.Client
}

type Document struct {
	FileID   string `json:"file_id"`
	UniqueID string `json:"file_unique_id"`
	Size     int64  `json:"file_size"`
}
type Message struct {
	ID   int64 `json:"message_id"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Document Document `json:"document"`
}

func New(token string, chatID int64) (*Client, error) {
	if !botTokenPattern.MatchString(token) || chatID >= 0 {
		return nil, errors.New("TG 机器人凭据或私密频道 ID 无效")
	}
	return &Client{token: token, ChatID: chatID, APIBase: "https://api.telegram.org", HTTP: &http.Client{Timeout: 150 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) endpoint(method string) string {
	return strings.TrimRight(c.APIBase, "/") + "/bot" + c.token + "/" + method
}
func (c *Client) request(ctx context.Context, method, contentType string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), body)
	if err != nil {
		return errors.New("TG 请求创建失败")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	// net/http errors contain the full token-bearing URL: never propagate them.
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("TG %s 网络请求失败或超时", method)
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&envelope); err != nil {
		return fmt.Errorf("TG %s 返回了无效响应 (HTTP %d)", method, resp.StatusCode)
	}
	if !envelope.OK || resp.StatusCode != 200 {
		message := strings.ReplaceAll(envelope.Description, c.token, "[redacted]")
		if envelope.Parameters.RetryAfter > 0 {
			message += fmt.Sprintf("，请在 %d 秒后重试", envelope.Parameters.RetryAfter)
		}
		return fmt.Errorf("TG %s 失败: %s", method, message)
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return fmt.Errorf("TG %s 响应解析失败", method)
	}
	return nil
}
func (c *Client) call(ctx context.Context, method string, fields any, result any) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return errors.New("TG 请求参数编码失败")
	}
	return c.request(ctx, method, "application/json", bytes.NewReader(raw), result)
}

func (c *Client) Upload(ctx context.Context, name, mimeType string, data []byte) (*Message, error) {
	if len(data) == 0 || len(data) > MaxFileSize {
		return nil, errors.New("TG 单图大小必须在 1 字节至 20MB 之间")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("chat_id", strconv.FormatInt(c.ChatID, 10)); err != nil {
		return nil, err
	}
	if err := form.WriteField("disable_notification", "true"); err != nil {
		return nil, err
	}
	if err := form.WriteField("disable_content_type_detection", "true"); err != nil {
		return nil, err
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "document", "filename": name}))
	h.Set("Content-Type", mimeType)
	part, err := form.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := form.Close(); err != nil {
		return nil, err
	}
	var message Message
	if err := c.request(ctx, "sendDocument", form.FormDataContentType(), &body, &message); err != nil {
		return nil, err
	}
	if message.ID <= 0 || message.Chat.ID != c.ChatID || message.Document.FileID == "" {
		return nil, errors.New("TG 上传响应缺少有效的频道、消息或文件 ID")
	}
	if message.Document.Size > MaxFileSize {
		return nil, errors.New("TG 返回文件超过 20MB 下载上限")
	}
	return &message, nil
}

func (c *Client) Download(ctx context.Context, fileID string) ([]byte, error) {
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &file); err != nil {
		return nil, err
	}
	if file.Size > MaxFileSize {
		return nil, errors.New("TG 文件超过 20MB 下载上限")
	}
	// Only Telegram-relative paths are allowed; never follow arbitrary returned URLs.
	path, err := url.Parse(file.Path)
	if err != nil || file.Path == "" || path.IsAbs() || path.Host != "" || path.RawQuery != "" || path.Fragment != "" || strings.HasPrefix(file.Path, "/") {
		return nil, errors.New("TG 返回了无效文件路径")
	}
	segments := strings.Split(file.Path, "/")
	for i, s := range segments {
		if s == ".." || s == "." || s == "" || strings.Contains(s, "\\") {
			return nil, errors.New("TG 返回了无效文件路径")
		}
		segments[i] = url.PathEscape(s)
	}
	address := strings.TrimRight(c.APIBase, "/") + "/file/bot" + c.token + "/" + strings.Join(segments, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("TG 下载请求创建失败")
	}
	// File paths should never redirect to another host (or expose a bot-bearing URL).
	downloader := *c.HTTP
	downloader.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := downloader.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("TG 文件下载网络失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("TG 文件下载返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileSize+1))
	if err != nil {
		return nil, errors.New("TG 图片读取失败")
	}
	if len(data) > MaxFileSize {
		return nil, errors.New("TG 文件超过 20MB 下载上限")
	}
	if len(data) == 0 {
		return nil, errors.New("TG 返回空文件")
	}
	if file.Size > 0 && int64(len(data)) != file.Size {
		return nil, errors.New("TG 文件下载长度不一致")
	}
	return data, nil
}

func (c *Client) Delete(ctx context.Context, chatID, messageID int64) error {
	if chatID >= 0 || messageID <= 0 {
		return errors.New("TG 图片缺少频道或消息索引")
	}
	var deleted bool
	if err := c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, &deleted); err != nil {
		return err
	}
	if !deleted {
		return errors.New("TG 未确认消息删除，保留图片索引")
	}
	return nil
}
func (c *Client) Probe(ctx context.Context) (bool, string) {
	botID, _ := strconv.ParseInt(strings.SplitN(c.token, ":", 2)[0], 10, 64)
	var member struct {
		Status    string `json:"status"`
		CanPost   bool   `json:"can_post_messages"`
		CanDelete bool   `json:"can_delete_messages"`
	}
	if err := c.call(ctx, "getChatMember", map[string]any{"chat_id": c.ChatID, "user_id": botID}, &member); err != nil {
		return false, err.Error()
	}
	if member.Status != "administrator" || !member.CanPost || !member.CanDelete {
		return false, "TG 机器人需要频道管理员、发消息及删除消息权限"
	}
	return true, "TG 私密频道已连接，图片按文件保存"
}
