// Package storage 读写兼容数据库，迁移前备份并保留原主键与关联。
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB           *sqlx.DB
	defaultsMu   sync.RWMutex
	maxMediaSize int
	summaryTime  string
	legacyMode   bool
}

func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		urlPath := filepath.ToSlash(path)
		if !strings.HasPrefix(urlPath, "/") {
			urlPath = "/" + urlPath
		}
		dsn = (&url.URL{Scheme: "file", Path: urlPath}).String()
	}
	db, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单一连接保证 PRAGMA 一致，并串行化 SQLite 写入。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db, maxMediaSize: 10, summaryTime: "07:00"}
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.migrate(context.Background(), path); err != nil {
		db.Close()
		return nil, err
	}
	columns, err := s.columns(context.Background(), "forward_rules")
	if err != nil {
		db.Close()
		return nil, err
	}
	s.legacyMode = columns["mode"]
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// ConfigureDefaults 应在应用启动时调用，应用 .env 中的 ORM 动态默认值。
func (s *Store) ConfigureDefaults(maxMediaSize int, summaryTime string) {
	s.defaultsMu.Lock()
	defer s.defaultsMu.Unlock()
	s.maxMediaSize = maxMediaSize
	s.summaryTime = summaryTime
}

func (s *Store) Rules(ctx context.Context) ([]model.Rule, error) {
	rules := []model.Rule{}
	if err := s.DB.SelectContext(ctx, &rules, "SELECT "+ColumnsSelect("forward_rules")+" FROM forward_rules ORDER BY id"); err != nil {
		return nil, err
	}
	for i := range rules {
		if err := s.relations(ctx, &rules[i]); err != nil {
			return nil, fmt.Errorf("读取规则 %d 关系: %w", rules[i].ID, err)
		}
	}
	return rules, nil
}

func (s *Store) Rule(ctx context.Context, id int64) (model.Rule, error) {
	var rule model.Rule
	if err := s.DB.GetContext(ctx, &rule, "SELECT "+ColumnsSelect("forward_rules")+" FROM forward_rules WHERE id=?", id); err != nil {
		return rule, err
	}
	err := s.relations(ctx, &rule)
	return rule, err
}

func (s *Store) Chats(ctx context.Context) ([]model.Chat, error) {
	chats := []model.Chat{}
	err := s.DB.SelectContext(ctx, &chats, "SELECT "+ColumnsSelect("chats")+" FROM chats ORDER BY id")
	return chats, err
}

