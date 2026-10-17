// Package rss 保留旧 RSS HTTP 接口、磁盘条目格式和管理页面。
package rss

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/config"
	"github.com/iLay1678/telegram-forwarder/internal/model"
	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
	"github.com/iLay1678/telegram-forwarder/internal/rules"
	"github.com/iLay1678/telegram-forwarder/internal/storage"
)

type Media struct {
	URL          string  `json:"url"`
	Type         string  `json:"type"`
	Size         int64   `json:"size"`
	Filename     string  `json:"filename"`
	OriginalName *string `json:"original_name"`
}

type Entry struct {
	ID           string  `json:"id"`
	RuleID       int64   `json:"rule_id"`
	MessageID    string  `json:"message_id"`
	Title        string  `json:"title"`
	Content      string  `json:"content"`
	Published    string  `json:"published"`
	Author       string  `json:"author"`
	Link         string  `json:"link"`
	Media        []Media `json:"media"`
	CreatedAt    string  `json:"created_at"`
	OriginalLink *string `json:"original_link"`
	SenderInfo   *string `json:"sender_info"`
}

type Server struct {
	cfg                 *config.Config
	store               *storage.Store
	ai                  model.AIProcessor
	dataPath, mediaPath string
	mu                  sync.Mutex
	secret              []byte
	mux                 *http.ServeMux
}

func New(cfg *config.Config, store *storage.Store, ai model.AIProcessor) (*Server, error) {
	s := &Server{cfg: cfg, store: store, ai: ai, dataPath: cfg.Path(cfg.String("RSS_DATA_PATH", "./rss/data")), mediaPath: cfg.Path(cfg.String("RSS_MEDIA_PATH", "./rss/media")), secret: make([]byte, 32), mux: http.NewServeMux()}
	if _, err := rand.Read(s.secret); err != nil {
		return nil, err
	}
	for _, dir := range []string{s.dataPath, s.mediaPath} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return nil, err
		}
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// 表单和本地条目接口都限制请求体，避免无界内存占用。
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != "" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Host != r.Host {
				reply(w, http.StatusForbidden, map[string]any{"detail": "跨站请求被拒绝"})
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("GET /register", s.registerPage)
	s.mux.HandleFunc("POST /register", s.register)
	s.mux.HandleFunc("GET /logout", s.logout)
	s.mux.HandleFunc("GET /rss/dashboard", s.dashboard)
	s.mux.HandleFunc("POST /rss/change_password", s.changePassword)
	s.mux.HandleFunc("POST /rss/config", s.configSave)
	s.mux.HandleFunc("GET /rss/toggle/{rule_id}", s.toggle)
	s.mux.HandleFunc("GET /rss/delete/{rule_id}", s.configDelete)
	s.mux.HandleFunc("GET /rss/patterns/{config_id}", s.patterns)
	s.mux.HandleFunc("POST /rss/pattern", s.patternSave)
	s.mux.HandleFunc("DELETE /rss/pattern/{pattern_id}", s.patternDelete)
	s.mux.HandleFunc("DELETE /rss/patterns/{config_id}", s.patternsDelete)
	s.mux.HandleFunc("POST /rss/test-regex", s.testRegex)
	s.mux.HandleFunc("GET /rss/feed/{rule_id}", s.feed)
	s.mux.HandleFunc("GET /media/{rule_id}/{filename}", s.media)
	s.mux.HandleFunc("POST /api/entries/{rule_id}/add", s.entryAdd)
	s.mux.HandleFunc("GET /api/entries/{rule_id}", s.entries)
	s.mux.HandleFunc("DELETE /api/entries/{rule_id}/{entry_id}", s.entryDelete)
	s.mux.HandleFunc("DELETE /api/rule/{rule_id}", s.ruleDelete)
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, err error) {
	reply(w, status, map[string]any{"success": false, "message": err.Error(), "detail": err.Error()})
}
func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		failure(w, 422, errors.New("ID 必须为正整数"))
		return 0, false
	}
	return id, true
}
func (s *Server) local(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified()) {
		return true
	}
	failure(w, 403, errors.New("此API端点仅允许本地或内部网络访问"))
	return false
}
func (s *Server) rule(ctx context.Context, id int64) (model.Rule, error) {
	rule, err := s.store.Rule(ctx, id)
	if err != nil {
		return rule, err
	}
	if rule.RSS == nil {
		return rule, errors.New("RSS 配置不存在")
	}
	return rule, nil
}

