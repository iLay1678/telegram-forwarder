package rss

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/iLay1678/telegram-forwarder/internal/pyregex"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	htmlrender "github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/title_template.json
var titlePatterns []byte

type feedXML struct {
	XMLName   xml.Name   `xml:"rss"`
	Version   string     `xml:"version,attr"`
	ContentNS string     `xml:"xmlns:content,attr"`
	Channel   channelXML `xml:"channel"`
}
type channelXML struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Items       []itemXML `xml:"item"`
}
type cdata struct {
	Text string `xml:",cdata"`
}
type guidXML struct {
	PermaLink string `xml:"isPermaLink,attr"`
	Value     string `xml:",chardata"`
}
type enclosureXML struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}
type itemXML struct {
	Title       string         `xml:"title"`
	GUID        guidXML        `xml:"guid"`
	Link        string         `xml:"link,omitempty"`
	Author      string         `xml:"author,omitempty"`
	Published   string         `xml:"pubDate"`
	Description cdata          `xml:"description"`
	Content     cdata          `xml:"content:encoded"`
	Enclosures  []enclosureXML `xml:"enclosure"`
}

func (s *Server) baseURL(r *http.Request) string {
	if base := s.cfg.String("RSS_MEDIA_BASE_URL", ""); base != "" {
		return strings.TrimRight(base, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if forward := r.Header.Get("X-Forwarded-Host"); forward != "" {
		host = strings.TrimSpace(strings.Split(forward, ",")[0])
		if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto == "https" || proto == "http" {
			scheme = proto
		}
	}
	return scheme + "://" + host
}
func (s *Server) autoExtract(content string) (string, string) {
	data := titlePatterns
	if custom, err := os.ReadFile(filepath.Join(s.cfg.Root, "rss", "app", "configs", "title_template.json")); err == nil {
		data = custom
	}
	var config struct {
		Patterns []struct {
			Pattern string `json:"pattern"`
		} `json:"patterns"`
	}
	if json.Unmarshal(data, &config) == nil {
		for _, p := range config.Patterns {
			matches, err := pyregex.Find("(?m)"+p.Pattern, content)
			if err == nil && len(matches) > 1 && strings.HasPrefix(content, matches[0]) {
				title := strings.ReplaceAll(matches[1], "*", "")
				title, _ = pyregex.Replace(`\[([^\]]+)\]\([^)]+\)`, `\1`, title)
				return strings.TrimSpace(strings.ReplaceAll(title, "\n", " ")), strings.TrimSpace(strings.TrimPrefix(content, matches[0]))
			}
		}
	}
	return shortTitle(strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(content, "*\n\r "), "\n", " ")), 20), content
}

var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(htmlrender.WithHardWraps(), htmlrender.WithUnsafe()))

func markdownHTML(content string) string {
	// 与旧 Python Markdown 保持一致：Telegram #标签不转换为 HTML 标题。
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "#") {
			lines[i] = "\\" + line
		}
	}
	content = strings.Join(lines, "\n")
	var out bytes.Buffer
	if markdown.Convert([]byte(content), &out) != nil {
		return "<p>" + html.EscapeString(content) + "</p>"
	}
	return out.String()
}
func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "rule_id")
	if !ok {
		return
	}
	rule, err := s.rule(r.Context(), id)
	if err != nil || !rule.RSS.EnableRSS {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	entries, err := s.readEntries(id)
	s.mu.Unlock()
	if err != nil {
		failure(w, 500, err)
		return
	}
	cfg := rule.RSS
	base := s.baseURL(r)
	title, desc, lang := cfg.RuleTitle, cfg.RuleDescription, cfg.Language
	if title == "" {
		title = fmt.Sprintf("TG Forwarder RSS - Rule %d", id)
	}
	if desc == "" {
		desc = fmt.Sprintf("TG Forwarder RSS - 规则 %d 的消息", id)
	}
	if lang == "" {
		lang = "zh-CN"
	}
	feed := feedXML{Version: "2.0", ContentNS: "http://purl.org/rss/1.0/modules/content/", Channel: channelXML{Title: title, Link: fmt.Sprintf("%s/rss/feed/%d", base, id), Description: desc, Language: lang}}
	for _, entry := range entries {
		content := entry.Content
		title := entry.Title
		if !cfg.IsAIExtract && (cfg.IsAutoTitle || cfg.IsAutoContent) {
			extractedTitle, extractedContent := s.autoExtract(content)
			if cfg.IsAutoTitle {
				title = extractedTitle
			}
			if cfg.IsAutoContent {
				content = extractedContent
			}
		}
		if !cfg.IsAIExtract || cfg.IsAutoMarkdownToHTML {
			content = markdownHTML(content)
		} else if !strings.HasPrefix(content, "<") {
			content = "<p>" + strings.ReplaceAll(html.EscapeString(content), "\n", "<br>\n") + "</p>"
		}
		item := itemXML{Title: title, GUID: guidXML{PermaLink: "false", Value: entry.ID}, Link: entry.Link, Author: entry.Author}
		if item.GUID.Value == "" {
			item.GUID.Value = entry.MessageID
		}
		for _, media := range entry.Media {
			name := media.Filename
			if media.URL != "" {
				if u, err := url.Parse(media.URL); err == nil {
					name = path.Base(u.Path)
				}
			}
			mediaURL := fmt.Sprintf("%s/media/%d/%s", base, id, url.PathEscape(name))
			escaped := html.EscapeString(mediaURL)
			label := media.Filename
			if media.OriginalName != nil && *media.OriginalName != "" {
				label = *media.OriginalName
			}
			label = html.EscapeString(label)
			typ := html.EscapeString(media.Type)
			switch {
			case strings.HasPrefix(media.Type, "image/"):
				content += `<p><img src="` + escaped + `" alt="` + label + `" style="max-width:100%;height:auto" /></p>`
			case strings.HasPrefix(media.Type, "video/"):
				content += `<p><video controls preload="none"><source src="` + escaped + `" type="` + typ + `"></video></p><p><a href="` + escaped + `">下载视频: ` + label + `</a></p>`
			case strings.HasPrefix(media.Type, "audio/"):
				content += `<p><audio controls><source src="` + escaped + `" type="` + typ + `"></audio></p><p><a href="` + escaped + `">下载音频: ` + label + `</a></p>`
			default:
				content += `<p><a href="` + escaped + `">下载文件: ` + label + `</a></p>`
			}
			item.Enclosures = append(item.Enclosures, enclosureXML{URL: mediaURL, Length: media.Size, Type: media.Type})
		}
		if content == "" {
			content = "<p>该消息没有文本内容。</p>"
		}
		for _, host := range []string{"127.0.0.1", "localhost", s.cfg.String("RSS_HOST", "127.0.0.1")} {
			content = strings.ReplaceAll(content, "http://"+host+":"+s.cfg.String("RSS_PORT", "8000"), base)
		}
		item.Description, item.Content = cdata{content}, cdata{content}
		published, err := time.Parse(time.RFC3339Nano, entry.Published)
		if err != nil {
			published, err = time.ParseInLocation("2006-01-02T15:04:05.999999", entry.Published, s.cfg.Timezone)
		}
		if err != nil {
			published = time.Now()
		}
		item.Published = published.Format(time.RFC1123Z)
		feed.Channel.Items = append(feed.Channel.Items, item)
	}
	data, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		failure(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(data)
}
