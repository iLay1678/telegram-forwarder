package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
)

func keywordMigrationFixture(t *testing.T, constraint bool) string {
	t.Helper()
	path := legacyDB(t, "historical.sql")
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	unique := ""
	if constraint {
		unique = `, CONSTRAINT "old keyword uniqueness" UNIQUE("rule_id", [keyword], ` + "`is_regex`" + `)`
	}
	ddl := `DROP TABLE keywords;
CREATE TABLE keywords (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 rule_id INTEGER NOT NULL REFERENCES forward_rules(id),
 keyword TEXT COLLATE NOCASE,
 is_regex BOOLEAN,
 is_blacklist BOOLEAN,
 extra BLOB DEFAULT X'0102',
 description TEXT DEFAULT 'text, UNIQUE(rule_id,keyword,is_regex)',
 generated TEXT GENERATED ALWAYS AS (keyword || ', generated') STORED,
 CHECK(length(keyword)>0)` + unique + `);
INSERT INTO keywords(id,rule_id,keyword,is_regex,is_blacklist,extra) VALUES(103,41,'新闻',0,1,X'CAFE');
INSERT INTO keywords(id,rule_id,keyword,is_regex,is_blacklist) VALUES(999,41,'sequence',0,1);
DELETE FROM keywords WHERE id=999;
CREATE TABLE keyword_refs(id INTEGER PRIMARY KEY, keyword_id INTEGER REFERENCES keywords(id) ON DELETE CASCADE);
INSERT INTO keyword_refs VALUES(27,103);
CREATE TABLE keyword_audit(keyword_id INTEGER);
CREATE INDEX keyword_extra_idx ON keywords(extra);
CREATE TRIGGER keyword_insert_audit AFTER INSERT ON keywords BEGIN INSERT INTO keyword_audit VALUES(NEW.id); END;
CREATE VIEW keyword_view AS SELECT id,extra FROM keywords;`
	if !constraint {
		ddl += `CREATE UNIQUE INDEX old_keyword_index ON keywords(rule_id,keyword,is_regex);`
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKeywordUniquenessMigrationPreservesDataAndSchema(t *testing.T) {
	for _, constraint := range []bool{false, true} {
		name := "explicit_index"
		if constraint {
			name = "table_constraint"
		}
		t.Run(name, func(t *testing.T) {
			path := keywordMigrationFixture(t, constraint)
			s := openTestStore(t, path)
			var value string
			if err := s.DB.Get(&value, `SELECT hex(extra)||':'||generated FROM keywords WHERE id=103`); err != nil || value != "CAFE:新闻, generated" {
				t.Fatalf("未知字段或生成列丢失: %q %v", value, err)
			}
			var count int
			for _, query := range []string{
				`SELECT COUNT(*) FROM keyword_refs WHERE id=27 AND keyword_id=103`,
				`SELECT COUNT(*) FROM keyword_view WHERE id=103`,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='keyword_extra_idx'`,
			} {
				if err := s.DB.Get(&count, query); err != nil || count != 1 {
					t.Fatal("外键、视图或索引丢失", query, count, err)
				}
			}
			if err := s.DB.Get(&count, `SELECT COUNT(*) FROM keyword_audit`); err != nil || count != 0 {
				t.Fatal("迁移不应重复触发业务触发器", count, err)
			}
			if err := s.DB.Get(&count, `PRAGMA foreign_keys`); err != nil || count != 1 {
				t.Fatal("迁移后未开启外键", err)
			}
			result, err := s.DB.Exec(`INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(41,'新闻',0,0)`)
			if err != nil {
				t.Fatal("同词黑白名单仍互斥", err)
			}
			id, err := result.LastInsertId()
			if err != nil || id <= 999 {
				t.Fatal("自增序列回退", id, err)
			}
			if err := s.DB.Get(&count, `SELECT COUNT(*) FROM keyword_audit WHERE keyword_id=?`, id); err != nil || count != 1 {
				t.Fatal("原触发器未恢复", err)
			}
			if _, err := s.DB.Exec(`INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(41,'新闻',0,0)`); err == nil {
				t.Fatal("四列唯一约束未生效")
			}
			if _, err := s.DB.Exec(`INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(41,'',0,0)`); err == nil {
				t.Fatal("原 CHECK 约束丢失")
			}
			backups, _ := filepath.Glob(path + ".backup-*")
			if len(backups) != 1 {
				t.Fatalf("预迁移备份 = %v", backups)
			}
			backup, err := sqlx.Open("sqlite", backups[0])
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			if _, err := backup.Exec(`INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(41,'新闻',0,0)`); err == nil {
				t.Fatal("备份未保留迁移前唯一约束")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			_ = openTestStore(t, path)
			backups, _ = filepath.Glob(path + ".backup-*")
			if len(backups) != 1 {
				t.Fatal("重复启动不幂等", backups)
			}
		})
	}
}

func TestKeywordMigrationFailureRollsBackOriginalSchema(t *testing.T) {
	path := keywordMigrationFixture(t, true)
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 同名的用户扩展索引使最后一步失败，验证整个重建和补列均能回滚。
	if _, err := db.Exec(`CREATE INDEX unique_rule_keyword_is_regex_is_blacklist ON keyword_refs(keyword_id)`); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := db.Get(&original, `SELECT sql FROM sqlite_master WHERE name='keywords'`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("预期索引冲突失败")
	}
	var after string
	if err := db.Get(&after, `SELECT sql FROM sqlite_master WHERE name='keywords'`); err != nil || after != original {
		t.Fatal("失败后未恢复原 DDL", err)
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM keyword_refs WHERE keyword_id=103`); err != nil || count != 1 {
		t.Fatal("失败后丢失关联", count, err)
	}
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_table_info('forward_rules') WHERE name='forward_mode'`); err != nil || count != 0 {
		t.Fatal("失败后仍残留迁移字段", err)
	}
	if _, err := db.Exec(`INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(41,'新闻',0,0)`); err == nil {
		t.Fatal("原约束未回滚")
	}
}

func TestKeywordMigrationDoesNotOverwriteExistingTemporaryTable(t *testing.T) {
	path := keywordMigrationFixture(t, true)
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE __telegram_forwarder_keywords_migration (marker TEXT); INSERT INTO __telegram_forwarder_keywords_migration VALUES('keep')`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("同名扩展表应阻止迁移")
	}
	var marker string
	if err := db.Get(&marker, `SELECT marker FROM __telegram_forwarder_keywords_migration`); err != nil || marker != "keep" {
		t.Fatal("覆盖了未知表", err)
	}
}

func TestHistoricalMissingColumnsKeepEnabledDefaults(t *testing.T) {
	s := openTestStore(t, legacyDB(t, "historical.sql"))
	rule, err := s.Rule(context.Background(), 41)
	if err != nil {
		t.Fatal(err)
	}
	if !rule.EnableRule || !rule.UseBot || !rule.IsTopSummary || !rule.Keywords[0].IsBlacklist {
		t.Fatal("缺失列应补旧版默认值", rule)
	}
}

func TestKeywordDDLParserPreservesUnrelatedConstraints(t *testing.T) {
	original := `CREATE TABLE "keywords" (id INTEGER PRIMARY KEY, rule_id INT, keyword TEXT DEFAULT 'a,b', is_regex BOOL, is_blacklist BOOL,
/* comment UNIQUE(not,a,match) */ CONSTRAINT old UNIQUE(rule_id, keyword COLLATE NOCASE, is_regex DESC),
CONSTRAINT other UNIQUE(id, keyword), CHECK(keyword != 'UNIQUE(rule_id,keyword,is_regex)')) STRICT`
	got, err := removeLegacyKeywordConstraint(original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "CONSTRAINT old") || !strings.Contains(got, "CONSTRAINT other") || !strings.Contains(got, "CHECK(keyword") || !strings.HasSuffix(got, "STRICT") {
		t.Fatal(got)
	}
}
