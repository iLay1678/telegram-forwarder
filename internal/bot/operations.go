package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
	"github.com/jmoiron/sqlx"
)

func (b *Bot) deleteRule(ctx context.Context, id int64) error {
	if err := b.Store.DeleteRule(ctx, id); err != nil {
		return err
	}
	if _, err := b.Store.DB.ExecContext(ctx, "DELETE FROM chats WHERE NOT EXISTS(SELECT 1 FROM forward_rules WHERE source_chat_id=chats.id OR target_chat_id=chats.id)"); err != nil {
		return err
	}
	if b.CleanupRule != nil {
		if err := b.CleanupRule(ctx, id); err != nil {
			slog.Warn("规则已删除，但 RSS 文件清理失败", "rule", id, "error", err)
		}
	}
	return nil
}

func (b *Bot) mutate(ctx context.Context, rule model.Rule, fn func(*sqlx.Tx, int64) error) error {
	tx, err := b.Store.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids := []int64{rule.ID}
	if rule.EnableSync {
		var extra []int64
		if err = tx.SelectContext(ctx, &extra, "SELECT DISTINCT sync_rule_id FROM rule_syncs JOIN forward_rules f ON f.id=sync_rule_id WHERE rule_id=? AND sync_rule_id<>?", rule.ID, rule.ID); err != nil {
			return err
		}
		ids = append(ids, extra...)
	}
	for _, id := range ids {
		if err = fn(tx, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if b.Sync != nil {
		for _, id := range ids {
			if err = b.Sync(ctx, id); err != nil {
				slog.Warn("UFB 同步失败，数据库操作已保存", "rule", id, "error", err)
			}
		}
	}
	return nil
}

func (b *Bot) copyRule(ctx context.Context, source, target model.Rule, kind string) error {
	if source.ID == target.ID {
		return errors.New("不能复制规则到自身")
	}
	return b.mutate(ctx, target, func(tx *sqlx.Tx, id int64) error {
		for _, k := range source.Keywords {
			if kind != "copy_rule" && kind != "copy_all_keywords" && (kind == "copy_replace" || k.IsRegex != (kind == "copy_keywords_regex")) {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(?,?,?,?)", id, k.Keyword, k.IsRegex, k.IsBlacklist); err != nil {
				return err
			}
		}
		if kind == "copy_replace" || kind == "copy_rule" {
			for _, r := range source.Replacements {
				if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO replace_rules(rule_id,pattern,content) VALUES(?,?,?)", id, r.Pattern, r.Content); err != nil {
					return err
				}
			}
		}
		if kind != "copy_rule" {
			return nil
		}
		value := reflect.ValueOf(source)
		typ := value.Type()
		for i := 0; i < value.NumField(); i++ {
			field := typ.Field(i).Tag.Get("db")
			if field == "" || field == "-" || field == "id" || field == "source_chat_id" || field == "target_chat_id" {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE forward_rules SET "+field+"=? WHERE id=?", value.Field(i).Interface(), id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_types(rule_id,photo,document,video,audio,voice) VALUES(?,0,0,0,0,0)", id); err != nil {
			return err
		}
		for _, field := range []string{"photo", "document", "video", "audio", "voice"} {
			if _, err := tx.ExecContext(ctx, "UPDATE media_types SET "+field+"=? WHERE rule_id=?", source.MediaTypes[field], id); err != nil {
				return err
			}
		}
		for _, ext := range source.Extensions {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_extensions(rule_id,extension) VALUES(?,?)", id, ext); err != nil {
				return err
			}
		}
		for _, p := range source.PushConfigs {
			if _, err := tx.ExecContext(ctx, "INSERT INTO push_configs(rule_id,enable_push_channel,push_channel,media_send_mode) SELECT ?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM push_configs WHERE rule_id=? AND push_channel=?)", id, p.EnablePushChannel, p.PushChannel, p.MediaSendMode, id, p.PushChannel); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO rule_syncs(rule_id,sync_rule_id) SELECT ?,s.sync_rule_id FROM rule_syncs s WHERE s.rule_id=? AND s.sync_rule_id<>? AND NOT EXISTS(SELECT 1 FROM rule_syncs t WHERE t.rule_id=? AND t.sync_rule_id=s.sync_rule_id)", id, source.ID, id, id); err != nil {
			return err
		}
		if source.RSS != nil {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO rss_configs(rule_id) VALUES(?)", id); err != nil {
				return err
			}
			v := reflect.ValueOf(*source.RSS)
			for i := 0; i < v.NumField(); i++ {
				field := v.Type().Field(i).Tag.Get("db")
				if field == "id" || field == "rule_id" {
					continue
				}
				if _, err := tx.ExecContext(ctx, "UPDATE rss_configs SET "+field+"=? WHERE rule_id=?", v.Field(i).Interface(), id); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO rss_patterns(rss_config_id,pattern,pattern_type,priority) SELECT t.id,p.pattern,p.pattern_type,p.priority FROM rss_patterns p JOIN rss_configs t ON t.rule_id=? WHERE p.rss_config_id=?", id, source.RSS.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (b *Bot) export(ctx context.Context, msg model.Message, rule model.Rule, command string) error {
	dir, err := os.MkdirTemp(b.Config.Path("temp"), "export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	files := map[string][]string{}
	if command == "export_replace" {
		for _, r := range rule.Replacements {
			files["replace_rules.txt"] = append(files["replace_rules.txt"], r.Pattern+"\t"+r.Content)
		}
	} else {
		for _, k := range rule.Keywords {
			name := "keywords.txt"
			if k.IsRegex {
				name = "regex_keywords.txt"
			}
			flag := "0"
			if k.IsBlacklist {
				flag = "1"
			}
			files[name] = append(files[name], k.Keyword+" "+flag)
		}
	}
	if len(files) == 0 {
		return errors.New("当前规则没有可导出的数据")
	}
	for _, name := range []string{"keywords.txt", "regex_keywords.txt", "replace_rules.txt"} {
		lines := files[name]
		if len(lines) == 0 {
			continue
		}
		path := filepath.Join(dir, name)
		if err = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
			return err
		}
		if _, err = b.Client.Send(ctx, msg.Chat, model.Outgoing{Media: []model.Media{{Path: path, Filename: name, Kind: "document", MIME: "text/plain"}}}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) importFile(ctx context.Context, msg model.Message, rule model.Rule, command string) error {
	if len(msg.Media) == 0 {
		return errors.New("请在发送命令时附带 TXT 文件")
	}
	if msg.Media[0].Size > 16*1024*1024 {
		return errors.New("导入文件不能超过 16 MB")
	}
	dir, err := os.MkdirTemp(b.Config.Path("temp"), "import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path, err := b.Client.Download(ctx, msg.Media[0], filepath.Join(dir, "rules.txt"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimPrefix(string(data), "\ufeff"), "\r\n", "\n"), "\n")
	var keywords []model.Keyword
	var replacements []model.Replacement
	for n, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if command == "import_replace" {
			pattern, content, _ := strings.Cut(line, "\t")
			if _, err = pyregex.Search(pattern, ""); err != nil {
				return fmt.Errorf("第 %d 行: %w", n+1, err)
			}
			replacements = append(replacements, model.Replacement{Pattern: pattern, Content: content})
		} else {
			index := strings.LastIndexAny(line, " \t")
			if index < 1 {
				return fmt.Errorf("第 %d 行缺少 0/1 黑白名单标记", n+1)
			}
			text, flag := strings.TrimSpace(line[:index]), strings.TrimSpace(line[index:])
			if flag != "0" && flag != "1" {
				return fmt.Errorf("第 %d 行标记必须为 0/1", n+1)
			}
			regex := command == "import_regex_keyword"
			if regex {
				if _, err = pyregex.Search(text, ""); err != nil {
					return fmt.Errorf("第 %d 行: %w", n+1, err)
				}
			}
			keywords = append(keywords, model.Keyword{Keyword: text, IsRegex: regex, IsBlacklist: flag == "1"})
		}
	}
	err = b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
		for _, k := range keywords {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(?,?,?,?)", id, k.Keyword, k.IsRegex, k.IsBlacklist); err != nil {
				return err
			}
		}
		for _, r := range replacements {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO replace_rules(rule_id,pattern,content) VALUES(?,?,?)", id, r.Pattern, r.Content); err != nil {
				return err
			}
		}
		if len(replacements) > 0 {
			_, err := tx.ExecContext(ctx, "UPDATE forward_rules SET is_replace=1 WHERE id=?", id)
			return err
		}
		return nil
	})
	if err == nil {
		b.reply(ctx, msg, fmt.Sprintf("已处理 %d 条关键词、%d 条替换规则，重复项自动跳过", len(keywords), len(replacements)), nil)
	}
	return err
}

func (b *Bot) forwardLink(ctx context.Context, msg model.Message) error {
	u, err := url.Parse(strings.TrimSpace(msg.Text))
	if err != nil {
		return err
	}
	if u.Host != "t.me" {
		return errors.New("不是 Telegram 消息链接")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return errors.New("消息链接缺少消息 ID")
	}
	var source string
	var messageID string
	if parts[0] == "c" {
		if len(parts) != 3 {
			return errors.New("私有频道消息链接格式错误")
		}
		source = "-100" + parts[1]
		messageID = parts[2]
	} else {
		source = "@" + parts[0]
		messageID = parts[1]
	}
	id, err := strconv.Atoi(messageID)
	if err != nil || id < 1 {
		return errors.New("消息 ID 无效")
	}
	peer, err := b.User.Resolve(ctx, source)
	if err != nil {
		return err
	}
	messages, err := b.User.Fetch(ctx, peer, []int{id})
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return errors.New("消息不存在或无权访问")
	}
	original := messages[0]
	if original.GroupedID != 0 {
		var ids []int
		for i := max(1, id-10); i <= id+10; i++ {
			ids = append(ids, i)
		}
		group, e := b.User.Fetch(ctx, peer, ids)
		if e != nil {
			return e
		}
		original.Media = nil
		for _, m := range group {
			if m.GroupedID == original.GroupedID {
				original.Media = append(original.Media, m.Media...)
				if m.Text != "" {
					original.Text = m.Text
				}
			}
		}
	}
	_, err = b.Client.Send(ctx, msg.Chat, model.Outgoing{Text: original.Text, Mode: "MARKDOWN", Preview: true, Media: original.Media, Buttons: original.Buttons})
	return err
}