func (s *Server) entryPath(id int64) string {
	return filepath.Join(s.dataPath, strconv.FormatInt(id, 10), "entries.json")
}
func (s *Server) readEntries(id int64) ([]Entry, error) {
	data, err := os.ReadFile(s.entryPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err = json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("RSS 条目文件无效: %w", err)
	}
	for i := range entries {
		if entries[i].MessageID == "" {
			entries[i].MessageID = entries[i].ID
		}
		if entries[i].Media == nil {
			entries[i].Media = []Media{}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Published > entries[j].Published })
	return entries, nil
}
func atomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".entries-*.json")
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
	return os.Rename(name, path)
}
func uuid() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	v := hex.EncodeToString(b)
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}

func (s *Server) add(ctx context.Context, rule model.Rule, entry Entry) error {
	if err := s.extract(ctx, rule, &entry); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readEntries(rule.ID)
	if err != nil {
		return err
	}
	entry.RuleID = rule.ID
	if entry.ID == "" {
		entry.ID = uuid()
	}
	if entry.MessageID == "" {
		entry.MessageID = entry.ID
	}
	if entry.Media == nil {
		entry.Media = []Media{}
	}
	entry.CreatedAt = time.Now().In(s.cfg.Timezone).Format("2006-01-02T15:04:05.999999")
	entries = append(entries, entry)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Published > entries[j].Published })
	max := rule.RSS.MaxItems
	if max <= 0 {
		max = 50
	}
	var removed []Entry
	if len(entries) > max {
		removed = entries[max:]
		entries = entries[:max]
	}
	if err = atomicJSON(s.entryPath(rule.ID), entries); err != nil {
		return err
	}
	// 仅清理已淘汰条目引用且未被保留条目共享的媒体。
	keep := map[string]bool{}
	for _, e := range entries {
		for _, m := range e.Media {
			keep[m.Filename] = true
		}
	}
	for _, e := range removed {
		for _, m := range e.Media {
			if !keep[m.Filename] && safeFilename(m.Filename) {
				if err = os.Remove(filepath.Join(s.mediaPath, strconv.FormatInt(rule.ID, 10), m.Filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Server) extract(ctx context.Context, rule model.Rule, entry *Entry) error {
	cfg := rule.RSS
	if cfg.IsAIExtract && s.ai != nil {
		result, err := s.ai.Process(ctx, rule.AIModel, cfg.AIExtractPrompt, entry.Content, nil)
		start, end := strings.Index(result, "{"), strings.LastIndex(result, "}")
		var data struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		if err == nil && start >= 0 && end >= start {
			err = json.Unmarshal([]byte(result[start:end+1]), &data)
		} else if err == nil {
			err = errors.New("RSS AI 提取未返回 JSON 对象")
		}
		if err != nil {
			// 旧版提取失败时仍保存原条目，避免 AI 临时故障导致消息丢失。
			slog.Warn("RSS AI 提取失败，保留原内容", "rule", rule.ID)
		} else {
			entry.Title, entry.Content = data.Title, data.Content
		}
	}
	if cfg.EnableCustomTitlePattern || cfg.EnableCustomContentPattern {
		rows, err := s.store.DB.QueryxContext(ctx, "SELECT pattern, pattern_type FROM rss_patterns WHERE rss_config_id = ? ORDER BY COALESCE(priority,0), id", cfg.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		original := entry.Content
		for rows.Next() {
			var pattern, kind string
			if err = rows.Scan(&pattern, &kind); err != nil {
				return err
			}
			if kind == "title" && !cfg.EnableCustomTitlePattern || kind == "content" && !cfg.EnableCustomContentPattern {
				continue
			}
			input := original
			if kind == "content" {
				input = entry.Content
			}
			matches, err := pyregex.Find(pattern, input)
			if err != nil {
				slog.Warn("RSS 提取正则无效，跳过该模式", "rule", rule.ID)
				continue
			}
			if len(matches) > 1 {
				if kind == "title" {
					entry.Title = matches[1]
				} else if kind == "content" {
					entry.Content = matches[1]
				}
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
	}
	if entry.SenderInfo != nil && strings.TrimSpace(*entry.SenderInfo) != "" {
		entry.Content = strings.TrimSpace(*entry.SenderInfo) + ":\n\n" + entry.Content
	}
	if entry.OriginalLink != nil {
		link := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(*entry.OriginalLink, "原始消息:", ""), "\n", ""), "\r", ""))
		if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
			label := "来源"
			if entry.Author != "" {
				label += ": " + entry.Author
			}
			entry.Content += "\n\n[" + label + "](" + link + ")"
		}
	}
	return nil
}

func (s *Server) Publish(ctx context.Context, rule model.Rule, message model.Message, text string, media []model.Media) error {
	if rule.RSS == nil || !rule.RSS.EnableRSS {
		return nil
	}
	entry := Entry{RuleID: rule.ID, MessageID: strconv.Itoa(message.ID), Title: shortTitle(text, 30), Content: text, Published: message.Date.Format(time.RFC3339Nano), Author: message.Sender.Name, Link: rules.MessageLink(message), Media: []Media{}}
	if message.GroupedID == 0 {
		// 旧版单条 RSS 使用原正文；相册才使用替换、AI 处理后的正文。
		entry.Content = message.Text
		entry.Title = singleTitle(message)
		if rule.IsOriginalSender {
			infoRule := rule
			infoRule.IsOriginalLink, infoRule.IsOriginalTime = false, false
			info := rules.AddInfo(infoRule, message, "", s.cfg.Timezone)
			entry.SenderInfo = &info
		}
		if rule.IsOriginalLink {
			infoRule := rule
			infoRule.IsOriginalSender, infoRule.IsOriginalTime = false, false
			info := rules.AddInfo(infoRule, message, "", s.cfg.Timezone)
			entry.OriginalLink = &info
		}
	} else {
		if len(media) == 0 {
			return nil
		}
		if entry.Title == "" {
			entry.Title = fmt.Sprintf("媒体组消息 (%d个文件)", len(media))
		}
	}
	if entry.Title == "" {
		entry.Title = "新消息"
	}
	for _, m := range media {
		if m.Path == "" {
			return errors.New("RSS 媒体未下载到本地")
		}
		name := m.Filename
		if name == "" {
			name = filepath.Base(m.Path)
		}
		name = sanitize(name)
		if name == "" {
			name = fmt.Sprintf("media_%d", m.MessageID)
		}
		// 同名上传必须各自保存，避免新消息覆盖旧条目的媒体。
		name = uuid() + "_" + name
		dest := filepath.Join(s.mediaPath, strconv.FormatInt(rule.ID, 10), name)
		if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
			return err
		}
		srcAbs, _ := filepath.Abs(m.Path)
		dstAbs, _ := filepath.Abs(dest)
		if srcAbs != dstAbs {
			if err := copyFile(m.Path, dest); err != nil {
				return err
			}
		}
		info, err := os.Stat(dest)
		if err != nil {
			return err
		}
		typ := m.MIME
		if typ == "" {
			typ = mime.TypeByExtension(filepath.Ext(name))
		}
		if typ == "" {
			typ = "application/octet-stream"
		}
		original := m.Filename
		entry.Media = append(entry.Media, Media{URL: fmt.Sprintf("/media/%d/%s", rule.ID, url.PathEscape(name)), Type: typ, Size: info.Size(), Filename: name, OriginalName: &original})
	}
	return s.add(ctx, rule, entry)
}

func singleTitle(message model.Message) string {
	first := strings.SplitN(message.Text, "\n", 2)[0]
	if chars := []rune(first); len(chars) >= 20 {
		return strings.TrimSpace(string(chars[:20])) + "..."
	}
	if title := strings.TrimSpace(first); title != "" {
		return title
	}
	if len(message.Media) > 0 {
		media := message.Media[0]
		switch media.Kind {
		case "photo":
			return "图片消息"
		case "video":
			return "视频消息"
		case "voice":
			return "语音消息"
		case "audio":
			if media.Filename != "" {
				return "音频: " + media.Filename
			}
			return "音频消息"
		default:
			if media.Filename != "" {
				return "文件: " + media.Filename
			}
			return "文件消息"
		}
	}
	return "新消息"
}
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".media-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	defer os.Remove(tmp)
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, dst)
}
func safeFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00:")
}
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune("<>:\"/\\|?*", r) {
			return '_'
		}
		return r
	}, filepath.Base(name))
}
func shortTitle(text string, max int) string {
	first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	r := []rune(first)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return first
}

