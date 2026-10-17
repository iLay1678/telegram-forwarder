package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jmoiron/sqlx"
)

type keywordIndexMigration struct {
	drop    map[string]bool
	rebuild bool
	create  bool
}

func (m keywordIndexMigration) changed() bool { return m.rebuild || m.create || len(m.drop) > 0 }

func (s *Store) keywordIndexes(ctx context.Context) (keywordIndexMigration, error) {
	m := keywordIndexMigration{drop: make(map[string]bool), create: true}
	var indexes []struct {
		Name   string `db:"name"`
		Origin string `db:"origin"`
	}
	if err := s.DB.SelectContext(ctx, &indexes, `SELECT name, origin FROM pragma_index_list('keywords') WHERE "unique"=1 AND partial=0`); err != nil {
		return m, err
	}
	for _, index := range indexes {
		var columns []string
		if err := s.DB.SelectContext(ctx, &columns, `SELECT COALESCE(name,'') FROM pragma_index_info(?) ORDER BY seqno`, index.Name); err != nil {
			return m, err
		}
		if sameColumns(columns, "rule_id", "keyword", "is_regex", "is_blacklist") {
			m.create = false
		}
		if sameColumns(columns, "rule_id", "keyword", "is_regex") {
			if index.Origin == "u" {
				m.rebuild = true
			} else if index.Origin == "c" {
				m.drop[index.Name] = true
			}
		}
	}
	return m, nil
}

func sameColumns(columns []string, expected ...string) bool {
	if len(columns) != len(expected) {
		return false
	}
	seen := make(map[string]bool, len(columns))
	for _, name := range columns {
		seen[strings.ToLower(name)] = true
	}
	for _, name := range expected {
		if !seen[name] {
			return false
		}
	}
	return true
}

