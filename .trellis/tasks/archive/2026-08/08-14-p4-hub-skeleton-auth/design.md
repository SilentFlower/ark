# 技术设计：P4-1 ark-hub 骨架与密码鉴权

## 1. 架构边界

```text
cmd/ark-hub/main.go
  -> internal/hub.Execute
       -> serve
            -> credential file (/var/lib/ark-hub/auth.json)
            -> in-memory sessions / CSRF / login limiter
            -> store.Open(/var/lib/ark/ark.db)
            -> net/http server
       -> admin init
       -> admin reset-password
       -> install
            -> internal/systemd hub service installer
```

- `cmd/ark-hub` 只调用 `hub.Execute()` 并把退出码交给 `os.Exit`。
- `internal/hub` 持有命令组装、HTTP 生命周期与鉴权，不依赖 backup、restore、verify 或 config。
- `store.Store` 仍是 `ark.db` 的唯一访问边界；P4-1 只验证可打开和并发持有，不新增列表 API。
- `ark-hub` 不包含调度器，不启动 backup/verify/restore。P4-2 再增加受控的 `ark` 子进程编排。

## 2. CLI 契约

```text
ark-hub serve
  [--listen 127.0.0.1:8080]
  [--state-db /var/lib/ark/ark.db]
  [--auth-file /var/lib/ark-hub/auth.json]
  [--secure-cookie]

ark-hub admin init
  [--username admin]
  [--auth-file /var/lib/ark-hub/auth.json]

ark-hub admin reset-password
  [--auth-file /var/lib/ark-hub/auth.json]

ark-hub install
  [--unit-dir /etc/systemd/system]
  [--listen 127.0.0.1:8080]
  [--state-db /var/lib/ark/ark.db]
  [--auth-file /var/lib/ark-hub/auth.json]
  [--secure-cookie]
```

- `serve` 在凭证文件不存在或非法时拒绝启动，错误只提示本机初始化命令和文件路径。
- `admin init` 使用排他创建；已有文件时拒绝覆盖。
- `admin reset-password` 只更新已存在的合法普通文件，并递增 revision。
- 密码必须从 TTY 无回显读取并二次确认；命令行、环境变量和错误中不接受或回显密码。
- 用户名默认 `admin`，允许显式设置，wire 规则固定为 1-64 个 ASCII 字母、数字、点、下划线或短横线。

## 3. 凭证文件

JSON schema v1：

```json
{
  "schema_version": 1,
  "username": "admin",
  "password_hash": "$argon2id$v=19$m=19456,t=2,p=1$...$...",
  "revision": 1
}
```

- password hash 使用 Argon2id、16-byte 随机 salt、32-byte key，并保存 PHC 参数。
- 密码允许 Unicode 和空格，不设字符种类规则；长度为 12-1024 bytes，不静默 trim 或截断。
- 读取时拒绝未知字段、未知 schema、非法 username、revision=0、非 Argon2id、越界参数和非法 base64。
- 文件目录为 `0700`，文件为 `0600`；符号链接、目录、设备文件与 group/other 权限全部拒绝。
- 初始化使用 `O_CREATE|O_EXCL`；重置使用同目录临时文件、`fsync`、原子 rename 和目录同步。
- 凭证文件不属于 `ark.db`，也不进入默认 hub 自备份；hub 重建后由管理员重新执行 init。

## 4. 会话与密码重置

- 登录成功生成 32-byte 随机 token 和独立 32-byte CSRF token。
- 浏览器 Cookie 保存 raw token 的 base64url；服务端 map 以 SHA-256(token) 为 key，避免 map 中保留 raw token。
- session 保存 username、credential revision、CSRF token、createdAt 和 expiresAt，绝对有效期 12 小时。
- 每个受保护请求重新读取轻量凭证文件并比较 revision；密码重置后旧 session 无需 IPC 即失效。
- logout 删除服务端 session 并写过期 Cookie；进程退出时全部内存 session 自然清空。
- session map 用 mutex 保护，创建/读取时顺便清理过期项；不新增后台清理 goroutine。

## 5. 登录、限流与 CSRF

- `GET /login` 生成登录前 CSRF token，写 HttpOnly、SameSite=Strict、Path=/ 的临时 Cookie，
  同一 token 只以 hidden field 出现在当前页面。
- `POST /login` 先校验 method、Content-Type、body 上限与 CSRF，再进入限流和 Argon2id 校验。
- 不存在的 username 使用固定 dummy PHC hash，错误统一为“用户名或密码错误”。
- 登录失败按 `RemoteAddr host + normalized username` 使用 5 分钟固定窗口，窗口内最多 5 次；
  超限返回 `429` 与 `Retry-After`，成功登录清除对应计数。
