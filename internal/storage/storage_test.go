package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/jmoiron/sqlx"
)

func legacyDB(t *testing.T, fixture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forward.db")
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "legacy", fixture))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLegacyRelationsAndNullDefaults(t *testing.T) {
	s := openTestStore(t, legacyDB(t, "forward.sql"))
	rules, err := s.Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d", len(rules))
	}
	rule := rules[0]
	if rule.ID != 41 || rule.Source.ID != 7 || rule.Target.ID != 9 || rule.ForwardMode != "BLACKLIST_THEN_WHITELIST" {
		t.Fatalf("旧主键或枚举变化: %+v", rule)
	}
	if rule.UseBot || rule.EnableRule || rule.IsTopSummary || rule.IsSendOverMediaSizeMessage || rule.MaxMediaSize != 10 || rule.DelaySeconds != 5 || rule.SummaryTime != "07:00" {
		t.Fatalf("旧 NULL 布尔值被启用或数值缺省丢失: %+v", rule)
	}
	if rules[1].Keywords[0].IsBlacklist {
		t.Fatal("NULL 黑名单标记不能变成 true")
	}
	var unchanged int
	if err := s.DB.Get(&unchanged, "SELECT COUNT(*) FROM forward_rules WHERE id=41 AND enable_rule IS NULL AND use_bot IS NULL"); err != nil || unchanged != 1 {
		t.Fatal("读取规则改写了旧 NULL", err)
	}
	if len(rule.Keywords) != 2 || rule.Keywords[0].ID != 103 || rule.Keywords[1].IsBlacklist {
		t.Fatalf("关键字变化: %+v", rule.Keywords)
	}
	if len(rule.Replacements) != 2 || rule.Replacements[1].Content != "" {
		t.Fatalf("替换规则变化: %+v", rule.Replacements)
	}
	if !rule.MediaTypes["photo"] || rule.MediaTypes["document"] || len(rule.Extensions) != 1 {
		t.Fatalf("媒体关系错误: %+v", rule)
	}
	if len(rule.PushConfigs) != 1 || rule.PushConfigs[0].ID != 601 || rule.RSS == nil || rule.RSS.ID != 701 || rule.RSS.MaxItems != 50 || rule.RSS.Language != "zh-CN" {
		t.Fatalf("推送或 RSS 关系错误: %+v", rule)
	}
	var password string
	if err := s.DB.Get(&password, "SELECT password FROM users WHERE id=901"); err != nil {
		t.Fatal(err)
	}
	if password != "pbkdf2:sha256:1$fixture-salt$not-a-real-password" {
		t.Fatal("旧密码被修改")
	}
	var temporary string
	if err := s.DB.Get(&temporary, "SELECT keyword FROM keywords_temp WHERE id=999"); err != nil || temporary != "遗留内容" {
		t.Fatal("遗留表丢失", err)
	}
}

func TestHistoricalMigrationBacksUpAndPreservesIDs(t *testing.T) {
	path := legacyDB(t, "historical.sql")
	s := openTestStore(t, path)
	rule, err := s.Rule(context.Background(), 41)
	if err != nil {
		t.Fatal(err)
	}
	if rule.ForwardMode != "WHITELIST" || rule.Keywords[0].ID != 103 || !rule.Keywords[0].IsBlacklist || !rule.MediaTypes["photo"] || !rule.MediaTypes["video"] {
		t.Fatalf("旧版字段迁移错误: %+v", rule)
	}
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("迁移备份 = %v, %v", backups, err)
	}
	backup, err := sqlx.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var mode string
	if err := backup.Get(&mode, "SELECT mode FROM forward_rules WHERE id=41"); err != nil || mode != "WHITELIST" {
		t.Fatalf("备份无法读取: %s %v", mode, err)
	}
	var count int
	if err := backup.Get(&count, "SELECT COUNT(*) FROM pragma_table_info('forward_rules') WHERE name='forward_mode'"); err != nil || count != 0 {
		t.Fatal("备份包含迁移后的结构", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path)
	backups, _ = filepath.Glob(path + ".backup-*")
	if len(backups) != 1 {
		t.Fatalf("幂等启动产生额外迁移: %v", backups)
	}
}

