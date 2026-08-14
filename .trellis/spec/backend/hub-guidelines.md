# Hub Guidelines

> `ark-hub` 的本地鉴权、HTTP 边界、会话、限流、状态库连接与常驻 service 契约。

---

## Scenario: 修改 ark-hub 鉴权或服务骨架

### 1. Scope / Trigger

以下改动必须遵守本规约：

- 修改 `cmd/ark-hub/`、`internal/hub/` 的 CLI、凭证文件、密码哈希、会话、CSRF 或 HTTP 路由；
- 修改 `ark-hub` 对 `store.Store` 的打开、关闭或请求期读取方式；
- 修改登录限流、Cookie、安全响应头、HTTP server 超时或优雅停止；
- 修改 `ark-hub install` 或 `ark-hub.service` 的生成、校验和安装行为。

`cmd/ark-hub` 必须保持薄入口。鉴权和 HTTP 逻辑属于 `internal/hub`；SQLite 细节属于
`internal/store`；通用 unit 原子安装内核属于 `internal/systemd`。`ark-hub` 不承载调度，
长任务由后续业务 API 启动 `ark` 子进程，不能在 handler 内直接实现备份、演练或恢复。

### 2. Signatures

CLI 契约：

```text
ark-hub serve [--listen <address>] [--state-db <absolute-path>]
              [--auth-file <absolute-path>] [--secure-cookie]
ark-hub admin init [--username <name>] [--auth-file <absolute-path>]
ark-hub admin reset-password [--auth-file <absolute-path>]
ark-hub install [--unit-dir <absolute-path>] [--listen <address>]
                [--state-db <absolute-path>] [--auth-file <absolute-path>]
                [--secure-cookie]
```

Go 入口：

```go
type ServeOptions struct {
    ListenAddress string
    StateDBPath   string
    AuthFile      string
    SecureCookie  bool
}

func Run(ctx context.Context, options ServeOptions) error
func BuildHubUnit(options systemd.HubInstallOptions) (systemd.Unit, error)
func InstallHub(ctx context.Context, options systemd.HubInstallOptions) (systemd.InstallResult, error)
```

HTTP 路由：

```text
GET  /healthz      public, text/plain
GET  /login        public login page
POST /login        public + login CSRF
GET  /             authenticated HTML shell
POST /logout       authenticated + session CSRF
GET  /api/session  authenticated JSON
```

凭证 schema v1：

```json
{
  "schema_version": 1,
  "username": "admin",
  "password_hash": "$argon2id$...",
  "revision": 1
}
```

### 3. Contracts

- 首版只有一个本地管理员，只使用密码，不实现 TOTP、OIDC、角色或 Web 安装入口。
- 密码只从本机 TTY 无回显读取两次，不接受明文 flag 或环境变量；长度为 12-1024 bytes，
  不 trim、不截断。哈希使用受参数上限保护的 Argon2id PHC。
- 默认凭证路径是 `/var/lib/ark-hub/auth.json`，独立于会被备份的 `ark.db`。生产进程以 root
  运行；读取时目录必须是普通目录、`0700` 且 owner UID 等于当前进程 effective UID，文件必须是
  普通文件、`0600`、owner UID 相同，符号链接、未知字段/schema 和越界 hash 参数全部拒绝。
- 初始化使用排他创建。初始化或重置必须锁定凭证目录，而不是会被 rename 替换的 auth 文件；
  同一把跨进程 flock 覆盖 revision 的读取、递增和提交，保证每次成功重置恰好递增一次。
- 重置使用同目录临时文件、文件 `fsync`、原子 rename 和目录同步。提交失败保留旧凭证；
  初始化失败必须尝试删除不完整目标并同步目录，原错误和清理错误都必须可通过错误链识别。
- 会话 token 和 CSRF token 各使用 32-byte 高熵随机值。Cookie 只保存 raw session token 的
  base64url；服务端 map 使用 SHA-256(token) 作为 key，会话绑定用户名、凭证 revision 和 12 小时
  绝对过期时间。退出立即撤销，密码重置后下一次请求因 revision 不匹配而撤销旧会话。
- 登录限流 key 固定为 peer host + 规范化用户名，5 分钟窗口最多 5 次失败或在途校验。
  请求进入 Argon2 前必须在 mutex 内原子预占名额；success/failure/cancel 幂等完成一次预占。
  map 最多保存 4096 个 key，每次进入时清理过期窗口；容量耗尽时 fail closed 返回 `429`。
- 除 `/healthz`、`GET /login`、`POST /login` 外全部路由默认鉴权。未认证 `/api/` 返回稳定
  `401` JSON，页面请求重定向登录；运行期 auth 存储错误返回 `503`，不能降级成未认证或放行。
- 所有响应设置 CSP、`nosniff`、`no-referrer`、`DENY` 和 `no-store`。Cookie 固定
  `HttpOnly`、`SameSite=Strict`、`Path=/`；`Secure` 只由显式 flag 控制，不信任 forwarding header。
- `Run` 只能通过 `store.Open` 获得 `Store` 并调用其公开 API，不持有 `*sql.DB` 或拼 SQL。
  凭证校验和状态库打开必须发生在 listen 前；context 取消后调用有界 `Shutdown`，必要时调用
  `Close`，并用 `errors.Join` 聚合 Serve、Shutdown、server Close 和 Store.Close 错误。
