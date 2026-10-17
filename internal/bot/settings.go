package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/jmoiron/sqlx"
)

var fieldLabels = map[string]string{
	"enable_rule": "启用规则", "use_bot": "机器人发送", "forward_mode": "转发模式", "add_mode": "关键词添加模式", "message_mode": "消息格式", "is_replace": "启用替换", "is_preview": "链接预览", "is_original_link": "附带原始链接", "is_original_sender": "附带发送人", "is_original_time": "附带时间", "is_delete_original": "删除原消息", "is_filter_user_info": "过滤发送人信息", "handle_mode": "处理模式", "enable_comment_button": "评论按钮", "enable_reverse_blacklist": "反转黑名单", "enable_reverse_whitelist": "反转白名单", "enable_delay": "延迟处理", "delay_seconds": "延迟秒数", "enable_media_type_filter": "媒体类型过滤", "enable_media_size_filter": "媒体大小过滤", "max_media_size": "最大媒体 MB", "is_send_over_media_size_message": "超限提示", "enable_extension_filter": "扩展名过滤", "extension_filter_mode": "扩展名过滤模式", "media_allow_text": "屏蔽媒体时保留文本", "is_ai": "AI 处理", "ai_model": "AI 模型", "ai_prompt": "AI 提示词", "enable_ai_upload_image": "上传图片到 AI", "is_keyword_after_ai": "AI 后关键词过滤", "is_summary": "定时总结", "summary_time": "总结时间", "summary_prompt": "总结提示词", "is_top_summary": "置顶总结", "enable_push": "启用推送", "enable_only_push": "仅外部推送", "only_rss": "仅 RSS", "enable_sync": "同步规则", "is_ufb": "UFB 同步", "ufb_domain": "UFB 域名", "ufb_item": "UFB 配置类型", "userinfo_template": "发送人模板", "time_template": "时间模板", "original_link_template": "链接模板",
}
var enums = map[string][]string{
	"forward_mode": {"BLACKLIST", "WHITELIST", "BLACKLIST_THEN_WHITELIST", "WHITELIST_THEN_BLACKLIST"}, "add_mode": {"BLACKLIST", "WHITELIST"}, "message_mode": {"MARKDOWN", "HTML"}, "is_preview": {"FOLLOW", "ON", "OFF"}, "handle_mode": {"FORWARD", "EDIT"}, "extension_filter_mode": {"BLACKLIST", "WHITELIST"}, "ufb_item": {"main", "content", "main_username", "content_username"},
}
var settingFields = []string{"enable_rule", "use_bot", "forward_mode", "add_mode", "message_mode", "is_replace", "is_preview", "handle_mode", "enable_delay", "delay_seconds", "is_original_link", "is_original_sender", "is_original_time", "is_filter_user_info", "is_delete_original", "enable_comment_button", "enable_reverse_blacklist", "enable_reverse_whitelist", "userinfo_template", "time_template", "original_link_template", "enable_media_type_filter", "enable_media_size_filter", "max_media_size", "is_send_over_media_size_message", "enable_extension_filter", "extension_filter_mode", "media_allow_text", "is_ai", "ai_model", "ai_prompt", "enable_ai_upload_image", "is_keyword_after_ai", "is_summary", "summary_time", "summary_prompt", "is_top_summary", "enable_push", "enable_only_push", "only_rss", "enable_sync", "is_ufb", "ufb_domain", "ufb_item"}

