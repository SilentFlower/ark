# P4-1 鉴权设计研究

## 本地证据

- `docs/roadmap.md` P4-1 要求常驻 `ark-hub`、本地鉴权，并规定长任务只能启动 `ark` 子进程。
- `docs/design.md` ADR-005 规定备份调度继续由 systemd timer 承担，`ark-hub` 停止不能影响备份。
- `.trellis/spec/backend/directory-structure.md` 已固定 `cmd/ark-hub/` 与 `internal/hub/` 的归属。
- `.trellis/spec/backend/database-guidelines.md` 规定 P4 调用方只能通过 `store.Store` 公开 API 使用 `ark.db`。
- `docs/operations.md` 禁止把登录介质放进 hub 的 restic 备份；现有 `ark.db` 会被在线导出备份。
- 本地 `/root/project/dnsmgr` 使用 Web 安装、本地密码和可选 TOTP；用户已明确决定 ark-hub
  不采用 TOTP，并选择本机 CLI 初始化管理员。

## 密码哈希

- 使用 `golang.org/x/crypto/argon2.IDKey` 实现 Argon2id，不引入第三方认证框架。
- 参数采用 OWASP 当前最低建议之一：memory=19456 KiB、time=2、parallelism=1，salt=16 bytes，
  key=32 bytes；编码为严格 PHC 字符串并保存全部参数，后续可在登录成功后升级成本。
- `golang.org/x/crypto v0.33.0` 的 `go.mod` 要求 Go 1.20，是当前检查到可用于项目 Go 1.22
  的最后一组版本；v0.34.0 起要求 Go 1.23。
- 比较派生 key 时使用 `crypto/subtle.ConstantTimeCompare`；用户名错误仍执行一次固定 dummy hash，
  避免明显的账号存在性时序差异。
- 解析 PHC 字符串时限制 memory、time、parallelism、salt 和 key 长度，避免损坏文件诱发资源耗尽。

参考：

- https://pkg.go.dev/golang.org/x/crypto/argon2
- https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html

## 管理员密码输入

- `ark-hub admin init` 与 `ark-hub admin reset-password` 通过
  `golang.org/x/term.ReadPassword` 从终端无回显读取两次，不提供 `--password` 参数。
- 使用 `golang.org/x/term v0.29.0`，其 `go.mod` 要求 Go 1.18，并与项目已有
  `golang.org/x/sys v0.30.0` 对齐。
- 测试通过注入密码读取函数，不要求测试进程拥有真实 TTY。

参考：https://pkg.go.dev/golang.org/x/term

## 凭证与会话边界

- 单管理员凭证固定存放在 `/var/lib/ark-hub/auth.json`，schema v1 字段只包含 username、
  Argon2id PHC hash 和递增 revision；不存放明文密码、会话 token 或 TOTP 字段。
- 目录权限 `0700`、文件 `0600`。初始化使用排他创建；重置在同目录写临时文件、同步并原子替换。
  符号链接、非普通文件、权限过宽、未知 schema 或非法 hash 全部 fail closed。
- 会话只在 `ark-hub` 内存中保存，Cookie 仅持有 32-byte 随机 token 的 base64url 表示；服务端只按
  token SHA-256 索引会话。进程重启自然使全部会话失效。
- 会话记录绑定凭证 revision。管理员重置密码后，服务在下一次请求重新读取 revision，旧会话立即失效。

## HTTP 与 CSRF

- 默认监听 `127.0.0.1:8080`，公网 TLS 由反向代理负责；Cookie 的 Secure 属性通过显式配置开启。
- 最小路由为 `GET/POST /login`、`POST /logout`、`GET /healthz`、受保护 `GET /` 和
  `GET /api/session`。P4-2 业务 API 不在本任务实现。
- 登录页使用服务端渲染 HTML。登录前 CSRF 使用 HttpOnly double-submit Cookie + hidden field；
  登录后 CSRF token 保存在服务端 session，状态变更请求同时校验 token。
- Cookie 固定 HttpOnly、SameSite=Strict、Path=/，会话绝对有效期 12 小时，不提供“记住我”。
- 登录失败使用内存固定窗口限流，单管理员首版按远端 peer + username 聚合；成功后清零。
- HTTP server 设置 header/body/timeouts 与安全响应头，表单 body 有严格大小上限。

## systemd

- P4-1 增加独立 `ark-hub.service`，由 `ark-hub install` 通过现有 systemd 原子安装模式写入并校验。
- service 为 `Type=simple`、`Restart=on-failure`、`WantedBy=multi-user.target`，不创建 timer。
- 停止或重启 `ark-hub.service` 不修改、不停止也不依赖 `ark-backup*` / `ark-verify*` units。
