package telegram

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

func mockClient(t *testing.T, invoke gotd.InvokeFunc) *Client {
	t.Helper()
	s, err := openState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{state: s, api: tg.NewClient(invoke), mediaSlots: make(chan struct{}, 2)}
}

func TestNativeForwardPreservesSourceAndBatches(t *testing.T) {
	count := 0
	c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		req, ok := input.(*tg.MessagesForwardMessagesRequest)
		if !ok {
			t.Fatalf("意外请求 %T", input)
		}
		if req.DropAuthor || req.DropMediaCaptions || len(req.ID) > 100 || len(req.RandomID) != len(req.ID) {
			t.Fatalf("原生转发丢失来源或分批错误: %+v", req)
		}
		if req.FromPeer.(*tg.InputPeerChannel).ChannelID != 1 || req.ToPeer.(*tg.InputPeerChat).ChatID != 2 {
			t.Fatal("转发来源或目标错误")
		}
		updates := &tg.Updates{}
		for i, id := range req.ID {
			if id != count+1 || req.RandomID[i] == 0 {
				t.Fatal("消息顺序或去重标识错误")
			}
			count++
			updates.Updates = append(updates.Updates, &tg.UpdateMessageID{ID: id + 1000, RandomID: req.RandomID[i]})
		}
		output.(*tg.UpdatesBox).Updates = updates
		return nil
	})
	var source []int
	for id := 1; id <= 103; id++ {
		source = append(source, id)
	}
	ids, err := c.Forward(context.Background(), model.Peer{ID: 1, Kind: "channel"}, model.Peer{ID: 2, Kind: "chat"}, source)
	if err != nil || count != 103 || len(ids) != 103 || ids[102] != 1103 {
		t.Fatalf("转发失败: %d %v %v", count, ids, err)
	}
}

func TestRegisterCommandsUsesDefaultScope(t *testing.T) {
	c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		req := input.(*tg.BotsSetBotCommandsRequest)
		if _, ok := req.Scope.(*tg.BotCommandScopeDefault); !ok || len(req.Commands) != 2 || req.Commands[0].Command != "start" || req.Commands[1].Description != "设置" {
			t.Fatalf("命令菜单不兼容: %+v", req)
		}
		output.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		return nil
	})
	c.bot = true
	if err := c.RegisterCommands(context.Background(), []Command{{Name: "/start", Description: "开始"}, {Name: "settings", Description: "设置"}}); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumUploadAndButtons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, []byte("synthetic jpeg"), 0600); err != nil {
		t.Fatal(err)
	}
	uploads, sent, edits := 0, 0, 0
	c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.UploadSaveFilePartRequest:
			output.(*tg.BoolBox).Bool = &tg.BoolTrue{}
		case *tg.MessagesUploadMediaRequest:
			if _, ok := req.Media.(*tg.InputMediaUploadedPhoto); !ok {
				t.Fatalf("照片被当成文件上传: %T", req.Media)
			}
			uploads++
			output.(*tg.MessageMediaBox).MessageMedia = &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: int64(uploads), AccessHash: 123}}
		case *tg.MessagesSendMultiMediaRequest:
			sent++
			if len(req.MultiMedia) != 2 || req.MultiMedia[0].Message != "相册" || req.MultiMedia[1].Message != "" {
				t.Fatalf("相册标题错误: %+v", req.MultiMedia)
			}
			if reply, ok := req.GetReplyTo(); !ok || reply.(*tg.InputReplyToMessage).ReplyToMsgID != 9 {
				t.Fatal("相册回复关系丢失")
			}
			output.(*tg.UpdatesBox).Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 50}, &tg.UpdateMessageID{ID: 51}}}
		case *tg.MessagesEditMessageRequest:
			edits++
			if req.ID != 50 || req.Message != "相册" {
				t.Fatal("相册按钮未附在首条消息")
			}
			if _, ok := req.GetReplyMarkup(); !ok {
				t.Fatal("相册按钮丢失")
			}
			output.(*tg.UpdatesBox).Updates = &tg.Updates{}
		default:
			t.Fatalf("意外请求 %T", input)
		}
		return nil
	})
	media := model.Media{Kind: "photo", Path: path}
	ids, err := c.Send(context.Background(), model.Peer{ID: 1, Kind: "chat"}, model.Outgoing{Text: "**相册**", Media: []model.Media{media, media}, ReplyTo: 9, Buttons: [][]model.Button{{{Text: "评论", URL: "https://t.me/example/1"}}}})
	if err != nil || uploads != 2 || sent != 1 || edits != 1 || !reflect.DeepEqual(ids, []int{50, 51}) {
		t.Fatalf("相册流程错误: ids=%v uploads=%d sent=%d edits=%d err=%v", ids, uploads, sent, edits, err)
	}
}

