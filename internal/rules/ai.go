package rules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
)

var historyPlaceholder = regexp.MustCompile(`\{(source|target)_message_(context|time):(\d+)\}`)

func (e *Engine) processAI(ctx context.Context, rule model.Rule, text string, media []model.Media) (string, error) {
	var images []model.AIImage
	if rule.EnableAIUploadImage {
		for _, item := range media {
			if item.Kind != "photo" && !strings.HasPrefix(item.MIME, "image/") {
				continue
			}
			data, err := os.ReadFile(item.Path)
			if err != nil {
				slog.Warn("AI 图片读取失败，跳过该图片", "rule", rule.ID, "error", err)
				continue
			}
			mime := item.MIME
			if mime == "" {
				mime = "image/jpeg"
			}
			images = append(images, model.AIImage{Data: data, MIME: mime})
		}
	}
	if text == "" && len(images) == 0 {
		return text, nil
	}
	prompt := rule.AIPrompt
	if prompt == "" {
		prompt = e.Config.String("DEFAULT_AI_PROMPT", "请尊重原意，保持原有格式不变，用简体中文重写下面的内容：")
	}
	name := rule.AIModel
	if name == "" {
		name = e.Config.String("DEFAULT_AI_MODEL", "gpt-4o")
	}
	input := text
	if input == "" {
		input = "[图片消息]"
	}
	prompt = e.expandAIPrompt(ctx, rule, prompt, input, time.Now())
	var processed string
	var err error
	if e.AI == nil {
		err = errors.New("AI 服务未初始化")
	} else {
		processed, err = e.AI.Process(ctx, name, prompt, input, images)
	}
	if ctx.Err() != nil {
		return text, ctx.Err()
	}
	if err != nil {
		// 与 Python 版一致，AI 不可用时仍转发替换规则处理后的正文。
		slog.Warn("AI 处理失败，保留原文继续转发", "rule", rule.ID, "error", err)
		return text, nil
	}
	return processed, nil
}

func (e *Engine) expandAIPrompt(ctx context.Context, rule model.Rule, prompt, text string, now time.Time) string {
	seen := make(map[string]bool)
	for _, match := range historyPlaceholder.FindAllStringSubmatch(prompt, -1) {
		kind := match[1] + ":" + match[2]
		// 旧版每种变量只取首次出现的参数，并替换所有同名实例。
		if seen[kind] {
			continue
		}
		seen[kind] = true
		chatID := rule.Source.TelegramChatID
		if match[1] == "target" {
			chatID = rule.Target.TelegramChatID
		}
		value := ""
		amount, err := strconv.Atoi(match[3])
		if err == nil {
			value, err = e.aiHistory(ctx, chatID, match[2], amount, now)
		}
		if err != nil {
			slog.Warn("AI 提示词聊天记录读取失败", "rule", rule.ID, "variable", match[0], "error", err)
		}
		prompt = strings.ReplaceAll(prompt, match[0], value)
	}
	return strings.ReplaceAll(prompt, "{Message}", text)
}

func (e *Engine) aiHistory(ctx context.Context, chatID, kind string, amount int, now time.Time) (string, error) {
	if amount <= 0 {
		return "", nil
	}
	peer, err := e.User.Resolve(ctx, chatID)
	if err != nil {
		return "", err
	}
	limit := amount
	var start time.Time
	if kind == "time" {
		if int64(amount) > int64((1<<63-1)/time.Minute) {
			return "", errors.New("AI 提示词时间范围过大")
		}
		start = now.Add(-time.Duration(amount) * time.Minute)
		limit = 500
	}
	var messages []model.Message
	offset := 0
	for remaining := limit; remaining > 0; {
		requested := min(100, remaining)
		batch, err := e.User.History(ctx, peer, offset, requested)
		if err != nil {
			return "", err
		}
		if len(batch) == 0 {
			break
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].ID > batch[j].ID })
		if len(batch) > requested {
			batch = batch[:requested]
		}
		next := batch[len(batch)-1].ID
		if next <= 0 || (offset != 0 && next >= offset) {
			return "", fmt.Errorf("AI 聊天记录分页未前进: %d", next)
		}
		done := false
		for _, message := range batch {
			if kind == "time" {
				if message.Date.Before(start) {
					done = true
					break
				}
				if message.Date.After(now) {
					continue
				}
			}
			if message.Text != "" {
				messages = append(messages, message)
			}
		}
		remaining -= len(batch)
		if done || len(batch) < requested {
			break
		}
		offset = next
	}
	if kind == "time" {
		sort.SliceStable(messages, func(i, j int) bool { return messages[i].Date.Before(messages[j].Date) })
	}
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	return strings.Join(texts, "\n---\n"), nil
}
