# Python → Go 兼容性与切换说明

Go 版本保持旧部署的路径、数据标识和配置格式。切换时使用 `ghcr.io/ilay1678/telegram-forwarder:latest`，保留 `.env` 与 Compose 挂载。实际验证情况以 [测试报告](test-report.md) 为准；协议实现范围以 [推送兼容表](push-compatibility.md) 为准。

## 磁盘兼容契约

| 原文件或挂载 | Go 行为 |
| --- | --- |
| `.env` → `/app/.env` | 沿用变量名，环境变量优先；不要求新增必填变量 |
| `db` → `/app/db` | 读取原 SQLite `forward.db`，保留规则、关联和主键；缺失字段通过幂等迁移补齐 |
| `sessions` → `/app/sessions` | 读取旧 Telethon 用户与机器人授权，另写 Go 会话与更新状态 |
| `config` → `/app/config` | 读取原 `ai_models.json` 与四类 TXT 菜单配置，已有文件不覆盖 |
| `ufb/config` → `/app/ufb/config` | 保留 `config.json` 的配置、同步时间戳及未知 JSON 字段 |
| `rss/data` → `/app/rss/data` | 读取 `<rule_id>/entries.json`，保留既有条目 ID 和 JSON 格式 |
| `rss/media` → `/app/rss/media` | 继续使用原规则媒体目录及 URL；支持原路径覆盖变量 |
| `logs` → `/app/logs` | JSON 日志同时写标准输出与 `telegram_forwarder.log`，文件 10 MiB 轮转并保留 3 份 |
| `temp` → `/app/temp` | 继续存放媒体处理临时文件 |

工作目录为 `/app`，RSS 默认端口为 `8000`。原来的可选主机映射 `9804:8000` 可继续使用。健康端点只监听容器回环地址 `127.0.0.1:8099`，不需要对外发布。

数据库迁移保留 11 张业务表中的既有数据与 SQLAlchemy 的枚举表示。需要迁移已有库时，先通过 SQLite 一致性备份生成 `forward.db.backup-<UTC时间>`，再补充缺失结构；重复启动不重复迁移。备份用于故障恢复，不替代切换前的完整部署备份。旧版实际上固定使用 `db/forward.db`；如果 `.env` 的 `DATABASE_URL` 指向别处，而旧路径仍存在，Go 会拒绝启动并报告路径冲突。

已有布尔字段的显式 `NULL` 按 Python 的 false 语义读取，不会因为新建规则默认启用而激活旧规则。缺失字段仍按旧版迁移默认值补齐。历史关键词三列唯一约束升级为包含 `is_blacklist` 的四列约束，使同一关键词可以分别存在于黑白名单中；普通索引原位替换，表内约束在备份后的事务中重建，保留主键、外键、未知字段、生成列、其他约束、索引、触发器和自增序列。任一步骤或外键校验失败都会回滚。

`.env` 遵循原 python-dotenv 1.0.0 的解析与插值：进程环境优先，`${VAR}` 和 `${VAR:-default}` 按文件顺序展开，单引号内也会插值，裸 `$VAR` 保留。显式空值、无赋值声明、注释、多行引号及转义行为均有旧版参考样本覆盖。

配置默认文件包括 `ai_models.json`、`summary_times.txt`、`delay_times.txt`、`max_media_size.txt`、`media_extensions.txt`。旧文件缺失时会创建默认内容；现有文件内容不自动重置。

## Telegram 会话

首次启动尝试读取 `sessions/user.session`、`sessions/bot.session` 中的 DC、授权密钥、实体与更新状态。旧授权有效时可免验证码登录；失效、被撤销或从未登录的会话仍需正常认证。

`PHONE_NUMBER` 和 `BOT_TOKEN` 仅在对应会话需要首次登录时要求提供；已有有效授权不因这两项缺省而被拒绝启动。`API_ID`、`API_HASH` 和管理账号配置仍需有效。

Go 分别写入：

- `user.gotd.json`、`bot.gotd.json`：Go 客户端授权。
- `user.state.json`、`bot.state.json`：Go 客户端实体缓存和更新状态。

旧 Telethon 文件保留，不被 Go 的状态写入覆盖。Go 后续运行不会实时同步旧 Telethon 更新游标，因此切回 Python 时可能存在重复补收或遗漏，不能把会话文件保留等同于消息处理的零损失回滚。诊断与正式运行之间，也应避免多个实例同时使用相同授权，防止重复转发或 Telegram 会话冲突。

## 功能边界

用户模式沿用旧版路径：关键词过滤后执行 Telegram 原生转发，保留原生转发来源。替换、媒体过滤、AI 处理、来源信息、RSS、外部推送、回复和删除等高级处理在机器人模式中执行。只有机器人模式才会应用这些对应开关；启用开关不会改变用户模式的处理路径。

