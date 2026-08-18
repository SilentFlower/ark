# Hub Guidelines

> `ark-hub` 的本地鉴权、HTTP API、会话、限流、状态投影、主动告警、手工操作、内嵌控制台与常驻 service 契约。

---

## Scenario: 修改 ark-hub 鉴权、HTTP API、内嵌控制台或服务骨架

### 1. Scope / Trigger

以下改动必须遵守本规约：

- 修改 `cmd/ark-hub/`、`internal/hub/` 的 CLI、凭证文件、密码哈希、会话、CSRF 或 HTTP 路由；
- 修改 `ark-hub` 对 `store.Store` 的打开、关闭或请求期读取方式；
- 修改 hosts、runs、alerts、operations 投影，或 backup、verify、restore 手工操作；
- 修改主动告警评估、钉钉投递、静默/恢复状态或告警 manager 生命周期；
- 修改登录限流、Cookie、安全响应头、CSP、缓存策略、HTTP server 超时或优雅停止；
- 修改 `internal/hub/webui/` 的内嵌产物、静态资源服务或 SPA fallback；
- 修改 `ark-hub install` 或 `ark-hub.service` 的生成、校验和安装行为。

`cmd/ark-hub` 必须保持薄入口。鉴权和 HTTP 逻辑属于 `internal/hub`；SQLite 细节属于
`internal/store`；通用 unit 原子安装内核属于 `internal/systemd`；内嵌前端产物属于
`internal/hub/webui`。`ark-hub` 不承载调度，长任务只能启动显式路径的 `ark --json` 子进程，
不能在 handler 内直接实现备份、演练或恢复，也不能导入 `internal/backup`、
`internal/verify`、`internal/restore` 等业务包。

### 2. Signatures

CLI 契约：

```text
ark-hub serve [--listen <address>] [--state-db <absolute-path>]
              [--auth-file <absolute-path>] [--config <absolute-path>]
              [--ark-binary <absolute-path>] [--secure-cookie]
ark-hub admin init [--username <name>] [--auth-file <absolute-path>]
ark-hub admin reset-password [--auth-file <absolute-path>]
ark-hub install [--unit-dir <absolute-path>] [--listen <address>]
                [--state-db <absolute-path>] [--auth-file <absolute-path>]
                [--config <absolute-path>] [--ark-binary <absolute-path>]
                [--secure-cookie]
```

Go 入口：

```go
type ServeOptions struct {
    ListenAddress string
    StateDBPath   string
    AuthFile      string
    ConfigPath    string
    ArkBinaryPath string
    SecureCookie  bool
}

func Run(ctx context.Context, options ServeOptions) error
func BuildHubUnit(options systemd.HubInstallOptions) (systemd.Unit, error)
func InstallHub(ctx context.Context, options systemd.HubInstallOptions) (systemd.InstallResult, error)

func monitoring.SendDingTalk(
    ctx context.Context,
    settings monitoring.DingTalkSettings,
    message monitoring.MarkdownMessage,
) error
func (s *store.Store) ListAlertStates(ctx context.Context) ([]store.AlertState, error)
func (s *store.Store) SaveAlertState(ctx context.Context, state store.AlertState) error
```

backup operation 的 JSON 结果必须包含：

```json
{
  "run_id": "run-id",
  "status": "ok|warn|fail",
  "heartbeat_status": "disabled|sent|failed"
}
```

`heartbeat_status` 是 `operationResultFields(backup)` 的必需白名单字段；未知值或缺失都拒绝持久化。

HTTP 路由：

```text
GET  /healthz      public, text/plain
GET  /login        public login page
POST /login        public + login CSRF
GET  /             authenticated, 内嵌控制台入口 HTML
POST /logout       authenticated + session CSRF
GET  /api/session  authenticated JSON
GET  /api/hosts
GET  /api/hosts/{host}
GET  /api/runs
GET  /api/alerts
GET  /api/operations
GET  /api/operations/{id}
POST /api/hosts/{host}/backup
POST /api/hosts/{host}/verify
POST /api/hosts/{host}/restore
其它路径           authenticated；/api/ 前缀返回 404 JSON，其余交给 webui
                   （命中嵌入文件则返回该文件，否则返回入口 HTML）
```

内嵌控制台入口：

