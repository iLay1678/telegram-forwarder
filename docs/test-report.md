# Go 重写验证记录

日期：2026-10-17。本记录区分已执行验证与待完成项目，不代表完整 Apprise 兼容声明。

## 已通过

- Windows Go 1.26.4：`go test ./...`、`go vet ./...`；最后规则与更新确认修复另通过 `go test ./internal/rules ./internal/telegram`。
- dotenv 兼容样本、SQLite 主键与关联保留、显式 NULL、旧关键词唯一约束迁移、失败回滚。
- 合成 Telethon 会话只读导入、旧文件保全、实体解析、媒体/相册、格式、原生转发与更新处理回归。
- 机器人命令/权限/输入状态、四种关键词模式、AI 失败回退/提示词变量/二次过滤、投递失败不删来源、评论回复。
- AI、RSS、UFB、推送的本地假 HTTP/WebSocket/SMTP 服务测试；旧 RSS 密码哈希与数据格式回归。
- 先前 Linux amd64/arm64 交叉构建通过；最终提交将由 GitHub 重新构建。
- Actions 配置已做 actionlint 与 YAML 解析检查；九个原挂载目标保持一致。
- `python` 已推送，指向原始提交 `96093f938f185673a01434e7af3395cd27267b8b`。

## 圣何塞副本

通过 Next Terminal 定时任务接口执行，任务禁用自动调度并手动运行。原部署为另一个失败的重构镜像，数据与配置来自 Python 版，容器原已停止。

- 一致性备份：`/root/docker-compose/TelegramForwarder/.go-validation/20261017-go-rewrite/original/`。
- 验证副本：同目录下 `data/`；主数据库包含 chats 5、规则 4、关键词 23、同步关联 3；没有推送目标。
- 备份使用 SQLite backup API，不打印配置密钥、消息内容或会话授权。
- 曾在远端尝试源码构建，未成功；用户要求后已结束并确认无遗留构建。此后编译和容器构建全部在 GitHub 执行。
- 已拉取 GitHub 构建的双架构成品镜像，匿名 GHCR 拉取成功。离线副本检查暴露 Telethon 实体 ID 为 0 的占位记录，已修复并补回归；检查过程中原 `.env`、数据库及两个 Telethon 会话的 SHA256 均未改变。
- 真实用户会话缓存含 161694 条实体，机器人会话 10 条，两者均含一条零 ID 占位。针对大型缓存，已拆分实体文件与更新游标，并将初始化合并为一次写入；原 Telethon 文件仍只读。
- 已删除此前 23 个临时任务，后续任务读取结果后立即删除；三个系统任务保留。
- 修复版 `09506ac` 成品镜像离线检查通过（约 1.4 秒），chats 5、规则 4、关键词 23、同步关联 3 全部保留；两个 Go 授权文件成功生成，原文件 SHA256 未改变。覆盖零 ID 占位及 Telethon 小数时间戳。
- 两个旧会话在线登录已通过，无验证码交互。首次 Go 探测遇到 `CHANNEL_PRIVATE`；使用远端已有 Python 镜像（Telethon 1.38.1）在独立会话副本执行相同只读查询，对照结果为规则 17 的源频道同样 `ChannelPrivateError`，规则 18–20 的源和全部四个目标均可访问。Python 诊断容器已退出，没有发送测试消息。
- 最终 `--probe` 关闭更新接收，不推进消息游标；复验仅规则 17 源会话失败，其他源/目标均通过，与 Python 对照一致。
- 正常模式另用 `runtime-data/` 副本启动，将副本规则关闭以避免真实转发；容器运行且 Docker 状态为 healthy，进程 `--healthcheck` 返回 0。旧账号已退出/更新过期的其他频道会打印明确告警，不影响进程就绪。容器优雅停止退出码为 0，无 OOM，测试容器已删除。原 Compose 与原容器未切换。
- 最终核对：原 `.env` 字节一致；数据库及两个 Telethon 会话与 SQLite 一致性备份的逻辑内容一致（SQLite backup API 不保证数据库文件逐字节相同）。未遗留测试容器，Next Terminal 仅剩三个原系统任务。

