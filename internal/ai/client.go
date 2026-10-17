// Package ai 提供与既有模型配置兼容的六家 AI 接口。
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
)

type Client struct {
	cfg  *config.Config
	HTTP *http.Client
}

func New(cfg *config.Config) *Client {
	return &Client{cfg: cfg, HTTP: &http.Client{Timeout: 3 * time.Minute}}
}

var bases = map[string]string{
	"openai": "https://api.openai.com/v1", "deepseek": "https://api.deepseek.com/v1",
	"grok": "https://api.x.ai/v1", "qwen": "https://dashscope.aliyuncs.com/compatible-mode/v1",
	"claude": "https://api.anthropic.com", "gemini": "https://generativelanguage.googleapis.com/v1beta",
}

func (c *Client) provider(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.cfg.Root, "config", "ai_models.json"))
	if err == nil {
		var providers map[string][]string
		if err := json.Unmarshal(data, &providers); err != nil {
			return "", fmt.Errorf("AI 模型配置无效: %w", err)
		}
		for provider, models := range providers {
			for _, n := range models {
				if n == name {
					if _, ok := bases[provider]; !ok {
						return "", fmt.Errorf("不支持的 AI 供应商 %q", provider)
					}
					return provider, nil
				}
			}
		}
		return "", fmt.Errorf("模型 %q 未在 ai_models.json 中配置", name)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, p := range []string{"gemini", "claude", "deepseek", "grok", "qwen", "qwq", "qvq"} {
		if strings.HasPrefix(name, p) {
			if p == "qwq" || p == "qvq" {
				p = "qwen"
			}
			return p, nil
		}
	}
	if strings.HasPrefix(name, "gpt-") || strings.HasPrefix(name, "chatgpt-") || strings.HasPrefix(name, "o1") || strings.HasPrefix(name, "o3") {
		return "openai", nil
	}
	return "", fmt.Errorf("不支持的模型 %q", name)
}

func (c *Client) Process(ctx context.Context, name, prompt, text string, images []model.AIImage) (string, error) {
	if name == "" {
		name = c.cfg.String("DEFAULT_AI_MODEL", "gpt-4o")
	}
	p, err := c.provider(name)
	if err != nil {
		return "", err
	}
	prefix := strings.ToUpper(p)
	key := c.cfg.String(prefix+"_API_KEY", "")
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("未设置 %s_API_KEY", prefix)
	}
	custom := strings.TrimSpace(c.cfg.String(prefix+"_API_BASE", ""))
	base := bases[p]
	if custom != "" {
		base = custom
	}
	endpoint := strings.TrimRight(base, "/")
	headers := map[string]string{}
	body := map[string]any{"model": name, "stream": true}
	switch {
	case p == "claude":
		if !strings.HasSuffix(endpoint, "/v1") {
			endpoint += "/v1"
		}
		endpoint += "/messages"
		headers["x-api-key"], headers["anthropic-version"] = key, "2023-06-01"
		parts := []any{map[string]any{"type": "text", "text": text}}
		for _, img := range images {
			parts = append(parts, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime(img), "data": base64.StdEncoding.EncodeToString(img.Data)}})
		}
		body["messages"] = []any{map[string]any{"role": "user", "content": parts}}
		body["max_tokens"] = 4096
		if prompt != "" {
			body["system"] = prompt
		}
	case p == "gemini" && custom == "":
		endpoint += "/models/" + url.PathEscape(strings.TrimPrefix(name, "models/")) + ":streamGenerateContent?alt=sse"
		headers["x-goog-api-key"] = key
		if prompt != "" {
			text = prompt + "\n\n" + text
		}
		parts := []any{map[string]any{"text": text}}
		for _, img := range images {
			parts = append(parts, map[string]any{"inline_data": map[string]any{"mime_type": mime(img), "data": base64.StdEncoding.EncodeToString(img.Data)}})
		}
		body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}}
		settings := []any{}
		for _, category := range []string{"HARASSMENT", "HATE_SPEECH", "SEXUALLY_EXPLICIT", "DANGEROUS_CONTENT"} {
			settings = append(settings, map[string]string{"category": "HARM_CATEGORY_" + category, "threshold": "BLOCK_NONE"})
		}
		body["safetySettings"] = settings
	default:
		endpoint += "/chat/completions"
		headers["Authorization"] = "Bearer " + key
		messages := []any{}
		if prompt != "" {
			messages = append(messages, map[string]any{"role": "system", "content": prompt})
		}
		var content any = text
		if len(images) > 0 {
			parts := []any{map[string]any{"type": "text", "text": text}}
			for _, img := range images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + mime(img) + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}})
			}
			content = parts
		}
		body["messages"] = append(messages, map[string]any{"role": "user", "content": content})
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%s API URL 无效", p)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%s AI 请求失败", p)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s AI 返回 HTTP %d", p, resp.StatusCode)
	}
	var result strings.Builder
	reasoning := false
	decode := func(data []byte) error {
		var chunk struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("%s AI 响应无效: %w", p, err)
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("%s AI 返回错误事件", p)
		}
		if len(chunk.Choices) > 0 {
			v := chunk.Choices[0]
			result.WriteString(v.Delta.Content)
			result.WriteString(v.Message.Content)
			reasoning = reasoning || v.Delta.Reasoning != ""
		}
		result.WriteString(chunk.Delta.Text)
		for _, v := range chunk.Content {
			result.WriteString(v.Text)
		}
		if len(chunk.Candidates) > 0 {
			for _, v := range chunk.Candidates[0].Content.Parts {
				result.WriteString(v.Text)
			}
		}
		return nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return "", err
		}
		if err = decode(data); err != nil {
			return "", err
		}
	} else {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		var event []string
		flush := func() error {
			if len(event) == 0 {
				return nil
			}
			d := strings.Join(event, "\n")
			event = nil
			if d == "[DONE]" {
				return nil
			}
			return decode([]byte(d))
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if err := flush(); err != nil {
					return "", err
				}
			} else if strings.HasPrefix(line, "data:") {
				event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		if err := scanner.Err(); err != nil {
			return "", err
		}
		if err := flush(); err != nil {
			return "", err
		}
	}
	if result.Len() == 0 && reasoning {
		return "模型未能生成有效回答", nil
	}
	return result.String(), nil
}

func mime(img model.AIImage) string {
	if img.MIME != "" {
		return img.MIME
	}
	return http.DetectContentType(img.Data)
}
