# Go 兼容重写实施计划

> 执行方式：本会话内按 software-company 协作协议分工实现，统一集成与审查。用户已批准在 main 实施、保留 python 分支，并在完成后提交推送；无需额外 worktree 或发布确认。

**目标：** Go 镜像直接读取旧配置、数据库与会话，实现原有转发、管理和扩展能力，发布仓库同名 GHCR 镜像。

**架构：** 独立 config/storage/model 包定义稳定磁盘格式和模块接口。Telegram 适配器隔离 MTProto；规则引擎通过 Transport 与扩展服务接口执行，HTTP、定时任务和机器人共享同一存储。

**技术栈：** Go 1.25+、gotd/td、modernc SQLite、sqlx、regexp2、标准 net/http、WebSocket、goldmark、x/crypto。

**规格：** `docs/superpowers/specs/2026-10-17-go-rewrite-design.md`，2026-10-17 用户确认，镜像目标 GHCR。

## 全局约束

- 原 Python 提交 `96093f938f185673a01434e7af3395cd27267b8b` 保存为 python。
- 原有 `.env`、九个挂载、11 张业务表、枚举名称、JSON 和 TXT 格式保持兼容。
- 不覆盖已有 Telethon 会话；不使用 Python 运行时；不在 main 遗留 Python 源文件。
- 注释与用户文档使用简体中文。故障必须明确返回，不留下空实现。
- 提交与推送由主代理在整体验证后统一执行；协作代理不能自行提交或改写公共契约。

## T01：基础设施与磁盘兼容（P0）

文件：`go.mod`、`internal/model/*.go`、`internal/config/*.go`、`internal/storage/*.go`、`internal/pyregex/*.go`、`testdata/legacy/*`。

依赖：无。输出共享模型、配置解析、数据库操作与 Python 正则兼容。

接口：`config.Load(root string) (*Config,error)`；`storage.Open(path string) (*Store,error)`；`Store.DB *sqlx.DB`；`Store.Rules(ctx)`、`Store.Rule(ctx,id)`；`pyregex.Search(pattern,text)`、`pyregex.Replace(pattern,replacement,text)`。完整结构以 `internal/model` 为准。

- [ ] 从旧模型生成无凭据 SQL fixture，使用固定 ID 的 BLACKLIST/WHITELIST、空字段和关联数据测试读取。
- [ ] 实现只补缺失字段的幂等迁移，已有库迁移前 VACUUM INTO 备份，验证读写后 ID 不变。
- [ ] 保留 dotenv 优先级、默认文件及现有选项列表，测试旧 `.env` 及含注释、引号、空字符串的值。
- [ ] 正则用例覆盖 `(?<=.)(?=$)`、`(?P<n>...)`、`\\1`、`\\g<n>`、Unicode 及非法表达式。
- [ ] 执行 `go test ./internal/config ./internal/storage ./internal/pyregex`。

## T02：Telegram 与机器人管理（P0）

文件：`internal/telegram/{client,session,media,format}.go`、`internal/bot/{commands,settings,callbacks,state}.go` 及测试。

依赖：T01 的 model/config/storage 契约。输出 `model.Transport` 实现和机器人 `HandleMessage`/`HandleCallback`。

- [ ] 合成 Telethon SQLite 授权密钥/实体样本；测试导入后旧文件字节不变、坏会话报错。
- [ ] 实现双 MTProto 客户端、首次登录、更新、实体缓存、相册、媒体下载上传与消息格式。
- [ ] 对照旧 `handlers/bot_handler.py` 移植全部命令及别名，settings 数据驱动呈现所有字段。
- [ ] 实现导入导出、复制、同步、链接转发、回调和输入状态；测试未授权者不能修改规则。
- [ ] 执行 `go test ./internal/telegram ./internal/bot`。

## T03：RSS 与外部服务（P0）

文件：`internal/ai/*.go`、`internal/rss/*.go` 与 templates、`internal/ufb/*.go`、`internal/push/*.go` 及测试。

