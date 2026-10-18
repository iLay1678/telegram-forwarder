package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

func (c *Client) handleUpdates(ctx context.Context, raw tg.UpdatesClass) error {
	var list []tg.UpdateClass
	switch v := raw.(type) {
	case *tg.Updates:
		if err := c.saveEntities(v.Users, v.Chats); err != nil {
			return err
		}
		list = v.Updates
	case *tg.UpdatesCombined:
		if err := c.saveEntities(v.Users, v.Chats); err != nil {
			return err
		}
		list = v.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{v.Update}
	case *tg.UpdatesTooLong:
		return fmt.Errorf("Telegram 更新差异过期，需要历史补收")
	}
	for _, item := range list {
		switch v := item.(type) {
		case *tg.UpdateNewMessage:
			if msg, ok := v.Message.(*tg.Message); ok {
				if err := c.deliverMessage(ctx, msg); err != nil {
					return err
				}
			}
		case *tg.UpdateNewChannelMessage:
			if msg, ok := v.Message.(*tg.Message); ok {
				if err := c.deliverMessage(ctx, msg); err != nil {
					return err
				}
			}
		case *tg.UpdateBotCallbackQuery:
			if c.OnCallback != nil {
				c.OnCallback(ctx, model.Callback{ID: v.QueryID, SenderID: v.UserID, Chat: c.cached(rawPeer(v.Peer)), MessageID: v.MsgID, Data: string(v.Data)})
			}
		}
	}
	return nil
}

