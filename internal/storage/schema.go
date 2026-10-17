package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type column struct {
	name       string
	definition string
	fallback   string
}

type table struct {
	name        string
	columns     []column
	constraints []string
}

func idColumn() column { return column{"id", "INTEGER PRIMARY KEY", "0"} }
func textColumn(name, fallback string) column {
	value := "'" + strings.ReplaceAll(fallback, "'", "''") + "'"
	return column{name, "TEXT DEFAULT " + value, value}
}
func intColumn(name string, fallback int) column {
	value := fmt.Sprint(fallback)
	return column{name, "INTEGER DEFAULT " + value, value}
}
func boolColumn(name string, fallback int) column {
	c := intColumn(name, fallback)
	c.definition = strings.Replace(c.definition, "INTEGER", "BOOLEAN", 1)
	return c
}
func reference(name, parent string) column {
	return column{name, "INTEGER NOT NULL REFERENCES " + parent + "(id)", "0"}
}

// 字段、枚举与缺省值来自原 SQLAlchemy 模型；新库补上其客户端默认值。
var schema = []table{
	{"chats", []column{
		idColumn(), {"telegram_chat_id", "TEXT NOT NULL UNIQUE", "''"},
		textColumn("name", ""), textColumn("current_add_id", ""),
	}, nil},
	{"forward_rules", []column{
		idColumn(), reference("source_chat_id", "chats"), reference("target_chat_id", "chats"),
		textColumn("forward_mode", "BLACKLIST"), boolColumn("use_bot", 1), textColumn("message_mode", "MARKDOWN"),
		boolColumn("is_replace", 0), textColumn("is_preview", "FOLLOW"), boolColumn("is_original_link", 0),
		boolColumn("is_ufb", 0), textColumn("ufb_domain", ""), textColumn("ufb_item", "main"),
		boolColumn("is_delete_original", 0), boolColumn("is_original_sender", 0), textColumn("userinfo_template", "**{name}**"),
		textColumn("time_template", "{time}"), textColumn("original_link_template", "原始连接：{original_link}"),
		boolColumn("is_original_time", 0), textColumn("add_mode", "BLACKLIST"), boolColumn("enable_rule", 1),
		boolColumn("is_filter_user_info", 0), textColumn("handle_mode", "FORWARD"), boolColumn("enable_comment_button", 0),
		boolColumn("enable_media_type_filter", 0), boolColumn("enable_media_size_filter", 0), intColumn("max_media_size", 10),
		boolColumn("is_send_over_media_size_message", 1), boolColumn("enable_extension_filter", 0), textColumn("extension_filter_mode", "BLACKLIST"),
		boolColumn("enable_reverse_blacklist", 0), boolColumn("enable_reverse_whitelist", 0), boolColumn("media_allow_text", 0),
		boolColumn("enable_push", 0), boolColumn("enable_only_push", 0), boolColumn("is_ai", 0), textColumn("ai_model", ""),
		textColumn("ai_prompt", ""), boolColumn("enable_ai_upload_image", 0), boolColumn("is_summary", 0),
		textColumn("summary_time", "07:00"), textColumn("summary_prompt", ""), boolColumn("is_keyword_after_ai", 0),
		boolColumn("is_top_summary", 1), boolColumn("enable_delay", 0), intColumn("delay_seconds", 5),
		boolColumn("only_rss", 0), boolColumn("enable_sync", 0),
	}, []string{"UNIQUE(source_chat_id, target_chat_id)"}},
	{"keywords", []column{
		idColumn(), reference("rule_id", "forward_rules"), textColumn("keyword", ""),
		boolColumn("is_regex", 0), boolColumn("is_blacklist", 1),
	}, []string{"UNIQUE(rule_id, keyword, is_regex, is_blacklist)"}},
	{"replace_rules", []column{
		idColumn(), reference("rule_id", "forward_rules"), {"pattern", "TEXT NOT NULL", "''"}, textColumn("content", ""),
	}, []string{"UNIQUE(rule_id, pattern, content)"}},
	{"media_types", []column{
		idColumn(), reference("rule_id", "forward_rules"), boolColumn("photo", 0), boolColumn("document", 0),
		boolColumn("video", 0), boolColumn("audio", 0), boolColumn("voice", 0),
	}, []string{"UNIQUE(rule_id)"}},
	{"media_extensions", []column{
		idColumn(), reference("rule_id", "forward_rules"), {"extension", "TEXT NOT NULL", "''"},
	}, []string{"UNIQUE(rule_id, extension)"}},
	{"rule_syncs", []column{
		idColumn(), reference("rule_id", "forward_rules"), {"sync_rule_id", "INTEGER NOT NULL", "0"},
	}, nil},
	{"push_configs", []column{
		idColumn(), reference("rule_id", "forward_rules"), boolColumn("enable_push_channel", 0),
		{"push_channel", "TEXT NOT NULL", "''"}, textColumn("media_send_mode", "Single"),
	}, nil},
	{"rss_configs", []column{
		idColumn(), reference("rule_id", "forward_rules"), boolColumn("enable_rss", 0), textColumn("rule_title", ""),
		textColumn("rule_description", ""), textColumn("language", "zh-CN"), intColumn("max_items", 50),
		boolColumn("is_auto_title", 0), boolColumn("is_auto_content", 0), boolColumn("is_ai_extract", 0),
		textColumn("ai_extract_prompt", ""), boolColumn("is_auto_markdown_to_html", 0),
		boolColumn("enable_custom_title_pattern", 0), boolColumn("enable_custom_content_pattern", 0),
	}, []string{"UNIQUE(rule_id)"}},
	{"rss_patterns", []column{
		idColumn(), reference("rss_config_id", "rss_configs"), {"pattern", "TEXT NOT NULL", "''"},
		{"pattern_type", "TEXT NOT NULL", "''"}, intColumn("priority", 0),
	}, []string{"UNIQUE(rss_config_id, pattern, pattern_type)"}},
	{"users", []column{
		idColumn(), {"username", "TEXT NOT NULL", "''"}, {"password", "TEXT NOT NULL", "''"},
	}, nil},
}

