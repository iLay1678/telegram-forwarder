// Package model 定义与传输层无关的规则和消息。
package model

import (
	"context"
	"time"
)

type Chat struct {
	ID             int64  `db:"id" json:"id"`
	TelegramChatID string `db:"telegram_chat_id" json:"telegram_chat_id"`
	Name           string `db:"name" json:"name"`
	CurrentAddID   string `db:"current_add_id" json:"current_add_id"`
}

type Rule struct {
	ID                         int64           `db:"id" json:"id"`
	SourceChatID               int64           `db:"source_chat_id" json:"source_chat_id"`
	TargetChatID               int64           `db:"target_chat_id" json:"target_chat_id"`
	ForwardMode                string          `db:"forward_mode" json:"forward_mode"`
	UseBot                     bool            `db:"use_bot" json:"use_bot"`
	MessageMode                string          `db:"message_mode" json:"message_mode"`
	IsReplace                  bool            `db:"is_replace" json:"is_replace"`
	IsPreview                  string          `db:"is_preview" json:"is_preview"`
	IsOriginalLink             bool            `db:"is_original_link" json:"is_original_link"`
	IsUFB                      bool            `db:"is_ufb" json:"is_ufb"`
	UFBDomain                  string          `db:"ufb_domain" json:"ufb_domain"`
	UFBItem                    string          `db:"ufb_item" json:"ufb_item"`
	IsDeleteOriginal           bool            `db:"is_delete_original" json:"is_delete_original"`
	IsOriginalSender           bool            `db:"is_original_sender" json:"is_original_sender"`
	UserinfoTemplate           string          `db:"userinfo_template" json:"userinfo_template"`
	TimeTemplate               string          `db:"time_template" json:"time_template"`
	OriginalLinkTemplate       string          `db:"original_link_template" json:"original_link_template"`
	IsOriginalTime             bool            `db:"is_original_time" json:"is_original_time"`
	AddMode                    string          `db:"add_mode" json:"add_mode"`
	EnableRule                 bool            `db:"enable_rule" json:"enable_rule"`
	IsFilterUserInfo           bool            `db:"is_filter_user_info" json:"is_filter_user_info"`
	HandleMode                 string          `db:"handle_mode" json:"handle_mode"`
	EnableCommentButton        bool            `db:"enable_comment_button" json:"enable_comment_button"`
	EnableMediaTypeFilter      bool            `db:"enable_media_type_filter" json:"enable_media_type_filter"`
	EnableMediaSizeFilter      bool            `db:"enable_media_size_filter" json:"enable_media_size_filter"`
	MaxMediaSize               int64           `db:"max_media_size" json:"max_media_size"`
	IsSendOverMediaSizeMessage bool            `db:"is_send_over_media_size_message" json:"is_send_over_media_size_message"`
	EnableExtensionFilter      bool            `db:"enable_extension_filter" json:"enable_extension_filter"`
	ExtensionFilterMode        string          `db:"extension_filter_mode" json:"extension_filter_mode"`
	EnableReverseBlacklist     bool            `db:"enable_reverse_blacklist" json:"enable_reverse_blacklist"`
	EnableReverseWhitelist     bool            `db:"enable_reverse_whitelist" json:"enable_reverse_whitelist"`
	MediaAllowText             bool            `db:"media_allow_text" json:"media_allow_text"`
	EnablePush                 bool            `db:"enable_push" json:"enable_push"`
	EnableOnlyPush             bool            `db:"enable_only_push" json:"enable_only_push"`
	IsAI                       bool            `db:"is_ai" json:"is_ai"`
	AIModel                    string          `db:"ai_model" json:"ai_model"`
	AIPrompt                   string          `db:"ai_prompt" json:"ai_prompt"`
	EnableAIUploadImage        bool            `db:"enable_ai_upload_image" json:"enable_ai_upload_image"`
	IsSummary                  bool            `db:"is_summary" json:"is_summary"`
	SummaryTime                string          `db:"summary_time" json:"summary_time"`
	SummaryPrompt              string          `db:"summary_prompt" json:"summary_prompt"`
	IsKeywordAfterAI           bool            `db:"is_keyword_after_ai" json:"is_keyword_after_ai"`
	IsTopSummary               bool            `db:"is_top_summary" json:"is_top_summary"`
	EnableDelay                bool            `db:"enable_delay" json:"enable_delay"`
	DelaySeconds               int64           `db:"delay_seconds" json:"delay_seconds"`
	OnlyRSS                    bool            `db:"only_rss" json:"only_rss"`
	EnableSync                 bool            `db:"enable_sync" json:"enable_sync"`
	Source                     Chat            `db:"-" json:"source"`
	Target                     Chat            `db:"-" json:"target"`
	Keywords                   []Keyword       `db:"-" json:"keywords"`
	Replacements               []Replacement   `db:"-" json:"replacements"`
	MediaTypes                 map[string]bool `db:"-" json:"media_types"`
	Extensions                 []string        `db:"-" json:"extensions"`
	PushConfigs                []PushConfig    `db:"-" json:"push_configs"`
	RSS                        *RSSConfig      `db:"-" json:"rss"`
}