func TestBindDefaultsOnLegacySchemaAndSelfBinding(t *testing.T) {
	s := openTestStore(t, legacyDB(t, "forward.sql"))
	s.ConfigureDefaults(27, "08:30")
	ctx := context.Background()
	rule, err := s.Bind(ctx, model.Peer{ID: 1007, Name: "更新来源"}, model.Peer{ID: 1020, Name: "新目标"})
	if err != nil {
		t.Fatal(err)
	}
	if !rule.UseBot || !rule.EnableRule || rule.MaxMediaSize != 27 || rule.SummaryTime != "08:30" || rule.MessageMode != "MARKDOWN" || rule.IsPreview != "FOLLOW" {
		t.Fatalf("旧库新增规则默认值错误: %+v", rule)
	}
	duplicate, err := s.Bind(ctx, model.Peer{ID: 1007}, model.Peer{ID: 1020})
	if err != nil || duplicate.ID != rule.ID {
		t.Fatal("重复绑定不幂等", err)
	}
	self, err := s.Bind(ctx, model.Peer{ID: 1007}, model.Peer{ID: 1007})
	if err != nil || self.ForwardMode != "WHITELIST" || self.AddMode != "WHITELIST" {
		t.Fatal("自绑定应默认白名单", err)
	}
	if _, err := s.Bind(ctx, model.Peer{ID: 1021}, model.Peer{ID: 1009}); err != nil {
		t.Fatal(err)
	}
	var selection string
	if err := s.DB.Get(&selection, "SELECT current_add_id FROM chats WHERE id=9"); err != nil || selection != "1007" {
		t.Fatal("绑定覆盖已有当前规则选择", selection, err)
	}
}

func TestRuleSyncAtomicAndDeletionRelations(t *testing.T) {
	s := openTestStore(t, legacyDB(t, "forward.sql"))
	ctx := context.Background()
	if err := s.UpdateRule(ctx, 41, "max_media_size", 42); err != nil {
		t.Fatal(err)
	}
	rule, err := s.Rule(ctx, 88)
	if err != nil || rule.MaxMediaSize != 42 {
		t.Fatal("规则配置未同步", err)
	}
	if err := s.UpdateRule(ctx, 41, "enable_rule", false); err != nil {
		t.Fatal(err)
	}
	rule, _ = s.Rule(ctx, 88)
	if rule.EnableRule {
		t.Fatal("规则开关不应同步")
	}
	if err := s.UpdateRule(ctx, 41, "ai_prompt = ''; DROP TABLE users; --", "bad"); err == nil {
		t.Fatal("接受了非白名单字段")
	}
	if err := s.UpdateRule(ctx, 41, "forward_mode", "whitelist"); err == nil {
		t.Fatal("接受了不兼容的枚举")
	}
	_, err = s.DB.Exec("CREATE TRIGGER reject_sync BEFORE UPDATE OF delay_seconds ON forward_rules WHEN NEW.id=88 BEGIN SELECT RAISE(ABORT,'fixture'); END")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRule(ctx, 41, "delay_seconds", 99); err == nil {
		t.Fatal("未传播同步失败")
	}
	rule, _ = s.Rule(ctx, 41)
	if rule.DelaySeconds != 5 {
		t.Fatal("同步失败未回滚源规则")
	}
	if err := s.DeleteRule(ctx, 41); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, 41); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("规则删除失败", err)
	}
	for _, table := range []string{"keywords", "replace_rules", "media_types", "media_extensions", "rule_syncs", "push_configs", "rss_configs", "rss_patterns"} {
		var count int
		query := "SELECT COUNT(*) FROM " + table
		if table == "keywords" {
			query += " WHERE rule_id=41"
		}
		if err := s.DB.Get(&count, query); err != nil || count != 0 {
			t.Fatalf("%s 仍有关联: %d %v", table, count, err)
		}
	}
	if _, err := s.Rule(ctx, 88); err != nil {
		t.Fatal("删除影响无关规则", err)
	}
}
