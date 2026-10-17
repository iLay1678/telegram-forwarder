// Package scheduler 实现旧版的每日总结及聊天名称更新。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/rules"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type Scheduler struct {
	Config    *config.Config
	Store     *storage.Store
	User, Bot model.Transport
	AI        model.AIProcessor
	mu        sync.Mutex
	running   map[int64]bool
	slots     chan struct{}
	now       func() time.Time
}

func New(cfg *config.Config, store *storage.Store, user, bot model.Transport, ai model.AIProcessor) *Scheduler {
	return &Scheduler{Config: cfg, Store: store, User: user, Bot: bot, AI: ai, running: make(map[int64]bool), slots: make(chan struct{}, 2), now: time.Now}
}

func nextRun(now time.Time, clock string) (time.Time, error) {
	parsed, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("无效定时时间 %q，必须为 HH:MM", clock)
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

type schedule struct {
	clock string
	next  time.Time
}

// Run 定期读取规则，因此运行中调整总结时间、关闭或删除规则立即生效。
func (s *Scheduler) Run(ctx context.Context) error {
	chatTime := s.Config.String("CHAT_UPDATE_TIME", "03:00")
	chatNext, err := nextRun(s.now().In(s.Config.Timezone), chatTime)
	if err != nil {
		return err
	}
	schedules := make(map[int64]schedule)
	var workers sync.WaitGroup
	defer workers.Wait()
	launch := func(fn func(context.Context) error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				slog.Error("定时任务失败", "error", err)
			}
		}()
	}
	refresh := func() {
		now := s.now().In(s.Config.Timezone)
		all, err := s.Store.Rules(ctx)
		if err != nil {
			slog.Error("读取定时规则失败", "error", err)
			return
		}
		active := make(map[int64]bool)
		for _, rule := range all {
			if !rule.IsSummary {
				continue
			}
			active[rule.ID] = true
			entry, ok := schedules[rule.ID]
			if !ok || entry.clock != rule.SummaryTime {
				next, err := nextRun(now, rule.SummaryTime)
				if err != nil {
					slog.Warn("忽略无效总结时间", "rule", rule.ID, "error", err)
					continue
				}
				entry = schedule{clock: rule.SummaryTime, next: next}
			} else if !now.Before(entry.next) {
				id := rule.ID
				launch(func(ctx context.Context) error { return s.summarize(ctx, id, false) })
				entry.next, _ = nextRun(now, entry.clock)
			}
			schedules[rule.ID] = entry
		}
		for id := range schedules {
			if !active[id] {
				delete(schedules, id)
			}
		}
		if !now.Before(chatNext) {
			launch(s.UpdateChats)
			chatNext, _ = nextRun(now, chatTime)
		}
	}
	refresh()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			refresh()
		}
	}
}

