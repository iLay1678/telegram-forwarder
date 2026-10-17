package pyregex

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPythonSearch(t *testing.T) {
	cases := []struct {
		pattern, input string
		want           bool
	}{
		{`(?<=.)(?=$)`, "你好😀", true},
		{`(?P<word>新闻)\s+(?P=word)`, "新闻 新闻", true},
		{`(?P<word>新闻)\s+(?P=word)`, "新闻 消息", false},
		{`(?i)news(?=！)`, "NEWS！", true},
		{`(?u)^\w+$`, "中文²_", true},
		{`(?a)^\w+$`, "中文", false},
		{`(?u)^\w+$`, "e\u0301", false},
		{`(?a:\w+)\w+`, "abc中文", true},
		{`\b²\b`, "²", true},
		{`\s`, "\x1c", true},
		{`a\Z`, "a\n", false},
		{`a$`, "a\n", true},
		{`(?P<a>x)(?(a)y|z)`, "xy", true},
		{"(?x)(a) # (ignored)\n(?P<b>b)", "ab", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+tc.input, func(t *testing.T) {
			got, err := Search(tc.pattern, tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("Search(%q,%q) = %v, %v; want %v", tc.pattern, tc.input, got, err, tc.want)
			}
		})
	}
}

func TestPythonReplacementAndCaptureNumbering(t *testing.T) {
	cases := []struct{ pattern, replacement, input, want string }{
		{`(?P<first>中)(文)`, `\1-\2-\g<first>-$1`, "中文", "中-文-中-$1"},
		{`(?P<first>中)(文)`, `\g<0>\n\2`, "中文", "中文\n文"},
		{`(?<=中)(文)`, `[\1]`, "中文", "中[文]"},
		{`(a)?b`, `\1`, "b ab", " a"},
		{`a`, `\\1`, "a", `\1`},
		{`a`, `\101\0`, "a", "A\x00"},
		{`a`, `\&`, "a", `\&`},
		{`x*`, "-", "abxd", "-a-b--d-"},
		{`|a`, "x", "a", "xxx"},
		{`.*?`, "-", "ab", "-----"},
		{`(?P<x>a)\1`, `\g<x>`, "aa", "a"},
		{`(?=😀)`, "+", "中😀😀", "中+😀+😀"},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+tc.replacement, func(t *testing.T) {
			got, err := Replace(tc.pattern, tc.replacement, tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("Replace(%q,%q,%q) = %q, %v; want %q", tc.pattern, tc.replacement, tc.input, got, err, tc.want)
			}
		})
	}
}

func TestFindReturnsFirstMatchAndAllGroups(t *testing.T) {
	got, err := Find(`(?P<title>标题)(:)(内容)?`, "前缀标题: 后缀标题:内容")
	if err != nil || !reflect.DeepEqual(got, []string{"标题:", "标题", ":", ""}) {
		t.Fatalf("捕获组次序错误: %v, %v", got, err)
	}
	if got, err := Find(`未出现`, "正文"); err != nil || got != nil {
		t.Fatalf("无匹配返回错误: %v, %v", got, err)
	}
}

func TestInvalidExpressionsAndReplacements(t *testing.T) {
	for _, pattern := range []string{`[`, `(?P<bad`, `(?P<x>a)(?P<x>b)`, `(?P=missing)`} {
		if _, err := Search(pattern, "x"); err == nil {
			t.Fatalf("接受非法表达式 %q", pattern)
		}
	}
	for _, replacement := range []string{`\2`, `\g<missing>`, `\g<1`, `\q`, `\400`, `\`} {
		if _, err := Replace(`(x)`, replacement, "x"); err == nil {
			t.Fatalf("接受非法替换 %q", replacement)
		}
	}
}

func TestCatastrophicPatternTimesOut(t *testing.T) {
	start := time.Now()
	if _, err := Search(`^(a+)+$`, strings.Repeat("a", 10000)+"!"); err == nil {
		t.Fatal("灾难回溯未超时")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时保护过慢: %v", elapsed)
	}
}
