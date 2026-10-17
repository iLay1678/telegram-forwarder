package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, provider, base string) *Client {
	t.Helper()
	t.Setenv(strings.ToUpper(provider)+"_API_KEY", "fixture-secret")
	t.Setenv(strings.ToUpper(provider)+"_API_BASE", base)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string][]string{provider: {"fixture-model"}})
	if err = os.WriteFile(filepath.Join(cfg.Root, "config", "ai_models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return New(cfg)
}

func TestProvidersStreamingAndImages(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek", "grok", "qwen", "gemini", "claude"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["model"] != "fixture-model" || body["stream"] != true {
					t.Errorf("请求参数 %v", body)
				}
				messages := body["messages"].([]any)
				var parts []any
				if provider == "claude" {
					if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "fixture-secret" || body["system"] != "提示" {
						t.Error("Claude 请求契约不匹配")
					}
					parts = messages[0].(map[string]any)["content"].([]any)
				} else {
					if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
						t.Error("OpenAI 兼容请求契约不匹配")
					}
					if messages[0].(map[string]any)["content"] != "提示" {
						t.Error("丢失 system prompt")
					}
					parts = messages[1].(map[string]any)["content"].([]any)
				}
				if len(parts) != 2 {
					t.Error("丢失图片")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "claude" {
					_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"回答\"}}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"隐含推理\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"回\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"答\"}}]}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			client := testClient(t, provider, server.URL+"/v1")
			got, err := client.Process(context.Background(), "fixture-model", "提示", "消息", []model.AIImage{{MIME: "image/png", Data: []byte{1, 2, 3}}})
			if err != nil || got != "回答" {
				t.Fatalf("结果 %q, %v", got, err)
			}
		})
	}
}

func TestGeminiNativeAndErrorRedaction(t *testing.T) {
	c := testClient(t, "gemini", "")
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "generativelanguage.googleapis.com" || r.URL.Path != "/v1beta/models/fixture-model:streamGenerateContent" || r.Header.Get("x-goog-api-key") != "fixture-secret" {
			t.Error("原生 Gemini 请求不匹配")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		parts := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
		if parts[0].(map[string]any)["text"] != "提示\n\n消息" || len(parts) != 2 {
			t.Error("原生 Gemini 多模态内容不匹配")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"结果\"}]}}]}\n\n"))}, nil
	})
	got, err := c.Process(context.Background(), "fixture-model", "提示", "消息", []model.AIImage{{MIME: "image/png", Data: []byte{1}}})
	if err != nil || got != "结果" {
		t.Fatalf("%q %v", got, err)
	}
	c.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("fixture-secret"))}, nil
	})
	_, err = c.Process(context.Background(), "fixture-model", "", "消息", nil)
	if err == nil || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("认证失败未脱敏: %v", err)
	}
}