// handleRealtimeUpdates dispatches message updates as soon as the Telegram
// network client receives them. The gotd updates manager still receives the
// same update afterwards to advance pts and recover gaps; dispatchMessage's
// message key makes that second delivery idempotent.
func (c *Client) handleRealtimeUpdates(ctx context.Context, raw tg.UpdatesClass) error {
	var list []tg.UpdateClass
	switch v := raw.(type) {
	case *tg.Updates:
		if err := c.saveEntities(v.Users, v.Chats); err != nil {
			return err
		}
		list = v.Updates
	case *tg.UpdatesCombined:
		if err := c.saveEntities(v.Users, v.Chats); err != nil {
			return err
		}
		list = v.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{v.Update}
	default:
		return nil
	}
	for _, item := range list {
		switch v := item.(type) {
		case *tg.UpdateNewMessage:
			if msg, ok := v.Message.(*tg.Message); ok {
				if err := c.deliverMessage(ctx, msg); err != nil {
					return err
				}
			}
		case *tg.UpdateNewChannelMessage:
			if msg, ok := v.Message.(*tg.Message); ok {
				if err := c.deliverMessage(ctx, msg); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Client) deliverMessage(ctx context.Context, msg *tg.Message) error {
	return c.dispatchMessage(ctx, c.convertMessage(msg))
}

const recoveryLookback = 30 * time.Minute

func (c *Client) dispatchMessage(ctx context.Context, msg model.Message) error {
	slog.Info("Telegram 消息收到", "chat_id", msg.Chat.ID, "chat_kind", msg.Chat.Kind, "message_id", msg.ID, "message_time", msg.Date.Format(time.RFC3339))
	if c.OnMessageAck != nil {
		if !c.claimSeen(msg) {
			return nil
		}
		if err := c.OnMessageAck(ctx, msg); err != nil {
			c.forgetSeen(msg)
			return err
		}
		return nil
	}
	if !c.markSeen(msg) {
		return nil
	}
	if c.OnMessage != nil {
		c.OnMessage(ctx, msg)
	}
	return nil
}

func (c *Client) markSeen(msg model.Message) bool {
	return c.claimSeen(msg)
}

func (c *Client) claimSeen(msg model.Message) bool {
	key := c.seenKey(msg)
	now := time.Now()
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seen == nil {
		c.seen = make(map[string]time.Time)
	}
	for oldKey, at := range c.seen {
		if now.Sub(at) > time.Hour {
			delete(c.seen, oldKey)
		}
	}
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = now
	return true
}

func (c *Client) seenKey(msg model.Message) string {
	return fmt.Sprintf("%s:%d:%d", msg.Chat.Kind, msg.Chat.ID, msg.ID)
}

func (c *Client) forgetSeen(msg model.Message) {
	key := c.seenKey(msg)
	c.seenMu.Lock()
	if c.seen != nil {
		delete(c.seen, key)
	}
	c.seenMu.Unlock()
}

func (c *Client) recoverChannel(channelID int64) {
	c.recoverMu.Lock()
	if c.recovering[channelID] {
		c.recoverMu.Unlock()
		return
	}
	c.recovering[channelID] = true
	c.recoverMu.Unlock()
	defer func() {
		c.recoverMu.Lock()
		delete(c.recovering, channelID)
		c.recoverMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	peer := c.cached(model.Peer{ID: channelID, Kind: "channel"})
	if peer.AccessHash == 0 {
		slog.Warn("Telegram 频道自动补收缺少 access_hash", "channel_id", channelID)
		return
	}
	cutoff := time.Now().Add(-recoveryLookback)
	var messages []model.Message
	offsetID := 0
	for page := 0; page < 20; page++ {
		batch, err := c.History(ctx, peer, offsetID, 100)
		if err != nil {
			slog.Error("Telegram 频道自动补收失败", "channel_id", channelID, "error", err)
			return
		}
		if len(batch) == 0 {
			break
		}
		messages = append(messages, batch...)
		oldest := batch[0]
		for _, candidate := range batch[1:] {
			if candidate.ID < oldest.ID {
				oldest = candidate
			}
		}
		if (!oldest.Date.IsZero() && oldest.Date.Before(cutoff)) || len(batch) < 100 || oldest.ID <= 0 || oldest.ID >= offsetID && offsetID != 0 {
			break
		}
		offsetID = oldest.ID
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	recovered := 0
	for _, msg := range messages {
		if !msg.Date.IsZero() && msg.Date.Before(cutoff) {
			continue
		}
		if err := c.dispatchMessage(ctx, msg); err != nil {
			slog.Error("Telegram 频道自动补收处理失败", "channel_id", channelID, "message_id", msg.ID, "error", err)
			continue
		}
		recovered++
	}
	slog.Info("Telegram 频道自动补收完成", "channel_id", channelID, "fetched", len(messages), "dispatched", recovered)
}

func (c *Client) convertMessage(m *tg.Message) model.Message {
	p := c.cached(rawPeer(m.PeerID))
	// Telethon entities 表不保存广播类型，迁移后首条频道帖子可补全这一信息。
	if p.Kind == "channel" && m.Post {
		p.Broadcast = true
	}
	sender := c.cached(rawPeer(m.FromID))
	if sender.ID == 0 {
		sender = p
	}
	out := model.Message{ID: m.ID, Chat: p, Sender: sender, Text: markdownText(m.Message, m.Entities), Date: time.Unix(int64(m.Date), 0), GroupedID: m.GroupedID, Outgoing: m.Out, Native: m, Link: messageLink(p, m.ID)}
	if reply, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		out.ReplyTo = reply.ReplyToMsgID
	}
	if _, ok := m.Media.(*tg.MessageMediaWebPage); ok {
		out.HasPreview = true
	}
	if media, ok := c.extractMedia(m); ok {
		out.Media = []model.Media{media}
	}
	if markup, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
		for _, row := range markup.Rows {
			var buttons []model.Button
			for _, b := range row.Buttons {
				switch v := b.(type) {
				case *tg.KeyboardButtonURL:
					buttons = append(buttons, model.Button{Text: v.Text, URL: v.URL})
				case *tg.KeyboardButtonCallback:
					buttons = append(buttons, model.Button{Text: v.Text, Data: string(v.Data)})
				}
			}
			if len(buttons) > 0 {
				out.Buttons = append(out.Buttons, buttons)
			}
		}
	}
	return out
}

func (c *Client) messageResult(res tg.MessagesMessagesClass) ([]model.Message, error) {
	var messages []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	switch v := res.(type) {
	case *tg.MessagesMessages:
		messages, users, chats = v.Messages, v.Users, v.Chats
	case *tg.MessagesMessagesSlice:
		messages, users, chats = v.Messages, v.Users, v.Chats
	case *tg.MessagesChannelMessages:
		messages, users, chats = v.Messages, v.Users, v.Chats
	default:
		return nil, fmt.Errorf("未知历史消息响应 %T", res)
	}
	if err := c.saveEntities(users, chats); err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(messages))
	for _, v := range messages {
		if m, ok := v.(*tg.Message); ok {
			out = append(out, c.convertMessage(m))
		}
	}
	return out, nil
}

// History 返回 offsetID 之前的消息，时间倒序，每次请求不超过 100 条。
func (c *Client) History(ctx context.Context, p model.Peer, offsetID, limit int) ([]model.Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	var messages []model.Message
	for len(messages) < limit {
		requested := limit - len(messages)
		res, err := c.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: inputPeer(c.cached(p)), OffsetID: offsetID, Limit: requested})
		if err != nil {
			return nil, err
		}
		batch, err := c.messageResult(res)
		if err != nil {
			return nil, err
		}
		messages = append(messages, batch...)
		var raw []tg.MessageClass
		switch v := res.(type) {
		case *tg.MessagesMessages:
			raw = v.Messages
		case *tg.MessagesMessagesSlice:
			raw = v.Messages
		case *tg.MessagesChannelMessages:
			raw = v.Messages
		}
		if len(raw) < requested {
			break
		}
		next := 0
		for _, message := range raw {
			if id := message.GetID(); id > 0 && (next == 0 || id < next) {
				next = id
			}
		}
		if next == 0 || offsetID > 0 && next >= offsetID {
			break
		}
		// 系统消息不会返回给规则引擎，但不能让它们使调用方误判已经读完历史。
		offsetID = next
	}
	return messages, nil
}