// ColumnsSelect 生成兼容旧库 NULL 值的投影；未知表返回空字符串。
func ColumnsSelect(name string) string {
	for _, t := range schema {
		if t.name != name {
			continue
		}
		fields := make([]string, 0, len(t.columns))
		for _, c := range t.columns {
			fallback := c.fallback
			if strings.HasPrefix(c.definition, "BOOLEAN ") {
				// Python 中 bool(None) 为 false；已有 NULL 不能按新建默认值启用。
				fallback = "0"
			}
			fields = append(fields, "COALESCE("+c.name+","+fallback+") AS "+c.name)
		}
		return strings.Join(fields, ",")
	}
	return ""
}

func (s *Store) migrate(ctx context.Context, path string) error {
	var changes []string
	var existing []string
	if err := s.DB.SelectContext(ctx, &existing, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"); err != nil {
		return err
	}
	tables := make(map[string]bool, len(existing))
	for _, name := range existing {
		tables[name] = true
	}
	var indexes keywordIndexMigration
	if tables["keywords"] {
		var err error
		indexes, err = s.keywordIndexes(ctx)
		if err != nil {
			return err
		}
	}
	for _, t := range schema {
		if !tables[t.name] {
			parts := make([]string, 0, len(t.columns)+len(t.constraints))
			for _, c := range t.columns {
				parts = append(parts, c.name+" "+c.definition)
			}
			parts = append(parts, t.constraints...)
			changes = append(changes, "CREATE TABLE "+t.name+" ("+strings.Join(parts, ",")+")")
			continue
		}
		columns, err := s.columns(ctx, t.name)
		if err != nil {
			return err
		}
		for _, c := range t.columns {
			if columns[c.name] {
				continue
			}
			if c.name == "id" || strings.Contains(c.definition, "REFERENCES") {
				return fmt.Errorf("旧数据库缺少必需字段 %s.%s，无法安全迁移", t.name, c.name)
			}
			changes = append(changes, "ALTER TABLE "+t.name+" ADD COLUMN "+c.name+" "+c.definition)
		}
		if t.name == "forward_rules" {
			if columns["mode"] && !columns["forward_mode"] {
				changes = append(changes, "UPDATE forward_rules SET forward_mode=COALESCE(mode,'BLACKLIST')")
			}
		}
	}
	if tables["forward_rules"] && !tables["media_types"] {
		columns, err := s.columns(ctx, "forward_rules")
		if err != nil {
			return err
		}
		if columns["selected_media_types"] {
			changes = append(changes, `INSERT INTO media_types(rule_id,photo,document,video,audio,voice)
				SELECT id, instr(selected_media_types,'photo')>0, instr(selected_media_types,'document')>0,
				instr(selected_media_types,'video')>0, instr(selected_media_types,'audio')>0,
				instr(selected_media_types,'voice')>0 FROM forward_rules WHERE selected_media_types IS NOT NULL`)
		}
	}
	if len(changes) == 0 && !indexes.changed() {
		return nil
	}
	if len(existing) != 0 && path != ":memory:" {
		backup := path + ".backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if _, err := s.DB.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("迁移前备份失败，未修改数据库结构: %w", err)
		}
	}
	if indexes.rebuild {
		// SQLite 无法删除表内 UNIQUE 的自动索引。重建时暂时关闭 FK 动作，
		// 防止 DROP TABLE 触发级联删除；提交前验证所有原有外键。
		if _, err := s.DB.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return err
		}
		defer s.DB.ExecContext(context.Background(), "PRAGMA foreign_keys=ON")
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range changes {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("数据库兼容迁移失败: %w", err)
		}
	}
	if err := indexes.apply(ctx, tx); err != nil {
		return fmt.Errorf("迁移关键词唯一约束: %w", err)
	}
	return tx.Commit()
}

func (s *Store) columns(ctx context.Context, name string) (map[string]bool, error) {
	rows, err := s.DB.QueryxContext(ctx, "PRAGMA table_info("+name+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		values := make(map[string]any)
		if err := rows.MapScan(values); err != nil {
			return nil, err
		}
		result[fmt.Sprint(values["name"])] = true
	}
	return result, rows.Err()
}