Telegram 普通文本单条上限为 4096 字符，媒体说明另受 Telegram 对应接口限制。与原 Telethon 发送行为一致，Go 不自动将超长内容拆成多条；AI 输出或追加来源信息使内容超限时，发送接口会返回错误，不会静默截断。

正则兼容层支持旧规则常用的前后向断言、命名组、Python 替换反向引用和全文替换行为。启用规则中的无效表达式在启动时报告规则及配置 ID，并沿用旧版处理方式跳过该项，继续其他关键词或替换；禁用规则不阻止启动。迁移时应使用自己的规则与样本检查结果，不能仅凭采用兼容引擎推断所有 Python `re` 边界行为一致。

接收更新后等待规则处理完成，再允许 Telegram 更新管理器保存进度；关闭时尚未完成的回调返回取消错误。消息发送与本地游标无法组成跨系统事务，进程在发送成功后、保存进度前被强制终止仍可能导致重复投递；不承诺 exactly-once。

RSS 支持旧 JSON 条目、媒体文件、订阅和管理接口，旧用户的 Werkzeug scrypt/PBKDF2 密码可继续登录。浏览器登录 Cookie 不作为持久数据迁移，重启或切换后可能需要重新登录。

推送配置保持原 URL 与数据库字段，但 Go 未实现全部 Apprise 服务插件。已支持协议的认证、参数和附件范围见 [推送兼容表](push-compatibility.md)。启用的不兼容配置在启动诊断阶段明确报错；不支持附件的协议返回错误，不能视为附件发送成功。本次圣何塞原部署检查时没有配置推送目标；没有使用真实外部推送凭据逐一验证。

## 推荐切换步骤

1. 记录旧 Compose 镜像及其 digest，停止旧容器，备份整个部署目录。停止后备份才能同时覆盖一致的数据库、会话与 RSS 文件。
2. 将备份解压到独立目录，在副本的 Compose 中仅替换镜像，保留路径与配置。副本的相对挂载会指向副本目录；若旧 Compose 使用绝对路径，先改成副本路径，避免检查时写回生产目录。
3. 在副本执行 `docker compose run --rm --no-deps telegram-forwarder --check`，确认配置、数据库、正则与启用推送协议可读取。
4. 在旧容器仍停止的条件下，需要时在副本执行 `docker compose run --rm --no-deps telegram-forwarder --probe`。它登录 Telegram 并检查启用规则的源/目标，不执行消息转发。
5. 确认副本检查结果后，在正式目录将 `image` 改为 Go 镜像，执行 `docker compose pull telegram-forwarder` 和 `docker compose up -d telegram-forwarder`。
6. 检查日志、容器健康状态、规则数量与预期转发。RSS、AI、UFB 和推送应按实际启用项目验证。不要让副本与正式容器同时转发。

`--check` 会加载配置并创建缺失默认文件，打开数据库并执行必要迁移，还可能导入旧授权到独立 Go 会话。它是离线校验，**不是只读命令**。`--probe` 在这些步骤之后执行在线登录并更新 Go 状态；它不会发送转发消息，但并非无副作用的网络探测。

若新旧客户端跨机器使用同一份授权做在线测试，需先关闭原实例；测试结束后也需关闭测试实例，再启动正式实例。对生产消息进行实际发送测试的结果，应与只登录和解析实体的 `--probe` 结果分别记录。

## 回滚

先停止 Go 容器，将 Compose 镜像改回切换前记录的 Python 镜像，再启动。保留旧 Telethon 文件有助于恢复登录，但不能保证 Go 运行期间旧更新游标仍然新鲜。

若需要恢复数据库或完整部署备份，应在所有实例停止后进行，并明确接受备份时间之后新增的规则、用户、RSS 条目和其他数据会丢失。保留 Go 运行期间的最新目录另作副本后再恢复。不要删除会话文件来解决普通启动错误，这可能迫使账号重新登录。

## 验证与发布

本地兼容测试使用无真实凭据的旧格式样本及本地 HTTP/WebSocket/SMTP 服务。生产会话登录、真实转发、Docker 构建、Actions 和 GHCR 发布分别是独立验证项，参见 [测试报告](test-report.md)，不能互相替代。

工作流先执行 `go test ./...`、`go vet ./...` 和 Linux `go test -race ./...`，再构建六个平台的二进制。镜像任务下载本次工作流的 Linux 制品、验证 SHA256，并按目标架构拷入 `linux/amd64`、`linux/arm64` 镜像，Dockerfile 不重复编译。`main` 推送成功后才发布 `latest`。旧代码存放在 `python` 分支，Go 的 `main` 不需要 Python 源码或运行时。
