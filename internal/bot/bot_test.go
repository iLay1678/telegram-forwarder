package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type fakeTransport struct {
	model.Transport
	sent, edited []model.Outgoing
	denied       bool
	deleted      []int
}

func (f *fakeTransport) Send(_ context.Context, _ model.Peer, out model.Outgoing) ([]int, error) {
	f.sent = append(f.sent, out)
	return []int{len(f.sent)}, nil
}
func (f *fakeTransport) Edit(_ context.Context, _ model.Peer, _ int, out model.Outgoing) error {
	f.edited = append(f.edited, out)
	return nil
}
func (f *fakeTransport) Delete(_ context.Context, _ model.Peer, ids []int) error {
	f.deleted = append(f.deleted, ids...)
	return nil
}
func (f *fakeTransport) AnswerCallback(_ context.Context, _ int64, text string, alert bool) error {
	f.denied = alert && strings.Contains(text, "权限")
	return nil
}
func (f *fakeTransport) Download(_ context.Context, media model.Media, destination string) (string, error) {
	data, err := os.ReadFile(media.Path)
	if err != nil {
		return "", err
	}
	return destination, os.WriteFile(destination, data, 0600)
}

func setupBot(t *testing.T) (*Bot, *fakeTransport, model.Rule, model.Message) {
	t.Helper()
	t.Setenv("BOT_MESSAGE_DELETE_TIMEOUT", "-1")
	t.Setenv("USER_MESSAGE_DELETE_ENABLE", "false")
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Admins = []int64{7}
	cfg.UserID = 7
	if err = os.MkdirAll(cfg.Path("temp"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(root, "forward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB.Close() })
	source := model.Peer{ID: 10, Kind: "channel", Name: "source"}
	target := model.Peer{ID: 20, Kind: "channel", Name: "target"}
	rule, err := store.Bind(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	b := New(cfg, store, transport, transport)
	return b, transport, rule, model.Message{ID: 1, Chat: target, Sender: model.Peer{ID: 7}}
}

func TestUnauthorizedCommandsAndCallbacksCannotMutate(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	msg.Sender.ID = 99
	msg.Text = "/clear_all"
	b.HandleMessage(context.Background(), msg)
	b.HandleCallback(context.Background(), model.Callback{SenderID: 99, Chat: msg.Chat, Data: fmt.Sprintf("delete:%d", rule.ID)})
	if _, err := b.Store.Rule(context.Background(), rule.ID); err != nil {
		t.Fatal("未授权者删除了规则", err)
	}
	if len(f.sent) != 0 || !f.denied {
		t.Fatalf("未授权回复异常: sent=%d denied=%t", len(f.sent), f.denied)
	}
}

type channelAdminTransport struct {
	*fakeTransport
	allowed bool
	checks  int
}

func (f *channelAdminTransport) IsChannelAdmin(context.Context, model.Peer, int64) (bool, error) {
	f.checks++
	return f.allowed, nil
}

func TestOrdinaryBroadcastSkipsAdminLookup(t *testing.T) {
	for _, stateKind := range []string{"none", "expired", "other_chat"} {
		t.Run(stateKind, func(t *testing.T) {
			b, f, rule, msg := setupBot(t)
			user := &channelAdminTransport{fakeTransport: f, allowed: true}
			b.User = user
			msg.Chat.Broadcast = true
			msg.Sender.ID = 999
			msg.Text = "普通频道广播"
			switch stateKind {
			case "expired":
				b.states[b.stateKey(msg)] = inputState{RuleID: rule.ID, Field: "ai_prompt", Expires: time.Now().Add(-time.Minute)}
			case "other_chat":
				other := msg
				other.Chat.ID++
				b.setState(other, rule.ID, "ai_prompt")
			}
			if b.HandleInput(context.Background(), msg) {
				t.Fatal("普通广播不应被当作管理输入")
			}
			b.HandleMessage(context.Background(), msg)
			if user.checks != 0 || len(f.sent) != 0 {
				t.Fatalf("无有效输入状态仍触发 RPC 或回复: checks=%d sent=%d", user.checks, len(f.sent))
			}
		})
	}
}

func TestBroadcastInputAndCommandsAuthorizeOnce(t *testing.T) {
	for _, entry := range []string{"input", "message", "command", "link_as_input", "cancel"} {
		t.Run(entry, func(t *testing.T) {
			b, f, rule, msg := setupBot(t)
			user := &channelAdminTransport{fakeTransport: f, allowed: true}
			b.User = user
			msg.Chat.Broadcast = true
			msg.Sender.ID = 999
			msg.Text = "新的提示词"
			b.setState(msg, rule.ID, "ai_prompt")
			switch entry {
			case "input":
				if !b.HandleInput(context.Background(), msg) {
					t.Fatal("有权限的待处理输入未消费")
				}
			case "command":
				msg.Text = "/help"
				b.HandleMessage(context.Background(), msg)
			case "link_as_input":
				msg.Text = "https://t.me/example/1"
				b.HandleMessage(context.Background(), msg)
			case "cancel":
				msg.Text = "/cancel"
				b.HandleMessage(context.Background(), msg)
			default:
				b.HandleMessage(context.Background(), msg)
			}
			if user.checks != 1 {
				t.Fatalf("管理操作应只查询一次权限，实际 %d", user.checks)
			}
			if _, ok := b.states[b.stateKey(msg)]; ok {
				t.Fatal("成功、取消或命令未清理旧输入状态")
			}
			updated, err := b.Store.Rule(context.Background(), rule.ID)
			if err != nil {
				t.Fatal(err)
			}
			if entry != "command" && entry != "cancel" {
				if updated.AIPrompt != msg.Text {
					t.Fatalf("输入未保存: %q", updated.AIPrompt)
				}
				if !b.HandleInput(context.Background(), msg) || user.checks != 1 {
					t.Fatal("已消费输入应直接去重，无须再次查询权限")
				}
			} else if updated.AIPrompt == msg.Text {
				t.Fatal("命令不应保存到提示词")
			}
		})
	}
}

func TestBroadcastInputStillRequiresAuthorization(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	user := &channelAdminTransport{fakeTransport: f}
	b.User = user
	msg.Chat.Broadcast = true
	msg.Sender.ID = 999
	msg.Text = "未授权提示词"
	b.setState(msg, rule.ID, "ai_prompt")
	if b.HandleInput(context.Background(), msg) {
		t.Fatal("未授权输入不应被消费")
	}
	b.HandleMessage(context.Background(), msg)
	if user.checks != 2 || len(f.sent) != 0 {
		t.Fatalf("两条调用各应鉴权一次且不回复: checks=%d sent=%d", user.checks, len(f.sent))
	}
	if _, ok := b.states[b.stateKey(msg)]; !ok || len(b.inputs) != 0 {
		t.Fatal("未授权输入改变了交互状态或被标记为已消费")
	}
	updated, err := b.Store.Rule(context.Background(), rule.ID)
	if err != nil || updated.AIPrompt == msg.Text {
		t.Fatalf("未授权输入修改了规则: err=%v prompt=%q", err, updated.AIPrompt)
	}
}

func TestStateRejectsInvalidInputAndCommandsCancelInput(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	b.setState(msg, rule.ID, "summary_time")
	msg.Text = "25:99"
	if !b.HandleInput(context.Background(), msg) {
		t.Fatal("未处理输入")
	}
	if _, ok := b.states[b.stateKey(msg)]; !ok {
		t.Fatal("失败后不应丢失输入状态")
	}
	msg.ID++
	msg.Text = "12:30"
	b.HandleInput(context.Background(), msg)
	updated, err := b.Store.Rule(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SummaryTime != "12:30" {
		t.Fatal(updated.SummaryTime)
	}
	if _, ok := b.states[b.stateKey(msg)]; ok {
		t.Fatal("成功后应清除状态")
	}
	b.setState(msg, rule.ID, "ai_prompt")
	msg.ID++
	msg.Text = "/help"
	b.HandleMessage(context.Background(), msg)
	updated, _ = b.Store.Rule(context.Background(), rule.ID)
	if updated.AIPrompt == "/help" {
		t.Fatal("命令被保存成提示词")
	}
	for _, out := range f.sent {
		if out.Mode != "TEXT" {
			t.Fatal("管理回复应使用纯文本")
		}
	}
}

func TestKeywordsIDsClearAndSync(t *testing.T) {
	b, _, rule, msg := setupBot(t)
	ctx := context.Background()
	other, err := b.Store.Bind(ctx, model.Peer{ID: 11, Kind: "channel"}, msg.Chat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Store.DB.Exec("INSERT INTO rule_syncs(rule_id,sync_rule_id) VALUES(?,?)", rule.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if err = b.Store.UpdateRule(ctx, rule.ID, "enable_sync", true); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ cmd, args string }{{"add", `"first word" second`}, {"add_regex", `\d+`}, {"remove_keyword_by_id", "2"}} {
		if err = b.command(ctx, msg, item.cmd, item.args); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{rule.ID, other.ID} {
		r, e := b.Store.Rule(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Keywords) != 2 || r.Keywords[0].Keyword != "first word" || !r.Keywords[1].IsRegex {
			t.Fatalf("规则 %d 列表序号/同步失败: %+v", id, r.Keywords)
		}
	}
	if err = b.command(ctx, msg, "clear_all_keywords", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{rule.ID, other.ID} {
		r, _ := b.Store.Rule(ctx, id)
		if len(r.Keywords) != 0 {
			t.Fatal("普通和正则关键词未同时清空")
		}
	}
	if err = b.command(ctx, msg, "replace", ".* replacement"); err != nil {
		t.Fatal(err)
	}
	if err = b.command(ctx, msg, "clear_all_replace", ""); err != nil {
		t.Fatal(err)
	}
	r, _ := b.Store.Rule(ctx, rule.ID)
	if r.IsReplace || len(r.Replacements) != 0 {
		t.Fatal("清空替换必须关闭替换模式")
	}
}

func TestCopyExplicitTargetAndRSSRelations(t *testing.T) {
	b, _, rule, msg := setupBot(t)
	ctx := context.Background()
	other, err := b.Store.Bind(ctx, model.Peer{ID: 11, Kind: "channel"}, msg.Chat)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		fmt.Sprintf("INSERT INTO keywords(rule_id,keyword,is_regex,is_blacklist) VALUES(%d,'keep',0,1)", rule.ID),
		fmt.Sprintf("INSERT INTO rss_configs(rule_id,rule_title,enable_rss) VALUES(%d,'RSS title',1)", rule.ID),
		fmt.Sprintf("INSERT INTO rss_patterns(rss_config_id,pattern,pattern_type,priority) SELECT id,'^title','title',3 FROM rss_configs WHERE rule_id=%d", rule.ID),
		fmt.Sprintf("INSERT INTO rule_syncs(rule_id,sync_rule_id) VALUES(%d,%d)", rule.ID, other.ID),
	} {
		if _, err = b.Store.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	msg.Chat = model.Peer{ID: 999, Kind: "user"}
	for range 2 {
		if err = b.command(ctx, msg, "copy_rule", fmt.Sprintf("%d %d", rule.ID, other.ID)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := b.Store.Rule(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Keywords) != 1 || r.RSS == nil || r.RSS.RuleTitle != "RSS title" {
		t.Fatalf("拷贝结果: %+v", r)
	}
	var count int
	if err = b.Store.DB.Get(&count, "SELECT COUNT(*) FROM rss_patterns WHERE rss_config_id=?", r.RSS.ID); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("RSS 模式未去重复制", count)
	}
	if err = b.Store.DB.Get(&count, "SELECT COUNT(*) FROM rule_syncs WHERE rule_id=sync_rule_id"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("复制形成了自同步")
	}
}

func TestLegacyCallbacksAndPushSettings(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	ctx := context.Background()
	for _, data := range []string{"push_settings", "set_delay_time", "toggle_ai_upload_image", "select_time:04:23", "select_delay_time:15", "select_model:custom:model", "toggle_media_extension:jpg:0"} {
		action, tail, ok := strings.Cut(data, ":")
		cbData := fmt.Sprintf("%s:%d", action, rule.ID)
		if ok {
			cbData += ":" + tail
		}
		if err := b.callback(ctx, model.Callback{SenderID: 7, Chat: msg.Chat, Data: cbData}); err != nil {
			t.Fatalf("%s: %v", cbData, err)
		}
	}
	r, err := b.Store.Rule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.EnableAIUploadImage || r.SummaryTime != "04:23" || r.DelaySeconds != 15 || r.AIModel != "custom:model" || len(r.Extensions) != 1 {
		t.Fatalf("旧按钮未生效: %+v", r)
	}
	if len(f.edited) != 7 {
		t.Fatal("菜单没有刷新", len(f.edited))
	}
	b.setState(msg, rule.ID, "ai_prompt")
	if err = b.callback(ctx, model.Callback{SenderID: 7, Chat: msg.Chat, Data: fmt.Sprintf("rule_settings:%d", rule.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.states[b.stateKey(msg)]; ok {
		t.Fatal("返回设置后仍在等待输入")
	}
}

func TestSummaryExplicitIDAndMultiUserDeletion(t *testing.T) {
	b, _, rule, msg := setupBot(t)
	ctx := context.Background()
	msg.Chat = model.Peer{ID: 999, Kind: "user"}
	called := int64(0)
	b.Summary = func(_ context.Context, id int64) error { called = id; return nil }
	if err := b.command(ctx, msg, "summary", fmt.Sprint(rule.ID)); err != nil {
		t.Fatal(err)
	}
	if called != rule.ID {
		t.Fatal("显式总结 ID 被当前聊天限制")
	}
	if _, err := b.Store.DB.Exec("INSERT INTO users(username,password) VALUES('a','hash'),('b','hash')"); err != nil {
		t.Fatal(err)
	}
	if err := b.command(ctx, msg, "delete_rss_user", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := b.Store.DB.Get(&count, "SELECT COUNT(*) FROM users"); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatal("无参数时误删多个 RSS 用户")
	}
}

func TestImportLegacyFilesAndAtomicInvalidRegex(t *testing.T) {
	b, _, rule, msg := setupBot(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "keywords.txt")
	if err := os.WriteFile(path, []byte("\ufeffmulti word 1\r\nwhite 0\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	msg.Media = []model.Media{{Path: path}}
	if err := b.importFile(ctx, msg, rule, "import_keyword"); err != nil {
		t.Fatal(err)
	}
	r, _ := b.Store.Rule(ctx, rule.ID)
	if len(r.Keywords) != 2 || r.Keywords[0].Keyword != "multi word" || r.Keywords[1].IsBlacklist {
		t.Fatalf("旧文件读取失败: %+v", r.Keywords)
	}
	if err := os.WriteFile(path, []byte("valid 1\n[bad 1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.importFile(ctx, msg, r, "import_regex_keyword"); err == nil {
		t.Fatal("无效正则没有报错")
	}
	r, _ = b.Store.Rule(ctx, rule.ID)
	if len(r.Keywords) != 2 {
		t.Fatal("导入失败时发生部分写入")
	}
}

func TestInputConsumedOnceAcrossClients(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	b.setState(msg, rule.ID, "ai_prompt")
	msg.Text = "new prompt"
	var wg sync.WaitGroup
	results := make(chan bool, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- b.HandleInput(context.Background(), msg) }()
	}
	wg.Wait()
	close(results)
	for handled := range results {
		if !handled {
			t.Fatal("第二客户端的已消费输入落入转发管线")
		}
	}
	if len(f.sent) != 1 {
		t.Fatalf("输入被重复处理 %d 次", len(f.sent))
	}
	r, err := b.Store.Rule(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.AIPrompt != "new prompt" {
		t.Fatal("输入未保存")
	}
}

func TestUFBOptionalTypeAndOldSelection(t *testing.T) {
	b, f, rule, msg := setupBot(t)
	ctx := context.Background()
	if err := b.command(ctx, msg, "ufb_bind", "Example.COM content_username"); err != nil {
		t.Fatal(err)
	}
	r, err := b.Store.Rule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.UFBDomain != "example.com" || r.UFBItem != "content_username" {
		t.Fatalf("可选类型解析失败: %s %s", r.UFBDomain, r.UFBItem)
	}
	if err = b.command(ctx, msg, "ufb_item_change", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.sent[len(f.sent)-1].Buttons) != 4 {
		t.Fatal("没有提供 UFB 类型选择")
	}
	if err = b.callback(ctx, model.Callback{SenderID: 7, Chat: msg.Chat, Data: "ufb_item:main"}); err != nil {
		t.Fatal(err)
	}
	if err = b.command(ctx, msg, "ufb_unbind", ""); err != nil {
		t.Fatal(err)
	}
	r, err = b.Store.Rule(ctx, rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.UFBDomain != "" || r.UFBItem != "main" {
		t.Fatal("UFB 解绑或旧按钮未生效")
	}
}

func TestLegacyDeleteTargetCleansRSSAndOrphanChats(t *testing.T) {
	b, _, source, msg := setupBot(t)
	ctx := context.Background()
	target, err := b.Store.Bind(ctx, model.Peer{ID: 11, Kind: "channel"}, msg.Chat)
	if err != nil {
		t.Fatal(err)
	}
	cleaned := int64(0)
	b.CleanupRule = func(_ context.Context, id int64) error { cleaned = id; return nil }
	if err = b.callback(ctx, model.Callback{SenderID: 7, Chat: msg.Chat, Data: fmt.Sprintf("perform_delete_rule:%d:%d", source.ID, target.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Store.Rule(ctx, source.ID); err != nil {
		t.Fatal("源规则被误删", err)
	}
	if _, err = b.Store.Rule(ctx, target.ID); err == nil {
		t.Fatal("目标规则未删除")
	}
	if cleaned != target.ID {
		t.Fatal("RSS 清理目标错误", cleaned)
	}
	var count int
	if err = b.Store.DB.Get(&count, "SELECT COUNT(*) FROM chats WHERE id=?", target.SourceChatID); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("孤立聊天未清理")
	}
}
