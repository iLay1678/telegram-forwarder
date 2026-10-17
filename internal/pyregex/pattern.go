package pyregex

import (
	"fmt"
	"strconv"
	"strings"
)

type flags struct{ verbose, ascii bool }

func translate(pattern string) (string, map[string]int, int, error) {
	var output strings.Builder
	names := make(map[string]int)
	groups := 0
	var current flags
	var scopes []flags
	inClass := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		if ch == '\\' && i+1 < len(pattern) {
			i++
			escaped := pattern[i]
			switch escaped {
			case 'Z':
				output.WriteString(`\z`)
			case 'w':
				word := `\p{L}\p{N}_`
				if current.ascii {
					word = "a-zA-Z0-9_"
				}
				if inClass {
					output.WriteString(word)
				} else {
					output.WriteString("[" + word + "]")
				}
			case 'W':
				if inClass {
					output.WriteString(`\W`)
					break
				}
				word := `\p{L}\p{N}_`
				if current.ascii {
					word = "a-zA-Z0-9_"
				}
				output.WriteString("[^" + word + "]")
			case 'd', 'D', 's', 'S':
				if current.ascii {
					classes := map[byte]string{'d': "0-9", 'D': "^0-9", 's': `\t\n\r\f\v `, 'S': `^\t\n\r\f\v `}
					if inClass && escaped >= 'a' {
						output.WriteString(classes[escaped])
					} else if !inClass {
						output.WriteString("[" + classes[escaped] + "]")
					} else {
						output.WriteByte('\\')
						output.WriteByte(escaped)
					}
				} else if escaped == 's' {
					if inClass {
						output.WriteString(`\s\x1c-\x1f`)
					} else {
						output.WriteString(`[\s\x1c-\x1f]`)
					}
				} else if escaped == 'S' && !inClass {
					output.WriteString(`[^\s\x1c-\x1f]`)
				} else {
					output.WriteByte('\\')
					output.WriteByte(escaped)
				}
			case 'b', 'B':
				if inClass {
					output.WriteByte('\\')
					output.WriteByte(escaped)
					break
				}
				word := `[\p{L}\p{N}_]`
				if current.ascii {
					word = `[a-zA-Z0-9_]`
				}
				if escaped == 'b' {
					output.WriteString(`(?:(?<=` + word + `)(?!` + word + `)|(?<!` + word + `)(?=` + word + `))`)
				} else {
					output.WriteString(`(?:(?<=` + word + `)(?=` + word + `)|(?<!` + word + `)(?!` + word + `))`)
				}
			default:
				output.WriteByte('\\')
				output.WriteByte(escaped)
			}
			continue
		}
		if ch == '[' && !inClass {
			inClass = true
			output.WriteByte(ch)
			continue
		}
		if inClass {
			if ch == ']' {
				inClass = false
			}
			output.WriteByte(ch)
			continue
		}
		if current.verbose && ch == '#' {
			end := strings.IndexByte(pattern[i:], '\n')
			if end < 0 {
				output.WriteString(pattern[i:])
				break
			}
			output.WriteString(pattern[i : i+end+1])
			i += end
			continue
		}
		if ch == ')' {
			if len(scopes) > 0 {
				current = scopes[len(scopes)-1]
				scopes = scopes[:len(scopes)-1]
			}
			output.WriteByte(ch)
			continue
		}
		if ch != '(' {
			output.WriteByte(ch)
			continue
		}
		remaining := pattern[i:]
		if strings.HasPrefix(remaining, "(?#") {
			end := strings.IndexByte(remaining, ')')
			if end < 0 {
				return "", nil, 0, fmt.Errorf("正则注释未结束")
			}
			output.WriteString(remaining[:end+1])
			i += end
			continue
		}
		if strings.HasPrefix(remaining, "(?P=") {
			end := strings.IndexByte(remaining, ')')
			if end < 0 {
				return "", nil, 0, fmt.Errorf("命名分组引用未结束")
			}
			name := remaining[4:end]
			number, ok := names[name]
			if !ok {
				return "", nil, 0, fmt.Errorf("引用未知分组 %q", name)
			}
			output.WriteString(`\k<` + strconv.Itoa(number) + ">")
			i += end
			continue
		}
		if strings.HasPrefix(remaining, "(?P<") {
			end := strings.IndexByte(remaining, '>')
			if end < 0 {
				return "", nil, 0, fmt.Errorf("命名分组未结束")
			}
			name := remaining[4:end]
			if !validName(name) || names[name] != 0 {
				return "", nil, 0, fmt.Errorf("命名分组名称无效或重复: %q", name)
			}
			groups++
			names[name] = groups
			output.WriteString("(?<" + strconv.Itoa(groups) + ">")
			scopes = append(scopes, current)
			i += end
			continue
		}
		if strings.HasPrefix(remaining, "(?(") {
			end := strings.IndexByte(remaining[3:], ')')
			if end < 0 {
				return "", nil, 0, fmt.Errorf("条件分组未结束")
			}
			name := remaining[3 : 3+end]
			if number, ok := names[name]; ok {
				name = strconv.Itoa(number)
			}
			output.WriteString("(?(" + name + ")")
			scopes = append(scopes, current)
			i += end + 3
			continue
		}
		if strings.HasPrefix(remaining, "(?") {
			end := 2
			for end < len(remaining) && strings.ContainsRune("aiLmsux-", rune(remaining[end])) {
				end++
			}
			if end > 2 && end < len(remaining) && (remaining[end] == ')' || remaining[end] == ':') {
				previous := current
				var translated strings.Builder
				enable := true
				for _, flag := range remaining[2:end] {
					switch flag {
					case '-':
						enable = false
						translated.WriteRune(flag)
					case 'a':
						current.ascii = enable
					case 'u':
						current.ascii = !enable
					case 'L':
						return "", nil, 0, fmt.Errorf("字符串正则不支持 locale 标记 L")
					case 'x':
						current.verbose = enable
						translated.WriteRune(flag)
					default:
						translated.WriteRune(flag)
					}
				}
				value := strings.TrimSuffix(translated.String(), "-")
				if remaining[end] == ':' {
					scopes = append(scopes, previous)
					output.WriteString("(?" + value + ":")
				} else if value != "" {
					output.WriteString("(?" + value + ")")
				}
				i += end
				continue
			}
			scopes = append(scopes, current)
			output.WriteByte(ch)
			continue
		}
		groups++
		scopes = append(scopes, current)
		output.WriteString("(?<" + strconv.Itoa(groups) + ">")
	}
	return output.String(), names, groups, nil
}
