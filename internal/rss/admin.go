package rss

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
)

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if s.current(r) == "" {
		http.Redirect(w, r, "/login", 302)
		return
	}
	rules, err := s.store.Rules(r.Context())
	if err != nil {
		failure(w, 500, err)
		return
	}
	configs := []map[string]any{}
	ruleList := []map[string]any{}
	for _, rule := range rules {
		ruleList = append(ruleList, map[string]any{"id": rule.ID, "source_chat": map[string]any{"id": rule.Source.ID, "name": rule.Source.Name}, "target_chat": map[string]any{"id": rule.Target.ID, "name": rule.Target.Name}})
		if rule.RSS != nil {
			data, _ := json.Marshal(rule.RSS)
			var item map[string]any
			_ = json.Unmarshal(data, &item)
			if prompt, ok := item["ai_extract_prompt"].(string); ok && prompt != "" {
				item["ai_extract_prompt"] = "BASE64:" + base64.StdEncoding.EncodeToString([]byte(prompt))
			}
			configs = append(configs, item)
		}
	}
	data, _ := json.Marshal(configs)
	s.render(w, 200, "rss_dashboard.html", map[string]any{"Configs": configs, "Rules": ruleList, "ConfigsJSON": string(data), "BaseURL": s.cfg.String("RSS_BASE_URL", ""), "Error": r.URL.Query().Get("error"), "Success": r.URL.Query().Get("success")})
}

