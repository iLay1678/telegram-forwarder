package rules

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type recordingAI struct {
	result, prompt, input string
	err                   error
}

func (a *recordingAI) Process(_ context.Context, _, prompt, text string, _ []model.AIImage) (string, error) {
	a.prompt, a.input = prompt, text
	return a.result, a.err
}

func TestAIFailureKeepsReplacedTextAndStillDelivers(t *testing.T) {
	e, tr, calls := newTestEngine(t)
	rule := baseRule()
	rule.IsAI, rule.IsReplace, rule.IsKeywordAfterAI = true, true, true
	rule.Replacements = []model.Replacement{{Pattern: "hello", Content: "替换后的正文"}}
	e.AI = &recordingAI{result: "不应投递的部分响应", err: errors.New("AI 请求失败")}
	if err := e.Process(context.Background(), rule, message()); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != 1 || tr.sent[0].Text != "替换后的正文" {
		t.Fatalf("AI 失败后正文丢失: %+v", tr.sent)
	}
	if !reflect.DeepEqual(*calls, []string{"send", "delete"}) {
		t.Fatalf("降级后仍应成功投递再删除: %v", *calls)
	}
}

func TestAISecondKeywordPassIncludesSender(t *testing.T) {
	e, tr, _ := newTestEngine(t)
	rule := baseRule()
	rule.IsAI, rule.IsKeywordAfterAI, rule.IsFilterUserInfo = true, true, true
	rule.ForwardMode = "WHITELIST"
	rule.Keywords = []model.Keyword{{Keyword: "指定作者"}}
	rule.AIPrompt = "改写 {Message}，保留 {Message} 的意思"
	msg := message()
	msg.Sender = model.Peer{ID: 99, Kind: "user", Name: "指定作者"}
	ai := &recordingAI{result: "AI 改写正文"}
	e.AI = ai
	if err := e.Process(context.Background(), rule, msg); err != nil {
		t.Fatal(err)
	}
	if len(tr.sent) != 1 || tr.sent[0].Text != ai.result {
		t.Fatalf("AI 二次过滤遗漏发送者信息: %+v", tr.sent)
	}
	if ai.prompt != "改写 hello，保留 hello 的意思" || ai.input != "hello" {
		t.Fatalf("消息变量未正确展开: %q / %q", ai.prompt, ai.input)
	}
}

func TestAICancellationNeverFallsBackToSending(t *testing.T) {
	e, tr, _ := newTestEngine(t)
	rule := baseRule()
	rule.IsAI = true
	e.AI = &recordingAI{err: context.Canceled}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Process(ctx, rule, message()); !errors.Is(err, context.Canceled) {
		t.Fatalf("应保留取消信号，实际为 %v", err)
	}
	if len(tr.sent) != 0 || len(tr.deleted) != 0 {
		t.Fatal("程序退出时不应继续发送或删除")
	}
}

type historyCall struct{ peer, offset, limit int }

type aiHistoryTransport struct {
	model.Transport
	messages               map[int64][]model.Message
	resolveErr, historyErr error
	calls                  []historyCall
}

func (f *aiHistoryTransport) Resolve(_ context.Context, raw string) (model.Peer, error) {
	if f.resolveErr != nil {
		return model.Peer{}, f.resolveErr
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return model.Peer{ID: id, Kind: "channel"}, err
}

func (f *aiHistoryTransport) History(_ context.Context, peer model.Peer, offset, limit int) ([]model.Message, error) {
	f.calls = append(f.calls, historyCall{int(peer.ID), offset, limit})
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	var result []model.Message
	for _, msg := range f.messages[peer.ID] {
		if offset == 0 || msg.ID < offset {
			result = append(result, msg)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func TestAIPromptExpandsSourceAndTargetHistory(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	e, _, _ := newTestEngine(t)
	tr := &aiHistoryTransport{messages: map[int64][]model.Message{
		11: {
			{ID: 4, Text: "源最新", Date: now.Add(-time.Minute)},
			{ID: 3, Text: "", Date: now.Add(-2 * time.Minute)},
			{ID: 2, Text: "源较早", Date: now.Add(-5 * time.Minute)},
			{ID: 1, Text: "源旧文", Date: now.Add(-6 * time.Minute)},
		},
		22: {
			{ID: 2, Text: "目标最新", Date: now.Add(-time.Minute)},
			{ID: 1, Text: "目标较早", Date: now.Add(-3 * time.Minute)},
		},
	}}
	e.User = tr
	rule := baseRule()
	rule.Source.TelegramChatID = "11"
	prompt := "{source_message_context:3}|{target_message_context:1}|{source_message_time:5}|{target_message_time:5}|{Message}"
	want := "源最新\n---\n源较早|目标最新|源较早\n---\n源最新|目标较早\n---\n目标最新|当前消息"
	if got := e.expandAIPrompt(context.Background(), rule, prompt, "当前消息", now); got != want {
		t.Fatalf("提示词不兼容\n实际: %q\n期望: %q", got, want)
	}
}

func TestAIHistoryPaginatesAndCountsMessagesWithoutText(t *testing.T) {
	e, _, _ := newTestEngine(t)
	tr := &aiHistoryTransport{messages: map[int64][]model.Message{}}
	for id := 205; id > 0; id-- {
		text := fmt.Sprint(id)
		if id == 200 {
			text = ""
		}
		tr.messages[11] = append(tr.messages[11], model.Message{ID: id, Text: text})
	}
	e.User = tr
	got, err := e.aiHistory(context.Background(), "11", "context", 101, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(got, "\n---\n")
	if len(parts) != 100 || parts[0] != "205" || parts[len(parts)-1] != "105" {
		t.Fatalf("数量模式的分页或无正文消息计数错误: %v", parts)
	}
	if !reflect.DeepEqual(tr.calls, []historyCall{{11, 0, 100}, {11, 106, 1}}) {
		t.Fatalf("分页请求错误: %v", tr.calls)
	}
}

func TestAIPromptHistoryFailureUsesEmptyContext(t *testing.T) {
	for _, mode := range []string{"resolve", "history"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _ := newTestEngine(t)
			tr := &aiHistoryTransport{}
			if mode == "resolve" {
				tr.resolveErr = errors.New("聊天不可访问")
			} else {
				tr.historyErr = errors.New("历史查询失败")
			}
			e.User = tr
			rule := baseRule()
			rule.Source.TelegramChatID = "11"
			prompt := "上下文:{source_message_context:2};正文:{Message}"
			if got := e.expandAIPrompt(context.Background(), rule, prompt, "保留正文", time.Now()); got != "上下文:;正文:保留正文" {
				t.Fatal(got)
			}
		})
	}
}