- `ark-hub.service` 只包含二进制路径和非秘密启动 flag，使用 `Type=simple`、`UMask=0077`、
  `Restart=on-failure`。安装只管理 `ark-hub.service`，不创建、扫描、删除、enable 或启动 timer。
- 密码、hash、session token、CSRF token、完整 Cookie 和请求 body 不得进入日志、错误、普通响应
  或状态库。本阶段不为 hub 引入日志框架。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| auth 文件不存在 | `serve` 在 listen 前失败，并提示本机 `admin init` |
| auth 目录/文件 owner 不等于进程 effective UID | fail closed，不读取内容 |
| auth 是 symlink、非普通文件、权限过宽或 JSON/hash 损坏 | fail closed，不分配越界 Argon2 资源 |
| 重复 init | 排他创建失败，不覆盖既有管理员 |
| 两个进程并发 reset | 串行提交，成功次数与 revision 增量一致 |
| init 的目录同步失败且删除也失败 | 同时返回原错误和清理错误，不伪报成功 |
| reset 临时写入或 rename 失败 | 返回失败，旧密码和 revision 保持有效 |
| 同一 key 有 5 个失败或在途 Argon2 | 后续请求在哈希前返回 `429` 和 `Retry-After` |
| 限流 key 达到 4096 | 新 key 返回 `429`；窗口过期后清理并恢复 |
| username 不存在或密码错误 | 使用同一响应，并执行一次受控成本 Argon2 |
| CSRF 缺失或不匹配 | 返回 `403`，不执行登录或退出状态变更 |
| session 伪造、过期或 revision 漂移 | 撤销 Cookie，API 返回 `401` |
| 已有 session 期间 auth 文件运行期损坏 | API 返回 `503` JSON，不伪装成普通未登录 |
| store 打不开 | 不创建 listener，返回带上下文错误 |
| listener 绑定失败 | 关闭 store，并聚合 listener 与 close 错误 |
| Shutdown、server Close、Store.Close 同时失败 | 返回可由 `errors.Is` 识别的完整聚合错误链 |
| hub unit verify、覆盖或 rename 失败 | 旧 service 不变或完成回滚，既有 timer 不变 |

### 5. Good / Base / Bad Cases

- **Good**：管理员在 root TTY 初始化，登录后访问受保护页面；密码重置使全部旧 session 在下次
  请求失效，多个并发重置的最终 revision 等于初始值加成功次数。
- **Good**：五个错误密码正在执行 Argon2 时，第六个请求立即收到 `429`，不会因并发检查与计数
  分离而绕过上限；五分钟后旧窗口被清理。
- **Base**：`GET /healthz` 无需鉴权，只返回 `ok` 和安全 header；未认证
  `GET /api/session` 返回 `{"authenticated":false}` 与 `401`。
- **Base**：`ark-hub.service` 独立运行；停止它不会停止或修改 `ark-backup*` / `ark-verify*`。
- **Bad**：先检查失败次数、Argon2 完成后才递增计数。并发请求会全部穿过检查，限流形同虚设。
- **Bad**：用 auth 文件本身做 flock。reset rename 后锁绑定旧 inode，另一个进程可锁住新文件并
  与前一个进程同时提交，导致 revision 丢失。
- **Bad**：初始化 `fsync` 失败后静默忽略 `Remove` 错误。命令报告失败，但不完整凭证仍留在磁盘。

### 6. Tests Required

修改 hub 鉴权或服务骨架至少运行：

```bash
go test ./internal/hub ./internal/systemd -race -count=10
make check
make build
go build -trimpath -o bin/ark-hub ./cmd/ark-hub
CGO_ENABLED=0 go build -trimpath -o bin/ark-nocgo ./cmd/ark
CGO_ENABLED=0 go build -trimpath -o bin/ark-hub-nocgo ./cmd/ark-hub
go mod verify
go test -v ./internal/systemd -run 'SystemdAnalyzeVerify' -count=1
git diff --check
```

测试必须覆盖：

- auth owner/mode/symlink/schema/hash 边界，重复 init、并发 reset、rename 失败保留旧凭证；
- 初始化同步与清理同时失败的错误聚合；密码终端读取器注入和 reset 命令 revision 递增；
- session 并发创建/读取/撤销，伪造、过期、退出和密码重置失效；
- 登录并发预占最多 5 个、4096 key 容量上限、窗口清理、成功清零和 `Retry-After`；
- 健康检查、受保护页面、CSRF、body 上限、安全 Cookie/header、运行期 auth 故障 `503`；
- store/listener/Serve/Shutdown/server Close/Store.Close 故障注入及 `errors.Is` 断言；
- hub unit 非法参数、非受管文件、symlink、verify/rename 失败、真实 systemd verify 和 timer 隔离。

### 7. Wrong vs Correct

#### Wrong

```go
if limiter.allow(key) {
    valid := verifyPassword(password)
    if !valid {
        limiter.failure(key)
    }
}
```

检查与计数分离时，多个请求会在任一请求完成 Argon2 前同时通过 `allow`。

#### Correct

```go
attempt, retryAfter, allowed := limiter.begin(key)
if !allowed {
    return tooManyRequests(retryAfter)
}
defer attempt.cancel()

valid := verifyPassword(password)
if !valid {
    attempt.failure()
    return unauthorized()
}
attempt.success()
```

预占、失败和成功都由同一个窗口状态机串行更新；`cancel` 只释放未完成尝试，且完成操作幂等。
