# Brief — P4-1 ark-hub 骨架与鉴权

## Goal

- 建立可长期运行的 `ark-hub` HTTP 服务与密码鉴权边界，为后续 P4 API 和 Vue 控制台提供安全、可测试的基础。

## Scope

- 新增薄入口 `cmd/ark-hub/` 与 `internal/hub/`，实现 `serve`、管理员初始化、密码重置和 systemd 安装命令。
- 使用独立的 `/var/lib/ark-hub/auth.json` 保存单管理员用户名、Argon2id 密码哈希与凭证 revision；文件按 root-only、原子更新和 fail-closed 规则管理。
- 实现内存会话、密码重置后会话失效、登录限流、CSRF、安全 Cookie、HTTP 超时与安全响应头。
- 提供最小登录页、受保护页面、`/healthz` 和 `/api/session`，并通过 `store.Store` 打开现有 `ark.db`。
- 新增独立 `ark-hub.service` 生成与原子安装能力，不创建 timer，也不改变现有 backup/verify units。
- 更新 roadmap、设计和运维文档，记录密码鉴权、无 TOTP、auth 文件排除与 hub 重建后的初始化步骤。

## Non-Goals

- 不实现 P4-2 的 hosts、runs、alerts、backup、verify 或 restore HTTP API。
- 不实现 P4-3 的 Vue 3 控制台、总览、主机详情和操作对话框。
- 不实现 P4-4 的 `go:embed`、pnpm 构建和 `make hub`。
- 不实现 P4-5 的钉钉告警、静默期和外部心跳。
- 不实现 TOTP 或其它二次验证、多管理员、角色、OIDC、API token、密码找回邮件和 TLS 终止。

## Key Decisions

- 用户明确决定完全移除 TOTP；首版只使用本地管理员密码鉴权，不保留隐藏开关或占位字段。
- 管理员只能通过 hub 本机 CLI 初始化和重置，不提供未认证 Web 安装页面，也不接受明文密码参数。
- 密码使用 Argon2id；鉴权文件与会话不写入会被 restic 导出的 `ark.db`，进程重启使内存会话全部失效。
- 默认监听 `127.0.0.1:8080`，公网访问与 TLS 由反向代理承担；应用不自动信任 forwarding headers。
- `ark-hub` 不承载调度或长任务，停止它不能影响 systemd 驱动的备份与恢复演练。

## Key Context

- 目录规范已固定 `cmd/ark-hub/` 与 `internal/hub/`；入口必须保持薄，HTTP 与鉴权逻辑不能扩散到 `cmd/`。
- P4 调用方只能通过 `store.Store` 公开 API 使用 `/var/lib/ark/ark.db`，不能持有 `*sql.DB` 或拼 SQL。
- 现有 `internal/systemd` 已提供 unit 暂存、`systemd-analyze verify`、原子替换和回滚机制，hub service 应复用该安装内核。
- 项目使用 Go 1.22；规划固定 `golang.org/x/crypto v0.33.0` 和 `golang.org/x/term v0.29.0`。
- 详细安全参数、路由、失败矩阵与依赖证据见 `design.md` 和 `research/auth-design.md`。

## Risks / Deferred

- 取消 TOTP 后，安全性更依赖强密码、默认 loopback、反向代理 TLS、限流、CSRF 和会话撤销；这些基础措施不能再省略。
- P4-1 不信任 `X-Forwarded-For`，反向代理后的多个客户端可能共享登录限流桶；可信代理模型留待部署需求明确后处理。
- 完整业务页面和操作流程延后到 P4-2/P4-3，本任务的受保护页面只用于证明鉴权与会话链路可用。

## Acceptance

- `ark-hub` 可构建、启动、响应并优雅停止；未初始化时拒绝监听且没有 Web 安装入口。
- CLI 可初始化管理员和重置密码；重复初始化、非法文件、权限过宽、损坏 hash 与原子更新失败均 fail closed。
- 未认证、错误密码、限流、CSRF、伪造/过期 session、退出和密码重置失效均有自动测试。
- 密码、hash、session token、Cookie 和请求体不进入日志、错误或普通响应。
- `ark-hub.service` 通过真实 `systemd-analyze verify`，且不创建、修改或停止任何 timer。
- `make check`、两套二进制常规与无 CGO 构建、`go mod verify` 和 `git diff --check` 通过。

## Next Step

- 实现、故障注入验收与 full Check-All 已通过；下一步进入 spec 更新与提交。