依赖：T01。输出 AI Process、RSS Handler/Add、UFB Run/Sync 和 Push Send 接口；具体签名见共享模型及模块源文件。

- [ ] 用 httptest 验证 OpenAI 流式、Claude、Gemini、图片请求与自定义 Base URL；不请求真实供应商。
- [ ] 读取旧 RSS JSON、Werkzeug scrypt/PBKDF2 密码，实现旧 HTTP 路由、Feed/XML、静态媒体和管理页面。
- [ ] 使用本地 WebSocket 服务测试 UFB 首次同步、更新、冲突和配置持久化。
- [ ] 按 Apprise URL 方言实现通知协议，明确列出验证覆盖及不能映射的协议；禁止静默丢弃。
- [ ] 执行 `go test ./internal/ai ./internal/rss ./internal/ufb ./internal/push`。

## T04：转发编排与定时任务（P0）

文件：`internal/rules/{engine,keywords,media,info}.go`、`internal/scheduler/*.go`、`cmd/telegram-forwarder/main.go` 及测试。

依赖：T01；以 model.Transport 假实现测试，最终连接 T02/T03。

- [ ] 根据旧 `filters/process.py` 保持过滤与发送顺序，黑白名单组合、反转、替换和媒体限制建立表驱动测试。
- [ ] 串接 AI、RSS、Telegram、Push、原消息删除，测试发送失败不删除源消息以及 only_rss/only_push 行为。
- [ ] 实现按时区总结、批量历史读取、长度拆分、置顶与聊天名称更新，测试固定时钟下计划执行。
- [ ] 主程序处理配置、目录、日志、优雅关闭、启动诊断和 healthcheck；不启动时发送额外欢迎消息。
- [ ] 执行 `go test ./internal/rules ./internal/scheduler ./cmd/telegram-forwarder`。

## T05：集成验证与发布（P0）

文件：`Dockerfile`、`docker-compose.yml`、`.dockerignore`、`.gitignore`、`.github/workflows/build.yml`、`README.md`、`docs/compatibility.md`、`docs/test-report.md`。

依赖：T02/T03/T04。

- [x] 镜像复用 Actions 已编译并通过校验和检查的 Linux 二进制，保留原挂载与 8000 端口，Compose 镜像改为 GHCR。
- [x] Actions 在 main/tag/手动触发测试、构建 amd64/arm64 镜像并发布，PR 只验证。上传二进制与 SHA256 制品。
- [x] 完成接口与导入一致性检查，运行 `go test ./...`、`go vet ./...`、适用的竞态测试与 Linux 双架构构建。
- [x] 清理被 Go 替代的 Python 跟踪文件，检查没有凭据或运行数据加入 Git。
- [ ] 更新真实验证报告；先推送 python 归档，再快进推送 main。
- [x] 检查 Actions 结果与 GHCR manifest；如外部权限阻塞，准确报告具体失败而不声称发布成功。

## 执行记录

- 2026-10-17：用户确认设计、完整迁移、GHCR 目标、分支与自动推送。本地 python 分支已保存原始提交；当前 main 开始实现。
- 2026-10-17：T01–T04 代码与本地回归完成；Python 跟踪文件已移除，`python` 远端归档已核实。T05 进入 GitHub 构建与真实副本验收，详见 `docs/test-report.md`。
- 用户进一步要求所有编译和容器构建在 GitHub 执行；圣何塞与本机不再执行构建。`go-validation` 分支触发测试与验证镜像标签，正式 `main/latest` 等待实机验收及 Apprise 范围答复。
- 2026-10-17：`09506ac` GitHub 测试、vet、race、六平台制品、双架构镜像均通过，六份归档 SHA256 独立核验。真实副本离线导入、双会话免登录、正常模式健康检查与优雅停止通过。规则 17 源频道权限异常经 Python 对照确认，其他源/目标一致；未修改原规则。全部 Next Terminal 临时任务及测试容器已清理。
- 正式远端 main 推送仍等待 Apprise 推送范围答复；python 归档和 go-validation 验证分支已推送。当前没有把范围缩减视为已获批准。