func TestChannelPermissions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		participant tg.ChannelParticipantClass
		err         error
		want        bool
	}{{"管理员", &tg.ChannelParticipantAdmin{}, nil, true}, {"所有者", &tg.ChannelParticipantCreator{}, nil, true}, {"成员", &tg.ChannelParticipant{}, nil, false}, {"未加入", nil, tgerr.New(400, "USER_NOT_PARTICIPANT"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
				if tc.err != nil {
					return tc.err
				}
				output.(*tg.ChannelsChannelParticipant).Participant = tc.participant
				return nil
			})
			got, err := c.IsChannelAdmin(context.Background(), model.Peer{ID: 1, Kind: "channel"}, 2)
			if err != nil || got != tc.want {
				t.Fatalf("权限判定错误: %v %v", got, err)
			}
		})
	}
}

func TestFloodWaitCanBeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	middleware := rpcMiddleware{timeout: time.Second}.Handle(gotd.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error {
		cancel()
		return tgerr.New(420, "FLOOD_WAIT_30")
	}))
	if err := middleware.Invoke(ctx, &tg.MessagesGetHistoryRequest{}, &tg.MessagesMessagesBox{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("限流等待没有响应退出信号: %v", err)
	}
}

func TestMarkdownBoundaryCompatibility(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		count       int
	}{
		{"****", "****", 0},
		{"[]()", "[]()", 0},
		{"[文本]()", "[文本]()", 0},
		{"[**标签**](https://example.com)", "**标签**", 1},
		{"\n ** 😀 ** \t", "😀", 1},
		{"```x **原文**```", "x **原文**", 1},
	} {
		plain, entities, err := parseText(tc.input, "markdown")
		if err != nil || plain != tc.want || len(entities) != tc.count {
			t.Errorf("%q => %q %+v %v", tc.input, plain, entities, err)
		}
		for _, e := range entities {
			if e.GetOffset() < 0 || e.GetLength() <= 0 {
				t.Errorf("剪裁后实体无效: %+v", e)
			}
		}
	}
}

func TestImportedChannelAndRemovedUsername(t *testing.T) {
	c := mockClient(t, func(context.Context, bin.Encoder, bin.Decoder) error { return nil })
	if err := c.state.putPeers([]model.Peer{{ID: 5, Kind: "channel", Username: "old_name", AccessHash: 99}}); err != nil {
		t.Fatal(err)
	}
	m := c.convertMessage(&tg.Message{ID: 10, PeerID: &tg.PeerChannel{ChannelID: 5}, Post: true, Message: "帖子"})
	if !m.Chat.Broadcast {
		t.Fatal("旧会话导入后的频道帖子未识别为广播频道")
	}
	if err := c.saveEntities(nil, []tg.ChatClass{&tg.Channel{ID: 5, Title: "频道", Broadcast: true}}); err != nil {
		t.Fatal(err)
	}
	p := c.cached(model.Peer{ID: 5, Kind: "channel"})
	if p.Username != "" || p.AccessHash != 99 || messageLink(p, 10) != "https://t.me/c/5/10" {
		t.Fatalf("移除用户名后仍使用失效的消息链接: %+v", p)
	}
}

