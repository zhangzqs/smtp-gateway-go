# SMTP Gateway Go

轻量 SMTP 网关，计划提供配置 API、SQLite/PostgreSQL 持久邮件队列和自动重试。
完整需求与验收方案见 [设计 issue #2](https://github.com/zhangzqs/smtp-gateway-go/issues/2)。

当前阶段是项目骨架：已提供命令入口、单元测试、lint 和 CI。
SMTP 接收、HTTP 管理接口和持久队列尚未实现。

## 开发环境

- Go 1.27.1，版本以 `go.mod` 为准。
- [Task](https://taskfile.dev/docs/installation) 3.54.0。
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/) 2.15.0。
- 数据竞争检测需要可用的 C 编译器；发布二进制使用 `CGO_ENABLED=0` 构建。

```sh
git clone https://github.com/zhangzqs/smtp-gateway-go.git
cd smtp-gateway-go
task ci
```

## 常用命令

| 命令 | 用途 |
| --- | --- |
| `task` | 列出任务 |
| `task tidy` | 整理模块依赖 |
| `task fix` | 格式化和自动修复 lint 问题 |
| `task check` | 检查模块依赖、lint 配置和代码 |
| `task test` | 使用 `require` 运行单元测试，启用 race、coverage 和随机测试顺序 |
| `task build` | 构建 `bin/smtp-gateway`，版本来自 Git 描述 |
| `task ci` | 顺序执行检查、测试和构建 |

```sh
./bin/smtp-gateway --help
./bin/smtp-gateway --version
```

无参数运行会显示帮助。命令当前不会启动邮件或 HTTP 服务。

## 目录结构

```text
cmd/smtp-gateway/       命令入口和 CLI 测试
.github/workflows/      Pull Request 与主分支检查
.golangci.yaml         lint 和格式化配置
Taskfile.yaml          本地与 CI 共用任务
```

后续按设计逐步加入配置、SMTP、数据库和投递模块。需要真实依赖的集成测试将使用
Testcontainers for Go，SQLite 存储测试使用临时数据库文件。

## CI

GitHub Actions 在 Pull Request、`main` 推送及手工触发时执行 `task ci`，并验证构建出的
程序可以输出版本。CI 固定工具版本及 Action 提交，使用只读仓库权限。
