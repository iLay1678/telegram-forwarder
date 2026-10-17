package push

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestValidateConfigsRejectsUnknownWithoutCredentials(t *testing.T) {
	err := ValidateConfigs([]model.PushConfig{{ID: 12, EnablePushChannel: true, PushChannel: "unknown://secret-token@example.com/path"}, {ID: 13, EnablePushChannel: false, PushChannel: "other://secret"}})
	if err == nil || !strings.Contains(err.Error(), "12") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "13") {
		t.Fatalf("错误检查或脱敏: %v", err)
	}
	for _, raw := range []string{"tgram://123:ABC/-10042", "gotifys://host/prefix/token", "slack://T/B/secret", "mailto://alice:password@gmail.com/bob@example.com"} {
		if err := Validate(raw); err != nil {
			t.Errorf("兼容 URL %q: %v", raw, err)
		}
	}
	if Validate("discord://id") == nil {
		t.Fatal("未检查 webhook token")
	}
}

func TestCustomJSONApprisePayloadAndAttachment(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(file, []byte("附件内容"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "user" || password != "password" || r.Header.Get("X-Test") != "yes" || r.URL.Query().Get("token") != "value" || r.Method != "PUT" {
			t.Error("通用 URL 参数未按 Apprise 解释")
		}
		var data map[string]any
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Error(err)
			return
		}
		if data["body"] != "通知文本" || data["custom"] != "extra" || data["message"] != nil || data["title"] != nil || data["version"] != "1.0" {
			t.Errorf("字段映射错误: %v", data)
		}
		attachments := data["attachments"].([]any)
		item := attachments[0].(map[string]any)
		if item["base64"] != base64.StdEncoding.EncodeToString([]byte("附件内容")) || item["filename"] != "sample.txt" {
			t.Error("JSON 附件不兼容")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	u.Scheme = "json"
	u.User = url.UserPassword("user", "password")
	u.RawQuery = url.Values{"method": {"PUT"}, "+X-Test": {"yes"}, "-token": {"value"}, ":message": {"body"}, ":title": {""}, ":custom": {"extra"}}.Encode()
	err := New(nil).Send(context.Background(), []model.PushConfig{{EnablePushChannel: true, PushChannel: u.String()}}, "通知文本", []model.Media{{Path: file, Filename: "sample.txt"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFormMultipartAndServiceErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a.txt")
	_ = os.WriteFile(file, []byte("hello"), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("message") != "正文" || r.FormValue("version") != "1.0" {
			t.Error("form payload")
		}
		if len(r.MultipartForm.File["file00"]) != 1 || len(r.MultipartForm.File["file01"]) != 1 {
			t.Errorf("附件字段: %v", r.MultipartForm.File)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":42,"errmsg":"secret-token"}`)
	}))
	defer server.Close()
	err := New(nil).Send(context.Background(), []model.PushConfig{{EnablePushChannel: true, PushChannel: strings.Replace(server.URL, "http://", "form://", 1), MediaSendMode: "Multiple"}}, "正文", []model.Media{{Path: file}, {Path: file}})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("失败未报告或泄露凭据: %v", err)
	}
}

func TestKnownHTTPProtocols(t *testing.T) {
	cases := []struct{ raw, host, path string }{
		{"ntfy://topic", "ntfy.sh", ""},
		{"ntfys://example.com/topic", "example.com", ""},
		{"gotify://example.com/proxy/token", "example.com", "/proxy/message"},
		{"barks://example.com/device", "example.com", "/push"},
		{"discord://id/token", "discord.com", "/api/webhooks/id/token"},
		{"tgram://123:ABC/-10042", "api.telegram.org", "/bot123:ABC/sendMessage"},
		{"slack://T/B/token", "hooks.slack.com", "/services/T/B/token"},
		{"pover://user@token", "api.pushover.net", "/1/messages.json"},
		{"dingtalk://secret@token", "oapi.dingtalk.com", "/robot/send"},
		{"feishu://token", "open.feishu.cn", "/open-apis/bot/v2/hook/token"},
		{"lark://token", "open.larksuite.com", "/open-apis/bot/v2/hook/token"},
		{"schan://token", "sctapi.ftqq.com", "/token.send"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			called := false
			c := New(nil)
			c.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.URL.Host != tc.host || r.URL.Path != tc.path {
					t.Errorf("请求 %s，不符合 %s%s", r.URL.Redacted(), tc.host, tc.path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			if err := c.Send(context.Background(), []model.PushConfig{{EnablePushChannel: true, PushChannel: tc.raw}}, "正文", nil); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("没有发送请求")
			}
		})
	}
}

func TestSMTPDeliveryMIME(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mailBody := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		proto := textproto.NewConn(conn)
		_ = proto.PrintfLine("220 localhost SMTP")
		for {
			line, err := proto.ReadLine()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				_ = proto.PrintfLine("250 localhost")
			case line == "DATA":
				_ = proto.PrintfLine("354 send data")
				data, err := proto.ReadDotBytes()
				if err != nil {
					return
				}
				mailBody <- data
				_ = proto.PrintfLine("250 accepted")
			case line == "QUIT":
				_ = proto.PrintfLine("221 bye")
				return
			default:
				_ = proto.PrintfLine("250 OK")
			}
		}
	}()
	file := filepath.Join(t.TempDir(), "说明.txt")
	_ = os.WriteFile(file, []byte("文件正文"), 0600)
	u := "mailto://" + listener.Addr().String() + "/receiver@example.com?from=sender@example.com&title=" + url.QueryEscape("中文标题")
	err = New(nil).Send(context.Background(), []model.PushConfig{{EnablePushChannel: true, PushChannel: u}}, "邮件正文", []model.Media{{Path: file, Filename: "说明.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-mailBody:
		message, err := mail.ReadMessage(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
		if err != nil || subject != "中文标题" {
			t.Fatal("邮件标题编码错误")
		}
		_, params, _ := mime.ParseMediaType(message.Header.Get("Content-Type"))
		reader := multipart.NewReader(message.Body, params["boundary"])
		for _, want := range []string{"邮件正文", "文件正文"} {
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
			if err != nil || string(decoded) != want {
				t.Fatalf("MIME 解码: %q %v", decoded, err)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 SMTP 正文")
	}
}
