# Telegram Forwarder

> [!IMPORTANT]
> **本项目迁移自 [Heavrnl/TelegramForwarder](https://github.com/Heavrnl/TelegramForwarder)，使用 Go 重写。** 保留原项目的数据与配置兼容能力，感谢原作者的开源贡献。

[![构建](https://github.com/iLay1678/telegram-forwarder/actions/workflows/build.yml/badge.svg)](https://github.com/iLay1678/telegram-forwarder/actions/workflows/build.yml)
[![许可证](https://img.shields.io/badge/license-GPL--3.0-blue)](LICENSE)

使用 Go 编写的 Telegram 消息转发服务，通过用户账号监听频道和群组，通过机器人管理转发规则。支持关键词过滤、内容替换、媒体处理、AI、RSS 和多平台通知。

```text
ghcr.io/ilay1678/telegram-forwarder:latest
```

## 功能

- **消息转发**：多来源、多目标，支持文本、图片、视频、文件和相册。
- **规则管理**：关键词黑白名单、正则匹配与替换、媒体类型和大小限制、规则复制同步、导入导出。
- **AI**：内容处理、图片请求、定时消息总结，支持 OpenAI 兼容接口及 Claude、Gemini 原生接口。
- **RSS**：订阅输出、媒体托管、账号登录和管理页面。
- **通知与同步**：Webhook、邮件及多种通知服务；UFB 配置和关键词同步。

用户模式执行关键词筛选和 Telegram 原生转发；内容替换、媒体过滤、AI、RSS、外部推送等高级处理使用机器人模式。通知协议及附件支持范围见[推送协议表](docs/push-compatibility.md)。

## Docker 部署

准备 Docker Engine 和 Compose，下载 [docker-compose.yml](docker-compose.yml) 与 [.env.example](.env.example) 到同一目录。

```bash
cp ".env.example" ".env"
mkdir -p "db" "logs" "sessions" "temp" "config" "ufb/config" "rss/data" "rss/media"
```

编辑 `.env`，填写以下配置：

| 配置 | 说明 |
| --- | --- |
| `API_ID`、`API_HASH` | 在 [Telegram API 页面](https://my.telegram.org/apps) 申请 |
| `PHONE_NUMBER` | 用户账号手机号，含国际区号，用于首次登录 |
| `BOT_TOKEN` | 从 [@BotFather](https://t.me/BotFather) 创建机器人并获取 |
| `USER_ID` | 管理员的 Telegram 数字 ID |
| `ADMINS` | 可选；多个管理员 ID 用逗号分隔，默认使用 `USER_ID` |

拉取镜像，首次以前台交互方式登录：

```bash
docker compose pull telegram-forwarder
docker compose run --rm --no-deps telegram-forwarder
```

按提示输入验证码和两步验证密码。看到服务就绪后按 Ctrl+C 退出，再启动后台服务：

```bash
docker compose up -d telegram-forwarder
docker compose logs --tail=100 -f telegram-forwarder
```

用户账号需要能够读取源聊天；选定的发送账号需要能够向目标聊天发送消息。已有有效会话时无需重新输入验证码。

## 使用

向机器人发送 `/help` 查看全部命令。常用操作如下：

| 命令 | 用途 |
| --- | --- |
| `/bind <源聊天> [目标聊天]` | 创建规则，目标省略时使用当前聊天 |
| `/settings [规则ID]` | 配置发送模式、过滤、AI、RSS 和推送 |
| `/list_rule`、`/switch` | 查看或切换当前规则 |
| `/add <关键词>`、`/add_regex <正则>` | 添加关键词或正则 |
| `/replace <正则> [替换内容]` | 添加替换规则 |
| `/summary [规则ID]` | 立即生成总结 |
| `/cancel` | 取消当前设置输入 |

例如，在目标群组中发送 `/bind https://t.me/source_channel` 创建规则，再通过 `/settings` 调整过滤和发送方式。

## 配置与数据

全部配置项见 [.env.example](.env.example)。环境变量优先于 `.env`；已有 JSON/TXT 配置不会被默认文件覆盖。

| 配置 | 用途 |
| --- | --- |
| `DEFAULT_TIMEZONE` | 定时任务时区，默认 `Asia/Shanghai` |
| `DEFAULT_MAX_MEDIA_SIZE` | 默认媒体大小限制，单位 MiB |
| `OPENAI_API_KEY`、`OPENAI_API_BASE` | OpenAI 或兼容服务的凭据和地址 |
| `CLAUDE_API_KEY`、`GEMINI_API_KEY` | Claude、Gemini 原生接口凭据 |
| `RSS_ENABLED`、`RSS_BASE_URL` | 启用 RSS 及设置外部访问地址 |
| `UFB_ENABLED`、`UFB_SERVER_URL`、`UFB_TOKEN` | UFB 同步配置 |

启用 RSS 时，取消 Compose 中 `9804:8000` 端口映射的注释。RSS 默认监听容器内 `0.0.0.0:8000`；自定义 `RSS_PORT` 后需同步调整映射。

容器工作目录为 `/app`，持久化文件由以下挂载保存：

| 本地路径 | 内容 |
| --- | --- |
| `.env` | 环境配置 |
| `db/` | SQLite 数据库，默认 `forward.db` |
| `sessions/` | Telegram 授权、更新游标和实体缓存 |
| `config/` | AI 模型和菜单选项等 JSON/TXT 配置 |
| `ufb/config/` | UFB 配置 |
| `rss/data/`、`rss/media/` | RSS 条目和媒体 |
| `logs/` | 应用日志 |
| `temp/` | 媒体临时文件 |

已有 Python 部署可保留 `.env`、数据库、配置、RSS 数据和全部挂载，只将镜像改为上方地址。切换前停止旧容器并备份部署目录。程序按需补齐数据库结构，导入 `user.session`、`bot.session`，将 Go 授权和状态写入独立文件，保留原 Telethon 文件。Go 运行期间不会同步旧 Telethon 游标，回退旧客户端可能重复或遗漏消息；同一份授权只运行一个实例。

## 更新与维护

```bash
docker compose pull telegram-forwarder
docker compose up -d telegram-forwarder
docker compose exec telegram-forwarder /app/telegram-forwarder --version
docker compose exec telegram-forwarder /app/telegram-forwarder --healthcheck
docker compose logs --tail=100 telegram-forwarder
```

更新前停止容器并备份整个部署目录，以保存一致的数据库、会话和 RSS 文件。应用日志以 JSON 写入标准输出及 `logs/telegram_forwarder.log`，文件达到 10 MiB 后轮转，保留 3 份。健康检查使用容器内 `127.0.0.1:8099`，无需映射端口；它检查进程就绪状态，发送权限和外部服务需另外验证。

| 启动参数 | 用途 |
| --- | --- |
| `--root <目录>` | 指定配置与数据目录，默认当前目录 |
| `--check` | 离线检查配置、数据库和会话，可能创建默认文件、升级数据库或导入授权；应在独立副本执行 |
| `--probe` | 登录 Telegram 并检查启用规则的源和目标，不接收消息更新或执行转发；会写入独立 Go 会话及缓存 |
| `--healthcheck` | 检查当前服务是否就绪 |
| `--version` | 显示版本与提交号 |

数据库地址仅支持 SQLite；自定义 `DATABASE_URL` 与发现的旧库路径冲突时会报错。`CHANNEL_PRIVATE` 通常需要检查账号是否仍有频道访问权限；更新差异过期时需按需使用历史转发补收。消息发送和本地游标不是跨系统事务，异常中断后可能重复投递。

## 构建与发布

[GitHub Actions](.github/workflows/build.yml) 在推送 `main`、`v*` 标签或手动触发时运行；PR 执行检查与构建，不发布镜像。

工作流先运行测试、`go vet` 和竞态检查，再构建 Linux、Windows、macOS 的 amd64/arm64 二进制及 SHA256 校验文件。镜像任务下载同次构建的 Linux 制品并验证校验和，直接拷入 amd64/arm64 镜像；Dockerfile 不重复编译。部署机器只需拉取成品镜像。

`main` 发布 `latest` 与 `sha-<完整提交号>`；语义版本标签额外发布对应版本和主次版本。二进制可从 [Actions](https://github.com/iLay1678/telegram-forwarder/actions) 的构建制品下载。模块要求 Go 1.25 及以上，正式二进制使用纯 Go SQLite 驱动，不依赖 Python 或 CGO。

## 来源与许可证

遵循 [GNU GPL v3](LICENSE)。原 Python 实现保存在 [`python` 分支](https://github.com/iLay1678/telegram-forwarder/tree/python)。
