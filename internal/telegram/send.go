package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

// Command 保持 Telegram 命令菜单的显示顺序。
type Command struct {
	Name, Description string
}

func (c *Client) RegisterCommands(ctx context.Context, commands []Command) error {
	if !c.bot {
		return errors.New("只有机器人可以设置命令菜单")
	}
	req := &tg.BotsSetBotCommandsRequest{Scope: &tg.BotCommandScopeDefault{}}
	for _, command := range commands {
		req.Commands = append(req.Commands, tg.BotCommand{Command: strings.TrimPrefix(command.Name, "/"), Description: command.Description})
	}
	_, err := c.api.BotsSetBotCommands(ctx, req)
	return err
}

// Forward 使用 Telegram 原生转发，保留原作者和媒体相册。
func (c *Client) Forward(ctx context.Context, from, to model.Peer, ids []int) ([]int, error) {
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("转发消息 ID 必须为正数")
		}
	}
	var sent []int
	for start := 0; start < len(ids); start += 100 {
		part := ids[start:min(start+100, len(ids))]
		req := &tg.MessagesForwardMessagesRequest{FromPeer: inputPeer(c.cached(from)), ToPeer: inputPeer(c.cached(to)), ID: part}
		for range part {
			id, err := randomID()
			if err != nil {
				return sent, err
			}
			req.RandomID = append(req.RandomID, id)
		}
		res, err := c.api.MessagesForwardMessages(ctx, req)
		if err != nil {
			return sent, err
		}
		batch, err := sentIDs(res)
		if err != nil {
			return sent, err
		}
		sent = append(sent, batch...)
	}
	return sent, nil
}

func randomID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}
func keyboard(rows [][]model.Button) (tg.ReplyMarkupClass, error) {
	markup := &tg.ReplyInlineMarkup{}
	for _, row := range rows {
		buttons := tg.KeyboardButtonRow{}
		for _, b := range row {
			if b.Text == "" {
				return nil, errors.New("按钮标题不能为空")
			}
			if b.URL != "" {
				buttons.Buttons = append(buttons.Buttons, &tg.KeyboardButtonURL{Text: b.Text, URL: b.URL})
			} else {
				if len(b.Data) == 0 || len(b.Data) > 64 {
					return nil, errors.New("按钮回调数据必须为 1–64 字节")
				}
				buttons.Buttons = append(buttons.Buttons, &tg.KeyboardButtonCallback{Text: b.Text, Data: []byte(b.Data)})
			}
		}
		if len(buttons.Buttons) > 0 {
			markup.Rows = append(markup.Rows, buttons)
		}
	}
	return markup, nil
}

func (c *Client) Send(ctx context.Context, p model.Peer, out model.Outgoing) ([]int, error) {
	p = c.cached(p)
	plain, entities, err := parseText(out.Text, out.Mode)
	if err != nil {
		return nil, err
	}
	markup, err := keyboard(out.Buttons)
	if err != nil {
		return nil, err
	}
	if len(out.Media) == 0 {
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		req := &tg.MessagesSendMessageRequest{Peer: inputPeer(p), Message: plain, RandomID: id, NoWebpage: !out.Preview}
		if len(entities) > 0 {
			req.SetEntities(entities)
		}
		if len(out.Buttons) > 0 {
			req.SetReplyMarkup(markup)
		}
		if out.ReplyTo > 0 {
			req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: out.ReplyTo})
		}
		res, err := c.api.MessagesSendMessage(ctx, req)
		if err != nil {
			return nil, err
		}
		return sentIDs(res)
	}
	if len(out.Media) == 1 {
		return c.sendOneMedia(ctx, p, out.Media[0], plain, entities, out.ReplyTo, markup, len(out.Buttons) > 0)
	}
	var ids []int
	for start := 0; start < len(out.Media); start += 10 {
		end := min(start+10, len(out.Media))
		batch := out.Media[start:end]
		if len(batch) == 1 {
			caption := ""
			var e []tg.MessageEntityClass
			if start == 0 {
				caption, e = plain, entities
			}
			sent, err := c.sendOneMedia(ctx, p, batch[0], caption, e, out.ReplyTo, markup, false)
			ids = append(ids, sent...)
			if err != nil {
				return ids, err
			}
			continue
		}
		req := &tg.MessagesSendMultiMediaRequest{Peer: inputPeer(p)}
		if out.ReplyTo > 0 {
			req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: out.ReplyTo})
		}
		for i, m := range batch {
			media, err := c.uploadMedia(ctx, m)
			if err != nil {
				return ids, err
			}
			// 相册要求先将上传文件转换为服务器媒体引用。
			uploaded, err := c.api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{Peer: inputPeer(p), Media: media})
			if err != nil {
				return ids, err
			}
			input, err := uploadedInput(uploaded)
			if err != nil {
				return ids, err
			}
			id, err := randomID()
			if err != nil {
				return ids, err
			}
			item := tg.InputSingleMedia{Media: input, RandomID: id}
			if start == 0 && i == 0 {
				item.Message = plain
				if len(entities) > 0 {
					item.SetEntities(entities)
				}
			}
			req.MultiMedia = append(req.MultiMedia, item)
		}
		res, err := c.api.MessagesSendMultiMedia(ctx, req)
		if err != nil {
			return ids, err
		}
		sent, err := sentIDs(res)
		if err != nil {
			return ids, err
		}
		ids = append(ids, sent...)
	}
	// MTProto 相册接口没有 reply_markup，发出后给首条加上按钮。
	if len(out.Buttons) > 0 && len(ids) > 0 {
		err = c.Edit(ctx, p, ids[0], model.Outgoing{Text: out.Text, Mode: out.Mode, Buttons: out.Buttons, Preview: out.Preview})
		if err != nil {
			return ids, err
		}
	}
	return ids, nil
}

