# 推送 URL 兼容范围

原生支持以下 Apprise 1.9.2 URL 方言，不依赖 Python。支持范围以本表为准，未覆盖 Apprise 的全部服务插件。

| URL scheme | 已实现范围 | 本地附件 |
| --- | --- | --- |
| `ntfy` / `ntfys` | 公共 ntfy.sh、自建服务器、Basic/Bearer、主题、优先级、标签 | 支持，逐个上传 |
| `json` / `jsons` | JSON Webhook、Basic、自定义请求头、查询参数、字段重映射 | Base64，每个文件最多 32 MiB |
| `form` / `forms` | 表单 Webhook、Basic、自定义请求头、查询参数、字段重映射 | multipart，支持 `attach-as` |
| `gotify` / `gotifys` | 自建服务器、路径前缀、应用 token、优先级 | 不支持 |
| `bark` / `barks` | 自建服务器、多设备 key、通知选项 | 不支持 |
| `discord` | Incoming webhook、用户名、TTS | 支持 |
| `tgram` | Bot token、多个 chat、话题 ID、静默、HTML/Markdown | 支持，以文档逐个发送 |
| `slack` | Incoming webhook、频道、用户名 | 不支持；Bot access-token URL 尚未实现 |
| `pover` | Pushover、设备、优先级、声音 | 支持，逐个发送 |
| `dingtalk` | 钉钉机器人、签名、手机号提及 | 不支持 |
| `feishu` / `lark` | 飞书/Lark 机器人、签名 | 不支持 |
| `schan` | Server 酱发送接口 | 不支持 |
| `mailto` / `mailtos` / `email` | SMTP、STARTTLS/TLS、Basic SMTP 认证、抄送/密送、常见邮箱自动配置 | 支持 MIME 附件 |

未列出的协议（例如 Matrix、Pushbullet、企业微信应用、Telegram 用户会话推送等）返回明确的不支持错误。启用配置会被 `push.ValidateConfigs` 离线检查；启动诊断调用它时可在发送前阻止不兼容配置。检查只报告配置 ID 和协议，避免记录 token 或完整 URL。新建目标也应调用 `push.Validate`。

已支持的服务仍可能存在尚未映射的 Apprise 扩展参数。上表的能力由本地 HTTP/SMTP 测试覆盖，请求目标和主要字段对照 Apprise 1.9.2；没有使用真实服务凭据发送通知。对于不支持附件的协议，带媒体消息会返回明确错误，避免只发文字却误报完整成功。`Single` 与 `Multiple` 保持旧数据库的值：前者逐个发送，后者在目标支持时批量发送。

通用 Webhook 保留 Apprise 参数约定：`+Header=value`（URL 编码为 `%2BHeader`）添加请求头，`-name=value` 添加目标查询参数，`:message=body` 把 `message` 字段重命名为 `body`，`:title=` 删除标题字段。邮件 `smtp` 可指定服务器，`mode` 支持 `insecure`、`starttls`、`ssl`。TLS 使用系统证书验证。
