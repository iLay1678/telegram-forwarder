package rules

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

func TestKeywordModes(t *testing.T) {
	tests := []struct {
		mode                       string
		reverseBlack, reverseWhite bool
		accepted                   []string
	}{
		{"BLACKLIST", false, false, []string{"none", "white"}},
		{"WHITELIST", false, false, []string{"white", "black white"}},
		{"WHITELIST_THEN_BLACKLIST", false, false, []string{"white"}},
		{"BLACKLIST_THEN_WHITELIST", false, false, []string{"white"}},
		{"BLACKLIST", false, true, []string{"none"}},
		{"WHITELIST", true, false, []string{"black white"}},
		{"WHITELIST_THEN_BLACKLIST", true, false, []string{"black white"}},
		{"BLACKLIST_THEN_WHITELIST", false, true, []string{"none"}},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+strings.Join(tt.accepted, ","), func(t *testing.T) {
			rule := model.Rule{ForwardMode: tt.mode, EnableReverseBlacklist: tt.reverseBlack, EnableReverseWhitelist: tt.reverseWhite, Keywords: []model.Keyword{{Keyword: "black", IsBlacklist: true}, {Keyword: "white"}}}
			for _, value := range []string{"none", "black", "white", "black white"} {
				want := false
				for _, accepted := range tt.accepted {
					want = want || value == accepted
				}
				got, err := MatchKeywords(rule, strings.ToUpper(value))
				if err != nil || got != want {
					t.Fatalf("%q: got %t, %v; want %t", value, got, err, want)
				}
			}
		})
	}
}

func TestMediaFilterAndReplacement(t *testing.T) {
	rule := model.Rule{EnableMediaTypeFilter: true, MediaTypes: map[string]bool{"document": true}}
	media := []model.Media{{Kind: "video", Filename: "video.MP4"}, {Kind: "photo"}}
	allowed, _, pass := FilterMedia(rule, media)
	if !pass || len(allowed) != 1 || allowed[0].Kind != "photo" {
		t.Fatal(allowed, pass)
	}
	_, _, pass = FilterMedia(rule, media[:1])
	if pass {
		t.Fatal("屏蔽全部媒体时应停止")
	}
	rule.MediaAllowText = true
	_, _, pass = FilterMedia(rule, media[:1])
	if !pass {
		t.Fatal("允许文本时不应停止")
	}
	rule = model.Rule{EnableMediaSizeFilter: true, MaxMediaSize: 1, IsSendOverMediaSizeMessage: true}
	allowed, notices, pass := FilterMedia(rule, []model.Media{{Kind: "document", Size: 2 << 20}})
	if !pass || len(allowed) != 0 || len(notices) != 1 {
		t.Fatal(allowed, notices, pass)
	}
	rule = model.Rule{IsReplace: true, Replacements: []model.Replacement{{Pattern: `(?P<n>\d+)`, Content: `[\g<n>]`}, {Pattern: `.*`, Content: "complete"}, {Pattern: "complete", Content: "wrong"}}}
	if got, err := ReplaceText(rule, "text 123"); err != nil || got != "complete" {
		t.Fatal(got, err)
	}
}

func TestSplitTextPreservesUnicode(t *testing.T) {
	text := strings.Repeat("中文😀\n", 1500)
	parts := SplitText(text, 4096)
	if strings.Join(parts, "") != text {
		t.Fatal("拆分丢失内容")
	}
	for _, part := range parts {
		if len(utf16.Encode([]rune(part))) > 4096 {
			t.Fatal("超出Telegram长度")
		}
	}
}

type fakeTransport struct {
	model.Transport
	calls     *[]string
	sent      []model.Outgoing
	sendErr   error
	deleted   []int
	forwarded []int
}

func (f *fakeTransport) Resolve(context.Context, string) (model.Peer, error) {
	return model.Peer{ID: 22, Kind: "channel"}, nil
}
func (f *fakeTransport) Send(_ context.Context, _ model.Peer, out model.Outgoing) ([]int, error) {
	*f.calls = append(*f.calls, "send")
	f.sent = append(f.sent, out)
	return []int{42}, f.sendErr
}
func (f *fakeTransport) Delete(_ context.Context, _ model.Peer, ids []int) error {
	*f.calls = append(*f.calls, "delete")
	f.deleted = append(f.deleted, ids...)
	return nil
}
func (f *fakeTransport) Forward(_ context.Context, _, _ model.Peer, ids []int) ([]int, error) {
	*f.calls = append(*f.calls, "forward")
	f.forwarded = ids
	return []int{42}, nil
}

type fakeRSS struct{ calls *[]string }

func (f fakeRSS) Publish(context.Context, model.Rule, model.Message, string, []model.Media) error {
	*f.calls = append(*f.calls, "rss")
	return nil
}

type fakePush struct {
	calls *[]string
	err   error
}