type Keyword struct {
	ID          int64  `db:"id" json:"id"`
	RuleID      int64  `db:"rule_id" json:"rule_id"`
	Keyword     string `db:"keyword" json:"keyword"`
	IsRegex     bool   `db:"is_regex" json:"is_regex"`
	IsBlacklist bool   `db:"is_blacklist" json:"is_blacklist"`
}
type Replacement struct {
	ID      int64  `db:"id" json:"id"`
	RuleID  int64  `db:"rule_id" json:"rule_id"`
	Pattern string `db:"pattern" json:"pattern"`
	Content string `db:"content" json:"content"`
}
type PushConfig struct {
	ID                int64  `db:"id" json:"id"`
	RuleID            int64  `db:"rule_id" json:"rule_id"`
	EnablePushChannel bool   `db:"enable_push_channel" json:"enable_push_channel"`
	PushChannel       string `db:"push_channel" json:"push_channel"`
	MediaSendMode     string `db:"media_send_mode" json:"media_send_mode"`
}
type RSSConfig struct {
	ID                         int64  `db:"id" json:"id"`
	RuleID                     int64  `db:"rule_id" json:"rule_id"`
	EnableRSS                  bool   `db:"enable_rss" json:"enable_rss"`
	RuleTitle                  string `db:"rule_title" json:"rule_title"`
	RuleDescription            string `db:"rule_description" json:"rule_description"`
	Language                   string `db:"language" json:"language"`
	MaxItems                   int    `db:"max_items" json:"max_items"`
	IsAutoTitle                bool   `db:"is_auto_title" json:"is_auto_title"`
	IsAutoContent              bool   `db:"is_auto_content" json:"is_auto_content"`
	IsAIExtract                bool   `db:"is_ai_extract" json:"is_ai_extract"`
	AIExtractPrompt            string `db:"ai_extract_prompt" json:"ai_extract_prompt"`
	IsAutoMarkdownToHTML       bool   `db:"is_auto_markdown_to_html" json:"is_auto_markdown_to_html"`
	EnableCustomTitlePattern   bool   `db:"enable_custom_title_pattern" json:"enable_custom_title_pattern"`
	EnableCustomContentPattern bool   `db:"enable_custom_content_pattern" json:"enable_custom_content_pattern"`
}

// Peer.ID 使用 Telegram 原始正 ID；Kind 用于区分 user/chat/channel。
type Peer struct {
	ID                   int64
	Kind, Name, Username string
	AccessHash           int64
	Broadcast            bool
}
type Media struct {
	MessageID                  int
	Kind, Filename, MIME, Path string
	Size                       int64
	Native                     any
}
type Message struct {
	ID         int
	Chat       Peer
	Sender     Peer
	Text       string
	Date       time.Time
	GroupedID  int64
	ReplyTo    int
	Media      []Media
	Buttons    [][]Button
	Link       string
	HasPreview bool
	Outgoing   bool
	Native     any
}
type Button struct{ Text, Data, URL string }
type Outgoing struct {
	Text    string
	Mode    string
	Media   []Media
	Buttons [][]Button
	Preview bool
	ReplyTo int
}
type Callback struct {
	ID        int64
	SenderID  int64
	Chat      Peer
	MessageID int
	Data      string
}

// Transport 将 Telegram API 与转发规则隔离，测试使用无网络实现。
type Transport interface {
	Resolve(context.Context, string) (Peer, error)
	Dialogs(context.Context) ([]Peer, error)
	Send(context.Context, Peer, Outgoing) ([]int, error)
	Edit(context.Context, Peer, int, Outgoing) error
	Delete(context.Context, Peer, []int) error
	History(context.Context, Peer, int, int) ([]Message, error)
	Fetch(context.Context, Peer, []int) ([]Message, error)
	Download(context.Context, Media, string) (string, error)
	Pin(context.Context, Peer, int) error
	AnswerCallback(context.Context, int64, string, bool) error
}

type AIImage struct {
	Data []byte
	MIME string
}
type AIProcessor interface {
	Process(context.Context, string, string, string, []AIImage) (string, error)
}
type RSSPublisher interface {
	Publish(context.Context, Rule, Message, string, []Media) error
}
type Pusher interface {
	Send(context.Context, []PushConfig, string, []Media) error
}