func fieldValue(rule model.Rule, name string) (reflect.Value, bool) {
	v := reflect.ValueOf(rule)
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if t.Field(i).Tag.Get("db") == name && fieldLabels[name] != "" {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func (b *Bot) showRules(ctx context.Context, msg model.Message, selectCurrent bool) error {
	all, err := b.targetRules(ctx, msg.Chat)
	if err != nil {
		return err
	}
	var buttons [][]model.Button
	for _, rule := range all {
		action := "rule_settings"
		if selectCurrent {
			action = "apply"
		}
		label := fmt.Sprintf("%d · %s", rule.ID, rule.Source.Name)
		if rule.Target.CurrentAddID == rule.Source.TelegramChatID {
			label = "✓ " + label
		}
		buttons = append(buttons, []model.Button{{Text: label, Data: fmt.Sprintf("%s:%d", action, rule.ID)}})
	}
	if len(buttons) == 0 {
		return errors.New("请先使用 /bind 创建规则")
	}
	b.reply(ctx, msg, "请选择转发规则：", buttons)
	return nil
}

func (b *Bot) settings(rule model.Rule, page int) model.Outgoing {
	const size = 8
	pages := (len(settingFields) + size - 1) / size
	if page < 0 || page >= pages {
		page = 0
	}
	var buttons [][]model.Button
	for _, name := range settingFields[page*size : min((page+1)*size, len(settingFields))] {
		v, _ := fieldValue(rule, name)
		value := fmt.Sprint(v.Interface())
		if v.Kind() == reflect.Bool {
			value = "关闭"
			if v.Bool() {
				value = "开启"
			}
		}
		if len([]rune(value)) > 30 {
			value = string([]rune(value)[:30]) + "…"
		}
		buttons = append(buttons, []model.Button{{Text: fieldLabels[name] + "：" + value, Data: fmt.Sprintf("field:%d:%s", rule.ID, name)}})
	}
	buttons = append(buttons, []model.Button{{Text: "上一页", Data: fmt.Sprintf("menu:%d:%d", rule.ID, (page+pages-1)%pages)}, {Text: fmt.Sprintf("%d / %d", page+1, pages), Data: "noop"}, {Text: "下一页", Data: fmt.Sprintf("menu:%d:%d", rule.ID, (page+1)%pages)}})
	buttons = append(buttons, []model.Button{{Text: "媒体类型", Data: fmt.Sprintf("media:%d", rule.ID)}, {Text: "扩展名", Data: fmt.Sprintf("extensions:%d", rule.ID)}, {Text: "推送地址", Data: fmt.Sprintf("push:%d", rule.ID)}})
	buttons = append(buttons, []model.Button{{Text: "同步目标", Data: fmt.Sprintf("sync:%d", rule.ID)}, {Text: "RSS", Data: fmt.Sprintf("rss:%d", rule.ID)}, {Text: "立即总结", Data: fmt.Sprintf("summary:%d", rule.ID)}})
	buttons = append(buttons, []model.Button{{Text: "应用当前规则", Data: fmt.Sprintf("apply:%d", rule.ID)}, {Text: "删除规则", Data: fmt.Sprintf("confirm_delete:%d", rule.ID)}})
	buttons = append(buttons, []model.Button{{Text: "复制规则", Data: fmt.Sprintf("copy_rule:%d", rule.ID)}, {Text: "复制关键词", Data: fmt.Sprintf("copy_keyword:%d", rule.ID)}, {Text: "复制替换", Data: fmt.Sprintf("copy_replace:%d", rule.ID)}})
	buttons = append(buttons, []model.Button{{Text: "清空关键词", Data: fmt.Sprintf("clear_keyword:%d", rule.ID)}, {Text: "清空替换", Data: fmt.Sprintf("clear_replace:%d", rule.ID)}, {Text: "关闭", Data: "close_settings"}})
	return model.Outgoing{Text: fmt.Sprintf("规则 %d：%s → %s\n当前添加模式：%s", rule.ID, rule.Source.Name, rule.Target.Name, rule.AddMode), Mode: "TEXT", Buttons: buttons}
}

func (b *Bot) showSettings(ctx context.Context, msg model.Message, id int64, page int) error {
	rule, err := b.Store.Rule(ctx, id)
	if err != nil {
		return err
	}
	out := b.settings(rule, page)
	b.reply(ctx, msg, out.Text, out.Buttons)
	return nil
}

func (b *Bot) HandleCallback(ctx context.Context, cb model.Callback) {
	if !b.isAdmin(cb.SenderID) {
		_ = b.Client.AnswerCallback(ctx, cb.ID, "无操作权限", true)
		return
	}
	err := b.callback(ctx, cb)
	text := ""
	if err != nil {
		text = "操作失败：" + err.Error()
	}
	if len([]rune(text)) > 180 {
		text = string([]rune(text)[:180])
	}
	_ = b.Client.AnswerCallback(ctx, cb.ID, text, err != nil)
}

func (b *Bot) callback(ctx context.Context, cb model.Callback) error {
	parts := strings.Split(cb.Data, ":")
	action := parts[0]
	if action == "noop" || action == "null" {
		return nil
	}
	msg := model.Message{ID: cb.MessageID, Chat: cb.Chat, Sender: model.Peer{ID: cb.SenderID}}
	b.clearState(msg)
	if action == "close_settings" {
		return b.Client.Delete(ctx, cb.Chat, []int{cb.MessageID})
	}
	if action == "cancel" || strings.HasPrefix(action, "cancel_") {
		return nil
	}
	if action == "settings" || action == "back_to_settings" {
		return b.showRules(ctx, msg, false)
	}
	if action == "page_rule" && len(parts) > 1 {
		return b.command(ctx, msg, "list_rule", parts[1])
	}
	if action == "page" && len(parts) >= 3 {
		command := map[string]string{"keyword": "list_keyword", "replace": "list_replace"}[parts[2]]
		if command == "" {
			return errors.New("列表类型无效")
		}
		return b.command(ctx, msg, command, parts[1])
	}
	if action == "ufb_item" && len(parts) == 2 {
		if !validUFBItem(parts[1]) {
			return errors.New("UFB 类型无效")
		}
		rule, err := b.current(ctx, cb.Chat)
		if err != nil {
			return err
		}
		if err = b.Store.UpdateRule(ctx, rule.ID, "ufb_item", parts[1]); err != nil {
			return err
		}
		return b.Client.Edit(ctx, cb.Chat, cb.MessageID, model.Outgoing{Text: "已切换 UFB 类型为 " + parts[1], Mode: "TEXT"})
	}
	if len(parts) < 2 {
		return errors.New("按钮参数不完整")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return errors.New("规则 ID 无效")
	}
	if action == "switch" {
		all, e := b.targetRules(ctx, cb.Chat)
		if e != nil {
			return e
		}
		for _, r := range all {
			if r.Source.TelegramChatID == parts[1] {
				id = r.ID
				action = "apply"
				break
			}
		}
	}
	if alias, ok := legacyActions[action]; ok {
		action = alias
	}
	if action == "push_toggle" || action == "push_mode" || action == "push_delete" || action == "push_detail" {
		return b.pushCallback(ctx, cb, action, id)
	}
	rule, err := b.Store.Rule(ctx, id)
	if err != nil {
		return err
	}
	edit := func(out model.Outgoing) error { return b.Client.Edit(ctx, cb.Chat, cb.MessageID, out) }
	if handled, err := b.legacyCallback(ctx, cb, rule, action, parts); handled {
		return err
	}
	if strings.HasPrefix(action, "toggle_") || strings.HasPrefix(action, "set_") {
		field := strings.TrimPrefix(strings.TrimPrefix(action, "toggle_"), "set_")
		if fieldLabels[field] != "" {
			action = "field"
			parts = []string{"field", parts[1], field}
		}
	}
	switch action {
	case "rule_settings", "menu", "media_settings", "ai_settings", "other_settings", "push_settings":
		page := 0
		if len(parts) > 2 {
			page, _ = strconv.Atoi(parts[2])
		}
		if action == "ai_settings" {
			page = 3
		}
		if action == "media_settings" {
			page = 2
		}
		if action == "other_settings" {
			page = 4
		}
		if action == "push_settings" {
			page = 4
		}
		return edit(b.settings(rule, page))
	case "apply", "apply_rule":
		_, err = b.Store.DB.ExecContext(ctx, "UPDATE chats SET current_add_id=? WHERE id=?", rule.Source.TelegramChatID, rule.TargetChatID)
		if err != nil {
			return err
		}
		return edit(b.settings(rule, 0))
	case "field":
		if len(parts) < 3 {
			return errors.New("缺少设置字段")
		}
		field := parts[2]
		v, ok := fieldValue(rule, field)
		if !ok {
			return errors.New("未知设置字段")
		}
		if v.Kind() == reflect.Bool {
			err = b.Store.UpdateRule(ctx, id, field, !v.Bool())
		} else if values := enums[field]; len(values) > 0 {
			next := values[0]
			for i, value := range values {
				if value == v.String() {
					next = values[(i+1)%len(values)]
				}
			}
			err = b.Store.UpdateRule(ctx, id, field, next)
		} else {
			b.setState(msg, id, field)
			return edit(b.valueMenu(id, field, 0))
		}
		if err != nil {
			return err
		}
		rule, err = b.Store.Rule(ctx, id)
		if err != nil {
			return err
		}
		page := 0
		for i, f := range settingFields {
			if f == field {
				page = i / 8
				break
			}
		}
		return edit(b.settings(rule, page))
	case "pick":
		if len(parts) < 4 {
			return errors.New("选项参数不完整")
		}
		index, e := strconv.Atoi(parts[3])
		values := b.valuesFor(parts[2])
		if e != nil || index < 0 || index >= len(values) {
			return errors.New("选项已失效")
		}
		msg.Text = values[index]
		err = b.applyInput(ctx, inputState{RuleID: id, Field: parts[2]}, msg)
		if err != nil {
			return err
		}
		b.mu.Lock()
		delete(b.states, b.stateKey(msg))
		b.mu.Unlock()
		rule, err = b.Store.Rule(ctx, id)
		if err != nil {
			return err
		}
		return edit(b.settings(rule, 0))
	case "confirm_delete":
		return edit(model.Outgoing{Text: fmt.Sprintf("确认删除规则 %d 及其关键词、替换与配置？", id), Mode: "TEXT", Buttons: [][]model.Button{{{Text: "确认删除", Data: fmt.Sprintf("delete:%d", id)}, {Text: "返回", Data: fmt.Sprintf("rule_settings:%d", id)}}}})
	case "delete", "perform_delete_rule":
		if action == "perform_delete_rule" && len(parts) > 2 {
			id, err = strconv.ParseInt(parts[2], 10, 64)
			if err != nil || id < 1 {
				return errors.New("删除目标 ID 无效")
			}
		}
		if err = b.deleteRule(ctx, id); err != nil {
			return err
		}
		return edit(model.Outgoing{Text: "规则已删除", Mode: "TEXT"})
	case "media", "toggle_media_type":
		if len(parts) > 2 {
			field := parts[2]
			if field != "photo" && field != "document" && field != "video" && field != "audio" && field != "voice" {
				return errors.New("无效媒体类型")
			}
			err = b.mutate(ctx, rule, func(tx *sqlx.Tx, rid int64) error {
				if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_types(rule_id,photo,document,video,audio,voice) VALUES(?,0,0,0,0,0)", rid); e != nil {
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE media_types SET "+field+"=? WHERE rule_id=?", !rule.MediaTypes[field], rid)
				return e
			})
			if err != nil {
				return err
			}
			rule, err = b.Store.Rule(ctx, id)
			if err != nil {
				return err
			}
		}
		var buttons [][]model.Button
		for _, field := range []string{"photo", "document", "video", "audio", "voice"} {
			mark := "□ "
			if rule.MediaTypes[field] {
				mark = "☑ "
			}
			buttons = append(buttons, []model.Button{{Text: mark + field, Data: fmt.Sprintf("media:%d:%s", id, field)}})
		}
		return edit(model.Outgoing{Text: "勾选表示屏蔽该媒体类型，需同时开启媒体类型过滤。", Mode: "TEXT", Buttons: withBack(buttons, id)})
	case "extensions", "set_media_extensions":
		b.setState(msg, id, "extensions")
		return edit(model.Outgoing{Text: "当前扩展名：" + strings.Join(rule.Extensions, " ") + "\n发送完整扩展名列表（空格分隔，不含点）；发送 - 清空。", Mode: "TEXT", Buttons: withBack(nil, id)})
	case "sync", "set_sync_rules":
		b.setState(msg, id, "sync")
		var ids []int64
		err = b.Store.DB.SelectContext(ctx, &ids, "SELECT sync_rule_id FROM rule_syncs WHERE rule_id=?", id)
		if err != nil {
			return err
		}
		return edit(model.Outgoing{Text: fmt.Sprintf("当前同步目标：%v\n发送规则 ID 列表（空格分隔）；发送 - 清空。另需开启同步规则。", ids), Mode: "TEXT", Buttons: withBack(nil, id)})
	case "push", "push_add", "add_push_channel", "push_page":
		if action == "push_add" || action == "add_push_channel" {
			b.setState(msg, id, "push")
			return edit(model.Outgoing{Text: "发送 Apprise 格式推送地址；输入 /cancel 取消。", Mode: "TEXT"})
		}
		var buttons [][]model.Button
		for _, p := range rule.PushConfigs {
			label := fmt.Sprintf("%d · 开启:%t · %s", p.ID, p.EnablePushChannel, p.MediaSendMode)
			buttons = append(buttons, []model.Button{{Text: label, Data: fmt.Sprintf("push_toggle:%d", p.ID)}, {Text: "发送模式", Data: fmt.Sprintf("push_mode:%d", p.ID)}, {Text: "删除", Data: fmt.Sprintf("push_delete:%d", p.ID)}})
		}
		buttons = append(buttons, []model.Button{{Text: "添加推送地址", Data: fmt.Sprintf("push_add:%d", id)}})
		return edit(model.Outgoing{Text: "推送配置（地址凭据不显示在菜单中）", Mode: "TEXT", Buttons: withBack(buttons, id)})
	case "rss", "toggle_rss":
		if rule.RSS == nil {
			_, err = b.Store.DB.ExecContext(ctx, "INSERT INTO rss_configs(rule_id,enable_rss,language,max_items) VALUES(?,1,'zh-CN',50)", id)
		} else {
			_, err = b.Store.DB.ExecContext(ctx, "UPDATE rss_configs SET enable_rss=NOT COALESCE(enable_rss,0) WHERE rule_id=?", id)
		}
		if err != nil {
			return err
		}
		return edit(model.Outgoing{Text: "已切换 RSS 开关。订阅：/rss/feed/" + fmt.Sprint(id) + "\n详细提取设置请进入 RSS 管理页面。", Mode: "TEXT", Buttons: withBack(nil, id)})
	case "summary", "execute_summary", "summary_now":
		if b.Summary == nil {
			return errors.New("总结服务未启动")
		}
		return b.Summary(ctx, id)
	default:
		return errors.New("此旧按钮已失效，请重新发送 /settings")
	}
}

func withBack(buttons [][]model.Button, id int64) [][]model.Button {
	return append(buttons, []model.Button{{Text: "返回设置", Data: fmt.Sprintf("rule_settings:%d", id)}})
}
func (b *Bot) setState(msg model.Message, id int64, field string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for k, s := range b.states {
		if now.After(s.Expires) {
			delete(b.states, k)
		}
	}
	b.states[b.stateKey(msg)] = inputState{RuleID: id, Field: field, Expires: now.Add(5 * time.Minute)}
}

func (b *Bot) applyInput(ctx context.Context, state inputState, msg model.Message) error {
	rule, err := b.Store.Rule(ctx, state.RuleID)
	if err != nil {
		return err
	}
	text := msg.Text
	switch state.Field {
	case "extensions":
		return b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			if _, e := tx.ExecContext(ctx, "DELETE FROM media_extensions WHERE rule_id=?", id); e != nil {
				return e
			}
			if strings.TrimSpace(text) == "-" {
				return nil
			}
			for _, ext := range strings.Fields(text) {
				ext = strings.ToLower(strings.TrimPrefix(ext, "."))
				if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_extensions(rule_id,extension) VALUES(?,?)", id, ext); e != nil {
					return e
				}
			}
			return nil
		})
	case "push":
		if !strings.Contains(text, "://") {
			return errors.New("推送地址缺少协议")
		}
		return b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO push_configs(rule_id,enable_push_channel,push_channel,media_send_mode) SELECT ?,1,?,'Single' WHERE NOT EXISTS(SELECT 1 FROM push_configs WHERE rule_id=? AND push_channel=?)", id, strings.TrimSpace(text), id, strings.TrimSpace(text))
			return err
		})
	case "sync":
		var ids []int64
		if strings.TrimSpace(text) != "-" {
			ids, err = parseIDs(text)
			if err != nil {
				return err
			}
		}
		for _, id := range ids {
			if id == rule.ID {
				return errors.New("不能同步到自身")
			}
			if _, err = b.Store.Rule(ctx, id); err != nil {
				return err
			}
		}
		tx, e := b.Store.DB.BeginTxx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, "DELETE FROM rule_syncs WHERE rule_id=?", rule.ID); e != nil {
			return e
		}
		seen := map[int64]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if _, e = tx.ExecContext(ctx, "INSERT INTO rule_syncs(rule_id,sync_rule_id) VALUES(?,?)", rule.ID, id); e != nil {
				return e
			}
		}
		return tx.Commit()
	default:
		v, ok := fieldValue(rule, state.Field)
		if !ok {
			return errors.New("不支持的设置")
		}
		var value any = text
		if v.Kind() == reflect.Int64 {
			n, e := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if e != nil || n < 0 {
				return errors.New("请输入非负整数")
			}
			value = n
		}
		if state.Field == "summary_time" {
			if _, e := time.Parse("15:04", strings.TrimSpace(text)); e != nil {
				return errors.New("时间格式必须为 HH:MM")
			}
			value = strings.TrimSpace(text)
		}
		return b.Store.UpdateRule(ctx, rule.ID, state.Field, value)
	}
}

