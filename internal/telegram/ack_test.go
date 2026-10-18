package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

func TestUpdateAcknowledgementStopsAtFailedMessage(t *testing.T) {
	c := mockClient(t, nil)
	want := errors.New("处理尚未完成")
	count := 0
	c.OnMessageAck = func(context.Context, model.Message) error {
		count++
		return want
	}
	err := c.handleUpdates(context.Background(), &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: 42}}},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: 42}}},
	}})
	if !errors.Is(err, want) || count != 1 {
		t.Fatalf("未完成更新不应确认或继续消费: count=%d err=%v", count, err)
	}
}

func TestMessageDispatchDeduplicatesUpdates(t *testing.T) {
	c := mockClient(t, nil)
	count := 0
	c.OnMessage = func(context.Context, model.Message) { count++ }
	update := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: 42}}},
	}}
	if err := c.handleUpdates(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if err := c.handleUpdates(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("重复更新不应重复入队: count=%d", count)
	}
}
