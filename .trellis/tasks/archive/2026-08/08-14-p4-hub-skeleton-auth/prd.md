# P4-1 ark-hub 骨架与鉴权

## Goal

建立可长期运行的 `ark-hub` HTTP 服务骨架，并在任何状态查询或生产操作能力出现之前，
先提供本地管理员账号与密码鉴权边界。P4-1 完成后应具备可启动、可停止、
可登录、可退出和可测试的受保护服务壳，但不提前实现 P4-2 业务 API 或 P4-3 完整前端。

## Background

- Roadmap 要求 `ark-hub` 常驻读取 `/var/lib/ark/ark.db`，但不承载调度；临时长任务只能启动
  `ark` 子进程（`docs/roadmap.md` P4-1、ADR-005）。
- `ark-hub` 未来能发起覆盖生产数据的恢复，因此内网部署也必须鉴权。
- Roadmap 原计划采用本地账号 + TOTP；用户已明确决定完全移除 TOTP，首版只保留密码鉴权。
- `docs/operations.md` 明确登录介质不得进入 hub 的 restic 备份。当前 `ark.db` 会被在线导出
  并备份，因此会话 token、会话密钥等鉴权秘密不能直接写入现有状态库。

## Requirements

### R1 服务边界

- 新增薄入口 `cmd/ark-hub/`，HTTP 服务实现归属 `internal/hub/`。
- 服务支持可测试的启动、优雅停止和明确的监听地址配置；默认值必须偏向本机安全部署。
- 服务可通过 `store.Store` 公开 API 读取现有状态库，不持有 `*sql.DB`，不拼 SQL，
  不依赖 SQLite 实现细节。
- P4-1 不新增备份调度循环，也不在 HTTP handler 内执行长任务。
- `ark-hub install` 生成独立的 `ark-hub.service`；该 service 不包含 timer，停止它不得影响
  现有 `ark-backup*` 与 `ark-verify*` units。

### R2 本地账号与凭证存储

- 首版只支持一个本地管理员账号，不引入多用户、角色、OIDC 或反向代理托管鉴权。
- 管理员必须通过 hub 本机 CLI 初始化；未初始化时 HTTP 服务不得开放 Web 安装入口，
  只能显示不可用状态或拒绝启动。
- CLI 支持显式初始化与密码重置，密码通过无回显终端输入获取，不接受明文密码参数。
- 密码只保存抗暴力破解的单向哈希，任何日志、错误、响应或测试快照都不得包含明文密码、
  会话 token、会话密钥或完整 Cookie。
- 鉴权状态使用独立、root-only 的持久化边界，不写入会被备份的 `/var/lib/ark/ark.db`；
  hub 重建后通过本机管理入口重新初始化或重置。
- P4-1 不实现、存储或展示 TOTP，不保留隐藏开关或未来占位字段。

### R3 密码登录与会话

- 登录只校验本地管理员账号与密码，错误响应不得泄露账号是否存在。
- 会话使用高熵随机 token、服务端可撤销状态和 `HttpOnly` Cookie；Cookie 的
  `SameSite`、`Secure`、过期和路径策略必须明确并有测试。
- 登录和其它状态变更请求必须具备 CSRF 防护；重复失败需要有界限流，不能无限尝试。
- 退出登录立即撤销服务端会话，密码重置后已有会话全部失效。

### R4 HTTP 保护边界

- 除明确列出的登录和必要存活检查外，全部路由默认要求已认证会话。
- 未认证浏览器请求进入登录流程；JSON/API 请求返回稳定的 `401`，不返回 HTML 成功页。
- P4-1 只提供证明鉴权生效的最小受保护页面或端点，不实现 hosts、runs、alerts、backup、
  verify、restore 等 P4-2 契约。

### R5 质量与运维

- 所有导出类型、函数和方法使用中文 Javadoc 风格注释；复杂安全逻辑解释设计原因。
- 鉴权失败、限流、会话失效和存储错误显式返回，不能 panic 或静默放行。
- 不因引入 `ark-hub` 自动加入日志框架；若确有新增日志依赖需求，必须先更新 ADR。
- 同步更新 `docs/roadmap.md` 与 `docs/design.md`，移除 ark-hub 必须使用 TOTP 的旧规划。

## Acceptance Criteria

- [ ] `go build ./cmd/ark-hub` 成功，服务能启动、响应并在 context/信号取消后优雅退出。
- [ ] 未初始化时服务不会暴露 Web 安装入口；本机 CLI 可创建管理员，重复初始化必须失败，
  显式密码重置后旧密码与已有会话立即失效。
- [ ] 未认证访问受保护路由失败；密码登录后可访问，退出后立即失效。
- [ ] 错误密码、过期/伪造会话、CSRF 缺失和连续失败限流均有测试。
- [ ] 密码哈希、会话 token、会话密钥与 Cookie 不出现在日志、错误或普通响应中。
- [ ] 鉴权秘密不写入 `/var/lib/ark/ark.db`，独立存储权限与非普通文件/符号链接拒绝策略有测试。
- [ ] `ark-hub` 停止不影响现有 `ark backup` / systemd timer；本任务不新增调度实现。
- [ ] `ark-hub install` 生成的 service 通过 `systemd-analyze verify`，且不创建或修改任何 timer。
- [ ] `docs/roadmap.md` 与 `docs/design.md` 已准确记录“密码鉴权、无 TOTP”的产品决定。
- [ ] `make check`、常规构建、`CGO_ENABLED=0` 构建与 `git diff --check` 通过。

## Out of Scope

- P4-2 的 hosts/runs/alerts 与 backup/verify/restore HTTP API。
- P4-3 的 Vue 3 应用、总览卡片、主机详情和操作对话框。
- P4-4 的 `go:embed`、pnpm 构建和 `make hub`。
- P4-5 的钉钉告警、静默期和外部心跳。
- 多管理员、细粒度权限、OIDC、API token、密码找回邮件和公网 TLS 终止。
- TOTP、短信、邮件验证码或其它二次验证机制。

## Technical Notes

- `internal/hub/` 是 roadmap 和目录规范已经保留的后端归属；`cmd/ark-hub/` 必须保持薄入口。
- 当前项目没有密码鉴权依赖；具体哈希库、参数、会话与存储格式在技术设计中确定。