## 发布门槛与边界

GitHub Actions [09506ac 构建](https://github.com/iLay1678/telegram-forwarder/actions/runs/39040653672) 的测试、vet、竞态检查、六平台二进制及双架构容器全部通过。六个归档均另行下载并验证 SHA256；镜像复用本次 Linux 二进制制品，没有在 Docker 中重复编译。

匿名 GHCR 拉取通过，验证镜像索引为 `sha256:0e987f11b226d35c13079d9ae842a3d239e04fecebaae2446e1459ae11fad4bb`，包含 linux/amd64 与 linux/arm64。实机验证固定使用 `sha-09506ac8c7187f5210fb9546ac575c282993f4ca` 标签，避免后续构建覆盖标签而混淆结果。用户已接受首版范围，正式 `main/latest` 进入发布与部署阶段。

推送支持范围见 [兼容表](push-compatibility.md)。用户于 2026-10-17 明确接受现有 21 个 URL scheme，无需补齐全部 Apprise 插件，并已授权修改圣何塞 Compose、使用最新镜像启动。真实外部 AI、推送、UFB 和生产消息发送尚未逐项验证；在线 `--probe` 仅登录和读取实体，不证明发送权限。

消息发送与更新游标不是跨系统事务，强制终止可能造成重复；回滚旧 Telethon 游标也不能承诺消息零重复或零漏收。

## 正式发布与部署

2026-10-17 已将 Go 实现快进推送至远端 `main`；`python` 仍指向原始提交。正式镜像对应 `0219f6869a790969931de7ecde2f90cd231e82eb`，后续纯文档提交使用 `[skip ci]`，不改变运行镜像。

- [正式 Actions](https://github.com/iLay1678/telegram-forwarder/actions/runs/39042430830) 全部成功：单元与兼容测试、vet、race、六平台二进制及双架构容器。六份归档另行下载，GitHub 制品摘要与内部 SHA256 均通过。
- 已发布 `ghcr.io/ilay1678/telegram-forwarder:latest` 及 `sha-0219f6869a790969931de7ecde2f90cd231e82eb`；索引摘要为 `sha256:668a19a7c5ba5a75b911192608dbecc6203b5aedf58df680446ba15d2122cb91`，含 linux/amd64 与 linux/arm64，匿名拉取成功。全部编译及镜像构建均在 GitHub 执行。
- 圣何塞 `/root/docker-compose/TelegramForwarder/docker-compose.yml` 仅修改镜像字段，归一化 Compose 配置对比通过，九个原绑定挂载全部保留。使用 `docker compose up -d --no-build --pull never telegram-forwarder` 启动已拉取镜像，实际镜像提交号与正式构建一致。
- 完整部署归档、SQLite 一致性备份及原 Compose 保存在 `/root/docker-compose/TelegramForwarder/.go-deploy/20261017T143659Z/`。该目录经符号链接解析为 `/www/1panel/docker/compose/TelegramForwarder/.go-deploy/20261017T143659Z/`。
- 容器于 15:00:26 UTC 启动，状态 `running/healthy`，进程健康检查退出码 0、重启次数 0、无 OOM。就绪日志确认四条规则。chats 5、规则 4、关键词 23、同步关联 3、推送配置 0 与切换前一致，规则 17–20 全部仍启用。
- 原 `.env`、`user.session`、`bot.session` 的 SHA256 与部署备份一致；Go 授权及更新状态使用独立文件。未遗留测试容器。
- 启动约两分钟后复查仍为 healthy、重启次数 0、无 OOM，数据与原会话校验仍通过。全部本次 Next Terminal 临时任务已删除，任务列表仅剩原有 Auto Renew Certificate、Auto Remove History Log、Asset Check Status 三项系统任务。
- 日志存在旧缓存频道的 `CHANNEL_PRIVATE` 与差异过期告警。在当前四条规则源中，仅规则 17 对应订阅失败，规则 18–20 没有这些告警，与此前 Python 对照一致；没有擅自禁用规则或补发历史消息。健康检查与登录成功不代表生产消息发送已逐项验收。
