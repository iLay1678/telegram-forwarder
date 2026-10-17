package rules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type Engine struct {
	Config    *config.Config
	Store     *storage.Store
	User, Bot model.Transport
	AI        model.AIProcessor
	RSS       model.RSSPublisher
	Push      model.Pusher
	BotID     int64
	queue     chan pendingMessage
	mu        sync.Mutex
	groups    map[string]time.Time
}

type pendingMessage struct {
	message model.Message
	done    chan error
}

func New(cfg *config.Config, store *storage.Store, user, bot model.Transport) *Engine {
	return &Engine{Config: cfg, Store: store, User: user, Bot: bot, queue: make(chan pendingMessage, 256), groups: make(map[string]time.Time)}
}

func (e *Engine) Submit(ctx context.Context, msg model.Message) {
	e.SubmitWithAck(ctx, msg, nil)
}

// SubmitWithAck 的 done 必须带一个缓冲位，调用方取消后 worker 仍可结束。
func (e *Engine) SubmitWithAck(ctx context.Context, msg model.Message, done chan error) {
	if msg.Sender.ID == e.BotID && e.BotID != 0 {
		if done != nil {
			done <- nil
		}
		return
	}
	select {
	case e.queue <- pendingMessage{message: msg, done: done}:
	case <-ctx.Done():
		if done != nil {
			done <- ctx.Err()
		}
	}
}

func (e *Engine) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case pending := <-e.queue:
					msg := pending.message
					err := e.handle(ctx, msg)
					if err != nil && ctx.Err() == nil {
						slog.Error("消息处理失败", "chat", msg.Chat.ID, "message", msg.ID, "error", err)
					}
					if pending.done != nil {
						pending.done <- err
					}
				}
			}
		}()
	}
	workers.Wait()
}