// Summary 立即总结指定规则；ID 为零时依次总结所有开启定时总结的规则。
func (s *Scheduler) Summary(ctx context.Context, id int64) error {
	if id != 0 {
		return s.summarize(ctx, id, true)
	}
	all, err := s.Store.Rules(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, rule := range all {
		if rule.IsSummary {
			if err := s.summarize(ctx, rule.ID, true); err != nil {
				failures = append(failures, err)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.Join(failures...)
}

func (s *Scheduler) summarize(ctx context.Context, id int64, immediate bool) error {
	s.mu.Lock()
	if s.running[id] {
		s.mu.Unlock()
		return fmt.Errorf("规则 %d 的总结正在进行", id)
	}
	s.running[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.running, id); s.mu.Unlock() }()
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.slots }()
	rule, err := s.Store.Rule(ctx, id)
	if err != nil {
		return err
	}
	if !immediate && !rule.IsSummary {
		return nil
	}
	if s.AI == nil {
		return errors.New("AI 总结服务未配置")
	}
	end := s.now().In(s.Config.Timezone)
	clock, err := time.Parse("15:04", rule.SummaryTime)
	if err != nil {
		return fmt.Errorf("规则 %d 总结时间无效: %w", id, err)
	}
	start := time.Date(end.Year(), end.Month(), end.Day(), clock.Hour(), clock.Minute(), 0, 0, end.Location()).AddDate(0, 0, -1)
	source, err := s.User.Resolve(ctx, rule.Source.TelegramChatID)
	if err != nil {
		return err
	}
	messages, err := s.history(ctx, source, start, end)
	if err != nil {
		return fmt.Errorf("规则 %d 获取总结历史: %w", id, err)
	}
	if len(messages) == 0 {
		return nil
	}
	modelName := rule.AIModel
	if modelName == "" {
		modelName = s.Config.String("DEFAULT_AI_MODEL", "gpt-4o")
	}
	prompt := rule.SummaryPrompt
	if prompt == "" {
		prompt = s.Config.String("DEFAULT_SUMMARY_PROMPT", "请总结以下频道/群组24小时内的消息。")
	}
	summary, err := s.AI.Process(ctx, modelName, prompt, strings.Join(messages, "\n"), nil)
	if err != nil {
		return fmt.Errorf("规则 %d AI 总结: %w", id, err)
	}
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	// 旧数据库保存原始正数 ID，用户会话的完整实体缓存可以确定频道/群组类型。
	// 机器人发送时由其传输层选择自身的 access_hash。
	target, err := s.User.Resolve(ctx, rule.Target.TelegramChatID)
	if err != nil {
		target, err = s.Bot.Resolve(ctx, rule.Target.TelegramChatID)
	}
	if err != nil {
		return err
	}
	name := rule.Source.Name
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	header := fmt.Sprintf("📋 %s - %.0f小时消息总结\n🕐 时间范围: %s - %s\n📊 消息数量: %d 条\n\n", name, math.RoundToEven(end.Sub(start).Hours()), start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04"), len(messages))
	parts := rules.SplitText(summary, 3700)
	first := 0
	for i, part := range parts {
		prefix := header
		if i > 0 {
			prefix = fmt.Sprintf("📋 %s - 总结报告 (续 %d/%d)\n\n", name, i+1, len(parts))
		}
		ids, err := s.Bot.Send(ctx, target, model.Outgoing{Text: prefix + part, Mode: "MARKDOWN"})
		if err != nil && invalidMarkup(err) {
			ids, err = s.Bot.Send(ctx, target, model.Outgoing{Text: prefix + part, Mode: "TEXT"})
		}
		if err != nil {
			return fmt.Errorf("规则 %d 发送总结第 %d 部分: %w", id, i+1, err)
		}
		if i == 0 && len(ids) > 0 {
			first = ids[0]
		}
	}
	if rule.IsTopSummary && first != 0 {
		if err := s.Bot.Pin(ctx, target, first); err != nil {
			slog.Warn("置顶总结失败", "rule", id, "error", err)
		}
	}
	return nil
}

func invalidMarkup(err error) bool {
	text := strings.ToUpper(err.Error())
	return strings.Contains(text, "MARKUP_INVALID") || strings.Contains(text, "ENTITY_BOUNDS_INVALID") || strings.Contains(text, "ENTITIES_TOO_LONG")
}

func (s *Scheduler) history(ctx context.Context, peer model.Peer, start, end time.Time) ([]string, error) {
	batchSize := max(1, min(100, s.Config.Int("SUMMARY_BATCH_SIZE", 20)))
	delay := time.Duration(max(0, s.Config.Int("SUMMARY_BATCH_DELAY", 2))) * time.Second
	var messages []string
	offset := 0
	for {
		batch, err := s.User.History(ctx, peer, offset, batchSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		done := false
		next := batch[len(batch)-1].ID
		if next <= 0 || (offset != 0 && next >= offset) {
			return nil, errors.New("历史消息分页未前进")
		}
		for _, msg := range batch {
			if msg.Date.After(end) {
				continue
			}
			if msg.Date.Before(start) {
				done = true
				break
			}
			if msg.Text != "" {
				messages = append(messages, msg.Text)
			}
		}
		if done {
			break
		}
		offset = next
		if err := pause(ctx, delay); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func pause(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Scheduler) UpdateChats(ctx context.Context) error {
	var chats []model.Chat
	if err := s.Store.DB.SelectContext(ctx, &chats, "SELECT id, telegram_chat_id, COALESCE(name,'') AS name, COALESCE(current_add_id,'') AS current_add_id FROM chats"); err != nil {
		return err
	}
	// Resolve 可以命中持久化缓存；先刷新对话实体，才能发现服务端的名称变化。
	dialogs, err := s.User.Dialogs(ctx)
	if err != nil {
		return fmt.Errorf("刷新聊天列表: %w", err)
	}
	for i, chat := range chats {
		if i > 0 {
			if err := pause(ctx, time.Second); err != nil {
				return err
			}
		}
		var peer model.Peer
		for _, candidate := range dialogs {
			if rules.MatchChat(chat.TelegramChatID, candidate) {
				peer = candidate
				break
			}
		}
		var err error
		if peer.ID == 0 {
			peer, err = s.User.Resolve(ctx, chat.TelegramChatID)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("更新聊天信息失败", "chat", chat.ID, "error", err)
			continue
		}
		if peer.Name != "" && peer.Name != chat.Name {
			if _, err := s.Store.DB.ExecContext(ctx, "UPDATE chats SET name=? WHERE id=?", peer.Name, chat.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
