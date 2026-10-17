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
- 尚待：成品镜像离线检查、双客户端旧会话在线探测、最终部署健康状态。

## 发布门槛与边界

GitHub 测试、竞态检查、六平台二进制、双架构容器和 GHCR 拉取验证尚待本次流水线结果。验证标签为 `go-validation`；正式 `main/latest` 在验收和范围确认后发布。

推送支持范围见 [兼容表](push-compatibility.md)。原生实现尚未覆盖全部 Apprise 插件，首版范围选择等待用户答复。真实外部 AI、推送、UFB 和生产消息发送尚未逐项验证；在线 `--probe` 仅登录和读取实体，不证明发送权限。

消息发送与更新游标不是跨系统事务，强制终止可能造成重复；回滚旧 Telethon 游标也不能承诺消息零重复或零漏收。
