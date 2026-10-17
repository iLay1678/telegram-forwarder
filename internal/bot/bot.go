// Package bot 实现原有管理命令和交互设置。
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/rules"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type inputState struct {
	RuleID  int64
	Field   string
	Expires time.Time
}
type Bot struct {
	Config       *config.Config
	Store        *storage.Store
	User, Client model.Transport
	Summary      func(context.Context, int64) error
	Sync         func(context.Context, int64) error
	CleanupRule  func(context.Context, int64) error
	mu           sync.Mutex
	states       map[string]inputState
	inputs       map[string]time.Time
}

func New(cfg *config.Config, store *storage.Store, user, client model.Transport) *Bot {
	return &Bot{Config: cfg, Store: store, User: user, Client: client, states: make(map[string]inputState), inputs: make(map[string]time.Time)}
}

func (b *Bot) isAdmin(id int64) bool {
	for _, admin := range b.Config.Admins {
		if id == admin {
			return true
		}
	}
	return false
}
func (b *Bot) authorize(ctx context.Context, msg model.Message) bool {
	if b.isAdmin(msg.Sender.ID) {
		return true
	}
	if msg.Chat.Kind != "channel" || !msg.Chat.Broadcast {
		return false
	}
	if verifier, ok := b.User.(interface {
		IsChannelAdmin(context.Context, model.Peer, int64) (bool, error)
	}); ok {
		for _, id := range b.Config.Admins {
			yes, err := verifier.IsChannelAdmin(ctx, msg.Chat, id)
			if err == nil && yes {
				return true
			}
		}
	}
	return false
}

func (b *Bot) stateKey(msg model.Message) string {
	id := msg.Sender.ID
	if msg.Chat.Broadcast {
		id = b.Config.UserID
	}
	return fmt.Sprintf("%d:%s:%d", id, msg.Chat.Kind, msg.Chat.ID)
}

func (b *Bot) HandleInput(ctx context.Context, msg model.Message) bool {
	return b.handleInput(ctx, msg, false)
}

func (b *Bot) handleInput(ctx context.Context, msg model.Message, authorized bool) bool {
	key := b.stateKey(msg)
	messageKey := fmt.Sprintf("%s:%d", key, msg.ID)
	text := strings.TrimSpace(msg.Text)
	now := time.Now()
	b.mu.Lock()
	if expiry, handled := b.inputs[messageKey]; handled && now.Before(expiry) {
		b.mu.Unlock()
		return true
	}
	state, ok := b.states[key]
	if ok && !now.Before(state.Expires) {
		delete(b.states, key)
		ok = false
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	// 只有待处理输入才需要鉴权，普通频道广播不能触发管理员查询 RPC。
	if !authorized && !b.authorize(ctx, msg) {
		return false
	}
	b.mu.Lock()
	now = time.Now()
	// 鉴权期间另一客户端可能已经消费输入，或用户已切换设置。
	if expiry, handled := b.inputs[messageKey]; handled && now.Before(expiry) {
		b.mu.Unlock()
		return true
	}
	current, ok := b.states[key]
	if !ok || current != state || !now.Before(state.Expires) {
		if ok && current == state {
			delete(b.states, key)
		}
		b.mu.Unlock()
		return false
	}
	for k, expiry := range b.inputs {
		if !now.Before(expiry) {
			delete(b.inputs, k)
		}
	}
	// 用户与机器人客户端可能同时收到同一条输入；先占用消息，避免重复应用或落入转发管线。
	if !strings.HasPrefix(text, "/") || text == "/cancel" {
		b.inputs[messageKey] = now.Add(5 * time.Minute)
	}
	if strings.HasPrefix(text, "/") {
		delete(b.states, key)
	}
	b.mu.Unlock()
	if text == "/cancel" {
		b.reply(ctx, msg, "已取消", nil)
		return true
	}
	if strings.HasPrefix(text, "/") {
		return false
	}
	if err := b.applyInput(ctx, state, msg); err != nil {
		b.reply(ctx, msg, "设置失败："+err.Error()+"\n请重新输入，或 /cancel 取消。", nil)
	} else {
		b.mu.Lock()
		if b.states[key] == state {
			delete(b.states, key)
		}
		b.mu.Unlock()
		b.reply(ctx, msg, "已保存", nil)
	}
	return true
}

func (b *Bot) HandleMessage(ctx context.Context, msg model.Message) {
	commandMessage := strings.HasPrefix(strings.TrimSpace(msg.Text), "/")
	linkMessage := strings.HasPrefix(msg.Text, "https://t.me/") || strings.HasPrefix(msg.Text, "http://t.me/")
	if !commandMessage && !linkMessage {
		b.HandleInput(ctx, msg)
		return
	}
	if !b.authorize(ctx, msg) {
		return
	}
	if b.handleInput(ctx, msg, true) {
		return
	}
	if linkMessage {
		if err := b.forwardLink(ctx, msg); err != nil {
			b.reply(ctx, msg, "转发失败："+err.Error(), nil)
		}
		return
	}
	command, args := cutWord(strings.TrimSpace(msg.Text))
	command = strings.SplitN(strings.TrimPrefix(command, "/"), "@", 2)[0]
	if alias, ok := aliases[command]; ok {
		command = alias
	}
	err := b.command(ctx, msg, command, args)
	if err != nil {
		b.reply(ctx, msg, "操作失败："+err.Error(), nil)
	}
	if b.Config.Bool("USER_MESSAGE_DELETE_ENABLE", false) {
		b.deleteLater(ctx, msg.Chat, []int{msg.ID}, 0)
	}
}

func (b *Bot) reply(ctx context.Context, msg model.Message, text string, buttons [][]model.Button) {
	for i, part := range rules.SplitText(text, 3800) {
		out := model.Outgoing{Text: part, Mode: "TEXT", ReplyTo: msg.ID}
		if i == 0 {
			out.Buttons = buttons
		}
		ids, err := b.Client.Send(ctx, msg.Chat, out)
		if err != nil {
			slog.Error("发送管理回复失败", "error", err)
			return
		}
		b.deleteLater(ctx, msg.Chat, ids, b.Config.Int("BOT_MESSAGE_DELETE_TIMEOUT", 300))
	}
}

func (b *Bot) clearState(msg model.Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.states, b.stateKey(msg))
}

