package rules

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
)

// MatchKeywords 保留旧版四种名单顺序和对应反转开关的含义。
func MatchKeywords(rule model.Rule, text string) (bool, error) {
	black, white := false, false
	for _, k := range rule.Keywords {
		match := strings.Contains(strings.ToLower(text), strings.ToLower(k.Keyword))
		if k.IsRegex {
			var err error
			match, err = pyregex.Search(k.Keyword, text)
			if err != nil {
				slog.Warn("跳过无效正则关键词", "rule", rule.ID, "keyword", k.ID)
				continue
			}
		}
		if k.IsBlacklist {
			black = black || match
		} else {
			white = white || match
		}
	}
	switch rule.ForwardMode {
	case "WHITELIST":
		return white && (!rule.EnableReverseBlacklist || black), nil
	case "BLACKLIST":
		return !black && (!rule.EnableReverseWhitelist || !white), nil
	case "WHITELIST_THEN_BLACKLIST":
		return white && (black == rule.EnableReverseBlacklist), nil
	case "BLACKLIST_THEN_WHITELIST":
		return !black && (white != rule.EnableReverseWhitelist), nil
	default:
		return false, fmt.Errorf("未知转发模式 %q", rule.ForwardMode)
	}
}

func ReplaceText(rule model.Rule, text string) (string, error) {
	if !rule.IsReplace || text == "" {
		return text, nil
	}
	for _, replacement := range rule.Replacements {
		if replacement.Pattern == ".*" {
			return replacement.Content, nil
		}
		replaced, err := pyregex.Replace(replacement.Pattern, replacement.Content, text)
		if err != nil {
			// 与 Python 版一致，保留当前文本并继续后续替换。
			slog.Warn("跳过无效替换规则", "rule", rule.ID, "replacement", replacement.ID)
			continue
		}
		text = replaced
	}
	return text, nil
}

func FilterMedia(rule model.Rule, media []model.Media) (allowed []model.Media, notices []string, pass bool) {
	pass = true
	blocked := 0
	for _, item := range media {
		if rule.EnableMediaTypeFilter && (rule.MediaTypes[item.Kind] || (item.Kind != "photo" && rule.MediaTypes["document"])) {
			blocked++
			continue
		}
		if rule.EnableExtensionFilter && item.Filename != "" && item.Kind != "photo" {
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(item.Filename), "."))
			if ext == "" {
				ext = "无扩展名"
			}
			found := false
			for _, x := range rule.Extensions {
				if strings.EqualFold(x, ext) {
					found = true
					break
				}
			}
			if (rule.ExtensionFilterMode == "WHITELIST" && !found) || (rule.ExtensionFilterMode != "WHITELIST" && found) {
				blocked++
				continue
			}
		}
		if rule.EnableMediaSizeFilter && rule.MaxMediaSize > 0 && item.Size > rule.MaxMediaSize*1024*1024 {
			if rule.IsSendOverMediaSizeMessage {
				notices = append(notices, fmt.Sprintf("⚠️ 文件 %s 超过大小限制（%.2f MB / %d MB）", item.Filename, float64(item.Size)/1024/1024, rule.MaxMediaSize))
			} else {
				blocked++
			}
			continue
		}
		allowed = append(allowed, item)
	}
	if len(media) > 0 && blocked == len(media) && !rule.MediaAllowText {
		pass = false
	}
	return
}

func MessageLink(msg model.Message) string {
	if msg.Link != "" {
		return msg.Link
	}
	if msg.Chat.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", msg.Chat.Username, msg.ID)
	}
	return fmt.Sprintf("https://t.me/c/%d/%d", msg.Chat.ID, msg.ID)
}

func AddInfo(rule model.Rule, msg model.Message, text string, zone *time.Location) string {
	if rule.IsOriginalSender {
		text = senderInfo(rule, msg) + "\n\n" + text
	}
	if rule.IsOriginalTime {
		template := rule.TimeTemplate
		if template == "" {
			template = "{time}"
		}
		text += "\n\n" + strings.ReplaceAll(template, "{time}", msg.Date.In(zone).Format("2006-01-02 15:04:05"))
	}
	if rule.IsOriginalLink {
		template := rule.OriginalLinkTemplate
		if template == "" {
			template = "原始消息: {original_link}"
		}
		text += "\n\n" + strings.ReplaceAll(template, "{original_link}", MessageLink(msg))
	}
	return text
}

func senderInfo(rule model.Rule, msg model.Message) string {
	template := rule.UserinfoTemplate
	if template == "" {
		template = "{name}"
	}
	sender := msg.Sender
	if sender.ID == 0 {
		sender = msg.Chat
	}
	if sender.Name == "" {
		sender.Name = "Unknown Sender"
	}
	return strings.NewReplacer("{name}", sender.Name, "{id}", fmt.Sprint(sender.ID)).Replace(template)
}

// MatchChat 同时识别旧表里的原始 ID 和 Telethon 带 -100 前缀的频道 ID。
func MatchChat(stored string, peer model.Peer) bool {
	if stored == fmt.Sprint(peer.ID) {
		return true
	}
	if peer.Kind == "channel" {
		return stored == fmt.Sprintf("-100%d", peer.ID) || stored == fmt.Sprintf("100%d", peer.ID)
	}
	return peer.Kind == "chat" && stored == fmt.Sprint(-peer.ID)
}

// SplitText 按 UTF-16 单元限制 Telegram 长度，避免拆开 Emoji。
func SplitText(text string, limit int) []string {
	if limit < 2 {
		limit = 2
	}
	var parts []string
	for text != "" {
		units, end, newline := 0, 0, -1
		for index, r := range text {
			n := 1
			if r > 0xffff {
				n = 2
			}
			if units+n > limit {
				break
			}
			units += n
			end = index + len(string(r))
			if r == '\n' {
				newline = end
			}
		}
		if end < len(text) && newline > 0 {
			end = newline
		}
		parts = append(parts, text[:end])
		text = text[end:]
	}
	return parts
}
