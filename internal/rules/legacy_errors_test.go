package rules

import (
	"context"
	"testing"

	"github.com/iLay1678/telegram-forwarder/internal/model"
)

func TestInvalidLegacyPatternsDoNotStopValidItems(t *testing.T) {
	rule := model.Rule{ForwardMode: "WHITELIST", Keywords: []model.Keyword{
		{Keyword: "[", IsRegex: true}, {Keyword: "hello"},
	}}
	if ok, err := MatchKeywords(rule, "hello"); err != nil || !ok {
		t.Fatalf("无效历史正则不应阻止其他关键词: %t %v", ok, err)
	}
	rule.IsReplace = true
	rule.Replacements = []model.Replacement{{Pattern: "[", Content: "wrong"}, {Pattern: "hello", Content: "你好"}}
	if got, err := ReplaceText(rule, "hello"); err != nil || got != "你好" {
		t.Fatalf("无效历史替换应保留文本并继续: %q %v", got, err)
	}
	if got, err := ReplaceText(rule, ""); err != nil || got != "" {
		t.Fatalf("空文本不应用替换: %q %v", got, err)
	}
}

type commentTransport struct{ *fakeTransport }

func (commentTransport) DiscussionLink(context.Context, model.Peer, int) (string, error) {
	return "https://t.me/test/7?comment=8", nil
}

func TestAlbumCommentIsReplyToDeliveredMessage(t *testing.T) {
	e, transport, _ := newTestEngine(t)
	e.User = commentTransport{transport}
	rule := baseRule()
	rule.EnableCommentButton = true
	msg := message()
	msg.GroupedID = 123
	if err := e.Process(context.Background(), rule, msg); err != nil {
		t.Fatal(err)
	}
	if len(transport.sent) != 2 || transport.sent[1].ReplyTo != 42 || transport.sent[1].Text != "💬 评论区" {
		t.Fatalf("媒体组应使用机器人回复评论按钮: %+v", transport.sent)
	}
	if len(transport.sent[0].Buttons) != 0 || transport.sent[1].Buttons[0][0].URL != "https://t.me/test/7?comment=8" {
		t.Fatal("评论按钮位置或链接错误")
	}
}