```go
func webui.Assets() (fs.FS, error)      // 以 dist/ 为根的只读产物
func webui.IndexHTML() ([]byte, error)  // 真实产物缺失时返回占位页
func webui.Built() bool                 // 是否由 make hub 构建
func webui.NewHandler() (*webui.Handler, error)
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
- 除 `/healthz`、`GET /login`、`POST /login` 外全部路由默认鉴权，**内嵌控制台的入口 HTML 与
  静态资源同样必须先过鉴权**。未认证 `/api/` 返回稳定 `401` JSON，页面请求重定向登录；
  运行期 auth 存储错误返回 `503`，不能降级成未认证或放行。SPA fallback 只能发生在鉴权之后，
  未认证请求任何路径都不得拿到入口 HTML。
- 未显式注册的路径按前缀分流：`/api/` 返回 `404` JSON（客户端要拿到 JSON 而不是 HTML），
  其余交给 webui。webui 命中嵌入文件就返回该文件，否则返回入口 HTML——这是 history 模式
  前端路由的前提。请求路径必须经 `fs.ValidPath` 校验，穿越尝试与普通未命中返回同一结果。
- 安全响应头对所有响应生效：CSP、`nosniff`、`no-referrer`、`DENY`。Cookie 固定
  `HttpOnly`、`SameSite=Strict`、`Path=/`；`Secure` 只由显式 flag 控制，不信任 forwarding header。
- CSP 固定为 `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self';
  base-uri 'none'; frame-ancestors 'none'`，并由测试精确断言整串。
  **脚本来源只能是 `'self'`**：不得引入 CDN、`'unsafe-inline'`、`'unsafe-eval'` 或 data: 脚本。
  `style-src` 的 `'unsafe-inline'` 是为 Vue 的 `:style` 绑定与服务端渲染登录页保留的既定取舍，
  不是遗漏；收窄它需要同时改造前端与登录页模板。
- 默认 `Cache-Control: no-store`。只有带内容 hash 的静态资源可以由 webui 覆盖为
  `public, max-age=31536000, immutable`；入口 HTML 和全部 API 必须保持 `no-store`，
  否则升级 Hub 后浏览器会继续加载旧版本引用的 JS 文件名。
- 登录与退出保持服务端表单路径（form-urlencoded + 登录 CSRF Cookie + 限流 + 重定向），
  不为前端新增 JSON 登录端点。前端收到 `401` 直接跳 `/login`。
- **渲染登录页时，已有合法形态的登录 CSRF Cookie 必须复用其值，并且每次渲染都要重新
  下发 Cookie 以续期。** 两条缺一不可，缺任何一条真实浏览器都登不进来：轮换值会被
  浏览器对子资源（如 `/favicon.ico`，它同样被重定向到登录页并触发渲染）的请求覆盖，
  用户提交时 Cookie 与表单 token 失配；只复用值不续期，则用户拿到一个即将到期的
  Cookie，页面停留片刻后提交时 Cookie 已被浏览器丢弃，请求根本不带 Cookie。
  两种情况都稳定失败在 `403`，且 httptest 都测不出来——必须有专门的回归测试。
  复用值不降低强度：CSRF 依赖攻击者读不到 HttpOnly Cookie，而非频繁轮换。
  Cookie 值不符合本服务签发的 token 形态（长度与字符集）时必须替换，不得写进表单。
- 前端产物不进版本库，`internal/hub/webui/dist/` 只提交 `PLACEHOLDER`（它让目录非空，
  是 `go:embed dist` 能编译的唯一保证；Vite 因此不能直接输出到该目录，`emptyOutDir`
  会把它删掉）。真实产物缺失时 webui 回落到包内 `placeholder.html` 并提示改用
  `make hub`，**不得因此拒绝启动**：前端缺失不影响 API、登录与备份调度。
  发布 `ark-hub` 必须走 `make hub`。
- 新业务 POST 必须在 session 鉴权后 constant-time 校验 `X-CSRF-Token`。JSON body 上限
  16 KiB，严格拒绝未知字段和尾随值；未知 host、非法 filter 和不支持的恢复 mode 不得产生 operation。
- `Run` 只能通过 `store.Open` 获得 `Store` 并调用其公开 API，不持有 `*sql.DB` 或拼 SQL。
  凭证、清单、Ark 二进制和状态库校验必须发生在 listen 前；清单和 Ark 二进制必须是绝对路径，
  二进制必须是可执行普通文件。业务 API 每次重新严格加载清单，运行期损坏时 fail closed 为 `503`。
- hosts 与 alerts 必须复用同一个健康投影。schedule 无法分析时健康为 `unknown`，返回
  `schedule_unavailable`，只展示状态库中的最后已知 `next_run_at`，不得伪造超时告警。
- 主动告警 manager 监听成功后立即评估一次，之后每分钟串行评估。它必须直接复用
  `application.projectHosts` 返回的 `alertResponse` 集合，不在通知层复制 overdue、连续失败或演练失败判定。
- 每轮重新严格加载清单和 `monitoring.env_file`。启动前已配置但秘密文件非法时拒绝 listen；
  运行期配置损坏或单轮投递失败只报告脱敏错误并跳过/重试该轮，不能终止 HTTP 服务。
- 告警生命周期固定为：首次立即、持续故障距最近成功投递满 24 小时重发、恢复通知一次、恢复后复发立即。
  只有成功投递才更新 `last_alert_sent_at` / `recovery_sent_at`；发送失败不能进入静默期。
- 当前投影消失时先保存 inactive + `resolved_at`。只有历史告警曾成功发送且 host 仍在清单中才发恢复；
  host 被删除只关闭状态，不把“停止监控”误报成恢复。
- 同轮到期活动告警和恢复事件按稳定 ID 排序，合并成一条 Markdown。先保存观察状态，再发送，成功后保存投递时间。
  这是 at-least-once：发送成功后、状态提交前崩溃可能重复一条，但不能永久漏报。
- 主机摘要的 `last_backup_bytes` 与 `recent_backup_sizes` 只能复用投影已取的 `ListHostRuns`
  结果，不得为趋势另开查询。采样只统计 `ok` 与 `warn` 的 run（失败 run 的字节数是半截数据，
  混进趋势会掩盖 ADR-011 要盯的体积腰斩信号），每点是该 run 全部 target 字节之和，
  按时间正序最多 14 点；没有成功记录时 `last_backup_bytes` 为 `null`、
  `recent_backup_sizes` 为空数组而不是 `null`。
- 普通 host DTO 不得返回 compose、env、SSH、repo 或凭证路径。doctor 报告必须先解码为白名单
  checks DTO，再精确替换清单中的敏感路径；禁止把状态库原始 JSON 直接透传给 API。
- 手工操作先持久化 `running`，再同步调用 `cmd.Start`；只有进程真正启动后才能返回 `202`。
  启动失败必须把 operation 完成为 `fail` 并返回 `500 operation_failed`。客户端断开不取消已启动任务。
- 同一 Hub 进程最多一个活动手工操作。子进程使用 application context、无 shell argv、
  `Pdeathsig=SIGTERM`、4 MiB stdout 与 64 KiB stderr 上限；非零退出、JSON 损坏或输出超限均持久化 fail，
  stderr 不入库也不回显。
- restore 预检 confirmation token 只保存在内存，绑定创建预检的 session、source host、精确 manifest
  snapshot 和 SHA-256 digest，10 分钟有效且只能展示、消费一次。真实恢复必须使用预检中的精确参数。
- context 取消后先有界 `Shutdown` HTTP，再取消并等待活动 operation，最后关闭 Store；使用
  `errors.Join` 聚合 Serve、Shutdown、server Close、operation 和 Store.Close 错误。
- 告警 manager 是 Serve 生命周期的一部分：HTTP shutdown 后必须先取消并等待告警循环，
  再关闭 operation manager 和 Store，禁止遗留访问已关闭 Store 的 goroutine。
- `ark-hub.service` 只包含二进制路径和非秘密启动 flag，使用 `Type=simple`、`UMask=0077`、
  `Restart=on-failure`。安装只管理 `ark-hub.service`，不创建、扫描、删除、enable 或启动 timer。
- 密码、hash、session token、CSRF token、完整 Cookie 和请求 body 不得进入日志、错误、普通响应
  或状态库。本阶段不为 hub 引入日志框架。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| 未认证请求任意页面路径（含 `/`、前端路由、静态资源） | 重定向登录，响应体不含入口 HTML |
| 未认证请求 `/api/` 未知路径 | `401` JSON，不落入 SPA fallback |
| 已认证请求 `/api/` 未知路径 | `404` JSON，绝不返回 HTML |
| 已认证请求未知非 API 路径 | 返回入口 HTML，`Cache-Control: no-store` |
| 请求路径含 `..` 或非法元素 | 与普通未命中同样回落入口 HTML，不读取产物之外的文件 |
| `internal/hub/webui/dist` 缺少 index.html | 使用占位页并提示 `make hub`，服务照常启动 |
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
| 已有未过期的登录 CSRF Cookie | 复用其值并刷新有效期，不轮换 token |
| 登录 CSRF Cookie 形态非法（长度/字符集不符） | 替换为新签发的 token，非法值不得进入表单 |
| CSRF 缺失或不匹配 | 返回 `403`，不执行登录或退出状态变更 |
| session 伪造、过期或 revision 漂移 | 撤销 Cookie，API 返回 `401` |
| 已有 session 期间 auth 文件运行期损坏 | API 返回 `503` JSON，不伪装成普通未登录 |
| 清单或 Ark 二进制在启动前非法 | 不创建 listener，返回带上下文错误 |
| 运行期清单损坏 | 业务 API 返回 `503`，登录和 `/healthz` 保持可用 |
| schedule 分析失败 | health=`unknown`、diagnostics 含 `schedule_unavailable`，不生成 `backup_overdue` |
| 首次出现稳定告警 ID | 保存 active 状态并立即加入本轮 Markdown |
| 活动告警距最近成功投递不足 24 小时 | 保持 active，不重复发送 |
| 活动告警距最近成功投递达到 24 小时 | 本轮重发，成功后刷新静默时间 |
| 钉钉投递失败 | 不写成功投递时间，下一评估周期继续尝试；HTTP 服务保持可用 |
| 已发送告警恢复且 host 仍存在 | 保存 resolved 并发送一次恢复；成功后写 `recovery_sent_at` |
| host 从清单删除 | 状态转 inactive，不发送恢复消息 |
| backup operation 缺少或包含非法 `heartbeat_status` | 拒绝 Ark JSON，不持久化伪造结果 |
| POST 缺 CSRF、body 超限、未知字段或非法 host | 返回 `403`/`400`/`404`，不创建 operation |
| 已有活动手工操作 | 返回 `409 conflict`，不启动第二个 Ark 子进程 |
| Ark 子进程启动失败 | operation 持久化为 fail，HTTP 返回 `500 operation_failed` |
| Ark 非零退出、JSON 损坏或输出超限 | operation 持久化为 fail，不回显 stderr |
| restore token 过期、session/source 不匹配或已消费 | 返回 `409` 或 `422`，不启动恢复 |
| Hub graceful shutdown 时存在活动 operation | 取消并等待子进程，持久化 interrupted 后关闭 Store |
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
- **Good**：backup POST 在 Ark 成功启动后返回 operation ID；刷新页面可从状态库轮询终态，
  请求连接提前断开也不影响已启动任务。
- **Good**：restore preview 只读探测目标资源，同一 session 第一次读取完成结果时获得一次性 token；
  URL source host 或目标冲突变化后旧 token 不能启动恢复。
- **Base**：schedule 暂时不可用时继续返回最后已知 next run，但健康明确为 unknown。
- **Good**：同一轮 3 个 host 的活动/恢复事件按稳定 ID 合并成一条钉钉 Markdown，成功后统一刷新对应状态。
- **Base**：未配置钉钉时 manager 不发送网络请求；API 的 `/api/alerts` 仍照常返回实时投影。
- **Bad**：发送失败也写 `last_alert_sent_at`，会把从未送达的告警静默 24 小时。
- **Bad**：host 从清单移除时发送“恢复”，会把停止监控误报成系统恢复健康。
- **Good**：未登录访问 `/hosts/web-01` 得到登录重定向且响应体不含入口 HTML；登录后同一路径
  返回入口 HTML 交给前端路由，而 `/api/typo` 仍是 `404` JSON。
- **Base**：用 `go build` 而不是 `make hub` 编译出的 `ark-hub` 照常启动，页面是占位提示，
  API、登录与备份调度不受影响。
- **Bad**：先检查失败次数、Argon2 完成后才递增计数。并发请求会全部穿过检查，限流形同虚设。
- **Bad**：把 SPA fallback 放在鉴权之前，或让 catch-all 无条件返回 index.html。
  未登录的人会直接拿到控制台入口和它引用的全部资源路径。
- **Bad**：给入口 HTML 也加上 `immutable` 缓存。升级 Hub 之后浏览器仍加载旧文件名的 JS，
  页面白屏且没有任何错误提示。
- **Bad**：为了「让前端好写」给 CSP 的 `script-src` 加 `'unsafe-inline'` 或 CDN 来源。
  这台 Hub 持有全部生产机的 SSH 私钥和 restic 仓库密码。
- **Bad**：每次渲染登录页都重新签发 CSRF Cookie。浏览器请求 `/favicon.ico` 时会再渲染
  一次并覆盖 Cookie，用户点提交必然 `403`——而所有 httptest 都是绿的。
- **Bad**：用 auth 文件本身做 flock。reset rename 后锁绑定旧 inode，另一个进程可锁住新文件并
  与前一个进程同时提交，导致 revision 丢失。
- **Bad**：初始化 `fsync` 失败后静默忽略 `Remove` 错误。命令报告失败，但不完整凭证仍留在磁盘。
- **Bad**：先返回 `202` 再异步调用 `cmd.Start`。二进制缺失时客户端会误以为任务已受理，
  数据库还可能长期留下状态不明的 running operation。
- **Bad**：直接把 `doctor_reports.report_json` 或 `config.Host` 编码进响应，会泄露 SSH 和宿主机路径。

### 6. Tests Required

修改 hub 鉴权或服务骨架至少运行：

```bash
go test ./internal/hub ./internal/hub/webui ./internal/systemd -race -count=10
make check
make build
make hub
go build -trimpath -o bin/ark-hub ./cmd/ark-hub
CGO_ENABLED=0 go build -trimpath -o bin/ark-nocgo ./cmd/ark
CGO_ENABLED=0 go build -trimpath -o bin/ark-hub-nocgo ./cmd/ark-hub
go mod verify
go test -v ./internal/systemd -run 'SystemdAnalyzeVerify' -count=1
git diff --check
```

改动 `web/` 时还要跑 `make web-check`（lint + 类型检查 + 单测），
并确认没有前端产物时 `go build ./...` 仍能编译。

测试必须覆盖：

- auth owner/mode/symlink/schema/hash 边界，重复 init、并发 reset、rename 失败保留旧凭证；
- 初始化同步与清理同时失败的错误聚合；密码终端读取器注入和 reset 命令 revision 递增；
- session 并发创建/读取/撤销，伪造、过期、退出和密码重置失效；
- 登录并发预占最多 5 个、4096 key 容量上限、窗口清理、成功清零和 `Retry-After`；
- 健康检查、受保护页面、CSRF、body 上限、安全 Cookie/header、运行期 auth 故障 `503`；
- store/listener/Serve/Shutdown/server Close/Store.Close 故障注入及 `errors.Is` 断言；
- hub unit 非法参数、非受管文件、symlink、verify/rename 失败、真实 systemd verify 和 timer 隔离。
- hosts/runs/alerts/operations 的空值、分页、未知筛选、schedule failure、稳定告警 kind 和路径脱敏；
- 大小趋势的无成功 run、只有失败 run、warn 计入、多 target 求和、超过 14 次的截断与正序；
- 未认证访问页面与静态资源不泄露入口 HTML；已认证时前端路径回落入口 HTML 而 `/api/` 仍 404 JSON；
- 登录 CSRF Cookie 在再次渲染时复用同值并续期，非法形态被替换，用首次 token 提交能登录成功；
- CSP 整串精确断言、入口 HTML 的 `no-store`、静态资源的 `immutable` 与 Content-Type、路径穿越；
- 缺少真实前端产物时回落占位页且服务可用；
- POST 的鉴权、CSRF、body 上限、未知字段、argv、进程启动失败、非零退出、JSON/输出上限和客户端断开；
- confirmation 的 session/source 绑定、过期、单次消费、精确 snapshot/digest，以及 shutdown interrupted。
- 告警首次、24 小时边界、恢复一次、复发立即、发送失败不静默、重启恢复状态和 host 删除；
- API 与钉钉使用完全一致的三个稳定 kind，schedule failure 不伪造 overdue；
- 同轮批量消息确定性排序、并发 evaluate 不重复、manager 取消等待和 Store 关闭顺序；
- backup operation 对 `heartbeat_status=disabled|sent|failed` 的白名单、必填和非法值拒绝。

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

```go
// 错误：HTTP 已经返回 202，实际进程可能根本没有启动。
go command.Run()
writeAccepted(operation.ID)

// 正确：running 已持久化且 Start 成功后才向客户端确认受理。
if err := command.Start(); err != nil {
    finishOperationAsFailed(operation.ID)
    return writeOperationFailed()
}
writeAccepted(operation.ID)
go waitAndPersist(command)
```

```go
// 错误：通知层重新实现健康规则，API 与钉钉迟早漂移。
alerts := deriveAlertsAgain(cfg, runs, verifications)

// 正确：告警 manager 复用 HTTP API 同一投影的 alertResponse 集合。
_, alerts, err := application.projectHosts(ctx, cfg)
```
