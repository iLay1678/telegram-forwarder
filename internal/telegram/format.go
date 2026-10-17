package telegram

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gotd/td/telegram/message/entity"
	tghtml "github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/tg"
)

func parseText(text, mode string) (string, []tg.MessageEntityClass, error) {
	switch strings.ToLower(mode) {
	case "html":
		var b entity.Builder
		if err := tghtml.HTML(strings.NewReader(text), &b, tghtml.Options{DisableTelegramEscape: true}); err != nil {
			return "", nil, err
		}
		plain, entities := b.Raw()
		plain, entities = trimFormatted(plain, entities)
		sort.SliceStable(entities, func(i, j int) bool {
			if entities[i].GetOffset() != entities[j].GetOffset() {
				return entities[i].GetOffset() < entities[j].GetOffset()
			}
			return entities[i].GetLength() > entities[j].GetLength()
		})
		return plain, entities, nil
	case "markdown", "md", "":
		plain, entities := parseMarkdown(text, 0, 0)
		plain, entities = trimFormatted(plain, entities)
		return plain, entities, nil
	case "text", "plain", "none":
		return text, nil, nil
	default:
		return "", nil, fmt.Errorf("未知消息格式 %q", mode)
	}
}

// Telethon Markdown 使用 **、__、~~、` 和 ```，不同于 Bot API MarkdownV2。
func parseMarkdown(source string, offset, depth int) (string, []tg.MessageEntityClass) {
	if depth > 32 {
		return source, nil
	}
	var out strings.Builder
	var entities []tg.MessageEntityClass
	length := 0
	write := func(s string) { out.WriteString(s); length += entity.ComputeLength(s) }
	for i := 0; i < len(source); {
		matched := false
		for _, delim := range []string{"```", "**", "__", "~~", "`"} {
			if !strings.HasPrefix(source[i:], delim) {
				continue
			}
			// Telethon 不会把空定界符（例如 ****）当成一个空实体。
			searchFrom := i + len(delim)
			if searchFrom >= len(source) {
				break
			}
			_, runeSize := utf8.DecodeRuneInString(source[searchFrom:])
			searchFrom += runeSize
			end := strings.Index(source[searchFrom:], delim)
			if end < 0 {
				break
			}
			end += searchFrom
			inner := source[i+len(delim) : end]
			start := offset + length
			var nested []tg.MessageEntityClass
			if delim != "`" && delim != "```" {
				inner, nested = parseMarkdown(inner, start, depth+1)
			}
			n := entity.ComputeLength(inner)
			write(inner)
			if n > 0 {
				var e tg.MessageEntityClass
				switch delim {
				case "**":
					e = &tg.MessageEntityBold{Offset: start, Length: n}
				case "__":
					e = &tg.MessageEntityItalic{Offset: start, Length: n}
				case "~~":
					e = &tg.MessageEntityStrike{Offset: start, Length: n}
				case "`":
					e = &tg.MessageEntityCode{Offset: start, Length: n}
				case "```":
					e = &tg.MessageEntityPre{Offset: start, Length: n}
				}
				entities = append(entities, e)
				entities = append(entities, nested...)
			}
			i = end + len(delim)
			matched = true
			break
		}
		if matched {
			continue
		}
		if source[i] == '[' {
			close := strings.Index(source[i+1:], "](")
			if close > 0 && !strings.Contains(source[i+1:i+1+close], "]") {
				close += i + 1
				end := strings.IndexByte(source[close+2:], ')')
				if end > 0 {
					end += close + 2
					label := source[i+1 : close]
					link := source[close+2 : end]
					start := offset + length
					write(label)
					n := entity.ComputeLength(label)
					if n > 0 {
						entities = append(entities, &tg.MessageEntityTextURL{Offset: start, Length: n, URL: link})
					}
					i = end + 1
					continue
				}
			}
		}
		_, n := utf8.DecodeRuneInString(source[i:])
		write(source[i : i+n])
		i += n
	}
	return out.String(), entities
}

// trimFormatted 保留 Telethon 的首尾空白剪裁和 UTF-16 实体偏移。
func trimFormatted(text string, entities []tg.MessageEntityClass) (string, []tg.MessageEntityClass) {
	space := func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }
	leftTrimmed := strings.TrimLeftFunc(text, space)
	left := entity.ComputeLength(text) - entity.ComputeLength(leftTrimmed)
	text = strings.TrimRightFunc(leftTrimmed, space)
	right := left + entity.ComputeLength(text)
	valid := entities[:0]
	for _, e := range entities {
		start, end := max(e.GetOffset(), left), min(e.GetOffset()+e.GetLength(), right)
		if end <= start {
			continue
		}
		offset, length := start-left, end-start
		// gotd 的实体接口只有 getter；生成的实体都公开相同的两个范围字段。
		value := reflect.ValueOf(e).Elem()
		value.FieldByName("Offset").SetInt(int64(offset))
		value.FieldByName("Length").SetInt(int64(length))
		valid = append(valid, e)
	}
	return text, valid
}

// markdownText 与 Telethon Message.text 一样将已有格式转回 Markdown，供规则处理。
func markdownText(text string, entities []tg.MessageEntityClass) string {
	type mark struct {
		pos, other, order int
		text              string
		closing           bool
	}
	var marks []mark
	for i, e := range entities {
		open, close := "", ""
		switch v := e.(type) {
		case *tg.MessageEntityBold:
			open, close = "**", "**"
		case *tg.MessageEntityItalic:
			open, close = "__", "__"
		case *tg.MessageEntityStrike:
			open, close = "~~", "~~"
		case *tg.MessageEntityCode:
			open, close = "`", "`"
		case *tg.MessageEntityPre:
			open, close = "```", "```"
		case *tg.MessageEntityTextURL:
			open, close = "[", "]("+v.URL+")"
		case *tg.MessageEntityMentionName:
			open, close = "[", "](tg://user?id="+strconv.FormatInt(v.UserID, 10)+")"
		}
		if open != "" && e.GetLength() > 0 {
			start, end := e.GetOffset(), e.GetOffset()+e.GetLength()
			marks = append(marks, mark{pos: start, other: end, order: i, text: open}, mark{pos: end, other: start, order: i, text: close, closing: true})
		}
	}
	sort.SliceStable(marks, func(i, j int) bool {
		a, b := marks[i], marks[j]
		if a.pos != b.pos {
			return a.pos < b.pos
		}
		if a.closing != b.closing {
			return a.closing
		}
		if a.other != b.other {
			return a.other > b.other
		}
		if a.closing {
			return a.order > b.order
		}
		return a.order < b.order
	})
	units := utf16.Encode([]rune(text))
	var out strings.Builder
	pos := 0
	for _, m := range marks {
		if m.pos < pos || m.pos > len(units) {
			continue
		}
		out.WriteString(string(utf16.Decode(units[pos:m.pos])))
		out.WriteString(m.text)
		pos = m.pos
	}
	out.WriteString(string(utf16.Decode(units[pos:])))
	return out.String()
}
