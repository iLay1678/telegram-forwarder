-- 较早版本使用 mode 和 selected_media_types，没有 RSS、同步等新表。
CREATE TABLE chats (id INTEGER PRIMARY KEY, telegram_chat_id VARCHAR NOT NULL UNIQUE, name VARCHAR, current_add_id VARCHAR);
CREATE TABLE forward_rules (id INTEGER PRIMARY KEY, source_chat_id INTEGER NOT NULL REFERENCES chats(id),
    target_chat_id INTEGER NOT NULL REFERENCES chats(id), mode VARCHAR NOT NULL, selected_media_types VARCHAR,
    UNIQUE(source_chat_id, target_chat_id));
CREATE TABLE keywords (id INTEGER PRIMARY KEY, rule_id INTEGER NOT NULL REFERENCES forward_rules(id), keyword VARCHAR, is_regex BOOLEAN);
INSERT INTO chats VALUES (7, '1007', '旧来源', NULL), (9, '1009', '旧目标', NULL);
INSERT INTO forward_rules VALUES (41, 7, 9, 'WHITELIST', '["photo", "video"]');
INSERT INTO keywords VALUES (103, 41, '新闻', 0);
