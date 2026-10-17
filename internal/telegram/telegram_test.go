package telegram

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

func legacyFixture(t *testing.T, path string, key []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"CREATE TABLE version (version INTEGER PRIMARY KEY)", "INSERT INTO version VALUES (7)",
		"CREATE TABLE sessions (dc_id INTEGER PRIMARY KEY, server_address TEXT, port INTEGER, auth_key BLOB, takeout_id INTEGER)",
		"CREATE TABLE entities (id INTEGER PRIMARY KEY, hash INTEGER NOT NULL, username TEXT, phone INTEGER, name TEXT, date INTEGER)",
		"CREATE TABLE update_state (id INTEGER PRIMARY KEY, pts INTEGER, qts INTEGER, date INTEGER, seq INTEGER)",
		"INSERT INTO entities VALUES (-1000000000123, 456, 'test_channel', NULL, '测试频道', 1234)",
		"INSERT INTO entities VALUES (42, 789, NULL, NULL, '测试用户', 1234)",
		"INSERT INTO entities VALUES (0, 0, NULL, NULL, NULL, 1234)",
		"INSERT INTO update_state VALUES (0, 100, 2, 1720000000, 9)",
		"INSERT INTO update_state VALUES (123, 20, 0, 1720000000, 0)",
	} {
		if _, err = db.Exec(query); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("INSERT INTO sessions VALUES (2,'149.154.167.51',443,?,NULL)", key); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTelethonImportPreservesOriginal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "user.session")
	key := bytes.Repeat([]byte{0x42}, 256)
	legacyFixture(t, path, key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(&config.Config{Root: root, APIID: 12345, APIHash: "synthetic"}, false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := (&session.Loader{Storage: &fileSession{path: filepath.Join(root, "sessions", "user.gotd.json")}}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha1.Sum(key)
	if loaded.DC != 2 || loaded.Addr != "149.154.167.51:443" || !bytes.Equal(loaded.AuthKey, key) || !bytes.Equal(loaded.AuthKeyID, hash[12:]) {
		t.Fatal("导入授权或 DC 不一致")
	}
	p, ok := c.state.peer(model.Peer{ID: 123, Kind: "channel"})
	if !ok || p.AccessHash != 456 || p.Username != "test_channel" {
		t.Fatalf("实体未导入: %+v", p)
	}
	if c.legacy.State.Pts != 100 || c.legacy.Channels[123] != 20 {
		t.Fatal("旧更新游标未导入")
	}
	if len(c.legacy.Peers) != 2 {
		t.Fatalf("实体占位记录应跳过，其他缓存应保留: %+v", c.legacy.Peers)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Telethon 原始会话被修改")
	}
	// 再次启动必须使用 Go 会话，不覆盖授权或重新读取旧游标。
	second, err := New(&config.Config{Root: root, APIID: 12345, APIHash: "synthetic"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.legacy == nil {
		t.Fatal("首次授权前中断导致旧游标丢失")
	}
}

func TestTelethonRejectsCorruptAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.session")
	legacyFixture(t, path, []byte{1, 2, 3})
	if _, err := readTelethon(context.Background(), path); err == nil {
		t.Fatal("损坏授权应报错")
	}
}

func TestPersistentUpdateState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetPts(ctx, 1, 3); err == nil {
		t.Fatal("不存在的用户状态应报错")
	}
	for _, err = range []error{s.SetState(ctx, 1, updates.State{Pts: 2}), s.SetDateSeq(ctx, 1, 100, 5), s.SetChannelPts(ctx, 1, 42, 9), s.SetChannelAccessHash(ctx, 1, 42, 123)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err = openState(path)
	if err != nil {
		t.Fatal(err)
	}
	state, ok, err := s.GetState(ctx, 1)
	if err != nil || !ok || state.Pts != 2 || state.Date != 100 || state.Seq != 5 {
		t.Fatalf("状态未持久化: %+v %v", state, err)
	}
	if err = s.ForEachChannels(ctx, 1, func(_ context.Context, id int64, pts int) error {
		if id != 42 || pts != 9 {
			t.Fatal("频道游标错误")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if hash, ok, err := s.GetChannelAccessHash(ctx, 1, 42); err != nil || !ok || hash != 123 {
		t.Fatal("频道 hash 错误")
	}
}

func TestPeerIDs(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  model.Peer
	}{{"123", model.Peer{ID: 123, Kind: "user"}}, {"-123", model.Peer{ID: 123, Kind: "chat"}}, {"-1001234567890", model.Peer{ID: 1234567890, Kind: "channel"}}, {"channel:123", model.Peer{ID: 123, Kind: "channel"}}} {
		p, err := ParsePeerID(tc.value)
		if err != nil || p != tc.want {
			t.Errorf("%s: %+v %v", tc.value, p, err)
		}
		round, err := ParsePeerID(stringID(MarkedID(p)))
		if err != nil || round != p {
			t.Fatal("marked ID 往返失败")
		}
	}
	for _, bad := range []string{"0", "channel:-1", "unknown:1", "@name", "-1000000000000"} {
		p, err := ParsePeerID(bad)
		if err == nil && p.ID > 0 {
			t.Errorf("应拒绝 %q", bad)
		}
	}
}
func stringID(id int64) string { return strconv.FormatInt(id, 10) }

func TestTelethonMarkdown(t *testing.T) {
	text, entities, err := parseText("😀 **粗体 __斜体__** [链接](https://example.com) `a_b` ~~删~~", "MARKDOWN")
	if err != nil {
		t.Fatal(err)
	}
	if text != "😀 粗体 斜体 链接 a_b 删" {
		t.Fatalf("转换错误 %q", text)
	}
	if len(entities) != 5 {
		t.Fatalf("实体数错误 %d", len(entities))
	}
	bold := entities[0].(*tg.MessageEntityBold)
	if bold.Offset != 3 || bold.Length != 5 {
		t.Fatalf("UTF-16 范围错误: %+v", bold)
	}
	if reconstructed := markdownText(text, entities); reconstructed != "😀 **粗体 __斜体__** [链接](https://example.com) `a_b` ~~删~~" {
		t.Fatalf("格式往返错误 %q", reconstructed)
	}
	plain, entities, err := parseText("*不是粗体* _不是斜体_ **未闭合", "markdown")
	if err != nil || plain != "*不是粗体* _不是斜体_ **未闭合" || len(entities) != 0 {
		t.Fatalf("Telethon 方言不兼容 %q", plain)
	}
}

func TestHTMLFormat(t *testing.T) {
	text, entities, err := parseText(`<b>😀中文</b> &amp; <a href="https://example.com">链接</a>`, "HTML")
	if err != nil {
		t.Fatal(err)
	}
	if text != "😀中文 & 链接" || len(entities) != 2 {
		t.Fatalf("HTML 转换错误 %q %+v", text, entities)
	}
	if entities[0].GetLength() != 4 {
		t.Fatalf("Emoji 必须按 UTF-16 长度计算: %+v", entities)
	}
}

func TestSendTextReplyButtonsAndPreview(t *testing.T) {
	s, err := openState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{state: s, api: tg.NewClient(gotd.InvokeFunc(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		req, ok := input.(*tg.MessagesSendMessageRequest)
		if !ok {
			t.Fatalf("意外请求 %T", input)
		}
		if req.Message != "粗体" || req.NoWebpage || req.RandomID == 0 {
			t.Fatalf("错误请求 %+v", req)
		}
		if reply, ok := req.GetReplyTo(); !ok || reply.(*tg.InputReplyToMessage).ReplyToMsgID != 12 {
			t.Fatal("回复字段未设置")
		}
		if markup, ok := req.GetReplyMarkup(); !ok || len(markup.(*tg.ReplyInlineMarkup).Rows) != 1 {
			t.Fatal("按钮未设置")
		}
		if entities, ok := req.GetEntities(); !ok || len(entities) != 1 {
			t.Fatal("实体未设置")
		}
		output.(*tg.UpdatesBox).Updates = &tg.UpdateShortSentMessage{ID: 99}
		return nil
	}))}
	ids, err := c.Send(context.Background(), model.Peer{ID: 1, Kind: "user"}, model.Outgoing{Text: "**粗体**", Mode: "markdown", Preview: true, ReplyTo: 12, Buttons: [][]model.Button{{{Text: "测试", Data: "callback"}}}})
	if err != nil || !reflect.DeepEqual(ids, []int{99}) {
		t.Fatalf("发送返回错误 %v %v", ids, err)
	}
}

func TestDownloadDelegatesToMediaOwnerAndCopiesLocal(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(source, []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	path, err := (&Client{}).Download(context.Background(), model.Media{Path: source, Filename: "../../file.bin"}, dir+string(os.PathSeparator))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatal("下载文件逃逸目标目录")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "sample" {
		t.Fatal("本地媒体复制错误")
	}
}

func TestSendErrorsAreReturned(t *testing.T) {
	s, err := openState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("network unavailable")
	c := &Client{state: s, api: tg.NewClient(gotd.InvokeFunc(func(context.Context, bin.Encoder, bin.Decoder) error { return want }))}
	_, err = c.Send(context.Background(), model.Peer{ID: 1, Kind: "user"}, model.Outgoing{Text: "hello"})
	if !errors.Is(err, want) {
		t.Fatalf("发送错误被吞掉 %v", err)
	}
}
