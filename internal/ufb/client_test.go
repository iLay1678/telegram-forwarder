package ufb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

func fixture(t *testing.T) (*Client, model.Rule) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(filepath.Join(cfg.Root, "db", "forward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rule, err := db.Bind(context.Background(), model.Peer{ID: 1, Kind: "channel", Name: "来源"}, model.Peer{ID: 2, Kind: "channel", Name: "目标"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DB.Exec("UPDATE forward_rules SET is_ufb=1,ufb_domain='example.com',ufb_item='main' WHERE id=?", rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, db), rule
}

func TestCloudImportUsesLegacyBlacklistDefaultAndPreservesUnknown(t *testing.T) {
	c, rule := fixture(t)
	conf := map[string]any{"unknown": map[string]any{"keep": true}, "userConfig": []any{map[string]any{"domain": "example.com", "mainAndSubPageKeywords": map[string]any{"keywords": []any{"广告", "广告"}, "regexPatterns": []any{`(?P<name>促销)`}}}}}
	if err := c.receive(context.Background(), conf, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.store.Rule(context.Background(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Keywords) != 2 {
		t.Fatalf("导入关键词: %v", loaded.Keywords)
	}
	for _, k := range loaded.Keywords {
		if !k.IsBlacklist {
			t.Fatal("旧版 UFB 导入使用黑名单默认值")
		}
	}
	err = c.Sync(context.Background(), rule.ID)
	if err == nil {
		t.Fatal("离线同步应明确报告连接状态")
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(data, &result) != nil || result["unknown"].(map[string]any)["keep"] != true {
		t.Fatal("未知字段丢失")
	}
}

func TestWebsocketFirstSyncConflictAndCancellation(t *testing.T) {
	c, _ := fixture(t)
	upgrader := websocket.Upgrader{}
	received := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var first map[string]any
		if err = conn.ReadJSON(&first); err != nil {
			return
		}
		received <- first
		_ = conn.WriteJSON(map[string]any{"type": "firstSync", "message": "firstSync_success", "localKeep": true})
		_ = conn.WriteJSON(map[string]any{"type": "configConflict"})
		var resolve map[string]any
		if err = conn.ReadJSON(&resolve); err != nil {
			return
		}
		received <- resolve
		_ = conn.WriteJSON(map[string]any{"type": "update", "cloudKeep": true})
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.session(ctx, conn) }()
	for i, want := range []string{"firstSync", "resolveConflict"} {
		select {
		case msg := <-received:
			if msg["type"] != want || (i == 1 && msg["choice"] != "useCloud") {
				t.Fatalf("错误协议: %v", msg)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("等待同步超时")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		saved, err := c.load()
		c.mu.Unlock()
		if err == nil && saved["cloudKeep"] == true {
			if saved["localKeep"] != true {
				t.Fatal("冲突合并丢失本地字段")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("冲突响应未落盘")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消未关闭 WebSocket")
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	local := map[string]any{"data": map[string]any{"keep": true}, "list": []any{"a"}}
	cloud := map[string]any{"data": map[string]any{"new": true}, "list": []any{"a", "b"}}
	merged := Merge(local, cloud)
	if len(merged["list"].([]any)) != 2 || merged["data"].(map[string]any)["keep"] != true {
		t.Fatal("合并值错误")
	}
	if len(local["data"].(map[string]any)) != 1 || len(local["list"].([]any)) != 1 {
		t.Fatal("合并修改输入")
	}
}
