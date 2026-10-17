// Package pyregex 将 Python re 的分组及替换语法转换为有超时的回溯正则。
package pyregex

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dlclark/regexp2"
)

const matchTimeout = 250 * time.Millisecond

type expression struct {
	re      *regexp2.Regexp
	pattern string
	names   map[string]int
	groups  int
}

func compile(pattern string) (*expression, error) {
	translated, names, groups, err := translate(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(translated, 0)
	if err != nil {
		return nil, fmt.Errorf("正则表达式无效: %w", err)
	}
	re.MatchTimeout = matchTimeout
	return &expression{re: re, pattern: translated, names: names, groups: groups}, nil
}

func Search(pattern, text string) (bool, error) {
	expr, err := compile(pattern)
	if err != nil {
		return false, err
	}
	return expr.re.MatchString(text)
}

// Find 返回第一个完整匹配及按 Python 出现顺序排列的所有捕获组。
func Find(pattern, text string) ([]string, error) {
	expr, err := compile(pattern)
	if err != nil {
		return nil, err
	}
	match, err := expr.re.FindStringMatch(text)
	if err != nil || match == nil {
		return nil, err
	}
	groups := make([]string, expr.groups+1)
	for i := range groups {
		groups[i] = match.GroupByNumber(i).String()
	}
	return groups, nil
}

func Replace(pattern, replacement, text string) (string, error) {
	expr, err := compile(pattern)
	if err != nil {
		return "", err
	}
	parts, err := expr.replacement(replacement)
	if err != nil {
		return "", err
	}
	runes := []rune(text)
	deadline := time.Now().Add(matchTimeout)
	match, err := expr.re.FindRunesMatch(runes)
	var output strings.Builder
	position := 0
	var nonempty *regexp2.Regexp
	for match != nil && err == nil {
		if time.Now().After(deadline) {
			return "", errors.New("正则替换超时")
		}
		output.WriteString(string(runes[position:match.Index]))
		output.WriteString(expand(parts, match))
		position = match.Index + match.Length
		if match.Length == 0 {
			// Python 3.7+ 允许空匹配之后在同一位置匹配非空分支。
			if nonempty == nil {
				nonempty, err = regexp2.Compile(`\G(?:`+expr.pattern+`)(?!\G)`, 0)
				if err != nil {
					return "", err
				}
				nonempty.MatchTimeout = matchTimeout
			}
			next, retryErr := nonempty.FindRunesMatchStartingAt(runes, position)
			if retryErr != nil {
				return "", retryErr
			}
			if next != nil {
				// 后续匹配必须回到原正则，不能用带 \G 的辅助正则继续。
				output.WriteString(expand(parts, next))
				position = next.Index + next.Length
				match, err = expr.re.FindRunesMatchStartingAt(runes, position)
				continue
			}
		}
		match, err = expr.re.FindNextMatch(match)
	}
	if err != nil {
		return "", err
	}
	output.WriteString(string(runes[position:]))
	return output.String(), nil
}

type replacementPart struct {
	text  string
	group int
}

func expand(parts []replacementPart, match *regexp2.Match) string {
	var result strings.Builder
	for _, part := range parts {
		if part.group >= 0 {
			result.WriteString(match.GroupByNumber(part.group).String())
		} else {
			result.WriteString(part.text)
		}
	}
	return result.String()
}

func (e *expression) replacement(input string) ([]replacementPart, error) {
	var parts []replacementPart
	var literal strings.Builder
	flush := func() {
		if literal.Len() != 0 {
			parts = append(parts, replacementPart{text: literal.String(), group: -1})
			literal.Reset()
		}
	}
	for i := 0; i < len(input); i++ {
		if input[i] != '\\' {
			literal.WriteByte(input[i])
			continue
		}
		i++
		if i == len(input) {
			return nil, errors.New("替换内容以未转义的反斜杠结尾")
		}
		ch := input[i]
		if ch == 'g' {
			if i+1 >= len(input) || input[i+1] != '<' {
				return nil, errors.New("替换分组引用必须使用 \\g<name> 或 \\g<number>")
			}
			end := strings.IndexByte(input[i+2:], '>')
			if end < 0 {
				return nil, errors.New("替换分组引用缺少 >")
			}
			name := input[i+2 : i+2+end]
			group, err := strconv.Atoi(name)
			if err != nil {
				var ok bool
				group, ok = e.names[name]
				if !ok {
					return nil, fmt.Errorf("替换引用未知分组 %q", name)
				}
			}
			if group < 0 || group > e.groups {
				return nil, fmt.Errorf("替换引用不存在的分组 %d", group)
			}
			flush()
			parts = append(parts, replacementPart{group: group})
			i += end + 2
			continue
		}
		if ch >= '0' && ch <= '9' {
			start := i
			if ch == '0' || i+2 < len(input) && octal(input[i]) && octal(input[i+1]) && octal(input[i+2]) {
				for i+1 < len(input) && i-start < 2 && octal(input[i+1]) {
					i++
				}
				n, err := strconv.ParseInt(input[start:i+1], 8, 32)
				if err != nil || n > 255 {
					return nil, errors.New("替换内容的八进制转义超出 0o377")
				}
				literal.WriteRune(rune(n))
				continue
			}
			if i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9' {
				i++
			}
			group, _ := strconv.Atoi(input[start : i+1])
			if group > e.groups {
				return nil, fmt.Errorf("替换引用不存在的分组 %d", group)
			}
			flush()
			parts = append(parts, replacementPart{group: group})
			continue
		}
		switch ch {
		case '\\':
			literal.WriteByte('\\')
		case 'n':
			literal.WriteByte('\n')
		case 'r':
			literal.WriteByte('\r')
		case 't':
			literal.WriteByte('\t')
		case 'a':
			literal.WriteByte('\a')
		case 'b':
			literal.WriteByte('\b')
		case 'f':
			literal.WriteByte('\f')
		case 'v':
			literal.WriteByte('\v')
		default:
			if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' {
				return nil, fmt.Errorf("替换内容含未知转义 \\%c", ch)
			}
			literal.WriteByte('\\')
			literal.WriteByte(ch)
		}
	}
	flush()
	return parts, nil
}

func octal(ch byte) bool { return ch >= '0' && ch <= '7' }

func validName(name string) bool {
	for i, ch := range name {
		if ch != '_' && !unicode.IsLetter(ch) && !(i > 0 && unicode.IsDigit(ch)) {
			return false
		}
	}
	return name != ""
}
