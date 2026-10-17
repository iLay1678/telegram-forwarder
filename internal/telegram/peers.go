package telegram

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

const channelOffset int64 = 1000000000000

// ParsePeerID 兼容 Telethon 的 marked ID，以及明确标注类型的原始 ID。
func ParsePeerID(value string) (model.Peer, error) {
	value = strings.TrimSpace(value)
	kind := ""
	if i := strings.IndexByte(value, ':'); i >= 0 {
		kind, value = value[:i], value[i+1:]
		if kind != "channel" && kind != "user" && kind != "chat" {
			return model.Peer{}, fmt.Errorf("无效会话类型 %q", kind)
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id == 0 {
		return model.Peer{}, fmt.Errorf("无效 Telegram ID %q", value)
	}
	if kind != "" {
		if id < 0 {
			return model.Peer{}, errors.New("带类型的 Telegram ID 必须为正数")
		}
		return model.Peer{ID: id, Kind: kind}, nil
	}
	if id <= -channelOffset {
		raw := -(id + channelOffset)
		if raw <= 0 {
			return model.Peer{}, fmt.Errorf("无效 Telegram 频道 ID %q", value)
		}
		return model.Peer{ID: raw, Kind: "channel"}, nil
	}
	if id < 0 {
		return model.Peer{ID: -id, Kind: "chat"}, nil
	}
	return model.Peer{ID: id, Kind: "user"}, nil
}

func MarkedID(p model.Peer) int64 {
	switch p.Kind {
	case "channel":
		return -channelOffset - p.ID
	case "chat":
		return -p.ID
	default:
		return p.ID
	}
}
func inputPeer(p model.Peer) tg.InputPeerClass {
	switch p.Kind {
	case "channel":
		return &tg.InputPeerChannel{ChannelID: p.ID, AccessHash: p.AccessHash}
	case "chat":
		return &tg.InputPeerChat{ChatID: p.ID}
	default:
		return &tg.InputPeerUser{UserID: p.ID, AccessHash: p.AccessHash}
	}
}
func rawPeer(p tg.PeerClass) model.Peer {
	switch v := p.(type) {
	case *tg.PeerChannel:
		return model.Peer{ID: v.ChannelID, Kind: "channel"}
	case *tg.PeerChat:
		return model.Peer{ID: v.ChatID, Kind: "chat"}
	case *tg.PeerUser:
		return model.Peer{ID: v.UserID, Kind: "user"}
	}
	return model.Peer{}
}
func userPeer(u *tg.User) model.Peer {
	return model.Peer{ID: u.ID, Kind: "user", Name: strings.TrimSpace(u.FirstName + " " + u.LastName), Username: u.Username, AccessHash: u.AccessHash}
}
func chatPeer(v tg.ChatClass) model.Peer {
	switch p := v.(type) {
	case *tg.Channel:
		return model.Peer{ID: p.ID, Kind: "channel", Name: p.Title, Username: p.Username, AccessHash: p.AccessHash, Broadcast: p.Broadcast}
	case *tg.ChannelForbidden:
		return model.Peer{ID: p.ID, Kind: "channel", Name: p.Title, AccessHash: p.AccessHash, Broadcast: p.Broadcast}
	case *tg.Chat:
		return model.Peer{ID: p.ID, Kind: "chat", Name: p.Title}
	case *tg.ChatForbidden:
		return model.Peer{ID: p.ID, Kind: "chat", Name: p.Title}
	}
	return model.Peer{}
}
func (c *Client) cached(p model.Peer) model.Peer {
	if v, ok := c.state.peer(p); ok {
		return v
	}
	if c.bot {
		p.AccessHash = 0
	}
	return p
}
func (c *Client) saveEntities(users []tg.UserClass, chats []tg.ChatClass) error {
	peers := make([]model.Peer, 0, len(users)+len(chats))
	for _, v := range users {
		if u, ok := v.(*tg.User); ok && !u.Min {
			peers = append(peers, userPeer(u))
		}
	}
	for _, v := range chats {
		if channel, ok := v.(*tg.Channel); ok && channel.Min {
			continue
		}
		peers = append(peers, chatPeer(v))
	}
	return c.state.putPeers(peers)
}

func (c *Client) Resolve(ctx context.Context, value string) (model.Peer, error) {
	value = strings.TrimSpace(value)
	if value == "me" || value == "self" {
		return c.cached(model.Peer{ID: c.SelfID(), Kind: "user"}), nil
	}
	if p, err := ParsePeerID(value); err == nil {
		if cached, ok := c.state.peer(p); ok {
			return cached, nil
		}
		// 正数旧配置可能保存了频道的 raw ID，优先从已有实体确认。
		if p.Kind == "user" {
			for _, v := range c.state.peers() {
				if v.ID == p.ID {
					return v, nil
				}
			}
		}
		if p.Kind == "chat" {
			if err := c.CheckPeer(ctx, p); err != nil {
				return p, err
			}
			return c.cached(p), nil
		}
		if !c.bot {
			if _, err = c.Dialogs(ctx); err != nil {
				return p, err
			}
			if cached, ok := c.state.peer(p); ok {
				return cached, nil
			}
			if p.Kind == "user" {
				for _, v := range c.state.peers() {
					if v.ID == p.ID {
						return v, nil
					}
				}
			}
		}
		if c.bot {
			// 旧数据库通常只保存正数 raw ID；机器人没有 dialogs 接口，须逐类查询。
			candidates := []model.Peer{p}
			if p.Kind == "user" && !strings.Contains(value, ":") {
				candidates = append(candidates, model.Peer{ID: p.ID, Kind: "channel"}, model.Peer{ID: p.ID, Kind: "chat"})
			}
			var failures []error
			for _, candidate := range candidates {
				if err := c.CheckPeer(ctx, candidate); err == nil {
					return c.cached(candidate), nil
				} else {
					failures = append(failures, err)
				}
				if ctx.Err() != nil {
					return p, ctx.Err()
				}
			}
			return p, fmt.Errorf("机器人无法访问会话 %s: %w", value, errors.Join(failures...))
		}
		return p, fmt.Errorf("找不到 %s 的 access_hash，请先加入目标或使用 @username", value)
	}
	username := strings.TrimPrefix(value, "@")
	for _, prefix := range []string{"https://t.me/", "http://t.me/", "t.me/", "https://telegram.me/"} {
		username = strings.TrimPrefix(username, prefix)
	}
	username = strings.TrimPrefix(username, "s/")
	username = strings.Split(strings.Split(username, "?")[0], "/")[0]
	if username == "" || strings.HasPrefix(username, "+") || username == "joinchat" {
		return model.Peer{}, errors.New("请先加入私有会话，再使用会话 ID")
	}
	res, err := c.api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return model.Peer{}, err
	}
	if err = c.saveEntities(res.Users, res.Chats); err != nil {
		return model.Peer{}, err
	}
	return c.cached(rawPeer(res.Peer)), nil
}

// CheckPeer 通过只读 RPC 检查会话可访问；它不保证拥有发消息或管理权限。
func (c *Client) CheckPeer(ctx context.Context, p model.Peer) error {
	p = c.cached(p)
	if p.ID <= 0 {
		return errors.New("会话 ID 无效")
	}
	if p.Kind == "user" {
		users, err := c.api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: p.ID, AccessHash: p.AccessHash}})
		if err != nil {
			return err
		}
		for _, raw := range users {
			if user, ok := raw.(*tg.User); ok && user.ID == p.ID && !user.Deleted {
				return c.saveEntities(users, nil)
			}
		}
		return fmt.Errorf("用户 %d 不存在或不可访问", p.ID)
	}
	var response tg.MessagesChatsClass
	var err error
	switch p.Kind {
	case "channel":
		response, err = c.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: p.ID, AccessHash: p.AccessHash}})
	case "chat":
		response, err = c.api.MessagesGetChats(ctx, []int64{p.ID})
	default:
		return fmt.Errorf("未知会话类型 %q", p.Kind)
	}
	if err != nil {
		return err
	}
	if err = c.saveEntities(nil, response.GetChats()); err != nil {
		return err
	}
	for _, raw := range response.GetChats() {
		switch chat := raw.(type) {
		case *tg.Channel:
			if p.Kind == "channel" && chat.ID == p.ID {
				return nil
			}
		case *tg.Chat:
			if p.Kind == "chat" && chat.ID == p.ID && !chat.Deactivated {
				return nil
			}
		}
	}
	return fmt.Errorf("会话 %s:%d 不存在或不可访问", p.Kind, p.ID)
}

