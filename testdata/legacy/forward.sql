-- 脱敏的 SQLAlchemy 2.x 旧格式样本；默认值来自客户端，表中没有 DEFAULT。
CREATE TABLE chats (id INTEGER PRIMARY KEY, telegram_chat_id VARCHAR NOT NULL UNIQUE, name VARCHAR, current_add_id VARCHAR);
CREATE TABLE forward_rules (
    id INTEGER PRIMARY KEY, source_chat_id INTEGER NOT NULL REFERENCES chats(id), target_chat_id INTEGER NOT NULL REFERENCES chats(id),
    forward_mode VARCHAR NOT NULL, use_bot BOOLEAN, message_mode VARCHAR NOT NULL, is_replace BOOLEAN, is_preview VARCHAR NOT NULL,
    is_original_link BOOLEAN, is_ufb BOOLEAN, ufb_domain VARCHAR, ufb_item VARCHAR, is_delete_original BOOLEAN, is_original_sender BOOLEAN,
    userinfo_template VARCHAR, time_template VARCHAR, original_link_template VARCHAR, is_original_time BOOLEAN,
    add_mode VARCHAR NOT NULL, enable_rule BOOLEAN, is_filter_user_info BOOLEAN, handle_mode VARCHAR NOT NULL,
    enable_comment_button BOOLEAN, enable_media_type_filter BOOLEAN, enable_media_size_filter BOOLEAN, max_media_size INTEGER,
    is_send_over_media_size_message BOOLEAN, enable_extension_filter BOOLEAN, extension_filter_mode VARCHAR NOT NULL,
    enable_reverse_blacklist BOOLEAN, enable_reverse_whitelist BOOLEAN, media_allow_text BOOLEAN, enable_push BOOLEAN, enable_only_push BOOLEAN,
    is_ai BOOLEAN, ai_model VARCHAR, ai_prompt VARCHAR, enable_ai_upload_image BOOLEAN, is_summary BOOLEAN, summary_time VARCHAR(5),
    summary_prompt VARCHAR, is_keyword_after_ai BOOLEAN, is_top_summary BOOLEAN, enable_delay BOOLEAN, delay_seconds INTEGER,
    only_rss BOOLEAN, enable_sync BOOLEAN, UNIQUE(source_chat_id, target_chat_id)
);
CREATE TABLE keywords (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), keyword VARCHAR, is_regex BOOLEAN,
    is_blacklist BOOLEAN, UNIQUE(rule_id, keyword, is_regex, is_blacklist));
CREATE TABLE replace_rules (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), pattern VARCHAR NOT NULL,
    content VARCHAR, UNIQUE(rule_id, pattern, content));
CREATE TABLE media_types (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL UNIQUE REFERENCES forward_rules(id),
    photo BOOLEAN, document BOOLEAN, video BOOLEAN, audio BOOLEAN, voice BOOLEAN);
CREATE TABLE media_extensions (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), extension VARCHAR NOT NULL,
    UNIQUE(rule_id, extension));
CREATE TABLE rule_syncs (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), sync_rule_id INTEGER NOT NULL);
CREATE TABLE push_configs (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), enable_push_channel BOOLEAN,
    push_channel VARCHAR NOT NULL, media_send_mode VARCHAR NOT NULL);
CREATE TABLE rss_configs (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL UNIQUE REFERENCES forward_rules(id), enable_rss BOOLEAN,
    rule_title VARCHAR, rule_description VARCHAR, language VARCHAR, max_items INTEGER, is_auto_title BOOLEAN, is_auto_content BOOLEAN,
    is_ai_extract BOOLEAN, ai_extract_prompt VARCHAR, is_auto_markdown_to_html BOOLEAN, enable_custom_title_pattern BOOLEAN,
    enable_custom_content_pattern BOOLEAN);
CREATE TABLE rss_patterns (id INTEGER PRIMARY KEY, rss_config_id INTEGER NOT NULL REFERENCES rss_configs(id), pattern VARCHAR NOT NULL,
    pattern_type VARCHAR NOT NULL, priority INTEGER, UNIQUE(rss_config_id, pattern, pattern_type));
CREATE TABLE users (id INTEGER PRIMARY KEY, username VARCHAR NOT NULL, password VARCHAR NOT NULL);
INSERT INTO chats VALUES (7, '1007', '来源频道', NULL), (9, '1009', '目标频道', '1007'), (19, '1019', NULL, NULL);
INSERT INTO forward_rules (id, source_chat_id, target_chat_id, forward_mode, message_mode, is_preview, add_mode, handle_mode, extension_filter_mode, enable_sync)
    VALUES (41, 7, 9, 'BLACKLIST_THEN_WHITELIST', 'MARKDOWN', 'FOLLOW', 'BLACKLIST', 'FORWARD', 'BLACKLIST', 1),
           (88, 7, 19, 'WHITELIST', 'HTML', 'OFF', 'WHITELIST', 'EDIT', 'WHITELIST', 0);
INSERT INTO keywords VALUES (103, 41, '广告', 0, 1), (109, 41, '(?P<tag>技术)', 1, 0), (120, 88, NULL, NULL, NULL);
INSERT INTO replace_rules VALUES (201, 41, '(?P<word>新闻)', '\g<word>摘要'), (205, 41, '删除', NULL);
INSERT INTO media_types VALUES (301, 41, 1, NULL, 1, 0, 0);
INSERT INTO media_extensions VALUES (401, 41, 'pdf');
INSERT INTO rule_syncs VALUES (501, 41, 88);
INSERT INTO push_configs VALUES (601, 41, 1, 'json://localhost:12345/test', 'Multiple');
INSERT INTO rss_configs (id, rule_id, enable_rss, rule_title, enable_custom_title_pattern) VALUES (701, 41, 1, '技术资讯', 1);
INSERT INTO rss_patterns VALUES (801, 701, '^(.+)', 'title', 0);
INSERT INTO users VALUES (901, 'fixture-user', 'pbkdf2:sha256:1$fixture-salt$not-a-real-password');
-- 保留旧迁移遗留表，不参与业务查询。
CREATE TABLE keywords_temp (id INTEGER PRIMARY KEY, keyword VARCHAR);
INSERT INTO keywords_temp VALUES (999, '遗留内容');