func (s *Server) entryAdd(w http.ResponseWriter, r *http.Request) {
	if !s.local(w, r) {
		return
	}
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	rule, err := s.rule(r.Context(), id)
	if err != nil {
		failure(w, 404, err)
		return
	}
	var entry Entry
	if err = json.NewDecoder(r.Body).Decode(&entry); err != nil || entry.Published == "" {
		failure(w, 422, errors.New("条目 JSON 或发布时间无效"))
		return
	}
	if entry.MessageID == "" {
		entry.MessageID = entry.ID
	}
	entry.ID = ""
	if err = s.add(r.Context(), rule, entry); err != nil {
		failure(w, 500, err)
		return
	}
	reply(w, 200, map[string]any{"status": "success", "message": fmt.Sprintf("条目已添加，媒体文件数量: %d", len(entry.Media))})
}
func (s *Server) entries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	limit, offset := 20, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	if limit < 0 || offset < 0 {
		failure(w, 422, errors.New("分页参数不能为负数"))
		return
	}
	s.mu.Lock()
	entries, err := s.readEntries(id)
	s.mu.Unlock()
	if err != nil {
		failure(w, 500, err)
		return
	}
	start := min(offset, len(entries))
	end := min(start+min(limit, len(entries)), len(entries))
	entries = entries[start:end]
	reply(w, 200, map[string]any{"entries": entries, "total": len(entries), "limit": limit, "offset": offset})
}
func (s *Server) entryDelete(w http.ResponseWriter, r *http.Request) {
	if !s.local(w, r) {
		return
	}
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readEntries(id)
	if err != nil {
		failure(w, 500, err)
		return
	}
	next := []Entry{}
	for _, e := range entries {
		if e.ID != r.PathValue("entry_id") {
			next = append(next, e)
		}
	}
	if len(next) == len(entries) {
		failure(w, 404, errors.New("条目未找到"))
		return
	}
	if err = atomicJSON(s.entryPath(id), next); err != nil {
		failure(w, 500, err)
		return
	}
	reply(w, 200, map[string]string{"status": "success", "message": "条目已删除"})
}
func (s *Server) removeRule(id int64) error {
	if id <= 0 {
		return errors.New("规则 ID 无效")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, base := range []string{s.dataPath, s.mediaPath} {
		path := filepath.Join(base, strconv.FormatInt(id, 10))
		if filepath.Dir(path) != filepath.Clean(base) {
			return errors.New("规则路径超出存储目录")
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRule 清理规则专属 RSS 条目与媒体；数据库记录由调用方的事务维护。
func (s *Server) RemoveRule(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.removeRule(id)
}
func (s *Server) ruleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.local(w, r) {
		return
	}
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	if err := s.removeRule(id); err != nil {
		failure(w, 500, err)
		return
	}
	reply(w, 200, map[string]any{"status": "success", "message": "规则数据和媒体已删除", "rule_id": id})
}
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	name := r.PathValue("filename")
	if !safeFilename(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.mediaPath, strconv.FormatInt(id, 10), name)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	base, err := filepath.EvalSymlinks(s.mediaPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	http.ServeFile(w, r, resolved)
}
