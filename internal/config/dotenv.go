package config

import (
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 与原版 python-dotenv 1.0 的语法一致，包括单引号内插值、空声明及无效行跳过。
// 正则只解析语法；变量按文件顺序展开，并优先读取进程环境（override=false）。
const dotenvSpace = `\s\p{Z}\x{85}\x{1c}-\x{1f}`
const dotenvInlineSpace = `\t\v\f\p{Z}\x{85}\x{1c}-\x{1f}`

var (
	dotenvWhitespace = regexp.MustCompile(`^[` + dotenvSpace + `]*`)
	dotenvInline     = regexp.MustCompile(`^[` + dotenvInlineSpace + `]*`)
	dotenvExport     = regexp.MustCompile(`^export[` + dotenvInlineSpace + `]+`)
	dotenvQuotedKey  = regexp.MustCompile(`^'([^']+)'`)
	dotenvKey        = regexp.MustCompile(`^([^=#` + dotenvSpace + `]+)`)
	dotenvSingle     = regexp.MustCompile(`^'((?:\\'|[^'])*)'`)
	dotenvDouble     = regexp.MustCompile(`^"((?:\\"|[^"])*)"`)
	dotenvComment    = regexp.MustCompile(`[` + dotenvSpace + `]+#.*`)
	dotenvVariable   = regexp.MustCompile(`\$\{([^}:]*)(?::-([^}]*))?\}`)
	dotenvEscapes    = regexp.MustCompile(`\\[\\'"abfnrtv]`)
)

func readDotEnv(path string, environ []string) (map[string]string, error) {
	env := make(map[string]string, len(environ))
	for _, entry := range environ {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("dotenv 文件不是有效的 UTF-8")
	}
	// Python 以文本模式打开文件，统一 CRLF 和 CR 换行。
	input := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	fileValues := make(map[string]*string)
	for offset := 0; offset < len(input); {
		start := offset
		key, value, valid := readDotEnvBinding(input, &offset)
		if !valid {
			// 与旧版一致忽略非法行；不把可能含有凭据的原文写入日志。
			slog.Warn("无法解析 dotenv 行，已跳过", "line", strings.Count(input[:start], "\n")+1)
			continue
		}
		if key == "" {
			continue
		}
		if value != nil {
			resolved := dotenvVariable.ReplaceAllStringFunc(*value, func(match string) string {
				parts := dotenvVariable.FindStringSubmatch(match)
				if v, ok := env[parts[1]]; ok {
					return v
				}
				if v, ok := fileValues[parts[1]]; ok {
					if v != nil {
						return *v
					}
					return ""
				}
				return parts[2]
			})
			value = &resolved
		}
		fileValues[key] = value
	}
	for key, value := range fileValues {
		if _, exists := env[key]; !exists && value != nil {
			env[key] = *value
		}
	}
	return env, nil
}

func readDotEnvBinding(input string, offset *int) (key string, value *string, valid bool) {
	consume := func(pattern *regexp.Regexp) []string {
		match := pattern.FindStringSubmatch(input[*offset:])
		if match != nil {
			*offset += len(match[0])
		}
		return match
	}
	defer func() {
		if !valid {
			for *offset < len(input) && input[*offset] != '\n' {
				*offset++
			}
			if *offset < len(input) {
				*offset++
			}
		}
	}()
	consume(dotenvWhitespace)
	if *offset == len(input) {
		return "", nil, true
	}
	consume(dotenvExport)
	if *offset == len(input) {
		return "", nil, false
	}
	if input[*offset] != '#' {
		pattern := dotenvKey
		if input[*offset] == '\'' {
			pattern = dotenvQuotedKey
		}
		match := consume(pattern)
		if match == nil {
			return "", nil, false
		}
		key = match[1]
	}
	consume(dotenvInline)
	if *offset < len(input) && input[*offset] == '=' {
		*offset++
		consume(dotenvInline)
		v := ""
		if *offset < len(input) && (input[*offset] == '\'' || input[*offset] == '"') {
			quote := input[*offset]
			pattern := dotenvSingle
			if quote == '"' {
				pattern = dotenvDouble
			}
			match := consume(pattern)
			if match == nil {
				return "", nil, false
			}
			v = decodeDotEnvEscapes(match[1], quote)
		} else {
			start := *offset
			for *offset < len(input) && input[*offset] != '\n' {
				*offset++
			}
			v = strings.TrimRightFunc(dotenvComment.ReplaceAllString(input[start:*offset], ""), dotenvIsSpace)
		}
		value = &v
	}
	consume(dotenvInline)
	if *offset < len(input) && input[*offset] == '#' {
		for *offset < len(input) && input[*offset] != '\n' {
			*offset++
		}
	}
	consume(dotenvInline)
	if *offset == len(input) {
		return key, value, true
	}
	if input[*offset] != '\n' {
		return "", nil, false
	}
	*offset++
	return key, value, true
}

func dotenvIsSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f
}

func decodeDotEnvEscapes(value string, quote byte) string {
	return dotenvEscapes.ReplaceAllStringFunc(value, func(escape string) string {
		if quote == '\'' && escape[1] != '\\' && escape[1] != '\'' {
			return escape
		}
		switch escape[1] {
		case 'a':
			return "\a"
		case 'b':
			return "\b"
		case 'f':
			return "\f"
		case 'n':
			return "\n"
		case 'r':
			return "\r"
		case 't':
			return "\t"
		case 'v':
			return "\v"
		default:
			return string(escape[1])
		}
	})
}