func (e *Engine) handle(ctx context.Context, msg model.Message) error {
	ruleList, err := e.Store.Rules(ctx)
	if err != nil {
		return err
	}
	var matching []model.Rule
	for _, rule := range ruleList {
		if rule.EnableRule && MatchChat(rule.Source.TelegramChatID, msg.Chat) {
			matching = append(matching, rule)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if msg.GroupedID != 0 {
		key := fmt.Sprintf("%s:%d:%d", msg.Chat.Kind, msg.Chat.ID, msg.GroupedID)
		e.mu.Lock()
		now := time.Now()
		for k, until := range e.groups {
			if now.After(until) {
				delete(e.groups, k)
			}
		}
		_, seen := e.groups[key]
		if !seen {
			e.groups[key] = now.Add(5 * time.Minute)
		}
		e.mu.Unlock()
		if seen {
			return nil
		}
		if err := wait(ctx, time.Second); err != nil {
			return err
		}
		ids := make([]int, 0, 21)
		for id := max(1, msg.ID-10); id <= msg.ID+10; id++ {
			ids = append(ids, id)
		}
		messages, err := e.User.Fetch(ctx, msg.Chat, ids)
		if err != nil {
			return fmt.Errorf("读取媒体组: %w", err)
		}
		sort.Slice(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
		msg.Media = nil
		for _, m := range messages {
			if m.GroupedID == msg.GroupedID {
				msg.Media = append(msg.Media, m.Media...)
				if m.Text != "" {
					msg.Text = m.Text
					msg.Buttons = m.Buttons
				}
			}
		}
	}
	var errs []error
	deleteIDs := map[int]bool{}
	for _, rule := range matching {
		if err := e.process(ctx, rule, msg, func(ids []int) error {
			for _, id := range ids {
				deleteIDs[id] = true
			}
			return nil
		}); err != nil {
			errs = append(errs, fmt.Errorf("规则 %d: %w", rule.ID, err))
		}
	}
	// 同一来源的所有规则均处理完毕后再删除，避免影响后续规则的延迟重取。
	if len(errs) == 0 && len(deleteIDs) > 0 {
		ids := make([]int, 0, len(deleteIDs))
		for id := range deleteIDs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		return e.User.Delete(ctx, msg.Chat, ids)
	}
	return errors.Join(errs...)
}

func (e *Engine) Process(ctx context.Context, rule model.Rule, msg model.Message) error {
	return e.process(ctx, rule, msg, func(ids []int) error { return e.User.Delete(ctx, msg.Chat, ids) })
}

func (e *Engine) process(ctx context.Context, rule model.Rule, msg model.Message, remove func([]int) error) error {
	if !rule.EnableRule {
		return nil
	}
	// Python 用户模式只做关键词筛选，然后原生转发，保留 Telegram 转发来源。
	if !rule.UseBot {
		pass, err := MatchKeywords(rule, keywordText(rule, msg))
		if err != nil || !pass {
			return err
		}
		forwarder, ok := e.User.(interface {
			Forward(context.Context, model.Peer, model.Peer, []int) ([]int, error)
		})
		if !ok {
			return errors.New("传输层不支持 Telegram 原生转发")
		}
		target, err := e.User.Resolve(ctx, rule.Target.TelegramChatID)
		if err != nil {
			return err
		}
		_, err = forwarder.Forward(ctx, msg.Chat, target, messageIDs(msg))
		return err
	}
	if rule.EnableDelay && rule.DelaySeconds > 0 {
		if err := wait(ctx, time.Duration(rule.DelaySeconds)*time.Second); err != nil {
			return err
		}
		updated, err := e.User.Fetch(ctx, msg.Chat, []int{msg.ID})
		if err != nil {
			return err
		}
		if len(updated) == 0 {
			return nil
		}
		media := msg.Media
		msg = updated[0]
		if msg.GroupedID != 0 {
			msg.Media = media
		}
	}
	pass, err := MatchKeywords(rule, keywordText(rule, msg))
	if err != nil || !pass {
		return err
	}
	text, err := ReplaceText(rule, msg.Text)
	if err != nil {
		return err
	}
	media, notices, pass := FilterMedia(rule, msg.Media)
	if !pass {
		return nil
	}
	var tempDir string
	if len(media) > 0 {
		if err = os.MkdirAll(e.Config.Path("temp"), 0700); err != nil {
			return err
		}
		tempDir, err = os.MkdirTemp(e.Config.Path("temp"), "forward-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)
		for i := range media {
			name := filepath.Base(media[i].Filename)
			if name == "." || name == "" {
				name = fmt.Sprintf("media-%d", i)
			}
			media[i].Path, err = e.User.Download(ctx, media[i], filepath.Join(tempDir, fmt.Sprintf("%d-%s", i, name)))
			if err != nil {
				return fmt.Errorf("下载媒体: %w", err)
			}
		}
	}
	if rule.IsAI {
		text, err = e.processAI(ctx, rule, text, media)
		if err != nil {
			return err
		}
		if rule.IsKeywordAfterAI {
			processed := msg
			processed.Text = text
			pass, err = MatchKeywords(rule, keywordText(rule, processed))
			if err != nil || !pass {
				return err
			}
		}
	}
	rssText := text
	text = AddInfo(rule, msg, text, e.Config.Timezone)
	if len(notices) > 0 {
		text += "\n\n" + strings.Join(notices, "\n")
	}
	buttons := msg.Buttons
	commentLink := ""
	if rule.EnableCommentButton && msg.Chat.Kind == "channel" {
		if discussion, ok := e.User.(interface {
			DiscussionLink(context.Context, model.Peer, int) (string, error)
		}); ok {
			link, linkErr := discussion.DiscussionLink(ctx, msg.Chat, msg.ID)
			if linkErr != nil {
				slog.Warn("评论区链接不可用", "error", linkErr)
			} else if link != "" {
				commentLink = link
				if msg.GroupedID == 0 {
					buttons = append(buttons, []model.Button{{Text: "💬 查看评论区", URL: link}})
				}
			}
		}
	}
	if e.RSS != nil && rule.RSS != nil && rule.RSS.EnableRSS {
		if err = e.RSS.Publish(ctx, rule, msg, rssText, media); err != nil {
			return err
		}
		if rule.OnlyRSS {
			return nil
		}
	}
	preview := rule.IsPreview == "ON" || (rule.IsPreview == "FOLLOW" && msg.HasPreview)
	out := model.Outgoing{Text: text, Mode: rule.MessageMode, Media: media, Buttons: buttons, Preview: preview}
	if rule.HandleMode == "EDIT" {
		if msg.Chat.Kind != "channel" {
			return nil
		}
		if text == msg.Text {
			return nil
		}
		out.Media = nil
		for i, id := range messageIDs(msg) {
			if i > 0 {
				out.Text = ""
			}
			if err := e.User.Edit(ctx, msg.Chat, id, out); err != nil {
				return err
			}
		}
		return nil
	}
	delivered := false
	if !rule.EnableOnlyPush && (text != "" || len(media) > 0) {
		client := e.User
		if rule.UseBot {
			client = e.Bot
		}
		target, resolveErr := client.Resolve(ctx, rule.Target.TelegramChatID)
		if resolveErr != nil {
			return fmt.Errorf("目标聊天: %w", resolveErr)
		}
		sent, sendErr := client.Send(ctx, target, out)
		if sendErr != nil {
			return sendErr
		}
		if msg.GroupedID != 0 && commentLink != "" && len(sent) > 0 {
			_, replyErr := e.Bot.Send(ctx, target, model.Outgoing{Text: "💬 评论区", ReplyTo: sent[0], Buttons: [][]model.Button{{{Text: "💬 查看评论区", URL: commentLink}}}})
			if replyErr != nil {
				slog.Warn("媒体组评论按钮回复失败", "rule", rule.ID, "error", replyErr)
			}
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		delivered = true
	}
	if rule.EnablePush {
		if e.Push == nil {
			return errors.New("推送服务未初始化")
		}
		if err = e.Push.Send(ctx, rule.PushConfigs, text, media); err != nil {
			return err
		}
		for _, p := range rule.PushConfigs {
			if p.EnablePushChannel && (text != "" || len(media) > 0) {
				delivered = true
			}
		}
	}
	if rule.IsDeleteOriginal && delivered {
		return remove(messageIDs(msg))
	}
	return nil
}

func keywordText(rule model.Rule, msg model.Message) string {
	if !rule.IsFilterUserInfo {
		return msg.Text
	}
	return senderInfo(rule, msg) + ":\n" + msg.Text
}

func messageIDs(msg model.Message) []int {
	seen := map[int]bool{msg.ID: true}
	for _, m := range msg.Media {
		if m.MessageID > 0 {
			seen[m.MessageID] = true
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