func formBool(r *http.Request, key string, fallback bool) bool {
	v, ok := r.Form[key]
	if !ok || len(v) == 0 {
		return fallback
	}
	switch strings.ToLower(v[0]) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
func formInt(r *http.Request, key string, fallback int64) int64 {
	v := r.FormValue(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}
func (s *Server) configSave(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	id, ruleID := formInt(r, "config_id", 0), formInt(r, "rule_id", 0)
	if ruleID <= 0 {
		failure(w, 422, errors.New("规则 ID 无效"))
		return
	}
	if _, err := s.store.Rule(r.Context(), ruleID); err != nil {
		failure(w, 404, errors.New("规则不存在"))
		return
	}
	max := formInt(r, "max_items", 50)
	if max <= 0 {
		failure(w, 422, errors.New("最大条目数必须大于 0"))
		return
	}
	columns := []string{"rule_id", "enable_rss", "rule_title", "rule_description", "language", "max_items", "is_auto_title", "is_auto_content", "is_ai_extract", "ai_extract_prompt", "is_auto_markdown_to_html", "enable_custom_title_pattern", "enable_custom_content_pattern"}
	language := r.FormValue("language")
	if language == "" {
		language = "zh-CN"
	}
	values := []any{ruleID, formBool(r, "enable_rss", true), r.FormValue("rule_title"), r.FormValue("rule_description"), language, max, formBool(r, "is_auto_title", false), formBool(r, "is_auto_content", false), formBool(r, "is_ai_extract", false), r.FormValue("ai_extract_prompt"), formBool(r, "is_auto_markdown_to_html", false), formBool(r, "enable_custom_title_pattern", false), formBool(r, "enable_custom_content_pattern", false)}
	if id > 0 {
		sets := []string{}
		for _, column := range columns {
			sets = append(sets, column+" = ?")
		}
		values = append(values, id)
		result, err := s.store.DB.ExecContext(r.Context(), "UPDATE rss_configs SET "+strings.Join(sets, ",")+" WHERE id = ?", values...)
		if err != nil {
			failure(w, 200, err)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			failure(w, 404, errors.New("配置不存在"))
			return
		}
	} else {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
		result, err := s.store.DB.ExecContext(r.Context(), "INSERT INTO rss_configs("+strings.Join(columns, ",")+") VALUES ("+marks+")", values...)
		if err != nil {
			failure(w, 200, errors.New("创建配置失败，该规则可能已经存在 RSS 配置"))
			return
		}
		id, _ = result.LastInsertId()
	}
	reply(w, 200, map[string]any{"success": true, "message": "RSS 配置已保存", "config_id": id, "rule_id": ruleID})
}

func (s *Server) toggle(w http.ResponseWriter, r *http.Request) {
	if s.current(r) == "" {
		http.Redirect(w, r, "/login", 302)
		return
	}
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	result, err := s.store.DB.ExecContext(r.Context(), "UPDATE rss_configs SET enable_rss = CASE WHEN COALESCE(enable_rss,0) = 0 THEN 1 ELSE 0 END WHERE rule_id = ?", id)
	if err != nil {
		failure(w, 500, err)
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		http.Redirect(w, r, "/rss/dashboard?error=配置不存在", 302)
		return
	}
	http.Redirect(w, r, "/rss/dashboard?success=RSS状态已切换", 302)
}
func (s *Server) configDelete(w http.ResponseWriter, r *http.Request) {
	if s.current(r) == "" {
		http.Redirect(w, r, "/login", 302)
		return
	}
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	tx, err := s.store.DB.BeginTxx(r.Context(), nil)
	if err != nil {
		failure(w, 500, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM rss_patterns WHERE rss_config_id IN (SELECT id FROM rss_configs WHERE rule_id = ?)", id); err == nil {
		_, err = tx.ExecContext(r.Context(), "DELETE FROM rss_configs WHERE rule_id = ?", id)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		err = s.removeRule(id)
	}
	if err != nil {
		failure(w, 500, err)
		return
	}
	http.Redirect(w, r, "/rss/dashboard?success=RSS配置已删除", 302)
}

type pattern struct {
	ID       int64  `db:"id" json:"id"`
	Pattern  string `db:"pattern" json:"pattern"`
	Type     string `db:"pattern_type" json:"pattern_type"`
	Priority int    `db:"priority" json:"priority"`
}

// 历史管理页面把规则 ID 作为 patterns 路径参数发送；同时接受真实配置 ID。
func (s *Server) resolveConfig(r *http.Request, id int64) (int64, error) {
	var result int64
	err := s.store.DB.GetContext(r.Context(), &result, "SELECT id FROM rss_configs WHERE rule_id = ?", id)
	if err != nil {
		err = s.store.DB.GetContext(r.Context(), &result, "SELECT id FROM rss_configs WHERE id = ?", id)
	}
	return result, err
}
func (s *Server) patterns(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	id, ok := pathID(w, r, "config_id")
	if !ok {
		return
	}
	id, err := s.resolveConfig(r, id)
	if err != nil {
		failure(w, 404, errors.New("配置不存在"))
		return
	}
	rows := []pattern{}
	if err = s.store.DB.SelectContext(r.Context(), &rows, "SELECT id, pattern, pattern_type, COALESCE(priority,0) AS priority FROM rss_patterns WHERE rss_config_id = ? ORDER BY priority,id", id); err != nil {
		failure(w, 500, err)
		return
	}
	reply(w, 200, map[string]any{"success": true, "patterns": rows})
}
func (s *Server) patternSave(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	configID := formInt(r, "rss_config_id", 0)
	var exists int
	if err := s.store.DB.GetContext(r.Context(), &exists, "SELECT COUNT(*) FROM rss_configs WHERE id = ?", configID); err != nil || exists == 0 {
		failure(w, 404, errors.New("RSS 配置不存在"))
		return
	}
	kind, expr := r.FormValue("pattern_type"), r.FormValue("pattern")
	if kind != "title" && kind != "content" {
		failure(w, 422, errors.New("模式类型必须是 title 或 content"))
		return
	}
	if _, err := pyregex.Find(expr, ""); err != nil {
		failure(w, 422, err)
		return
	}
	id := formInt(r, "pattern_id", 0)
	priority := formInt(r, "priority", 0)
	if id > 0 {
		res, err := s.store.DB.ExecContext(r.Context(), "UPDATE rss_patterns SET pattern = ?, pattern_type = ?, priority = ? WHERE id = ? AND rss_config_id = ?", expr, kind, priority, id, configID)
		if err != nil {
			failure(w, 200, err)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			failure(w, 404, errors.New("找不到该模式"))
			return
		}
	} else {
		res, err := s.store.DB.ExecContext(r.Context(), "INSERT INTO rss_patterns(rss_config_id,pattern,pattern_type,priority) VALUES (?,?,?,?)", configID, expr, kind, priority)
		if err != nil {
			failure(w, 200, err)
			return
		}
		id, _ = res.LastInsertId()
	}
	reply(w, 200, map[string]any{"success": true, "message": "模式已保存", "pattern_id": id})
}
func (s *Server) patternDelete(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	id, ok := pathID(w, r, "pattern_id")
	if !ok {
		return
	}
	res, err := s.store.DB.ExecContext(r.Context(), "DELETE FROM rss_patterns WHERE id = ?", id)
	if err != nil {
		failure(w, 500, err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		failure(w, 200, errors.New("找不到该模式"))
		return
	}
	reply(w, 200, map[string]any{"success": true, "message": "模式删除成功"})
}
func (s *Server) patternsDelete(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	id, ok := pathID(w, r, "config_id")
	if !ok {
		return
	}
	res, err := s.store.DB.ExecContext(r.Context(), "DELETE FROM rss_patterns WHERE rss_config_id = ?", id)
	if err != nil {
		failure(w, 500, err)
		return
	}
	n, _ := res.RowsAffected()
	reply(w, 200, map[string]any{"success": true, "message": "模式删除成功", "count": n})
}
func (s *Server) testRegex(w http.ResponseWriter, r *http.Request) {
	if !s.require(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		failure(w, 400, err)
		return
	}
	match, err := pyregex.Find(r.FormValue("pattern"), r.FormValue("test_text"))
	if err != nil {
		failure(w, 200, err)
		return
	}
	result := map[string]any{"success": true, "matched": len(match) > 0, "has_groups": len(match) > 1}
	if len(match) > 1 {
		result["extracted"] = match[1]
		result["message"] = "匹配成功！"
	} else if len(match) == 1 {
		result["message"] = "匹配成功，但没有捕获组。请使用括号 () 来创建捕获组。"
	} else {
		result["message"] = "未找到匹配"
	}
	reply(w, 200, result)
}
