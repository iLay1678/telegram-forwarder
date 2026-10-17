// Package push 解析已实现的 Apprise URL 方言；未知协议和不支持的附件会明确报错。
package push

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type Client struct {
	HTTP *http.Client
	cfg  *config.Config
}

func New(cfg *config.Config) *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, cfg: cfg}
}

var SupportedSchemes = []string{"ntfy", "ntfys", "json", "jsons", "form", "forms", "gotify", "gotifys", "bark", "barks", "discord", "tgram", "slack", "pover", "dingtalk", "feishu", "lark", "schan", "mailto", "mailtos", "email"}

type UnsupportedError struct{ Scheme string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("尚未实现 Apprise 协议 %q；未发送通知", e.Scheme)
}

func parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "tgram://") {
		rest := strings.TrimPrefix(raw, "tgram://")
		host, tail, _ := strings.Cut(rest, "/")
		u, err := url.Parse("https://telegram.invalid/" + tail)
		if err != nil {
			return nil, errors.New("Telegram 推送 URL 无效")
		}
		u.Scheme = "tgram"
		u.Host = host
		return u, nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errors.New("推送 URL 无效")
	}
	return u, nil
}

func Validate(raw string) error {
	u, err := parse(raw)
	if err != nil {
		return err
	}
	for _, scheme := range SupportedSchemes {
		if scheme == u.Scheme {
			return validateTarget(u)
		}
	}
	return &UnsupportedError{u.Scheme}
}

