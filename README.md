# Telegram Forwarder · Go

使用 Go 实现的 Telegram 消息转发与管理服务，直接读取 Python 版的 `.env`、SQLite 数据库、配置文件、RSS 数据和 Telethon 登录会话。容器工作目录仍是 `/app`，原有九个挂载保持一致。

`main` 维护 Go 实现；[`python`](https://github.com/iLay1678/telegram-forwarder/tree/python) 保存迁移前的 Python 版本。容器不依赖 Python。

```text
ghcr.io/ilay1678/telegram-forwarder:latest
```

## 功能与兼容范围

- 用户和机器人双 MTProto 客户端，支持频道、群组、文本、媒体、相册及规则管理。
- 关键词黑白名单、正则、替换、媒体类型/大小/扩展名过滤、规则复制同步及导入导出。
- AI 内容处理、图片请求、按时区执行的历史消息总结，支持 OpenAI 兼容接口及 Claude/Gemini 原生接口。
- RSS 订阅、媒体、管理页面与旧账号密码；UFB WebSocket 配置和关键词同步。
- 多平台推送，具体协议与附件支持见[推送兼容表](docs/push-compatibility.md)。Go 实现尚未覆盖所有 Apprise 插件；启用的不支持配置会在启动校验时报错。

**发送模式沿用旧行为：用户模式仅执行关键词筛选和 Telegram 原生转发；替换、媒体过滤、AI、RSS、外部推送等高级处理使用机器人模式。** 保留源信息、评论按钮、回复、编辑和删除等开关也由对应模式与规则控制。

迁移细节与边界见[兼容性说明](docs/compatibility.md)，实际执行过的验证见[测试报告](docs/test-report.md)。支持某项功能不代表已经用真实外部服务逐一验证。

## 从 Python 版切换

现有部署只需替换 Compose 的 `image`，无须改 `.env` 或移动数据。首次切换前停止旧容器并备份部署目录；数据库可能自动补充缺失字段，Go 会在 `sessions` 内建立独立状态文件。

```bash
cd "/root/docker-compose/TelegramForwarder"
docker compose stop telegram-forwarder
backup_dir="../TelegramForwarder-backup-$(date +%Y%m%d-%H%M%S)"
mkdir -m 700 "$backup_dir"
tar -czf "$backup_dir/deployment.tar.gz" "."
```

把现有 `docker-compose.yml` 中该服务的镜像改为：

```yaml
image: ghcr.io/ilay1678/telegram-forwarder:latest
```

保留全部挂载、端口和其他自定义配置，然后启动：

```bash
docker compose pull telegram-forwarder
docker compose up -d telegram-forwarder
docker compose logs --tail=100 -f telegram-forwarder
```

已有 Telegram 授权有效时无需重新登录。首次启动导入 `sessions/user.session` 和 `sessions/bot.session`，不覆盖这两个旧文件。Go 运行后不实时更新旧 Telethon 游标，因此回滚到旧镜像不能承诺消息零重复或零漏收。切换与回滚期间只运行一个转发实例。

需要先检查旧数据时，在备份解出的**独立副本**中替换镜像，再运行：

```bash
docker compose run --rm --no-deps telegram-forwarder --check
```

`--check` 不连接 Telegram，但会补默认配置、执行必要的数据库迁移，并可能生成 Go 会话文件，**不是只读检查**。`--probe` 会登录 Telegram 并检查规则的源与目标，不转发消息，但会更新 Go 会话状态；运行前先停止使用同一账号授权的旧实例。详细步骤见[兼容性说明](docs/compatibility.md)。

## 新部署

下载本仓库的 `docker-compose.yml` 和 `.env.example`，将 `.env.example` 复制为 `.env`，填写 `API_ID`、`API_HASH`、`PHONE_NUMBER`、`BOT_TOKEN` 和 `USER_ID`。`API_ID` 与 `API_HASH` 从 <https://my.telegram.org/apps> 获取。

```bash
cp ".env.example" ".env"
mkdir -p "db" "logs" "sessions" "temp" "ufb/config" "config" "rss/data" "rss/media"
# 编辑 .env 后，首次以前台交互方式登录。
docker compose run --rm telegram-forwarder
```

按提示输入 Telegram 验证码和两步验证密码。登录完成后按 Ctrl+C 结束前台进程，再执行 `docker compose up -d`。向机器人发送 `/help` 查看命令，通过 `/settings` 管理规则。用户账号需有源聊天读取权限，选择的发送账号需有目标聊天发送权限。

启用 RSS 时在 `.env` 设置 `RSS_ENABLED=true` 和对外访问的 `RSS_BASE_URL`，并取消 Compose 中 `9804:8000` 端口映射的注释。容器内 RSS 默认监听 `0.0.0.0:8000`；自定义 `RSS_PORT` 时同步调整映射。健康检查仅使用容器内 `127.0.0.1:8099`，无需映射该端口。

## 配置与运维

环境变量优先于 `.env`，显式空值保留。原有 JSON/TXT 配置只在缺失时补默认文件，不覆盖已存在内容。数据库默认位于 `db/forward.db`；若自定义 `DATABASE_URL` 与发现的旧数据库路径冲突，程序报错并要求明确选择，避免误建空库。

```bash
docker compose exec telegram-forwarder /app/telegram-forwarder --version
docker compose exec telegram-forwarder /app/telegram-forwarder --healthcheck
docker compose logs --tail=100 telegram-forwarder
docker compose stop telegram-forwarder
```

Go 日志以 JSON 同时写入标准输出和 `logs/telegram_forwarder.log`；文件达到 10 MiB 后轮转，保留 3 份。Docker 标准输出另由日志驱动轮转。`--healthcheck` 检查当前进程是否就绪，不等同于测试 Telegram 发送权限或外部 AI/推送服务。

| 参数 | 行为 |
| --- | --- |
| `--root <目录>` | 使用指定配置、数据库与运行数据根目录，默认当前目录 |
| `--check` | 离线校验，可能补默认配置、迁移数据库和导入会话，建议仅用于副本 |
| `--probe` | 在线验证 Telegram 登录及启用规则源/目标，不转发消息；会写独立 Go 会话 |
| `--healthcheck` | 检查容器内健康端点 |
| `--version` | 输出构建版本和提交号 |

## 构建与自动发布

模块要求 Go 1.25 及以上；Docker 和 GitHub Actions 固定使用 Go 1.26.4。SQLite 驱动为纯 Go，正式二进制使用 `CGO_ENABLED=0`。

```bash
go test ./...
go vet ./...
# Linux 上启用 CGO 并安装 C 编译器后运行竞态检查。
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=0 go build -trimpath -o "telegram-forwarder" ./cmd/telegram-forwarder
docker build -t telegram-forwarder:local .
```

[`build.yml`](.github/workflows/build.yml) 在 `main` 推送、`v*` 标签和手动触发时运行；针对 `main` 的 PR 同样测试和构建，但不发布镜像。测试、`go vet` 和 Linux 竞态检查成功后，生成 Linux、Windows、macOS 的 amd64/arm64 二进制归档与 SHA256 校验文件，制品名为 `telegram-forwarder-<os>-<arch>`。

全部二进制构建成功后，构建 `linux/amd64` 和 `linux/arm64` 镜像。`main` 发布 `latest` 和完整提交号的 `sha-<commit>` 标签；有效语义版本标签额外发布对应版本及主次版本标签。只有镜像作业申请 `packages: write`，通过仓库的 `GITHUB_TOKEN` 推送与仓库同名的 GHCR 包。手动选择非 `main`、非 `v*` 标签的引用时仅构建。

发布是否成功以 Actions 结果和 GHCR 实际镜像摘要为准。镜像不发布到原上游的 Docker Hub 命名空间。

## 许可证

沿用原项目的 [GNU GPL v3](LICENSE)。