func (c *Client) Fetch(ctx context.Context, p model.Peer, ids []int) ([]model.Message, error) {
	p = c.cached(p)
	var out []model.Message
	for start := 0; start < len(ids); start += 100 {
		end := min(start+100, len(ids))
		input := make([]tg.InputMessageClass, 0, end-start)
		for _, id := range ids[start:end] {
			if id > 0 {
				input = append(input, &tg.InputMessageID{ID: id})
			}
		}
		if len(input) == 0 {
			continue
		}
		var res tg.MessagesMessagesClass
		var err error
		if p.Kind == "channel" {
			res, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: p.ID, AccessHash: p.AccessHash}, ID: input})
		} else {
			res, err = c.api.MessagesGetMessages(ctx, input)
		}
		if err != nil {
			return nil, err
		}
		messages, err := c.messageResult(res)
		if err != nil {
			return nil, err
		}
		for _, m := range messages {
			if m.Chat.ID == p.ID && m.Chat.Kind == p.Kind {
				out = append(out, m)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *Client) Delete(ctx context.Context, p model.Peer, ids []int) error {
	p = c.cached(p)
	for start := 0; start < len(ids); start += 100 {
		part := ids[start:min(start+100, len(ids))]
		var err error
		if p.Kind == "channel" {
			_, err = c.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: &tg.InputChannel{ChannelID: p.ID, AccessHash: p.AccessHash}, ID: part})
		} else {
			_, err = c.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: part})
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (c *Client) Pin(ctx context.Context, p model.Peer, id int) error {
	_, err := c.api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{Peer: inputPeer(c.cached(p)), ID: id, Silent: true})
	return err
}
func (c *Client) AnswerCallback(ctx context.Context, id int64, text string, alert bool) error {
	req := &tg.MessagesSetBotCallbackAnswerRequest{QueryID: id, Alert: alert}
	if text != "" {
		req.SetMessage(text)
	}
	_, err := c.api.MessagesSetBotCallbackAnswer(ctx, req)
	return err
}