func (c *Client) sendOneMedia(ctx context.Context, p model.Peer, m model.Media, text string, entities []tg.MessageEntityClass, replyTo int, markup tg.ReplyMarkupClass, hasButtons bool) ([]int, error) {
	media, err := c.uploadMedia(ctx, m)
	if err != nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	req := &tg.MessagesSendMediaRequest{Peer: inputPeer(p), Media: media, Message: text, RandomID: id}
	if len(entities) > 0 {
		req.SetEntities(entities)
	}
	if hasButtons {
		req.SetReplyMarkup(markup)
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	res, err := c.api.MessagesSendMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	return sentIDs(res)
}
func uploadedInput(raw tg.MessageMediaClass) (tg.InputMediaClass, error) {
	switch v := raw.(type) {
	case *tg.MessageMediaPhoto:
		if p, ok := v.Photo.(*tg.Photo); ok {
			return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference}, Spoiler: v.Spoiler}, nil
		}
	case *tg.MessageMediaDocument:
		if d, ok := v.Document.(*tg.Document); ok {
			return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}, Spoiler: v.Spoiler}, nil
		}
	}
	return nil, fmt.Errorf("相册不支持媒体 %T", raw)
}

func (c *Client) Edit(ctx context.Context, p model.Peer, id int, out model.Outgoing) error {
	plain, entities, err := parseText(out.Text, out.Mode)
	if err != nil {
		return err
	}
	markup, err := keyboard(out.Buttons)
	if err != nil {
		return err
	}
	req := &tg.MessagesEditMessageRequest{Peer: inputPeer(c.cached(p)), ID: id, NoWebpage: !out.Preview}
	req.SetMessage(plain)
	req.SetEntities(entities)
	req.SetReplyMarkup(markup)
	if len(out.Media) > 1 {
		return errors.New("一次只能编辑一条消息的媒体")
	}
	if len(out.Media) == 1 {
		media, err := c.uploadMedia(ctx, out.Media[0])
		if err != nil {
			return err
		}
		req.SetMedia(media)
	}
	_, err = c.api.MessagesEditMessage(ctx, req)
	if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return nil
	}
	return err
}

func sentIDs(raw tg.UpdatesClass) ([]int, error) {
	var updates []tg.UpdateClass
	switch v := raw.(type) {
	case *tg.UpdateShortSentMessage:
		return []int{v.ID}, nil
	case *tg.Updates:
		updates = v.Updates
	case *tg.UpdatesCombined:
		updates = v.Updates
	case *tg.UpdateShort:
		updates = []tg.UpdateClass{v.Update}
	}
	seen := map[int]bool{}
	for _, u := range updates {
		switch v := u.(type) {
		case *tg.UpdateMessageID:
			seen[v.ID] = true
		case *tg.UpdateNewMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				seen[m.ID] = true
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				seen[m.ID] = true
			}
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("Telegram 发送响应缺少消息 ID（%T）", raw)
	}
	return ids, nil
}
