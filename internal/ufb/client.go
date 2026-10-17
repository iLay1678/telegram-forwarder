// Package ufb 实现旧版 UFB WebSocket 配置同步协议。
package ufb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type Client struct {
	cfg     *config.Config
	store   *storage.Store
	path    string
	mu      sync.Mutex
	writeMu sync.Mutex
	conn    *websocket.Conn
}

func New(cfg *config.Config, store *storage.Store) *Client {
	return &Client{cfg: cfg, store: store, path: filepath.Join(cfg.Root, "ufb", "config", "config.json")}
}

func (c *Client) load() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var conf map[string]any
	if err = json.Unmarshal(data, &conf); err != nil {
		return nil, fmt.Errorf("UFB 配置无效: %w", err)
	}
	if conf == nil {
		conf = map[string]any{}
	}
	return conf, nil
}

func (c *Client) save(conf map[string]any) error {
	data, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".config-*.json")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, c.path)
}

// Merge 保留未知字段，字典递归合并，列表按完整值去重，标量采用云端值。
func Merge(local, cloud map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range local {
		merged[k] = v
	}
	for k, v := range cloud {
		switch v := v.(type) {
		case map[string]any:
			old, _ := merged[k].(map[string]any)
			merged[k] = Merge(old, v)
		case []any:
			old, _ := merged[k].([]any)
			items := append([]any{}, old...)
			for _, n := range v {
				found := false
				for _, o := range items {
					if reflect.DeepEqual(o, n) {
						found = true
						break
					}
				}
				if !found {
					items = append(items, n)
				}
			}
			merged[k] = items
		default:
			merged[k] = v
		}
	}
	return merged
}

func object(parent map[string]any, key string) map[string]any {
	v, ok := parent[key].(map[string]any)
	if !ok {
		v = map[string]any{}
		parent[key] = v
	}
	return v
}
func stamp(conf map[string]any, overwrite bool) {
	syncCfg := object(object(conf, "globalConfig"), "SYNC_CONFIG")
	if _, ok := syncCfg["lastSyncTime"]; !ok || overwrite {
		syncCfg["lastSyncTime"] = time.Now().UnixMilli()
	}
}

func (c *Client) endpoint() (string, error) {
	server := strings.TrimSpace(c.cfg.String("UFB_SERVER_URL", ""))
	token := c.cfg.String("UFB_TOKEN", "")
	if server == "" || token == "" {
		return "", errors.New("UFB_SERVER_URL 和 UFB_TOKEN 必须同时配置")
	}
	if !strings.Contains(server, "://") {
		server = "wss://" + server
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return "", errors.New("UFB_SERVER_URL 无效")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", errors.New("UFB_SERVER_URL 协议必须为 HTTP 或 WebSocket")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/config/" + token
	return u.String(), nil
}

// Run 断线后重连，取消上下文会立即关闭连接。
func (c *Client) Run(ctx context.Context) error {
	if !c.cfg.Bool("UFB_ENABLED", false) {
		<-ctx.Done()
		return ctx.Err()
	}
	endpoint, err := c.endpoint()
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 15 * time.Second}).DialContext(ctx, endpoint, nil)
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if err == nil {
			err = c.session(ctx, conn)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("UFB 连接已中断，10 秒后重试")
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) session(ctx context.Context, conn *websocket.Conn) error {
	c.mu.Lock()
	c.conn = conn
	conf, err := c.load()
	_, existsErr := os.Stat(c.path)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		conn.Close()
	}()
	if err != nil {
		return err
	}
	stamp(conf, false)
	if errors.Is(existsErr, os.ErrNotExist) {
		conf["type"] = "firstSync"
	} else {
		conf["type"] = "update"
	}
	if err = c.send(conf); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	conn.SetReadLimit(16 << 20)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	var conflict map[string]any
	for {
		var data map[string]any
		if err := conn.ReadJSON(&data); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		if conflict != nil {
			data = Merge(conflict, data)
			conflict = nil
			if err = c.receive(ctx, data, true); err != nil {
				return err
			}
			continue
		}
		switch data["type"] {
		case "firstSync":
			if data["message"] == "firstSync_success" {
				if err = c.receive(ctx, data, true); err != nil {
					return err
				}
			}
		case "update":
			if err = c.receive(ctx, data, data["additional_info"] != "to_server"); err != nil {
				return err
			}
		case "configConflict":
			c.mu.Lock()
			conflict, err = c.load()
			c.mu.Unlock()
			if err != nil {
				return err
			}
			if err = c.send(map[string]any{"type": "resolveConflict", "choice": "useCloud"}); err != nil {
				return err
			}
		case "delete":
			if data["success"] == false {
				return errors.New("UFB 服务端拒绝删除配置")
			}
		}
	}
}