func TestStartupUpdatesWaitForCursorManager(t *testing.T) {
	var received []int
	gate := &startupUpdates{handler: gotd.UpdateHandlerFunc(func(_ context.Context, raw tg.UpdatesClass) error {
		received = append(received, raw.(*tg.UpdateShortMessage).ID)
		return nil
	})}
	ctx := context.Background()
	if err := gate.Handle(ctx, &tg.UpdateShortMessage{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 {
		t.Fatal("授权期间短更新绕过了游标管理器")
	}
	if err := gate.start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := gate.Handle(ctx, &tg.UpdateShortMessage{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(received, []int{1, 2}) || len(gate.pending) != 0 {
		t.Fatalf("启动缓冲丢失更新: %v", received)
	}
}

func TestLoginInputCanBeCancelled(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	auth := interactiveAuth{reader: bufio.NewReader(r)}
	if _, err := auth.ask(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等待验证码时没有响应退出信号: %v", err)
	}
}

func TestHistorySkipsServiceMessagesWithoutEndingPagination(t *testing.T) {
	calls := 0
	c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		calls++
		req := input.(*tg.MessagesGetHistoryRequest)
		var messages []tg.MessageClass
		if calls == 1 {
			if req.OffsetID != 0 || req.Limit != 2 {
				t.Fatalf("历史首批请求错误: %+v", req)
			}
			messages = []tg.MessageClass{&tg.MessageService{ID: 10}, &tg.Message{ID: 9, PeerID: &tg.PeerChat{ChatID: 1}, Message: "新消息"}}
		} else {
			if req.OffsetID != 9 || req.Limit != 1 {
				t.Fatalf("系统消息导致分页游标错误: %+v", req)
			}
			messages = []tg.MessageClass{&tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 1}, Message: "旧消息"}}
		}
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: messages}
		return nil
	})
	messages, err := c.History(context.Background(), model.Peer{ID: 1, Kind: "chat"}, 0, 2)
	if err != nil || calls != 2 || len(messages) != 2 || messages[1].ID != 8 {
		t.Fatalf("服务消息截断了历史: %v %v", messages, err)
	}
}

func TestBotResolvesLegacyPositiveChannelIDWithoutCache(t *testing.T) {
	var methods []string
	c := mockClient(t, func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		switch req := input.(type) {
		case *tg.UsersGetUsersRequest:
			methods = append(methods, "user")
			output.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.UserEmpty{ID: 123}}
		case *tg.ChannelsGetChannelsRequest:
			methods = append(methods, "channel")
			channel := req.ID[0].(*tg.InputChannel)
			if channel.ChannelID != 123 || channel.AccessHash != 0 {
				t.Fatalf("机器人使用了其他账号的 hash: %+v", channel)
			}
			output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: 123, AccessHash: 456, Broadcast: true, Title: "频道"}}}
		default:
			t.Fatalf("意外请求 %T", input)
		}
		return nil
	})
	c.bot = true
	p, err := c.Resolve(context.Background(), "123")
	if err != nil || p.Kind != "channel" || p.ID != 123 || p.AccessHash != 456 || !reflect.DeepEqual(methods, []string{"user", "channel"}) {
		t.Fatalf("旧 raw ID 解析错误: %+v %v %v", p, methods, err)
	}
	if _, err := c.Resolve(context.Background(), "123"); err != nil || len(methods) != 2 {
		t.Fatal("解析后的频道实体没有缓存")
	}
}

func TestCheckPeerRejectsForbiddenChannel(t *testing.T) {
	c := mockClient(t, func(_ context.Context, _ bin.Encoder, output bin.Decoder) error {
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.ChannelForbidden{ID: 1}}}
		return nil
	})
	if err := c.CheckPeer(context.Background(), model.Peer{ID: 1, Kind: "channel"}); err == nil {
		t.Fatal("频道不可访问时探测应返回错误")
	}
}