func (c *Client) Dialogs(ctx context.Context) ([]model.Peer, error) {
	if c.bot {
		return c.state.peers(), nil
	}
	req := &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 100}
	result := map[string]model.Peer{}
	for {
		res, err := c.api.MessagesGetDialogs(ctx, req)
		if err != nil {
			return nil, err
		}
		var dialogs []tg.DialogClass
		var messages []tg.MessageClass
		var users []tg.UserClass
		var chats []tg.ChatClass
		switch v := res.(type) {
		case *tg.MessagesDialogs:
			dialogs, messages, users, chats = v.Dialogs, v.Messages, v.Users, v.Chats
		case *tg.MessagesDialogsSlice:
			dialogs, messages, users, chats = v.Dialogs, v.Messages, v.Users, v.Chats
		default:
			return nil, fmt.Errorf("未知对话响应 %T", res)
		}
		if err = c.saveEntities(users, chats); err != nil {
			return nil, err
		}
		var last *tg.Dialog
		for _, d := range dialogs {
			if v, ok := d.(*tg.Dialog); ok {
				p := c.cached(rawPeer(v.Peer))
				result[peerKey(p)] = p
				last = v
			}
		}
		if len(dialogs) < 100 || last == nil {
			break
		}
		p := c.cached(rawPeer(last.Peer))
		req.OffsetPeer = inputPeer(p)
		req.OffsetID = last.TopMessage
		req.ExcludePinned = true
		for _, v := range messages {
			if m, ok := v.(*tg.Message); ok && m.ID == last.TopMessage && peerKey(rawPeer(m.PeerID)) == peerKey(p) {
				req.OffsetDate = m.Date
				break
			}
		}
	}
	out := make([]model.Peer, 0, len(result))
	for _, p := range result {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return MarkedID(out[i]) < MarkedID(out[j]) })
	return out, nil
}