func (s *Store) relations(ctx context.Context, rule *model.Rule) error {
	for _, chat := range []struct {
		id  int64
		dst *model.Chat
	}{{rule.SourceChatID, &rule.Source}, {rule.TargetChatID, &rule.Target}} {
		if err := s.DB.GetContext(ctx, chat.dst, "SELECT "+ColumnsSelect("chats")+" FROM chats WHERE id=?", chat.id); err != nil {
			return err
		}
	}
	queries := []struct {
		table string
		dst   any
	}{
		{"keywords", &rule.Keywords}, {"replace_rules", &rule.Replacements}, {"push_configs", &rule.PushConfigs},
	}
	for _, query := range queries {
		if err := s.DB.SelectContext(ctx, query.dst, "SELECT "+ColumnsSelect(query.table)+" FROM "+query.table+" WHERE rule_id=? ORDER BY id", rule.ID); err != nil {
			return err
		}
	}
	var media struct {
		Photo    bool `db:"photo"`
		Document bool `db:"document"`
		Video    bool `db:"video"`
		Audio    bool `db:"audio"`
		Voice    bool `db:"voice"`
	}
	err := s.DB.GetContext(ctx, &media, "SELECT COALESCE(photo,0) photo,COALESCE(document,0) document,COALESCE(video,0) video,COALESCE(audio,0) audio,COALESCE(voice,0) voice FROM media_types WHERE rule_id=?", rule.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rule.MediaTypes = map[string]bool{"photo": media.Photo, "document": media.Document, "video": media.Video, "audio": media.Audio, "voice": media.Voice}
	if err := s.DB.SelectContext(ctx, &rule.Extensions, "SELECT COALESCE(extension,'') FROM media_extensions WHERE rule_id=? ORDER BY id", rule.ID); err != nil {
		return err
	}
	var rss model.RSSConfig
	err = s.DB.GetContext(ctx, &rss, "SELECT "+ColumnsSelect("rss_configs")+" FROM rss_configs WHERE rule_id=?", rule.ID)
	if err == nil {
		rule.RSS = &rss
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// Bind 在一个事务中更新聊天与创建规则；重复绑定返回已有规则。
func (s *Store) Bind(ctx context.Context, source, target model.Peer) (model.Rule, error) {
	if source.ID <= 0 || target.ID <= 0 {
		return model.Rule{}, errors.New("绑定聊天需要 Telegram 原始正 ID")
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return model.Rule{}, err
	}
	defer tx.Rollback()
	sourceID, err := upsertChat(ctx, tx, source)
	if err != nil {
		return model.Rule{}, err
	}
	targetID, err := upsertChat(ctx, tx, target)
	if err != nil {
		return model.Rule{}, err
	}
	var id int64
	err = tx.GetContext(ctx, &id, "SELECT id FROM forward_rules WHERE source_chat_id=? AND target_chat_id=?", sourceID, targetID)
	if errors.Is(err, sql.ErrNoRows) {
		// 旧库的列没有 SQL DEFAULT，因此显式提供所有 ORM 默认值。
		var names, values []string
		for _, c := range schema[1].columns {
			if c.name == "id" {
				continue
			}
			names = append(names, c.name)
			switch c.name {
			case "source_chat_id", "target_chat_id", "max_media_size", "summary_time":
				values = append(values, "?")
			case "forward_mode", "add_mode":
				if sourceID == targetID {
					values = append(values, "'WHITELIST'")
				} else {
					values = append(values, c.fallback)
				}
			default:
				values = append(values, c.fallback)
			}
		}
		if s.legacyMode {
			names = append(names, "mode")
			mode := "'BLACKLIST'"
			if sourceID == targetID {
				mode = "'WHITELIST'"
			}
			values = append(values, mode)
		}
		s.defaultsMu.RLock()
		maxSize, summaryTime := s.maxMediaSize, s.summaryTime
		s.defaultsMu.RUnlock()
		result, insertErr := tx.ExecContext(ctx, "INSERT INTO forward_rules("+strings.Join(names, ",")+") VALUES("+strings.Join(values, ",")+")", sourceID, targetID, maxSize, summaryTime)
		if insertErr != nil {
			return model.Rule{}, insertErr
		}
		id, err = result.LastInsertId()
	}
	if err != nil {
		return model.Rule{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE chats SET current_add_id=? WHERE id=? AND COALESCE(current_add_id,'')=''", strconv.FormatInt(source.ID, 10), targetID); err != nil {
		return model.Rule{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Rule{}, err
	}
	return s.Rule(ctx, id)
}

func upsertChat(ctx context.Context, tx *sqlx.Tx, peer model.Peer) (int64, error) {
	telegramID := strconv.FormatInt(peer.ID, 10)
	if _, err := tx.ExecContext(ctx, `INSERT INTO chats(telegram_chat_id,name,current_add_id) VALUES(?,?,'')
		ON CONFLICT(telegram_chat_id) DO UPDATE SET name=CASE WHEN excluded.name='' THEN chats.name ELSE excluded.name END`, telegramID, peer.Name); err != nil {
		return 0, err
	}
	var id int64
	err := tx.GetContext(ctx, &id, "SELECT id FROM chats WHERE telegram_chat_id=?", telegramID)
	return id, err
}

func (s *Store) UpdateRule(ctx context.Context, id int64, field string, value any) error {
	allowed := false
	for _, c := range schema[1].columns {
		if c.name == field && field != "id" && field != "source_chat_id" && field != "target_chat_id" {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("不可更新的规则字段: %s", field)
	}
	if values, ok := enumValues[field]; ok {
		text, ok := value.(string)
		if !ok || !strings.Contains("|"+values+"|", "|"+text+"|") {
			return fmt.Errorf("字段 %s 的枚举值无效", field)
		}
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var syncEnabled bool
	if err := tx.GetContext(ctx, &syncEnabled, "SELECT COALESCE(enable_sync,0) FROM forward_rules WHERE id=?", id); err != nil {
		return err
	}
	ids := []int64{id}
	if syncEnabled && field != "enable_sync" && field != "enable_rule" {
		var targets []int64
		if err := tx.SelectContext(ctx, &targets, "SELECT DISTINCT sync_rule_id FROM rule_syncs WHERE rule_id=?", id); err != nil {
			return err
		}
		ids = append(ids, targets...)
	}
	for _, target := range ids {
		if _, err := tx.ExecContext(ctx, "UPDATE forward_rules SET "+field+"=? WHERE id=?", value, target); err != nil {
			return err
		}
		if field == "forward_mode" && s.legacyMode {
			if _, err := tx.ExecContext(ctx, "UPDATE forward_rules SET mode=? WHERE id=?", value, target); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

var enumValues = map[string]string{
	"forward_mode": "BLACKLIST|WHITELIST|BLACKLIST_THEN_WHITELIST|WHITELIST_THEN_BLACKLIST",
	"message_mode": "MARKDOWN|HTML", "is_preview": "ON|OFF|FOLLOW", "add_mode": "BLACKLIST|WHITELIST",
	"extension_filter_mode": "BLACKLIST|WHITELIST", "handle_mode": "FORWARD|EDIT",
}

func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM rss_patterns WHERE rss_config_id IN(SELECT id FROM rss_configs WHERE rule_id=?)", id); err != nil {
		return err
	}
	for _, table := range []string{"keywords", "replace_rules", "media_types", "media_extensions", "push_configs", "rss_configs"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE rule_id=?", id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM rule_syncs WHERE rule_id=? OR sync_rule_id=?", id, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM forward_rules WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}