func (c *Client) send(data map[string]any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("UFB 未连接；本地配置已保存，重连后会同步")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}
	return conn.WriteJSON(data)
}

var itemKeys = map[string]string{"main": "mainAndSubPageKeywords", "content": "contentPageKeywords", "main_username": "mainAndSubPageUserKeywords", "content_username": "contentPageUserKeywords"}

func (c *Client) Sync(ctx context.Context, ruleID int64) error {
	rule, err := c.store.Rule(ctx, ruleID)
	if err != nil {
		return err
	}
	if !rule.IsUFB || rule.UFBDomain == "" {
		return nil
	}
	key, ok := itemKeys[rule.UFBItem]
	if !ok {
		return fmt.Errorf("规则 %d 的 UFB 类型无效", ruleID)
	}
	c.mu.Lock()
	conf, err := c.load()
	if err != nil {
		c.mu.Unlock()
		return err
	}
	users, _ := conf["userConfig"].([]any)
	found := false
	for _, raw := range users {
		user, ok := raw.(map[string]any)
		if !ok || user["domain"] != rule.UFBDomain {
			continue
		}
		kw := object(user, key)
		words, patterns := []string{}, []string{}
		for _, k := range rule.Keywords {
			if k.IsRegex {
				patterns = append(patterns, k.Keyword)
			} else {
				words = append(words, k.Keyword)
			}
		}
		kw["keywords"], kw["regexPatterns"] = words, patterns
		found = true
		break
	}
	if !found {
		c.mu.Unlock()
		return fmt.Errorf("UFB 配置中没有规则 %d 绑定的域名", ruleID)
	}
	stamp(conf, true)
	conf["type"] = "update"
	conf["additional_info"] = "to_server"
	err = c.save(conf)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.send(conf)
}

func (c *Client) receive(ctx context.Context, conf map[string]any, toDB bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if toDB {
		if err := c.importKeywords(ctx, conf); err != nil {
			return err
		}
	}
	return c.save(conf)
}

func (c *Client) importKeywords(ctx context.Context, conf map[string]any) error {
	rules, err := c.store.Rules(ctx)
	if err != nil {
		return err
	}
	users, _ := conf["userConfig"].([]any)
	tx, err := c.store.DB.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, rule := range rules {
		if !rule.IsUFB || rule.UFBDomain == "" {
			continue
		}
		key, ok := itemKeys[rule.UFBItem]
		if !ok {
			continue
		}
		for _, raw := range users {
			user, ok := raw.(map[string]any)
			if !ok || user["domain"] != rule.UFBDomain {
				continue
			}
			kw, _ := user[key].(map[string]any)
			if _, err = tx.ExecContext(ctx, "DELETE FROM keywords WHERE rule_id = ?", rule.ID); err != nil {
				return err
			}
			for _, kind := range []string{"keywords", "regexPatterns"} {
				items, _ := kw[kind].([]any)
				for _, item := range items {
					word, ok := item.(string)
					if !ok {
						return errors.New("UFB 关键词必须为字符串")
					}
					if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO keywords(rule_id, keyword, is_regex, is_blacklist) VALUES (?, ?, ?, 1)", rule.ID, word, kind == "regexPatterns"); err != nil {
						return err
					}
				}
			}
			break
		}
	}
	return tx.Commit()
}
