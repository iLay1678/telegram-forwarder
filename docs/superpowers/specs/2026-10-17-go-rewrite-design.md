# Telegram Forwarder Go 重写设计（待确认）

## 目标与验收边界

将现有 Python 应用完整迁移到 Go。已有部署保持 `.env`、所有绑定挂载、数据库、配置和 Telegram 登录会话，仅替换 Docker 镜像后继续工作。最终 `python` 分支保存迁移前的 Python 实现；`main` 分支维护 Go 应用、测试、静态资源、文档和构建配置，不包含 Python 源码或 Python 运行时依赖。

用户已经要求分支调整、完成后提交和推送。只在实现与验证通过后更新远端，不以部分功能的镜像替代正式版本。

## 已核实的仓库现状

- 原始提交：`96093f938f185673a01434e7af3395cd27267b8b`。
- 远端：`https://github.com/iLay1678/telegram-forwarder.git`；本地和远端目前只有 `main`，默认分支为 `main`。
- 现有 Python 源码约 16,983 个非空行，103 个受 Git 跟踪文件。
- Telegram 使用 Telethon 1.38.1，用户与机器人分别通过 MTProto 连接。
- 数据库是 SQLAlchemy 2.0.37 管理的 SQLite；代码实际固定打开 `./db/forward.db`，虽在 `.env.example` 提供 `DATABASE_URL`，现有实现没有使用该变量。
- RSS 使用 FastAPI、JSON 条目文件和本地媒体，登录用户保存在主 SQLite 数据库，密码使用 Werkzeug 3.1.3 哈希。
- 多平台推送使用 Apprise 1.9.2，而非单一 Webhook 协议。
- 现有 Docker 镜像是上游 `heavrnl/telegramforwarder:latest`。仓库仅有关闭不活跃 Issue 的工作流，没有镜像构建工作流。
- 当前工作区未提供运行中的 `.env`、数据库或 Telegram 会话，无法直接验证真实账号与生产数据。

## 方案选择

推荐完整 Go 重写，保留磁盘格式和外部接口。按模块实现并验证，最后统一发布。纯 Go 运行时符合最终 `main` 的要求，但 Telegram 会话、Python 正则和 Apprise 协议必须单独实现兼容层。

另两种方案是先交付核心转发、再补周边功能，或在 Go 容器中保留 Python 辅助进程。前者在完整功能补齐前不能作为无缝替换版本；后者不符合最终只保留 Go 实现的要求，因此不作为默认方案。

## 兼容契约

| 对象 | 既有格式/路径 | Go 实现要求 |
| --- | --- | --- |
| 环境配置 | `/app/.env` 与环境变量 | 保留原变量名、默认值、空字符串处理与环境变量优先级；不要求新增必填配置 |
| 主数据库 | `/app/db/forward.db` | 保留全部表、字段、主键、关联和大写枚举持久化值；不重建已有表或重新编号 |
| Telegram 会话 | `/app/sessions/user.session`、`bot.session` | 读取 Telethon SQLite 中的 DC、地址、端口、授权密钥及实体信息；旧授权有效时不重新登录 |
| Go 运行状态 | `/app/sessions/` | Go 专属更新游标等使用独立文件；不覆盖 Telethon 原始会话，以支持回退 |
| AI 模型与菜单配置 | `/app/config/ai_models.json`、`summary_times.txt`、`delay_times.txt`、`max_media_size.txt`、`media_extensions.txt` | 保持数据结构与逐行解析；默认文件只在缺失时生成 |
| UFB 配置 | `/app/ufb/config/config.json` | 保留未知字段、时间戳、域名绑定、关键词与 WebSocket 消息结构 |
| RSS 条目 | `/app/rss/data/<rule_id>/entries.json` | 保留 JSON 字段、条目 ID、时间格式、排序及容量控制 |
| RSS 媒体 | `/app/rss/media/<rule_id>/` | 保留已有文件和 URL；继续支持 RSS 路径覆盖变量 |
| RSS HTTP | 默认容器端口 `8000` | 保留订阅、媒体、登录、注册、仪表盘、配置、模式和条目接口 |
| RSS 密码 | `users.password` | 支持已有 Werkzeug scrypt/PBKDF2 格式；不要求重置密码；浏览器 Cookie 可按既有重启行为失效 |
| 规则正则 | Python `re` 搜索及替换 | 覆盖前后向断言、命名组、`\1`/`\g<name>` 替换与全文替换特殊处理，不能直接改成 Go RE2 语义 |
| 容器挂载 | Compose 中九个现有挂载 | 保持 `/app` 工作目录与各挂载目标，保持交互式首次登录和正常退出 |

数据库共 11 张业务表：`chats`、`forward_rules`、`keywords`、`replace_rules`、`media_types`、`media_extensions`、`rule_syncs`、`push_configs`、`rss_configs`、`rss_patterns`、`users`。若存在历史迁移遗留临时表，只保留、不参与常规查询。

数据库初始化只在新部署时建表。兼容旧版本需要的新增字段采用事务和幂等迁移；迁移前生成一致性备份。自定义 `DATABASE_URL` 与实际遗留数据库位置不一致时，必须明确诊断，不能静默创建空数据库造成规则丢失的表象。

## 应用结构

