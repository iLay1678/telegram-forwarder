package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
	"github.com/jmoiron/sqlx"
)

const helpText = `Telegram Forwarder · Go
/bind(/b) <源聊天链接或名称> [目标聊天] — 创建规则
/settings(/s) [规则ID] — 设置所有规则选项
/switch — 选择当前规则
/list_rule(/lr) · /delete_rule(/dr) <规则ID...>
/add(/a) · /add_regex(/ar) <关键词...>
/add_all(/aa) · /add_regex_all(/ara) <关键词...>
/list_keyword(/lk) · /remove_keyword(/rk) <关键词...>
/remove_keyword_by_id(/rkbi) <序号...>
/remove_all_keyword(/rak) <关键词...>
/replace(/r) · /replace_all(/ra) <正则> [替换内容]
/list_replace(/lrp) · /remove_replace(/rr) <序号...>
/clear_all_keywords(/cak) · /clear_all_keywords_regex(/cakr)
/clear_all_replace(/car)
/copy_keywords(/ck) · /copy_keywords_regex(/ckr) · /copy_replace(/crp) <源规则ID>
/copy_rule(/cr) <源规则ID> [目标规则ID]
/export_keyword(/ek) · /export_replace(/er)
/import_keyword(/ik) · /import_regex_keyword(/irk) · /import_replace(/ir) — 同时发送文件
/ufb_bind(/ub) <域名> [类型] · /ufb_unbind(/uu) · /ufb_item_change(/uic)
/delete_rss_user(/dru) [用户名]
/summary [规则ID] — 立即总结
/cancel — 取消当前输入
/changelog(/cl)
也可发送 Telegram 消息链接转发到当前聊天。`