func quoteIdentifier(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func (m keywordIndexMigration) apply(ctx context.Context, tx *sqlx.Tx) error {
	if !m.changed() {
		return nil
	}
	if m.rebuild {
		if err := m.rebuildTable(ctx, tx); err != nil {
			return err
		}
	} else {
		for name := range m.drop {
			if _, err := tx.ExecContext(ctx, "DROP INDEX "+quoteIdentifier(name)); err != nil {
				return err
			}
		}
	}
	if m.create {
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX unique_rule_keyword_is_regex_is_blacklist ON keywords(rule_id,keyword,is_regex,is_blacklist)`); err != nil {
			return err
		}
	}
	if m.rebuild {
		var violations int
		if err := tx.GetContext(ctx, &violations, "SELECT COUNT(*) FROM pragma_foreign_key_check"); err != nil {
			return err
		}
		if violations != 0 {
			return fmt.Errorf("外键校验失败（%d 条），迁移已回滚", violations)
		}
	}
	return nil
}

func (m keywordIndexMigration) rebuildTable(ctx context.Context, tx *sqlx.Tx) error {
	var original string
	if err := tx.GetContext(ctx, &original, `SELECT sql FROM sqlite_master WHERE type='table' AND name='keywords'`); err != nil {
		return err
	}
	definition, err := removeLegacyKeywordConstraint(original)
	if err != nil {
		return err
	}
	var objects []struct {
		Name string `db:"name"`
		SQL  string `db:"sql"`
	}
	if err := tx.SelectContext(ctx, &objects, `SELECT name, sql FROM sqlite_master WHERE tbl_name='keywords' AND type IN ('index','trigger') AND sql IS NOT NULL`); err != nil {
		return err
	}
	var columns []string
	if err := tx.SelectContext(ctx, &columns, `SELECT name FROM pragma_table_xinfo('keywords') WHERE hidden=0 ORDER BY cid`); err != nil {
		return err
	}
	for i, name := range columns {
		columns[i] = quoteIdentifier(name)
	}
	projection := strings.Join(columns, ",")
	var sequence sql.NullInt64
	var hasSequence int
	if err := tx.GetContext(ctx, &hasSequence, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='sqlite_sequence'`); err != nil {
		return err
	}
	if hasSequence != 0 {
		if err := tx.GetContext(ctx, &sequence, `SELECT seq FROM sqlite_sequence WHERE name='keywords'`); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// 保留原 DDL 的未知列、CHECK、外键和生成列。复制回原名避免 SQLite 重命名
	// 改写外部视图或外键；触发器在复制完数据后恢复，不对旧数据重复执行。
	const temporary = `"__telegram_forwarder_keywords_migration"`
	statements := []string{
		"CREATE TABLE " + temporary + definition,
		"INSERT INTO " + temporary + "(" + projection + ") SELECT " + projection + " FROM keywords",
		"DROP TABLE keywords",
		"CREATE TABLE keywords" + definition,
		"INSERT INTO keywords(" + projection + ") SELECT " + projection + " FROM " + temporary,
		"DROP TABLE " + temporary,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if sequence.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=MAX(seq,?) WHERE name='keywords'`, sequence.Int64); err != nil {
			return err
		}
	}
	for _, object := range objects {
		if !m.drop[object.Name] {
			if _, err := tx.ExecContext(ctx, object.SQL); err != nil {
				return err
			}
		}
	}
	return nil
}

type sqlToken struct {
	text       string
	start, end int
	quoted     bool
}

// 只识别 DDL 的边界，不重新生成字段定义，避免丢弃未知类型、约束或排序规则。
func sqlTokens(input string) ([]sqlToken, error) {
	var tokens []sqlToken
	for i := 0; i < len(input); {
		if strings.HasPrefix(input[i:], "--") {
			if end := strings.IndexByte(input[i:], '\n'); end >= 0 {
				i += end + 1
				continue
			}
			break
		}
		if strings.HasPrefix(input[i:], "/*") {
			end := strings.Index(input[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("表结构包含未结束的注释")
			}
			i += end + 4
			continue
		}
		if unicode.IsSpace(rune(input[i])) {
			i++
			continue
		}
		start := i
		if strings.ContainsRune("\"'`[", rune(input[i])) {
			quote := input[i]
			if quote == '[' {
				quote = ']'
			}
			i++
			var value strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == quote {
					i++
					if quote != ']' && i < len(input) && input[i] == quote {
						value.WriteByte(quote)
						i++
						continue
					}
					closed = true
					break
				}
				value.WriteByte(input[i])
				i++
			}
			if !closed {
				return nil, errors.New("表结构包含未结束的引号")
			}
			tokens = append(tokens, sqlToken{value.String(), start, i, true})
			continue
		}
		if strings.ContainsRune("(),", rune(input[i])) {
			i++
		} else {
			for i < len(input) && !unicode.IsSpace(rune(input[i])) && !strings.ContainsRune("(),\"'`[", rune(input[i])) && !strings.HasPrefix(input[i:], "--") && !strings.HasPrefix(input[i:], "/*") {
				i++
			}
		}
		tokens = append(tokens, sqlToken{input[start:i], start, i, false})
	}
	return tokens, nil
}

func removeLegacyKeywordConstraint(original string) (string, error) {
	tokens, err := sqlTokens(original)
	if err != nil {
		return "", err
	}
	depth, start, first := 0, 0, 0
	removed := false
	var parts []string
	for i, token := range tokens {
		if token.quoted {
			continue
		}
		switch token.text {
		case "(":
			depth++
			if depth == 1 {
				start, first = token.end, i+1
			}
		case ")", ",":
			if depth != 1 {
				if token.text == ")" {
					depth--
				}
				continue
			}
			if legacyUniqueClause(tokens[first:i]) {
				removed = true
			} else {
				parts = append(parts, original[start:token.start])
			}
			if token.text == ")" {
				if !removed {
					return "", errors.New("无法安全定位旧关键词 UNIQUE 约束")
				}
				return " (" + strings.Join(parts, ",") + original[token.start:], nil
			}
			start, first = token.end, i+1
		}
	}
	return "", errors.New("无法解析旧关键词表结构")
}

func legacyUniqueClause(tokens []sqlToken) bool {
	if len(tokens) > 2 && !tokens[0].quoted && strings.EqualFold(tokens[0].text, "CONSTRAINT") {
		tokens = tokens[2:]
	}
	if len(tokens) < 4 || tokens[0].quoted || !strings.EqualFold(tokens[0].text, "UNIQUE") || tokens[1].text != "(" {
		return false
	}
	var columns []string
	next := true
	for _, token := range tokens[2:] {
		if !token.quoted && token.text == ")" {
			return sameColumns(columns, "rule_id", "keyword", "is_regex")
		}
		if !token.quoted && token.text == "," {
			next = true
		} else if next {
			columns = append(columns, token.text)
			next = false
		}
	}
	return false
}