- `cmd/telegram-forwarder/`：配置验证、启动、信号处理和版本信息。
- `internal/config/`：dotenv、默认值及既有 JSON/TXT 文件。
- `internal/storage/`：SQLite 模型、事务、旧数据兼容和规则操作。
- `internal/telegram/`：MTProto 客户端、会话导入、实体解析、收发、媒体、相册与更新处理。
- `internal/rules/`：关键词、替换、媒体、AI 前后过滤、元信息及发送顺序。
- `internal/bot/`：现有命令、短别名、按钮回调、临时输入状态与管理员检查。
- `internal/ai/`：OpenAI 兼容接口、Claude/Gemini 原生接口和图片请求。
- `internal/rss/`：旧 JSON 存储、Feed、媒体、认证、管理接口及 Go 模板。
- `internal/ufb/`：WebSocket 同步、冲突处理及关键词同步。
- `internal/push/`：Apprise URL 解析和对应通知协议。
- `internal/scheduler/`：聊天名称更新、按时区执行的总结、分批历史读取及长消息拆分。
- `testdata/`：不含真实凭据的旧格式数据库、会话、JSON、配置与行为样本。

候选依赖：`gotd/td`（MTProto）、纯 Go SQLite 驱动、dotenv 解析、支持回溯的正则引擎及 Python 语法兼容层、WebSocket、`x/crypto`（旧密码哈希）、Markdown 渲染。版本在实现时核实并锁定，不引入 Python 子进程。

## 行为覆盖

保持现有处理顺序：初始化/相册归并 → 延迟 → 关键词 → 替换 → 媒体 → AI → 原始信息 → 评论按钮 → RSS → 编辑 → Telegram 发送 → 回复 → 外部推送 → 原消息删除。

必须覆盖用户与机器人发送、来源/目标解析、保护内容的下载再上传、相册、文本格式与链接预览、所有黑白名单及反转组合、媒体类型/大小/扩展名限制、同步规则、导入导出、链接转发、管理员权限、命令自动删除、AI 图片与定时总结、RSS 管理和 UFB 双向同步。

管理命令与既有短别名全部保留。数据库中的功能开关必须对应真实实现，不能仅读取字段后忽略。网络操作设置超时；Telegram 限流按服务端等待时间重试；正常退出等待数据库和文件写入完成。新日志不打印授权密钥、API Key 或完整推送凭据。

Apprise 是单独的兼容风险：Go 通知库的协议覆盖不能假定与 Apprise 相同。需要逐项核对 URL 方言、认证和媒体附件支持。未覆盖协议不能被标记为完成或默默丢弃；在解决之前不能宣称完整无缝替换。

## 分支与发布

1. 实现前在本地将 `python` 指向原始提交，完整保留原程序。
2. Go 实现由同一历史继续演进，最终 `main` 的跟踪文件中移除 Python 源码与依赖清单。
3. 验证通过后先推送 `python`，核实归档引用，再正常快进推送 `main`；无需强制推送或删除远端 `main`。默认分支仍为 `main`。
4. `main` 推送、版本标签与手动触发执行 GitHub Actions。PR 执行检查与构建，不发布镜像。
5. 测试成功后构建 `linux/amd64`、`linux/arm64` 镜像；主分支发布 `latest` 与提交 SHA 标签，版本标签额外发布语义版本标签。
6. 推荐镜像目标：`ghcr.io/ilay1678/telegram-forwarder`，由仓库名称自动推导并转为小写，工作流使用 `GITHUB_TOKEN` 与 `packages: write`。二进制制品统一命名 `telegram-forwarder-<os>-<arch>`，附校验和。
7. 若“同名”指现有上游 Docker Hub 地址 `heavrnl/telegramforwarder`，必须有该命名空间的发布凭据；不能仅凭当前 GitHub 仓库权限承诺发布。
8. 推送后检查 Actions 状态与镜像摘要、平台清单。工作流文件存在、代码推送成功和镜像已发布是三个独立状态，分别报告。

## 验证与发布门槛

- 以原始 Python 数据模型生成脱敏旧格式样本，断言 Go 读写后主键、枚举、空值、用户密码、规则与关系保持有效。
- 使用合成 Telethon SQLite 会话验证授权密钥和 DC 导入、坏文件报错及原文件未被覆盖。真实 Telegram 重连需要有效旧会话；没有真实验证时明确标注。
- 对原始 Python 过滤行为建立输入输出样本，覆盖名单组合、正则、替换转义、媒体、发送顺序和中文/Emoji 文本。
- 使用本地 HTTP/WebSocket 测试服务验证 AI、RSS、推送和 UFB，不发送真实消息或使用生产凭据。
- 验证全部管理命令、别名与回调的权限及数据变更，验证导入导出和同步行为。
- RSS 旧 JSON 与媒体路径、旧密码登录、Feed/XML、管理 API、模板和 HTTP 状态码回归。
- `go test ./...`、静态检查、可用环境下的竞态检测、两个 Linux 架构构建及 Docker 挂载启动测试。
- 只有完整功能、兼容性和发布检查通过才推送正式 Go 分支并发布 `latest`；测试报告写明真实外部服务尚未验证的边界。

## 已批准的设计决策

1. 首个可切换版本按完整等价迁移执行，包含 AI、RSS、UFB 与推送；不把核心功能子集当成最终交付。
2. “同名制品镜像”默认解释为当前仓库同名 GHCR 镜像 `ghcr.io/ilay1678/telegram-forwarder:latest`。

当前状态：用户于 2026-10-17 批准实施与自动提交推送。Go 实现与本地回归已完成，原始提交保存到 `python`；正在通过 GitHub Actions 构建验证镜像，并在圣何塞旧数据副本验收。此后所有编译与镜像构建仅在 GitHub 执行，用户机器仅运行成品。

范围待决：原生推送已覆盖 21 个 URL scheme，未覆盖完整 Apprise 插件生态，详见 `docs/push-compatibility.md`。已向用户提出首版范围选择，答复前仅发布 `go-validation` 验证标签，不更新正式 `main/latest`。真实验证状态见 `docs/test-report.md`。
