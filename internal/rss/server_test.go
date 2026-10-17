package rss

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

// 使用 Python hashlib 按 Werkzeug 参数生成的固定样本，避免只验证 Go 自身哈希往返。
const legacyScrypt = "scrypt:32768:8:1$legacySalt123$43e85e2f6dc95c5ea674a3f6ce7b4af0007427785b60c19ef2a54cd52d2c6e636f87ab6b8ca4208d1b9d5a7bfa2cfd9b5afcccfe4a5cbd1a1e0b95e291cdd229"
const legacyPBKDF2 = "pbkdf2:sha256:600000$legacySalt123$1b845a3a123dddbd625122b56c911128d8ea134b668f58a95349a9b055183396"

func TestWerkzeugPasswordCompatibility(t *testing.T) {
	for _, sample := range []string{legacyScrypt, legacyPBKDF2} {
		if !VerifyPassword(sample, "旧密码Pass123!") || VerifyPassword(sample, "错误密码") {
			t.Fatal("Werkzeug 样本验证不匹配")
		}
	}
	for _, sample := range []string{"", "pbkdf2:sha256:0$salt$00", "scrypt:1073741824:8:1$salt$00", "scrypt:3:8:1$salt$00"} {
		if VerifyPassword(sample, "x") {
			t.Fatal("接受非法参数")
		}
	}
}

func rssFixture(t *testing.T) (*Server, model.Rule) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(cfg.Root, "db", "forward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rule, err := store.Bind(context.Background(), model.Peer{ID: 101, Kind: "channel", Name: "来源"}, model.Peer{ID: 102, Kind: "channel", Name: "目标"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.DB.Exec("INSERT INTO rss_configs(rule_id,enable_rss,rule_title,rule_description,language,max_items) VALUES (?,1,'旧订阅','保留说明','zh-CN',2)", rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule, err = store.Rule(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, rule
}