func (b *Bot) deleteLater(ctx context.Context, chat model.Peer, ids []int, seconds int) {
	if seconds < 0 || len(ids) == 0 {
		return
	}
	go func() {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := b.Client.Delete(ctx, chat, ids); err != nil && ctx.Err() == nil {
			slog.Warn("删除管理消息失败", "error", err)
		}
	}()
}

func (b *Bot) current(ctx context.Context, chat model.Peer) (model.Rule, error) {
	all, err := b.targetRules(ctx, chat)
	if err != nil {
		return model.Rule{}, err
	}
	for _, r := range all {
		if r.Target.CurrentAddID == r.Source.TelegramChatID {
			return r, nil
		}
	}
	if len(all) > 0 {
		return all[0], nil
	}
	return model.Rule{}, errors.New("当前聊天没有绑定规则，请先 /bind")
}
func (b *Bot) targetRules(ctx context.Context, chat model.Peer) ([]model.Rule, error) {
	all, err := b.Store.Rules(ctx)
	if err != nil {
		return nil, err
	}
	var result []model.Rule
	for _, r := range all {
		if rules.MatchChat(r.Target.TelegramChatID, chat) {
			result = append(result, r)
		}
	}
	return result, nil
}
func cutWord(text string) (string, string) {
	for i, c := range text {
		if c == ' ' || c == '\n' || c == '\t' {
			return text[:i], strings.TrimLeft(text[i:], " \n\t")
		}
	}
	return text, ""
}

// splitQuoted 用于旧命令中的带引号普通关键词；正则命令直接保留反斜杠。
func splitQuoted(text string) ([]string, error) {
	var result []string
	var current strings.Builder
	var quote rune
	escaped, started := false, false
	for _, r := range text {
		if escaped {
			current.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			if started {
				result = append(result, current.String())
				current.Reset()
				started = false
			}
			continue
		}
		current.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, errors.New("引号或转义未闭合")
	}
	if started {
		result = append(result, current.String())
	}
	return result, nil
}

var aliases = map[string]string{
	"sw": "switch",
	"b":  "bind", "s": "settings", "a": "add", "ar": "add_regex", "aa": "add_all", "ara": "add_regex_all", "lk": "list_keyword", "rk": "remove_keyword", "rkbi": "remove_keyword_by_id", "rak": "remove_all_keyword", "r": "replace", "ra": "replace_all", "lrp": "list_replace", "rr": "remove_replace", "ek": "export_keyword", "er": "export_replace", "ik": "import_keyword", "irk": "import_regex_keyword", "ir": "import_replace", "ub": "ufb_bind", "uu": "ufb_unbind", "uic": "ufb_item_change", "cak": "clear_all_keywords", "cakr": "clear_all_keywords_regex", "car": "clear_all_replace", "ck": "copy_keywords", "ckr": "copy_keywords_regex", "crp": "copy_replace", "cr": "copy_rule", "cl": "changelog", "lr": "list_rule", "dr": "delete_rule", "dru": "delete_rss_user", "ca": "clear_all", "h": "help",
}