func (b *Bot) command(ctx context.Context, msg model.Message, command, args string) error {
	switch command {
	case "start", "help":
		b.reply(ctx, msg, helpText, nil)
		return nil
	case "changelog":
		b.reply(ctx, msg, "Go 版本：兼容原有数据库、配置与 Telethon 会话。", nil)
		return nil
	case "cancel":
		b.reply(ctx, msg, "当前没有等待输入的设置", nil)
		return nil
	case "bind":
		parts, err := splitQuoted(args)
		if err != nil {
			return err
		}
		if len(parts) < 1 || len(parts) > 2 {
			return errors.New("用法：/bind <源聊天> [目标聊天]")
		}
		source, err := b.resolve(ctx, parts[0])
		if err != nil {
			return err
		}
		target := msg.Chat
		if len(parts) == 2 {
			target, err = b.resolve(ctx, parts[1])
			if err != nil {
				return err
			}
		}
		rule, err := b.Store.Bind(ctx, source, target)
		if err != nil {
			return err
		}
		b.reply(ctx, msg, fmt.Sprintf("已创建规则 %d：%s → %s", rule.ID, source.Name, target.Name), [][]model.Button{{{Text: "⚙️ 设置", Data: fmt.Sprintf("rule_settings:%d", rule.ID)}}})
		return nil
	case "settings", "switch":
		if args != "" {
			id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
			if err != nil {
				return err
			}
			return b.showSettings(ctx, msg, id, 0)
		}
		return b.showRules(ctx, msg, command == "switch")
	case "list_rule":
		all, err := b.Store.Rules(ctx)
		if err != nil {
			return err
		}
		page := 1
		if strings.TrimSpace(args) != "" {
			page, err = strconv.Atoi(strings.TrimSpace(args))
			if err != nil {
				return errors.New("页码必须是整数")
			}
		}
		pages := max(1, (len(all)+29)/30)
		page = max(1, min(pages, page))
		var lines []string
		for _, r := range all[(page-1)*30 : min(page*30, len(all))] {
			lines = append(lines, fmt.Sprintf("%d. %s → %s（启用：%t）", r.ID, r.Source.Name, r.Target.Name, r.EnableRule))
		}
		if len(lines) == 0 {
			lines = append(lines, "没有转发规则")
		}
		b.reply(ctx, msg, fmt.Sprintf("转发规则（%d/%d）：\n", page, pages)+strings.Join(lines, "\n"), pageButtons(page, pages, "page_rule"))
		return nil
	case "delete_rule":
		ids, err := parseIDs(args)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err = b.deleteRule(ctx, id); err != nil {
				return err
			}
		}
		b.reply(ctx, msg, "已删除指定规则", nil)
		return nil
	case "delete_rss_user":
		username := strings.TrimSpace(args)
		if username == "" {
			var users []string
			if err := b.Store.DB.SelectContext(ctx, &users, "SELECT username FROM users ORDER BY id"); err != nil {
				return err
			}
			if len(users) == 0 {
				return errors.New("RSS 系统没有用户账户")
			}
			if len(users) > 1 {
				b.reply(ctx, msg, "请使用 /delete_rss_user <用户名> 指定用户：\n"+strings.Join(users, "\n"), nil)
				return nil
			}
			username = users[0]
		}
		result, err := b.Store.DB.ExecContext(ctx, "DELETE FROM users WHERE username=?", username)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return errors.New("未找到指定 RSS 用户")
		}
		b.reply(ctx, msg, "RSS 用户已删除", nil)
		return nil
	case "clear_all":
		all, err := b.Store.Rules(ctx)
		if err != nil {
			return err
		}
		for _, r := range all {
			if err = b.deleteRule(ctx, r.ID); err != nil {
				return err
			}
		}
		_, err = b.Store.DB.ExecContext(ctx, "DELETE FROM chats")
		if err != nil {
			return err
		}
		b.reply(ctx, msg, "已清空转发规则、关键词与聊天数据", nil)
		return nil
	}
	if command == "summary" && strings.TrimSpace(args) != "" {
		if b.Summary == nil {
			return errors.New("总结服务未启动")
		}
		ids, err := parseIDs(args)
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			return errors.New("用法：/summary [规则ID]")
		}
		if err = b.Summary(ctx, ids[0]); err != nil {
			return err
		}
		b.reply(ctx, msg, "总结完成", nil)
		return nil
	}
	var rule model.Rule
	var err error
	if command == "copy_rule" && len(strings.Fields(args)) == 2 {
		ids, e := parseIDs(args)
		if e != nil {
			return e
		}
		rule, err = b.Store.Rule(ctx, ids[1])
	} else {
		rule, err = b.current(ctx, msg.Chat)
	}
	if err != nil {
		return err
	}
	selected := []model.Rule{rule}
	if command == "add_all" || command == "add_regex_all" || command == "replace_all" || command == "remove_all_keyword" {
		selected, err = b.targetRules(ctx, msg.Chat)
		if err != nil {
			return err
		}
	}
	switch command {
	case "add", "add_all", "add_regex", "add_regex_all":
		isRegex := strings.Contains(command, "regex")
		var words []string
		if isRegex {
			words = strings.Fields(args)
		} else {
			words, err = splitQuoted(args)
			if err != nil {
				return err
			}
		}
		if len(words) == 0 {
			return errors.New("请提供关键词")
		}
		for _, word := range words {
			if isRegex {
				if _, err = pyregex.Search(word, ""); err != nil {
					return err
				}
			}
		}
		for _, r := range selected {
			err = b.mutate(ctx, r, func(tx *sqlx.Tx, id int64) error {
				for _, word := range words {
					if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(?,?,?,?)", id, word, isRegex, r.AddMode != "WHITELIST"); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	case "replace", "replace_all":
		pattern, content := cutWord(args)
		if pattern == "" {
			return errors.New("用法：/replace <正则> [替换内容]")
		}
		if _, err = pyregex.Search(pattern, ""); err != nil {
			return err
		}
		for _, r := range selected {
			err = b.mutate(ctx, r, func(tx *sqlx.Tx, id int64) error {
				if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO replace_rules(rule_id,pattern,content) VALUES(?,?,?)", id, pattern, content); e != nil {
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE forward_rules SET is_replace=1 WHERE id=?", id)
				return e
			})
			if err != nil {
				return err
			}
		}
	case "list_keyword":
		var lines []string
		index := 0
		for _, k := range rule.Keywords {
			if k.IsBlacklist != (rule.AddMode != "WHITELIST") {
				continue
			}
			index++
			suffix := ""
			if k.IsRegex {
				suffix = "（正则）"
			}
			lines = append(lines, fmt.Sprintf("%d. %s %s", index, k.Keyword, suffix))
		}
		return b.replyList(ctx, msg, "keyword", "关键词（"+rule.AddMode+"）：", lines, args)
	case "list_replace":
		var lines []string
		for i, r := range rule.Replacements {
			lines = append(lines, fmt.Sprintf("%d. %s → %s", i+1, r.Pattern, r.Content))
		}
		return b.replyList(ctx, msg, "replace", "替换规则：", lines, args)
	case "remove_keyword", "remove_all_keyword":
		words, e := splitQuoted(args)
		if e != nil {
			return e
		}
		if len(words) == 0 {
			return errors.New("请提供要删除的关键词")
		}
		for _, r := range selected {
			err = b.mutate(ctx, r, func(tx *sqlx.Tx, id int64) error {
				for _, word := range words {
					if _, e := tx.ExecContext(ctx, "DELETE FROM keywords WHERE rule_id=? AND keyword=? AND is_blacklist=?", id, word, r.AddMode != "WHITELIST"); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	case "remove_keyword_by_id":
		ids, e := parseIDs(args)
		if e != nil {
			return e
		}
		var keys []model.Keyword
		for _, k := range rule.Keywords {
			if k.IsBlacklist == (rule.AddMode != "WHITELIST") {
				keys = append(keys, k)
			}
		}
		for _, id := range ids {
			if id < 1 || id > int64(len(keys)) {
				return errors.New("关键词序号超出范围")
			}
		}
		err = b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			for _, n := range ids {
				k := keys[n-1]
				if _, e := tx.ExecContext(ctx, "DELETE FROM keywords WHERE rule_id=? AND keyword=? AND is_regex=? AND is_blacklist=?", id, k.Keyword, k.IsRegex, k.IsBlacklist); e != nil {
					return e
				}
			}
			return nil
		})
	case "remove_replace":
		ids, e := parseIDs(args)
		if e != nil {
			return e
		}
		for _, id := range ids {
			if id < 1 || id > int64(len(rule.Replacements)) {
				return errors.New("替换规则序号超出范围")
			}
		}
		err = b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			for _, n := range ids {
				r := rule.Replacements[n-1]
				if _, e := tx.ExecContext(ctx, "DELETE FROM replace_rules WHERE rule_id=? AND pattern=? AND COALESCE(content,'')=?", id, r.Pattern, r.Content); e != nil {
					return e
				}
			}
			return nil
		})
	case "clear_all_keywords", "clear_all_keywords_regex", "clear_all_replace":
		err = b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			if command == "clear_all_replace" {
				if _, e := tx.ExecContext(ctx, "DELETE FROM replace_rules WHERE rule_id=?", id); e != nil {
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE forward_rules SET is_replace=0 WHERE id=?", id)
				return e
			}
			query := "DELETE FROM keywords WHERE rule_id=?"
			if command == "clear_all_keywords_regex" {
				query += " AND is_regex=1"
			}
			_, e := tx.ExecContext(ctx, query, id)
			return e
		})
	case "copy_keywords", "copy_keywords_regex", "copy_replace", "copy_rule":
		ids, e := parseIDs(args)
		if e != nil {
			return e
		}
		if (command == "copy_rule" && len(ids) > 2) || (command != "copy_rule" && len(ids) != 1) {
			return errors.New("复制命令的规则 ID 数量不正确")
		}
		source, e := b.Store.Rule(ctx, ids[0])
		if e != nil {
			return e
		}
		target := rule
		if command == "copy_rule" && len(ids) > 1 {
			target, e = b.Store.Rule(ctx, ids[1])
			if e != nil {
				return e
			}
		}
		err = b.copyRule(ctx, source, target, command)
	case "export_keyword", "export_replace":
		return b.export(ctx, msg, rule, command)
	case "import_keyword", "import_regex_keyword", "import_replace":
		return b.importFile(ctx, msg, rule, command)
	case "ufb_bind":
		parts := strings.Fields(strings.ToLower(args))
		if len(parts) < 1 || len(parts) > 2 {
			return errors.New("用法：/ufb_bind <域名> [main|content|main_username|content_username]")
		}
		item := "main"
		if len(parts) == 2 {
			item = parts[1]
		}
		if !validUFBItem(item) {
			return errors.New("UFB 类型无效")
		}
		_, err = b.Store.DB.ExecContext(ctx, "UPDATE forward_rules SET ufb_domain=?,ufb_item=? WHERE id=?", parts[0], item, rule.ID)
	case "ufb_unbind":
		err = b.Store.UpdateRule(ctx, rule.ID, "ufb_domain", "")
	case "ufb_item_change":
		var buttons [][]model.Button
		for _, item := range enums["ufb_item"] {
			buttons = append(buttons, []model.Button{{Text: item, Data: "ufb_item:" + item}})
		}
		b.reply(ctx, msg, "请选择 UFB 同步配置类型：", buttons)
		return nil
	case "summary":
		if b.Summary == nil {
			return errors.New("总结服务未启动")
		}
		id := rule.ID
		if args != "" {
			id, err = strconv.ParseInt(strings.TrimSpace(args), 10, 64)
			if err != nil {
				return err
			}
		}
		err = b.Summary(ctx, id)
	default:
		return fmt.Errorf("未知命令 /%s，请使用 /help", command)
	}
	if err != nil {
		return err
	}
	b.reply(ctx, msg, "操作完成", nil)
	return nil
}

func validUFBItem(value string) bool {
	for _, item := range enums["ufb_item"] {
		if value == item {
			return true
		}
	}
	return false
}

func pageButtons(page, pages int, action string) [][]model.Button {
	if pages <= 1 {
		return nil
	}
	data := func(p int) string {
		if p < 1 || p > pages {
			return "noop"
		}
		return fmt.Sprintf("%s:%d", action, p)
	}
	return [][]model.Button{{{Text: "上一页", Data: data(page - 1)}, {Text: fmt.Sprintf("%d/%d", page, pages), Data: "noop"}, {Text: "下一页", Data: data(page + 1)}}}
}

func (b *Bot) replyList(ctx context.Context, msg model.Message, kind, title string, lines []string, args string) error {
	page := 1
	if args != "" {
		var err error
		page, err = strconv.Atoi(strings.TrimSpace(args))
		if err != nil {
			return errors.New("页码必须是整数")
		}
	}
	size := max(1, b.Config.Int("KEYWORDS_PER_PAGE", 50))
	pages := max(1, (len(lines)+size-1)/size)
	page = max(1, min(page, pages))
	buttons := pageButtons(page, pages, "page")
	for i := range buttons {
		for j := range buttons[i] {
			if buttons[i][j].Data != "noop" {
				buttons[i][j].Data += ":" + kind
			}
		}
	}
	b.reply(ctx, msg, title+"\n"+strings.Join(lines[(page-1)*size:min(page*size, len(lines))], "\n"), buttons)
	return nil
}

func (b *Bot) resolve(ctx context.Context, input string) (model.Peer, error) {
	peer, err := b.User.Resolve(ctx, input)
	if err == nil {
		return peer, nil
	}
	dialogs, dialogErr := b.User.Dialogs(ctx)
	if dialogErr != nil {
		return model.Peer{}, err
	}
	for _, p := range dialogs {
		if strings.Contains(strings.ToLower(p.Name), strings.ToLower(input)) {
			return p, nil
		}
	}
	return model.Peer{}, fmt.Errorf("无法找到聊天 %q：%w", input, err)
}

func parseIDs(text string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Fields(text) {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id < 1 {
			return nil, errors.New("ID 必须为正整数")
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("请提供规则 ID 或列表序号")
	}
	return ids, nil
}
