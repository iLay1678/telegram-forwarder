package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type transportStub struct {
	model.Transport
	pages         map[int][]model.Message
	offsets       []int
	sends         []model.Outgoing
	pinned        int
	markupFailure bool
}

func (f *transportStub) Resolve(_ context.Context, _ string) (model.Peer, error) {
	return model.Peer{ID: 20, Name: "new name"}, nil
}
func (f *transportStub) Dialogs(_ context.Context) ([]model.Peer, error) {
	return []model.Peer{{ID: 10, Kind: "channel", Name: "source renamed"}, {ID: 20, Kind: "channel", Name: "target renamed"}}, nil
}
func (f *transportStub) History(_ context.Context, _ model.Peer, offset, limit int) ([]model.Message, error) {
	f.offsets = append(f.offsets, offset)
	return f.pages[offset], nil
}
func (f *transportStub) Send(_ context.Context, _ model.Peer, out model.Outgoing) ([]int, error) {
	if f.markupFailure && out.Mode == "MARKDOWN" {
		f.markupFailure = false
		return nil, errors.New("rpc error MARKUP_INVALID")
	}
	f.sends = append(f.sends, out)
	return []int{100 + len(f.sends)}, nil
}
func (f *transportStub) Pin(_ context.Context, _ model.Peer, id int) error { f.pinned = id; return nil }

type aiStub struct {
	output, model, prompt, input string
	calls                        int
}

func (a *aiStub) Process(_ context.Context, model, prompt, input string, _ []model.AIImage) (string, error) {
	a.calls++
	a.model = model
	a.prompt = prompt
	a.input = input
	return a.output, nil
}

func setupScheduler(t *testing.T) (*Scheduler, *transportStub, *aiStub, model.Rule) {
	t.Helper()
	t.Setenv("SUMMARY_BATCH_DELAY", "0")
	t.Setenv("SUMMARY_BATCH_SIZE", "2")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(cfg.Root, "forward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	rule, err := store.Bind(context.Background(), model.Peer{ID: 10, Kind: "channel", Name: "source"}, model.Peer{ID: 20, Kind: "channel", Name: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("UPDATE forward_rules SET summary_time='08:00',ai_model='',summary_prompt='',is_top_summary=1 WHERE id=?", rule.ID); err != nil {
		t.Fatal(err)
	}
	f := &transportStub{pages: map[int][]model.Message{}}
	a := &aiStub{output: strings.Repeat("📚内容\n", 1500)}
	s := New(cfg, store, f, f, a)
	s.now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, cfg.Timezone) }
	return s, f, a, rule
}

func TestSummaryHistoryRangePaginationSplitAndPin(t *testing.T) {
	s, f, a, rule := setupScheduler(t)
	now := s.now()
	start := time.Date(2026, 10, 16, 8, 0, 0, 0, s.Config.Timezone)
	f.pages[0] = []model.Message{{ID: 10, Date: now.Add(time.Hour), Text: "future"}, {ID: 9, Date: now, Text: "newest"}}
	f.pages[9] = []model.Message{{ID: 8, Date: now.Add(-time.Hour), Text: "middle"}, {ID: 7, Date: start, Text: "boundary"}}
	f.pages[7] = []model.Message{{ID: 6, Date: start.Add(-time.Second), Text: "old"}}
	f.markupFailure = true
	if err := s.Summary(context.Background(), rule.ID); err != nil {
		t.Fatal(err)
	}
	if a.input != "newest\nmiddle\nboundary" || a.calls != 1 {
		t.Fatalf("总结范围或顺序不兼容: %q", a.input)
	}
	if a.model != "gpt-4o" || a.prompt == "" {
		t.Fatal("默认模型或提示缺失")
	}
	if len(f.offsets) != 3 || f.offsets[1] != 9 || f.offsets[2] != 7 {
		t.Fatal("分页游标错误", f.offsets)
	}
	if len(f.sends) < 2 || f.pinned != 101 || f.sends[0].Mode != "TEXT" {
		t.Fatalf("分长/纯文本重试/置顶失败: sends=%d pin=%d", len(f.sends), f.pinned)
	}
	if !strings.Contains(f.sends[0].Text, "28小时消息总结") || !strings.Contains(f.sends[0].Text, "消息数量: 3 条") {
		t.Fatal("总结头信息错误")
	}
	for _, out := range f.sends {
		if len(utf16.Encode([]rune(out.Text))) > 4096 {
			t.Fatal("总结超过 Telegram UTF-16 长度限制")
		}
	}
}

func TestNoMessagesNoAIAndScheduledDisabled(t *testing.T) {
	s, f, a, rule := setupScheduler(t)
	if err := s.Summary(context.Background(), rule.ID); err != nil {
		t.Fatal(err)
	}
	if a.calls != 0 || len(f.sends) != 0 {
		t.Fatal("空历史不应调用 AI 或发消息")
	}
	f.pages[0] = []model.Message{{ID: 1, Text: "message", Date: s.now()}}
	if err := s.summarize(context.Background(), rule.ID, false); err != nil {
		t.Fatal(err)
	}
	if a.calls != 0 {
		t.Fatal("关闭总结后仍执行定时总结")
	}
}

func TestHistoryDetectsStalledCursorAndCancellation(t *testing.T) {
	s, f, _, _ := setupScheduler(t)
	now := s.now()
	f.pages[0] = []model.Message{{ID: 9, Date: now, Text: "one"}}
	f.pages[9] = f.pages[0]
	if _, err := s.history(context.Background(), model.Peer{}, now.Add(-time.Hour), now); err == nil {
		t.Fatal("重复游标未终止")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.history(ctx, model.Peer{}, now.Add(-time.Hour), now); !errors.Is(err, context.Canceled) {
		t.Fatal("取消没有传播", err)
	}
}

func TestNextRunUsesCalendarDayAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 7, 8, 0, 0, 0, zone)
	next, err := nextRun(now, "08:00")
	if err != nil {
		t.Fatal(err)
	}
	if next.Day() != 8 || next.Hour() != 8 || next.Sub(now) != 23*time.Hour {
		t.Fatal("夏令时应按本地日历调度", next)
	}
	if _, err = nextRun(now, "25:00"); err == nil {
		t.Fatal("接受了无效时间")
	}
}

func TestChatNamesRefreshFromDialogs(t *testing.T) {
	s, _, _, rule := setupScheduler(t)
	if err := s.UpdateChats(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := s.Store.Rule(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source.Name != "source renamed" || r.Target.Name != "target renamed" {
		t.Fatalf("名称没有刷新: %s %s", r.Source.Name, r.Target.Name)
	}
}