- 登录成功旋转预登录 CSRF Cookie，创建 session Cookie，并重定向 `/`。
- 登录后的 `POST /logout` 必须同时存在有效 session 与匹配的 session CSRF token。
- P4-1 不信任 `X-Forwarded-For`；反向代理场景的精确客户端限流留到部署信任模型明确后再做。

## 6. HTTP 路由与输出

| 路由 | 鉴权 | 行为 |
| --- | --- | --- |
| `GET /healthz` | 否 | 固定纯文本存活响应，不包含配置和版本详情 |
| `GET /login` | 否 | 最小服务端登录页 |
| `POST /login` | 否，需 CSRF | 密码登录 |
| `GET /` | 是 | 最小受保护壳，明确 P4 控制台尚在建设 |
| `POST /logout` | 是，需 CSRF | 撤销会话 |
| `GET /api/session` | 是 | 返回 `{authenticated:true, username}`，供 P4-3 复用 |

- 未认证的 `/api/` 请求返回 `401` JSON；浏览器页面请求重定向 `/login`。
- 所有 handler 设置 CSP、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、
  `X-Frame-Options: DENY` 和 `Cache-Control: no-store`。
- 不记录请求 body、Authorization、Cookie、密码、hash 或 token；本任务不引入日志框架。
- `http.Server` 固定设置 ReadHeaderTimeout、ReadTimeout、WriteTimeout、IdleTimeout 和 MaxHeaderBytes。

## 7. 服务生命周期

- `serve` 在 listen 前完成凭证加载和 `store.Open`；任一步失败都零监听副作用。
- `Run(ctx)` 显式创建 listener；context 取消后用 10 秒独立 context 调用 `Shutdown`。
- Serve、Shutdown 和 Store.Close 错误用 `errors.Join` 聚合；`http.ErrServerClosed` 只在正常 shutdown 时忽略。
- 默认只监听 `127.0.0.1:8080`。非 loopback 地址必须由操作者显式配置；TLS 终止由反向代理负责。
- `--secure-cookie` 只控制 Cookie Secure 标志，不依据可伪造的 forwarding header 自动猜测。

## 8. systemd

- 在 `internal/systemd` 增加 hub unit 的独立 options/build/install API，并复用现有同目录暂存、
  `systemd-analyze verify`、原子替换和回滚机制。
- `ark-hub.service` 使用 `Type=simple`、`Restart=on-failure`、`RestartSec=5`、`UMask=0077`、
  `NoNewPrivileges=true`、`PrivateTmp=true` 和 `WantedBy=multi-user.target`。
- unit 只包含 ark-hub 二进制路径与非秘密 flag，不嵌入密码、hash、session 或 Cookie。
- `ark-hub install` 只写 service，不 daemon-reload、不 enable、不 start，也不扫描或清理任何 timer。

## 9. 兼容与文档

- 新增 `golang.org/x/crypto v0.33.0` 和 `golang.org/x/term v0.29.0`，保持 Go 1.22 兼容。
- 不修改现有 `ark` 命令、`ark.db` schema、备份 timer、恢复或 verify 行为。
- `docs/roadmap.md`、`docs/design.md` 改为本地密码鉴权且无 TOTP。
- `docs/operations.md` 增加 auth 文件不备份、重建后运行 `ark-hub admin init` 的步骤。
- 修正 `cmd/ark/main.go` 已过期的“每台机器 agent”注释，使其符合当前 hub oneshot 架构。

## 10. 失败矩阵

| 条件 | 结果 |
| --- | --- |
| auth 文件不存在 | `serve` 拒绝监听，提示执行本机 init |
| auth 文件是 symlink、非普通文件或权限过宽 | fail closed，不读取目标内容 |
| PHC schema/参数损坏 | fail closed，不分配越界 Argon2 资源 |
| 重复 init | 不覆盖现有管理员 |
| reset 原子替换失败 | 保留旧凭证可登录，返回失败 |
| 密码错误或 username 不存在 | 同一错误文本，并执行一次 Argon2id |
| 连续失败超限 | `429`，不执行 Argon2id，窗口后自动恢复 |
| session 伪造、过期或 revision 漂移 | 撤销 Cookie，返回未认证 |
| CSRF 缺失或不匹配 | `403`，不执行状态变更 |
| store 打不开或 listener 绑定失败 | 进程非零退出，不报告服务已启动 |
| shutdown 与 close 同时失败 | 返回可识别的聚合错误链 |