func perform(s *Server, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestLegacyLoginDashboardAndManagement(t *testing.T) {
	s, rule := rssFixture(t)
	if _, err := s.store.DB.Exec("INSERT INTO users(username,password) VALUES ('legacy',?)", legacyPBKDF2); err != nil {
		t.Fatal(err)
	}
	w := perform(s, "POST", "/login", url.Values{"username": {"legacy"}, "password": {"旧密码Pass123!"}}.Encode(), nil)
	if w.Code != 302 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("旧密码登录失败 %d: %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	w = perform(s, "GET", "/rss/dashboard", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "旧订阅") || !strings.Contains(w.Body.String(), "configs = JSON.parse(") {
		t.Fatalf("仪表盘未完整渲染 %d", w.Code)
	}
	w = perform(s, "POST", "/rss/pattern", url.Values{"rss_config_id": {fmt.Sprint(rule.RSS.ID)}, "pattern_type": {"title"}, "pattern": {`(?P<title>标题)`}}.Encode(), nil)
	if w.Code != 401 {
		t.Fatal("匿名用户可以写入模式")
	}
	w = perform(s, "POST", "/rss/pattern", url.Values{"rss_config_id": {fmt.Sprint(rule.RSS.ID)}, "pattern_type": {"title"}, "pattern": {`(?P<title>标题)`}}.Encode(), cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("保存模式: %s", w.Body.String())
	}
	w = perform(s, "POST", "/rss/test-regex", url.Values{"pattern": {`(?<=开头)(标题)`}, "test_text": {"开头标题"}}.Encode(), cookie)
	if !strings.Contains(w.Body.String(), `"extracted":"标题"`) {
		t.Fatalf("正则兼容接口: %s", w.Body.String())
	}
	w = perform(s, "GET", "/rss/patterns/"+fmt.Sprint(rule.ID), "", cookie)
	if !strings.Contains(w.Body.String(), "标题") {
		t.Fatalf("获取模式: %s", w.Body.String())
	}
	var stored string
	if err := s.store.DB.Get(&stored, "SELECT password FROM users WHERE username='legacy'"); err != nil || stored != legacyPBKDF2 {
		t.Fatal("登录改写旧密码")
	}
	w = perform(s, "POST", "/register", "username=second&password=long-password&confirm_password=long-password", nil)
	if w.Code != 403 {
		t.Fatal("存在用户后仍允许注册")
	}
}

func TestLegacyEntriesFeedAndMediaRetention(t *testing.T) {
	s, rule := rssFixture(t)
	legacy := `[{"id":"legacy-guid","rule_id":1,"message_id":"123","title":"旧消息","content":"**正文**","published":"2026-10-16T12:00:00","author":"作者","link":"https://t.me/example/123","media":[{"url":"http://127.0.0.1:8000/media/1/旧图.png","type":"image/png","size":3,"filename":"旧图.png","original_name":null}],"created_at":null,"original_link":null,"sender_info":null}]`
	if err := os.MkdirAll(filepath.Dir(s.entryPath(rule.ID)), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.entryPath(rule.ID), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	mediaDir := filepath.Join(s.mediaPath, fmt.Sprint(rule.ID))
	_ = os.MkdirAll(mediaDir, 0750)
	_ = os.WriteFile(filepath.Join(mediaDir, "旧图.png"), []byte{1, 2, 3}, 0600)
	w := perform(s, "GET", fmt.Sprintf("http://rss.example/rss/feed/%d", rule.ID), "", nil)
	if w.Code != 200 {
		t.Fatalf("feed %d: %s", w.Code, w.Body.String())
	}
	var feed struct {
		Channel struct {
			Items []struct {
				GUID        string `xml:"guid"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if len(feed.Channel.Items) != 1 || feed.Channel.Items[0].GUID != "legacy-guid" || !strings.Contains(feed.Channel.Items[0].Description, "<strong>正文</strong>") || !strings.Contains(feed.Channel.Items[0].Description, "http://rss.example/media/") {
		t.Fatalf("旧 Feed 不兼容: %s", w.Body.String())
	}
	w = perform(s, "GET", fmt.Sprintf("/media/%d/", rule.ID)+url.PathEscape("旧图.png"), "", nil)
	if w.Code != 200 || w.Body.Len() != 3 {
		t.Fatal("旧媒体路径不可用")
	}
	src := filepath.Join(t.TempDir(), "same.txt")
	_ = os.WriteFile(src, []byte("第一个"), 0600)
	msg := model.Message{ID: 124, Date: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}
	if err := s.Publish(context.Background(), rule, msg, "新一", []model.Media{{Path: src, Filename: "same.txt"}}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(src, []byte("第二个"), 0600)
	msg.ID++
	msg.Date = msg.Date.Add(time.Second)
	if err := s.Publish(context.Background(), rule, msg, "新二", []model.Media{{Path: src, Filename: "same.txt"}}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.readEntries(rule.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("容量控制: %v", err)
	}
	if entries[0].Media[0].Filename == entries[1].Media[0].Filename {
		t.Fatal("同名媒体被覆盖")
	}
	if _, err = os.Stat(filepath.Join(mediaDir, "旧图.png")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("过期媒体未清理")
	}
	content, err := os.ReadFile(filepath.Join(mediaDir, entries[1].Media[0].Filename))
	if err != nil || string(content) != "第一个" {
		t.Fatal("旧媒体内容被改写")
	}
}

type failingAI struct{}

func (failingAI) Process(context.Context, string, string, string, []model.AIImage) (string, error) {
	return "", errors.New("temporary failure")
}

func TestEntryAPIRejectsExternalAndKeepsOriginalWhenAIUnavailable(t *testing.T) {
	s, rule := rssFixture(t)
	s.ai = failingAI{}
	_, err := s.store.DB.Exec("UPDATE rss_configs SET is_ai_extract=1 WHERE rule_id=?", rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{MessageID: "5", Content: "原始内容", Title: "原标题", Published: "2026-10-17T12:00:00+08:00"}
	data, _ := json.Marshal(entry)
	r := httptest.NewRequest("POST", fmt.Sprintf("/api/entries/%d/add", rule.ID), strings.NewReader(string(data)))
	r.RemoteAddr = "8.8.8.8:5000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("外网调用本地写入接口")
	}
	r = httptest.NewRequest("POST", fmt.Sprintf("/api/entries/%d/add", rule.ID), strings.NewReader(string(data)))
	r.RemoteAddr = "127.0.0.1:5000"
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("AI 故障不应丢条目: %s", w.Body.String())
	}
	entries, err := s.readEntries(rule.ID)
	if err != nil || len(entries) != 1 || entries[0].Content != "原始内容" {
		t.Fatal("未保留原条目")
	}
}

func TestPublishPreservesLegacySingleAndAlbumTextSources(t *testing.T) {
	s, rule := rssFixture(t)
	rule.IsOriginalSender, rule.IsOriginalLink, rule.IsOriginalTime = true, true, true
	rule.UserinfoTemplate, rule.OriginalLinkTemplate = "", ""
	msg := model.Message{ID: 123, Text: "原始正文", Date: time.Now(), Chat: model.Peer{ID: 1, Kind: "channel", Username: "source"}, Sender: model.Peer{ID: 2, Name: "作者"}}
	if err := s.Publish(context.Background(), rule, msg, "AI 改写正文", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := s.readEntries(rule.ID)
	if err != nil || len(entries) != 1 || entries[0].Title != "原始正文" || entries[0].Content != "作者:\n\n原始正文\n\n[来源: 作者](https://t.me/source/123)" {
		t.Fatalf("单条 RSS 来源不兼容: %+v %v", entries, err)
	}
	file := filepath.Join(t.TempDir(), "media.txt")
	_ = os.WriteFile(file, []byte("media"), 0600)
	msg.GroupedID, msg.ID, msg.Date = 9, 124, msg.Date.Add(time.Second)
	if err = s.Publish(context.Background(), rule, msg, "AI 改写正文", []model.Media{{Path: file}}); err != nil {
		t.Fatal(err)
	}
	entries, err = s.readEntries(rule.ID)
	if err != nil || entries[0].Content != "AI 改写正文" || entries[0].SenderInfo != nil || entries[0].OriginalLink != nil {
		t.Fatalf("相册 RSS 元信息不兼容: %+v %v", entries, err)
	}
	if strings.Contains(markdownHTML("# 标签\n正文"), "<h1>") {
		t.Fatal("Telegram 标签被转为标题")
	}
}
