# SMTP Gateway Go

轻量 SMTP 网关：应用通过 SMTP AUTH 提交邮件，网关先将原始邮件和实际 `RCPT TO` 收件人写入 SQLite 或 PostgreSQL，再由后台 Worker 使用一个全局上游 SMTP 配置投递。完整需求见 [issue #2](https://github.com/zhangzqs/smtp-gateway-go/issues/2)，实现约束见 [设计文档](docs/design.md)。

## 启动

需要 Go 1.27.2，或使用 Docker。复制 `config.example.yaml` 为 `config.yaml`，填写管理密码、提交密码和随机加密密钥：

```sh
cp config.example.yaml config.yaml
openssl rand -base64 32
go run ./cmd/smtp-gateway serve --config config.yaml
```

本机直接运行时，将 `database.dsn` 改成可写的本地路径，例如 `./gateway.db`。也可以运行 `docker compose up --build`，此时保留示例中的 `/data/gateway.db`。Compose 仅将 HTTP 和 SMTP 端口发布到宿主机回环地址，并将 SQLite 数据库保存在命名卷中。示例配置显式允许本机开发使用明文 SMTP AUTH；生产部署应配置 SMTP `starttls` 或 `implicit` TLS，并通过 HTTPS 或受控反向代理访问管理 API。

配置文件包含 HTTP/SMTP 监听与独立凭据、数据库驱动及 DSN、Base64 编码的 32 字节加密密钥、队列限额和 Worker 参数。PostgreSQL 由部署环境先创建数据库及账号；将 `database.driver` 改成 `postgres` 并填写连接串。多个实例必须使用相同的静态配置和加密密钥。数据库 schema 在启动时迁移；迁移失败则停止启动。

无上游配置时仍可启动和管理，SMTP 提交返回临时错误。可通过 API 配置上游，或在配置文件中写入完整 `relay` 对象。文件配置优先，API 返回 `read_only=true` 并拒绝 PUT；移除文件中的 `relay` 后，原先保存在数据库里的 API 配置再次生效。文件变更需要重启，API 更新对下一次投递尝试立即生效。

## 管理 API

除健康检查外，接口都要求 HTTP Basic Auth。`/metrics` 也受 Basic Auth 保护。

| 接口 | 用途 |
| --- | --- |
| `GET /v1/relay` | 查询脱敏配置及 `configured/read_only/has_password` |
| `PUT /v1/relay` | 完整替换上游与发件身份 |
| `GET /v1/messages?status=pending&limit=50&offset=0` | 查询邮件和逐收件人状态，不返回正文 |
| `POST /v1/messages/{id}/retry` | 重试仍在保留期内的失败或过期收件人 |
| `GET /healthz` | 进程存活 |
| `GET /readyz` | 数据库和服务就绪 |
| `GET /metrics` | 队列与投递指标 |

可以先启动 Compose 调试配置 `docker compose --profile debug up --build`，再通过 API 将上游指向 Compose 网络中的 Mailpit：

```sh
curl -u manager -X PUT http://127.0.0.1:8080/v1/relay \
  -H 'Content-Type: application/json' \
  -d '{"host":"mailpit","port":1025,"tls":"none","username":"","from_address":"alerts@example.com","from_name":"通知"}'
```

`PUT` 的 `password` 省略表示仅在上游主机、端口、TLS 和用户名均未改变时保留旧密码；认证上下文改变必须提供新密码。用户名为空会清除旧密码。接口不会返回密码，数据库中的上游密码使用配置密钥进行 AES-GCM 加密。

## 投递语义

SMTP 仅在邮件及全部收件人事务提交后回复 `250`。上游暂时不可用时，已接收邮件留在持久队列并按指数退避重试；永久拒收、到期和成功状态按收件人记录。Worker 通过有期限的租约领取邮件，重启后可恢复到期任务。同一封邮件的各收件人可以有不同的重试时间，手工重试不会重发已成功的收件人。

`accepted_by_upstream` 表示上游 SMTP 接受了 DATA，不能保证邮件最终送达收件箱。上游接受之后、网关写入成功状态之前若进程崩溃，恢复投递可能产生重复邮件；这是至少一次投递语义。

## 开发与验证

```sh
task ci
task integration
task build-all
docker build -t smtp-gateway-go:local .
```

`task ci` 运行依赖及 lint 检查、漏洞扫描、race 测试、无 CGO SQLite 测试和构建。默认测试使用临时 SQLite 文件及本地 SMTP 故障服务端，无需 Docker。`task integration` 使用 Testcontainers 验证 PostgreSQL 契约及容器重启恢复，需要可用的 Docker daemon；CI 单独运行这些测试。`CGO_ENABLED=0` 可构建 Linux amd64 和 arm64 二进制。

2026-10-19 本地构建的未压缩产物大小：Linux amd64 二进制 20,476,064 字节，Linux arm64 二进制 19,333,280 字节，Linux amd64 完整镜像 22,590,042 字节。容器测试通过 SQLite 数据卷与 PostgreSQL 数据库分别验证了强制停止后的恢复投递。
