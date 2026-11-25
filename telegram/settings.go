package telegram

import (
	"context"
	"errors"
	"sort"
)

type BotInfo struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}
type ChatInfo struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

func (c *Client) GetMe(ctx context.Context) (BotInfo, error) {
	var b BotInfo
	e := c.call(ctx, "getMe", map[string]any{}, &b)
	if e == nil && (!b.IsBot || b.ID <= 0) {
		e = errors.New("TG 返回的机器人身份无效")
	}
	return b, e
}
func (c *Client) GetChat(ctx context.Context) (ChatInfo, error) {
	var b ChatInfo
	e := c.call(ctx, "getChat", map[string]any{"chat_id": c.ChatID}, &b)
	if e == nil && (b.ID != c.ChatID || b.Type != "channel") {
		e = errors.New("请使用专用 Telegram 频道，不能使用个人聊天或群组")
	}
	return b, e
}
func (c *Client) DiscoverChannels(ctx context.Context) ([]ChatInfo, error) {
	var hook struct {
		URL string `json:"url"`
	}
	if e := c.call(ctx, "getWebhookInfo", map[string]any{}, &hook); e != nil {
		return nil, e
	}
	if hook.URL != "" {
		return nil, errors.New("机器人已配置 Webhook，请手动填写频道 ID；不会改动现有 Webhook")
	}
	var updates []struct {
		ChannelPost *struct {
			Chat ChatInfo `json:"chat"`
		} `json:"channel_post"`
		Member *struct {
			Chat ChatInfo `json:"chat"`
		} `json:"my_chat_member"`
	}
	// Never acknowledge/consume updates by setting an offset, nor change allowed_updates.
	if e := c.call(ctx, "getUpdates", map[string]any{"timeout": 0, "limit": 100}, &updates); e != nil {
		return nil, e
	}
	seen := map[int64]ChatInfo{}
	for _, u := range updates {
		if u.ChannelPost != nil {
			b := u.ChannelPost.Chat
			if b.Type == "channel" && b.ID < 0 {
				seen[b.ID] = b
			}
		}
		if u.Member != nil {
			b := u.Member.Chat
			if b.Type == "channel" && b.ID < 0 {
				seen[b.ID] = b
			}
		}
	}
	out := make([]ChatInfo, 0, len(seen))
	for _, b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