func (b *Bot) valuesFor(field string) []string {
	if field == "ai_model" {
		data, err := os.ReadFile(b.Config.Path("config/ai_models.json"))
		if err != nil {
			return nil
		}
		var providers map[string][]string
		if json.Unmarshal(data, &providers) != nil {
			return nil
		}
		var keys, models []string
		for k := range providers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			models = append(models, providers[k]...)
		}
		return models
	}
	file := map[string]string{"summary_time": "summary_times.txt", "delay_seconds": "delay_times.txt", "max_media_size": "max_media_size.txt"}[field]
	if file == "" {
		return nil
	}
	data, err := os.ReadFile(b.Config.Path("config/" + file))
	if err != nil {
		return nil
	}
	var values []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			values = append(values, line)
		}
	}
	return values
}

func (b *Bot) pushCallback(ctx context.Context, cb model.Callback, action string, id int64) error {
	var p model.PushConfig
	if err := b.Store.DB.GetContext(ctx, &p, "SELECT id,rule_id,COALESCE(enable_push_channel,0) AS enable_push_channel,push_channel,COALESCE(media_send_mode,'Single') AS media_send_mode FROM push_configs WHERE id=?", id); err != nil {
		return err
	}
	rule, err := b.Store.Rule(ctx, p.RuleID)
	if err != nil {
		return err
	}
	err = b.mutate(ctx, rule, func(tx *sqlx.Tx, ruleID int64) error {
		switch action {
		case "push_toggle":
			_, err = tx.ExecContext(ctx, "UPDATE push_configs SET enable_push_channel=? WHERE rule_id=? AND push_channel=?", !p.EnablePushChannel, ruleID, p.PushChannel)
		case "push_mode":
			mode := "Multiple"
			if p.MediaSendMode == "Multiple" {
				mode = "Single"
			}
			_, err = tx.ExecContext(ctx, "UPDATE push_configs SET media_send_mode=? WHERE rule_id=? AND push_channel=?", mode, ruleID, p.PushChannel)
		case "push_delete":
			_, err = tx.ExecContext(ctx, "DELETE FROM push_configs WHERE rule_id=? AND push_channel=?", ruleID, p.PushChannel)
		case "push_detail":
			return nil
		default:
			return errors.New("未知推送操作")
		}
		return err
	})
	if err != nil {
		return err
	}
	cb.Data = fmt.Sprintf("push:%d", p.RuleID)
	return b.callback(ctx, cb)
}

var legacyActions = map[string]string{"toggle_mode": "toggle_forward_mode", "toggle_bot": "toggle_use_bot", "toggle_preview": "toggle_is_preview", "toggle_replace": "toggle_is_replace", "toggle_original_link": "toggle_is_original_link", "toggle_original_sender": "toggle_is_original_sender", "toggle_original_time": "toggle_is_original_time", "toggle_delete_original": "toggle_is_delete_original", "toggle_ai": "toggle_is_ai", "toggle_summary": "toggle_is_summary", "toggle_filter_user_info": "toggle_is_filter_user_info", "toggle_keyword_after_ai": "toggle_is_keyword_after_ai", "toggle_top_summary": "toggle_is_top_summary", "toggle_reverse_blacklist": "toggle_enable_reverse_blacklist", "toggle_reverse_whitelist": "toggle_enable_reverse_whitelist", "set_media_types": "media", "set_userinfo_template": "set_userinfo_template", "set_original_link_template": "set_original_link_template"}