// ValidateConfigs 离线检查启用的推送配置，错误只包含配置 ID，不暴露 URL 凭据。
func ValidateConfigs(configs []model.PushConfig) error {
	var failures []error
	for _, cfg := range configs {
		if cfg.EnablePushChannel {
			if err := Validate(cfg.PushChannel); err != nil {
				failures = append(failures, fmt.Errorf("推送配置 %d: %w", cfg.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func validateTarget(u *url.URL) error {
	parts := targetParts(u)
	switch u.Scheme {
	case "json", "jsons", "form", "forms":
		if !validMethod(strings.ToUpper(u.Query().Get("method"))) {
			return errors.New("通用推送 HTTP method 无效")
		}
	case "mailto", "mailtos", "email":
		_, err := mailConfig(u)
		return err
	case "tgram":
		if !strings.Contains(u.Host, ":") || len(parts) == 0 {
			return errors.New("Telegram 推送需要 bot token 和目标 chat ID")
		}
	case "discord":
		if len(parts) != 1 {
			return errors.New("Discord 推送需要 webhook ID 和 token")
		}
	case "slack":
		if len(parts) < 2 {
			return errors.New("Slack 推送需要完整 incoming webhook token")
		}
	case "gotify", "gotifys", "bark", "barks":
		if len(parts) == 0 {
			return errors.New("推送地址缺少 token 或目标")
		}
	case "pover":
		if u.User == nil || u.User.Username() == "" {
			return errors.New("Pushover 推送缺少 user key")
		}
	}
	return nil
}

func (c *Client) Send(ctx context.Context, configs []model.PushConfig, text string, media []model.Media) error {
	if text == "" && len(media) == 0 {
		return nil
	}
	var failures []error
	for _, cfg := range configs {
		if !cfg.EnablePushChannel {
			continue
		}
		u, err := parse(cfg.PushChannel)
		if err == nil {
			err = Validate(cfg.PushChannel)
		}
		if err == nil {
			if len(media) > 1 && !strings.EqualFold(cfg.MediaSendMode, "Multiple") {
				for i, m := range media {
					body := text
					if i > 0 {
						body = fmt.Sprintf("媒体组的第 %d 个文件", i+1)
					}
					if sendErr := c.send(ctx, u, body, []model.Media{m}); sendErr != nil {
						err = errors.Join(err, sendErr)
					}
				}
			} else {
				err = c.send(ctx, u, text, media)
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("推送配置 %d: %w", cfg.ID, err))
		}
	}
	return errors.Join(failures...)
}

func targetParts(u *url.URL) []string {
	var parts []string
	for _, v := range strings.FieldsFunc(strings.Trim(u.Path, "/"), func(r rune) bool { return r == '/' || r == ',' }) {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return parts
}
func webBase(u *url.URL) string {
	scheme := "http"
	if strings.HasSuffix(u.Scheme, "s") {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}
func copyHeaders(u *url.URL) http.Header {
	h := make(http.Header)
	if u.User != nil {
		password, _ := u.User.Password()
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password)))
	}
	for key, values := range u.Query() {
		if strings.HasPrefix(key, "+") {
			h.Set(strings.TrimPrefix(key, "+"), values[0])
		}
	}
	return h
}
func integer(v string, fallback int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
func truth(v string) bool {
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func (c *Client) send(ctx context.Context, u *url.URL, text string, media []model.Media) error {
	for _, m := range media {
		if m.Path == "" {
			return errors.New("推送附件未下载到本地")
		}
		if info, err := os.Stat(m.Path); err != nil || info.IsDir() {
			return errors.New("推送附件不存在或不是文件")
		}
	}
	switch u.Scheme {
	case "mailto", "mailtos", "email":
		return c.email(ctx, u, text, media)
	case "ntfy", "ntfys":
		return c.ntfy(ctx, u, text, media)
	case "tgram":
		return c.telegram(ctx, u, text, media)
	case "discord":
		parts := targetParts(u)
		if len(parts) != 1 {
			return errors.New("Discord URL 需要 webhook ID 和 token")
		}
		endpoint := "https://discord.com/api/webhooks/" + url.PathEscape(u.Host) + "/" + url.PathEscape(parts[0]) + "?wait=true"
		data := map[string]any{"content": text, "tts": truth(u.Query().Get("tts"))}
		if u.User != nil {
			data["username"] = u.User.Username()
		}
		if len(media) == 0 {
			return c.json(ctx, endpoint, data, nil)
		}
		body, _ := json.Marshal(data)
		return c.multipart(ctx, endpoint, map[string]string{"payload_json": string(body)}, media, "files[%d]", nil)
	case "pover":
		if u.User == nil {
			return errors.New("Pushover URL 缺少 user key")
		}
		fields := map[string]string{"token": u.Host, "user": u.User.Username(), "message": text, "title": u.Query().Get("title"), "priority": u.Query().Get("priority"), "device": strings.Join(targetParts(u), ",")}
		for _, key := range []string{"sound", "url", "url_title", "retry", "expire"} {
			if v := u.Query().Get(key); v != "" {
				fields[key] = v
			}
		}
		if len(media) == 0 {
			return c.form(ctx, "https://api.pushover.net/1/messages.json", fields, nil)
		}
		for _, m := range media {
			if err := c.multipart(ctx, "https://api.pushover.net/1/messages.json", fields, []model.Media{m}, "attachment", nil); err != nil {
				return err
			}
		}
		return nil
	case "json", "jsons", "form", "forms":
		return c.generic(ctx, u, text, media)
	}
	if len(media) > 0 {
		return fmt.Errorf("%s 推送尚不支持本地附件，未发送通知", u.Scheme)
	}
	q := u.Query()
	title := q.Get("title")
	switch u.Scheme {
	case "gotify", "gotifys":
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 0 || parts[len(parts)-1] == "" {
			return errors.New("Gotify URL 缺少 token")
		}
		token := parts[len(parts)-1]
		prefix := strings.Join(parts[:len(parts)-1], "/")
		endpoint := webBase(u)
		if prefix != "" {
			endpoint += "/" + prefix
		}
		endpoint += "/message?token=" + url.QueryEscape(token)
		return c.json(ctx, endpoint, map[string]any{"title": title, "message": text, "priority": integer(q.Get("priority"), 0)}, copyHeaders(u))
	case "bark", "barks":
		targets := targetParts(u)
		if len(targets) == 0 {
			return errors.New("Bark URL 缺少设备 key")
		}
		for _, target := range targets {
			data := map[string]any{"device_key": target, "body": text, "title": title}
			for _, key := range []string{"sound", "group", "icon", "url", "level", "badge", "copy", "isArchive", "autoCopy"} {
				if v := q.Get(key); v != "" {
					data[key] = v
				}
			}
			if err := c.json(ctx, webBase(u)+"/push", data, copyHeaders(u)); err != nil {
				return err
			}
		}
		return nil
	case "slack":
		parts := targetParts(u)
		if len(parts) < 2 {
			return errors.New("仅支持 Slack incoming webhook URL；需要三个 token")
		}
		data := map[string]any{"text": text}
		if u.User != nil {
			data["username"] = u.User.Username()
		}
		endpoint := "https://hooks.slack.com/services/" + url.PathEscape(u.Host) + "/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
		if len(parts) > 2 {
			for _, target := range parts[2:] {
				data["channel"] = target
				if err := c.json(ctx, endpoint, data, nil); err != nil {
					return err
				}
			}
			return nil
		}
		return c.json(ctx, endpoint, data, nil)
	case "dingtalk":
		endpoint := "https://oapi.dingtalk.com/robot/send?access_token=" + url.QueryEscape(u.Host)
		if u.User != nil && u.User.Username() != "" {
			secret := u.User.Username()
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(ts + "\n" + secret))
			endpoint += "&timestamp=" + ts + "&sign=" + url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		}
		data := map[string]any{"msgtype": "text", "text": map[string]string{"content": text}, "at": map[string]any{"atMobiles": targetParts(u), "isAtAll": truth(q.Get("atall"))}}
		return c.json(ctx, endpoint, data, nil)
	case "feishu", "lark":
		host := "https://open.feishu.cn"
		if u.Scheme == "lark" {
			host = "https://open.larksuite.com"
		}
		data := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
		if u.User != nil {
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(ts+"\n"+u.User.Username()))
			data["timestamp"] = ts
			data["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		return c.json(ctx, host+"/open-apis/bot/v2/hook/"+url.PathEscape(u.Host), data, nil)
	case "schan":
		if title == "" {
			title = "Telegram Forwarder"
		}
		return c.form(ctx, "https://sctapi.ftqq.com/"+url.PathEscape(u.Host)+".send", map[string]string{"title": title, "desp": text}, nil)
	default:
		return &UnsupportedError{u.Scheme}
	}
}

func (c *Client) do(ctx context.Context, method, endpoint, contentType string, body io.Reader, headers http.Header) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("推送目标 URL 无效")
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("推送网络请求失败")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return errors.New("读取推送响应失败")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("推送服务返回 HTTP %d", response.StatusCode)
	}
	if strings.Contains(response.Header.Get("Content-Type"), "json") {
		var payload map[string]any
		if json.Unmarshal(data, &payload) == nil {
			if ok, exists := payload["ok"].(bool); exists && !ok {
				return errors.New("推送服务返回失败状态")
			}
			if status, ok := payload["status"].(float64); ok && status == 0 {
				return errors.New("推送服务返回失败状态")
			}
			if code, ok := payload["errcode"].(float64); ok && code != 0 {
				return errors.New("推送服务返回错误码")
			}
			if code, ok := payload["code"].(float64); ok && code != 0 && code != 200 {
				return errors.New("推送服务返回错误码")
			}
		}
	}
	return nil
}
func (c *Client) json(ctx context.Context, endpoint string, value any, headers http.Header) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.do(ctx, "POST", endpoint, "application/json", bytes.NewReader(data), headers)
}
func (c *Client) form(ctx context.Context, endpoint string, fields map[string]string, headers http.Header) error {
	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	return c.do(ctx, "POST", endpoint, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), headers)
}

// multipart 通过管道流式发送附件，避免将大文件读入内存。
func (c *Client) multipart(ctx context.Context, endpoint string, fields map[string]string, media []model.Media, key string, headers http.Header) error {
	return c.multipartMethod(ctx, http.MethodPost, endpoint, fields, media, key, headers)
}

func (c *Client) multipartMethod(ctx context.Context, method, endpoint string, fields map[string]string, media []model.Media, key string, headers http.Header) error {
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		err := func() error {
			for k, v := range fields {
				if err := form.WriteField(k, v); err != nil {
					return err
				}
			}
			for i, m := range media {
				name := m.Filename
				if name == "" {
					name = filepath.Base(m.Path)
				}
				field := key
				if strings.Contains(key, "%") {
					field = fmt.Sprintf(key, i)
				}
				part, err := form.CreateFormFile(field, name)
				if err != nil {
					return err
				}
				file, err := os.Open(m.Path)
				if err != nil {
					return err
				}
				_, err = io.Copy(part, file)
				file.Close()
				if err != nil {
					return err
				}
			}
			return form.Close()
		}()
		writer.CloseWithError(err)
		done <- err
	}()
	err := c.do(ctx, method, endpoint, form.FormDataContentType(), reader, headers)
	reader.Close()
	writeErr := <-done
	if err != nil {
		return err
	}
	return writeErr
}

func (c *Client) ntfy(ctx context.Context, u *url.URL, text string, media []model.Media) error {
	base := webBase(u)
	targets := targetParts(u)
	if len(targets) == 0 {
		base = "https://ntfy.sh"
		targets = []string{u.Host}
	}
	q := u.Query()
	headers := copyHeaders(u)
	if token := q.Get("token"); token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	for _, topic := range targets {
		if len(media) == 0 {
			data := map[string]any{"topic": topic, "message": text}
			for _, key := range []string{"title", "priority", "click", "attach", "filename", "delay", "email"} {
				if v := q.Get(key); v != "" {
					if key == "priority" {
						data[key] = integer(v, 3)
					} else {
						data[key] = v
					}
				}
			}
			if v := q.Get("tags"); v != "" {
				data["tags"] = strings.Split(v, ",")
			}
			if err := c.json(ctx, base, data, headers); err != nil {
				return err
			}
		} else {
			for i, m := range media {
				params := url.Values{}
				caption := text
				if i > 0 {
					caption = fmt.Sprintf("媒体组的第 %d 个文件", i+1)
				}
				params.Set("message", caption)
				name := m.Filename
				if name == "" {
					name = filepath.Base(m.Path)
				}
				params.Set("filename", name)
				for _, key := range []string{"title", "priority", "tags", "click", "delay", "email"} {
					if v := q.Get(key); v != "" {
						params.Set(key, v)
					}
				}
				file, err := os.Open(m.Path)
				if err != nil {
					return err
				}
				err = c.do(ctx, "PUT", base+"/"+url.PathEscape(topic)+"?"+params.Encode(), "application/octet-stream", file, headers)
				file.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *Client) telegram(ctx context.Context, u *url.URL, text string, media []model.Media) error {
	token, err := url.PathUnescape(u.Host)
	if err != nil || !strings.Contains(token, ":") {
		return errors.New("Telegram 推送 bot token 无效")
	}
	targets := targetParts(u)
	if len(targets) == 0 {
		return errors.New("Telegram 推送必须指定目标 chat ID")
	}
	q := u.Query()
	base := "https://api.telegram.org/bot" + token
	for _, target := range targets {
		chat, thread, _ := strings.Cut(target, ":")
		fields := map[string]string{"chat_id": chat, "disable_notification": strconv.FormatBool(truth(q.Get("silent"))), "disable_web_page_preview": strconv.FormatBool(!truth(q.Get("preview")))}
		if thread != "" {
			fields["message_thread_id"] = thread
		}
		format := q.Get("format")
		if format == "html" {
			fields["parse_mode"] = "HTML"
		} else if format == "markdown" {
			fields["parse_mode"] = "Markdown"
		}
		if len(media) == 0 {
			fields["text"] = text
			if err := c.form(ctx, base+"/sendMessage", fields, nil); err != nil {
				return err
			}
		} else {
			for i, m := range media {
				fields["caption"] = text
				if i > 0 {
					fields["caption"] = fmt.Sprintf("媒体组的第 %d 个文件", i+1)
				}
				if err := c.multipart(ctx, base+"/sendDocument", fields, []model.Media{m}, "document", nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validMethod(method string) bool {
	switch method {
	case "", "POST", "GET", "DELETE", "PUT", "HEAD", "PATCH":
		return true
	}
	return false
}

func (c *Client) generic(ctx context.Context, u *url.URL, text string, media []model.Media) error {
	q := u.Query()
	endpoint := webBase(u) + u.EscapedPath()
	if u.Path == "" {
		endpoint += "/"
	}
	headers := copyHeaders(u)
	method := strings.ToUpper(q.Get("method"))
	if method == "" {
		method = "POST"
	}
	if !validMethod(method) {
		return errors.New("通用推送 HTTP method 无效")
	}
	params := url.Values{}
	for k, v := range q {
		if strings.HasPrefix(k, "-") {
			params[strings.TrimPrefix(k, "-")] = v
		}
	}
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	if u.Scheme == "form" || u.Scheme == "forms" {
		fields := map[string]string{"version": "1.0", "title": q.Get("title"), "message": text, "type": "info"}
		for k, v := range q {
			if strings.HasPrefix(k, ":") {
				key := strings.TrimPrefix(k, ":")
				if previous, ok := fields[key]; ok {
					delete(fields, key)
					if v[0] != "" {
						fields[v[0]] = previous
					}
				} else {
					fields[key] = v[0]
				}
			}
		}
		if len(media) > 0 {
			key := "file%02d"
			if configured := q.Get("attach-as"); configured != "" {
				key = strings.ReplaceAll(configured, "{}", "%02d")
				key = strings.ReplaceAll(key, "*", "%02d")
			}
			return c.multipartMethod(ctx, method, endpoint, fields, media, key, headers)
		}
		values := url.Values{}
		for k, v := range fields {
			values.Set(k, v)
		}
		return c.do(ctx, method, endpoint, "application/x-www-form-urlencoded", strings.NewReader(values.Encode()), headers)
	}
	data := map[string]any{"version": "1.0", "title": q.Get("title"), "message": text, "type": "info", "attachments": []map[string]any{}}
	if len(media) > 0 {
		attachments := []map[string]any{}
		for _, m := range media {
			info, err := os.Stat(m.Path)
			if err != nil {
				return err
			}
			if info.Size() > 32<<20 {
				return errors.New("通用 JSON 附件超过 32 MiB，请使用支持流式附件的协议")
			}
			bytes, err := os.ReadFile(m.Path)
			if err != nil {
				return err
			}
			name := m.Filename
			if name == "" {
				name = path.Base(m.Path)
			}
			kind := m.MIME
			if kind == "" {
				kind = mime.TypeByExtension(filepath.Ext(name))
			}
			if kind == "" {
				kind = "application/octet-stream"
			}
			attachments = append(attachments, map[string]any{"filename": name, "base64": base64.StdEncoding.EncodeToString(bytes), "mimetype": kind})
		}
		data["attachments"] = attachments
	}
	for k, v := range q {
		if strings.HasPrefix(k, ":") {
			key := strings.TrimPrefix(k, ":")
			if previous, ok := data[key]; ok {
				delete(data, key)
				if v[0] != "" {
					data[v[0]] = previous
				}
			} else {
				data[key] = v[0]
			}
		}
	}
	bytes, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.do(ctx, method, endpoint, "application/json", strings.NewReader(string(bytes)), headers)
}
