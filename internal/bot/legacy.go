package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/jmoiron/sqlx"
)

// 保留升级前已经发送到聊天中的按钮，避免已有设置消息失效。
func init() {
	for action, current := range map[string]string{
		"toggle_current": "apply", "toggle_ufb": "toggle_is_ufb", "toggle_ai_upload_image": "toggle_enable_ai_upload_image",
		"toggle_send_over_media_size_message": "toggle_is_send_over_media_size_message", "toggle_enable_media_extension_filter": "toggle_enable_extension_filter", "toggle_media_extension_filter_mode": "toggle_extension_filter_mode",
		"set_delay_time": "set_delay_seconds", "change_model": "set_ai_model", "set_sync_rule": "sync", "sync_rule_page": "sync", "media_extensions_page": "extensions",
		"toggle_push_config": "push_detail", "toggle_push_config_status": "push_toggle", "toggle_media_send_mode": "push_mode", "delete_push_config": "push_delete", "delete_rule": "confirm_delete",
	} {
		legacyActions[action] = current
	}
}

func (b *Bot) legacyCallback(ctx context.Context, cb model.Callback, rule model.Rule, action string, parts []string) (bool, error) {
	edit := func(out model.Outgoing) error { return b.Client.Edit(ctx, cb.Chat, cb.MessageID, out) }
	msg := model.Message{ID: cb.MessageID, Chat: cb.Chat, Sender: model.Peer{ID: cb.SenderID}}
	if field := map[string]string{"select_time": "summary_time", "select_delay_time": "delay_seconds", "select_model": "ai_model", "select_max_media_size": "max_media_size"}[action]; field != "" {
		if len(parts) < 3 {
			return true, errors.New("缺少设置值")
		}
		msg.Text = strings.Join(parts[2:], ":")
		if err := b.applyInput(ctx, inputState{RuleID: rule.ID, Field: field}, msg); err != nil {
			return true, err
		}
		updated, err := b.Store.Rule(ctx, rule.ID)
		if err != nil {
			return true, err
		}
		return true, edit(b.settings(updated, 0))
	}
	if field := map[string]string{"time_page": "summary_time", "delay_time_page": "delay_seconds", "model_page": "ai_model"}[action]; field != "" {
		page := 0
		if len(parts) > 2 {
			page, _ = strconv.Atoi(parts[2])
		}
		b.setState(msg, rule.ID, field)
		return true, edit(b.valueMenu(rule.ID, field, page))
	}
	if action == "values" {
		if len(parts) < 4 || fieldLabels[parts[2]] == "" {
			return true, errors.New("选择菜单参数无效")
		}
		page, err := strconv.Atoi(parts[3])
		if err != nil {
			return true, err
		}
		b.setState(msg, rule.ID, parts[2])
		return true, edit(b.valueMenu(rule.ID, parts[2], page))
	}
	switch action {
	case "toggle_rule_sync":
		if len(parts) < 3 {
			return true, errors.New("缺少同步目标")
		}
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id == rule.ID {
			return true, errors.New("无效同步目标")
		}
		if _, err = b.Store.Rule(ctx, id); err != nil {
			return true, err
		}
		tx, err := b.Store.DB.BeginTxx(ctx, nil)
		if err != nil {
			return true, err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, "DELETE FROM rule_syncs WHERE rule_id=? AND sync_rule_id=?", rule.ID, id)
		if err != nil {
			return true, err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			if _, err = tx.ExecContext(ctx, "INSERT INTO rule_syncs(rule_id,sync_rule_id) VALUES(?,?)", rule.ID, id); err != nil {
				return true, err
			}
		}
		if err = tx.Commit(); err != nil {
			return true, err
		}
		cb.Data = fmt.Sprintf("sync:%d", rule.ID)
		return true, b.callback(ctx, cb)
	case "toggle_media_extension":
		if len(parts) < 3 || strings.TrimSpace(parts[2]) == "" {
			return true, errors.New("缺少扩展名")
		}
		ext := strings.ToLower(strings.TrimPrefix(parts[2], "."))
		enabled := false
		for _, existing := range rule.Extensions {
			if existing == ext {
				enabled = true
			}
		}
		err := b.mutate(ctx, rule, func(tx *sqlx.Tx, id int64) error {
			if enabled {
				_, err := tx.ExecContext(ctx, "DELETE FROM media_extensions WHERE rule_id=? AND extension=?", id, ext)
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_extensions(rule_id,extension) VALUES(?,?)", id, ext)
			return err
		})
		if err != nil {
			return true, err
		}
		cb.Data = fmt.Sprintf("extensions:%d", rule.ID)
		return true, b.callback(ctx, cb)
	case "copy_rule", "copy_keyword", "copy_replace":
		all, err := b.Store.Rules(ctx)
		if err != nil {
			return true, err
		}
		var candidates []model.Rule
		for _, r := range all {
			if r.ID != rule.ID {
				candidates = append(candidates, r)
			}
		}
		page := 0
		if len(parts) > 2 {
			page, _ = strconv.Atoi(parts[2])
		}
		pages := max(1, (len(candidates)+19)/20)
		page = max(0, min(pages-1, page))
		var buttons [][]model.Button
		for _, r := range candidates[page*20 : min((page+1)*20, len(candidates))] {
			buttons = append(buttons, []model.Button{{Text: fmt.Sprintf("%d · %s → %s", r.ID, r.Source.Name, r.Target.Name), Data: fmt.Sprintf("perform_%s:%d:%d", action, rule.ID, r.ID)}})
		}
		buttons = append(buttons, []model.Button{{Text: "上一页", Data: fmt.Sprintf("%s:%d:%d", action, rule.ID, max(0, page-1))}, {Text: "下一页", Data: fmt.Sprintf("%s:%d:%d", action, rule.ID, min(pages-1, page+1))}})
		return true, edit(model.Outgoing{Text: "选择将当前配置复制到的目标规则：", Mode: "TEXT", Buttons: withBack(buttons, rule.ID)})
	case "perform_copy_rule", "perform_copy_keyword", "perform_copy_replace":
		if len(parts) != 3 {
			return true, errors.New("复制按钮参数无效")
		}
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return true, err
		}
		target, err := b.Store.Rule(ctx, id)
		if err != nil {
			return true, err
		}
		kind := strings.TrimPrefix(action, "perform_")
		if kind == "copy_keyword" {
			kind = "copy_all_keywords"
		}
		if err = b.copyRule(ctx, rule, target, kind); err != nil {
			return true, err
		}
		return true, edit(model.Outgoing{Text: fmt.Sprintf("已从规则 %d 复制到 %d", rule.ID, id), Mode: "TEXT", Buttons: withBack(nil, rule.ID)})
	case "clear_keyword", "clear_replace":
		return true, edit(model.Outgoing{Text: "确认清空该规则的配置？", Mode: "TEXT", Buttons: withBack([][]model.Button{{{Text: "确认清空", Data: fmt.Sprintf("perform_%s:%d", action, rule.ID)}}}, rule.ID)})
	case "perform_clear_keyword", "perform_clear_replace":
		target := rule
		if len(parts) > 2 {
			id, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				return true, err
			}
			target, err = b.Store.Rule(ctx, id)
			if err != nil {
				return true, err
			}
		}
		err := b.mutate(ctx, target, func(tx *sqlx.Tx, id int64) error {
			if action == "perform_clear_keyword" {
				_, err := tx.ExecContext(ctx, "DELETE FROM keywords WHERE rule_id=?", id)
				return err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM replace_rules WHERE rule_id=?", id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "UPDATE forward_rules SET is_replace=0 WHERE id=?", id)
			return err
		})
		if err != nil {
			return true, err
		}
		return true, edit(model.Outgoing{Text: "已清空", Mode: "TEXT", Buttons: withBack(nil, rule.ID)})
	}
	return false, nil
}

func (b *Bot) valueMenu(id int64, field string, page int) model.Outgoing {
	values := b.valuesFor(field)
	pages := max(1, (len(values)+39)/40)
	page = max(0, min(pages-1, page))
	var buttons [][]model.Button
	for index := page * 40; index < min((page+1)*40, len(values)); index++ {
		buttons = append(buttons, []model.Button{{Text: values[index], Data: fmt.Sprintf("pick:%d:%s:%d", id, field, index)}})
	}
	if pages > 1 {
		buttons = append(buttons, []model.Button{{Text: "上一页", Data: fmt.Sprintf("values:%d:%s:%d", id, field, max(0, page-1))}, {Text: fmt.Sprintf("%d/%d", page+1, pages), Data: "noop"}, {Text: "下一页", Data: fmt.Sprintf("values:%d:%s:%d", id, field, min(pages-1, page+1))}})
	}
	buttons = append(buttons, []model.Button{{Text: "取消", Data: "cancel"}})
	return model.Outgoing{Text: "请发送新的" + fieldLabels[field] + "，或选择下面的值。输入 /cancel 取消。", Mode: "TEXT", Buttons: buttons}
}