func (c *Client) IsChannelAdmin(ctx context.Context, p model.Peer, userID int64) (bool, error) {
	if p.Kind != "channel" {
		return false, nil
	}
	p = c.cached(p)
	user := c.cached(model.Peer{ID: userID, Kind: "user"})
	res, err := c.api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{Channel: &tg.InputChannel{ChannelID: p.ID, AccessHash: p.AccessHash}, Participant: inputPeer(user)})
	if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch res.Participant.(type) {
	case *tg.ChannelParticipantAdmin, *tg.ChannelParticipantCreator:
		return true, nil
	}
	return false, nil
}

func (c *Client) DiscussionLink(ctx context.Context, p model.Peer, id int) (string, error) {
	if p.Kind != "channel" {
		return "", nil
	}
	p = c.cached(p)
	res, err := c.api.MessagesGetDiscussionMessage(ctx, &tg.MessagesGetDiscussionMessageRequest{Peer: inputPeer(p), MsgID: id})
	if tgerr.Is(err, "MSG_ID_INVALID") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err = c.saveEntities(res.Users, res.Chats); err != nil {
		return "", err
	}
	for _, raw := range res.Messages {
		if m, ok := raw.(*tg.Message); ok {
			target := c.cached(rawPeer(m.PeerID))
			if target.ID != p.ID {
				return messageLink(target, m.ID), nil
			}
		}
	}
	return "", nil
}

func messageLink(p model.Peer, id int) string {
	if p.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", p.Username, id)
	}
	if p.Kind == "channel" {
		return fmt.Sprintf("https://t.me/c/%d/%d", p.ID, id)
	}
	return ""
}