func (f fakePush) Send(context.Context, []model.PushConfig, string, []model.Media) error {
	*f.calls = append(*f.calls, "push")
	return f.err
}

type fakeAI struct {
	calls *[]string
	text  string
}

func (f fakeAI) Process(context.Context, string, string, string, []model.AIImage) (string, error) {
	*f.calls = append(*f.calls, "ai")
	return f.text, nil
}

func newTestEngine(t *testing.T) (*Engine, *fakeTransport, *[]string) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	transport := &fakeTransport{calls: &calls}
	return New(cfg, nil, transport, transport), transport, &calls
}
func baseRule() model.Rule {
	return model.Rule{ID: 1, EnableRule: true, UseBot: true, ForwardMode: "BLACKLIST", IsDeleteOriginal: true, Target: model.Chat{TelegramChatID: "22"}}
}
func message() model.Message {
	return model.Message{ID: 7, Text: "hello", Chat: model.Peer{ID: 11, Kind: "channel"}, Date: time.Now()}
}

func TestDeliveryFailureNeverDeletesSource(t *testing.T) {
	for _, mode := range []string{"telegram", "push", "empty", "no-enabled-push"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, calls := newTestEngine(t)
			r := baseRule()
			m := message()
			switch mode {
			case "telegram":
				tr.sendErr = errors.New("send failed")
			case "push":
				r.EnablePush = true
				r.PushConfigs = []model.PushConfig{{EnablePushChannel: true}}
				e.Push = fakePush{calls: calls, err: errors.New("push failed")}
			case "empty":
				m.Text = ""
			case "no-enabled-push":
				r.EnableOnlyPush = true
				r.EnablePush = true
				e.Push = fakePush{calls: calls}
			}
			err := e.Process(context.Background(), r, m)
			if (mode == "telegram" || mode == "push") && err == nil {
				t.Fatal("发送失败应返回错误")
			}
			if len(tr.deleted) > 0 {
				t.Fatal("未成功完成投递却删除了来源")
			}
		})
	}
}

func TestDeliveryOrderAndOnlyFlags(t *testing.T) {
	for _, mode := range []string{"all", "rss", "push", "rss-disabled"} {
		t.Run(mode, func(t *testing.T) {
			e, _, calls := newTestEngine(t)
			r := baseRule()
			r.EnablePush = true
			r.PushConfigs = []model.PushConfig{{EnablePushChannel: true}}
			r.RSS = &model.RSSConfig{EnableRSS: true}
			e.RSS = fakeRSS{calls}
			e.Push = fakePush{calls: calls}
			want := []string{"rss", "send", "push", "delete"}
			switch mode {
			case "rss":
				r.OnlyRSS = true
				want = []string{"rss"}
			case "push":
				r.EnableOnlyPush = true
				want = []string{"rss", "push", "delete"}
			case "rss-disabled":
				r.OnlyRSS = true
				e.RSS = nil
				want = []string{"send", "push", "delete"}
			}
			if err := e.Process(context.Background(), r, message()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*calls, want) {
				t.Fatal(*calls, want)
			}
		})
	}
}

func TestNativeForwardDoesNotRewriteOrDelete(t *testing.T) {
	e, tr, calls := newTestEngine(t)
	r := baseRule()
	r.UseBot = false
	r.IsAI = true
	r.IsReplace = true
	r.Replacements = []model.Replacement{{Pattern: ".*", Content: "changed"}}
	m := message()
	m.Media = []model.Media{{MessageID: 9}, {MessageID: 8}}
	m.GroupedID = 10
	if err := e.Process(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*calls, []string{"forward"}) || !reflect.DeepEqual(tr.forwarded, []int{7, 8, 9}) {
		t.Fatal(*calls, tr.forwarded)
	}
}

func TestAISecondKeywordPassCanStopDelivery(t *testing.T) {
	e, tr, calls := newTestEngine(t)
	r := baseRule()
	r.IsAI = true
	r.IsKeywordAfterAI = true
	r.Keywords = []model.Keyword{{Keyword: "blocked", IsBlacklist: true}}
	e.AI = fakeAI{calls, "blocked"}
	if err := e.Process(context.Background(), r, message()); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) > 0 || len(tr.deleted) > 0 {
		t.Fatal("AI后过滤应停止投递")
	}
}

func TestMultipleRulesDeleteAfterAllDestinations(t *testing.T) {
	e, _, calls := newTestEngine(t)
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e.Store = store
	ctx := context.Background()
	for _, target := range []int64{22, 33} {
		r, err := store.Bind(ctx, model.Peer{ID: 11, Kind: "channel"}, model.Peer{ID: target, Kind: "channel"})
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]any{"use_bot": true, "forward_mode": "BLACKLIST", "is_delete_original": true} {
			if err = store.UpdateRule(ctx, r.ID, key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = e.handle(ctx, message()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*calls, []string{"send", "send", "delete"}) {
		t.Fatal(*calls)
	}
}
