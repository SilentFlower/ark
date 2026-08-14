# 执行计划

## 1. 依赖与基础入口

- 添加 Go 1.22 兼容的 `golang.org/x/crypto v0.33.0` 与 `golang.org/x/term v0.29.0`。
- 创建薄入口 `cmd/ark-hub/main.go` 与 `internal/hub` package doc、根命令和统一退出语义。
- 先补命令构造的依赖注入点，保证密码终端读取、文件操作、listener 和 shutdown 可测试。

验证：

```bash
go test ./internal/hub -run 'TestCommand|TestExecute' -count=1
go build ./cmd/ark-hub
```

## 2. 凭证存储与 Argon2id

- 实现 schema v1 credential、username/password 校验、PHC 编解码和 constant-time verify。
- 实现 `admin init` 的排他创建，以及 `admin reset-password` 的原子替换和 revision 递增。
- 拒绝 symlink、非普通文件、权限过宽、未知字段/schema 与越界 Argon2 参数。
- CLI 密码无回显读取两次，不提供明文 password flag。

验证：

```bash
go test ./internal/hub -run 'TestCredential|TestPassword|TestAdmin' -race -count=1
```

回滚点：该步骤独立于 HTTP；失败时删除新增 `internal/hub` 文件和依赖即可，不修改 `ark.db`。

## 3. Session、CSRF 与限流

- 实现内存 session manager、token hash 索引、12 小时过期、logout 和 opportunistic cleanup。
- session 绑定 credential revision，密码重置后请求级校验使旧 session 失效。
- 实现登录前 double-submit CSRF、登录后 session CSRF 和固定窗口登录限流。
- 覆盖并发 session 创建/读取/撤销，确保 race 检查通过。

验证：

```bash
go test ./internal/hub -run 'TestSession|TestCSRF|TestLoginLimiter' -race -count=10
```

## 4. HTTP 服务

- 实现安全 header、body 上限、Cookie 策略和统一浏览器/API 未认证响应。
- 实现 `/healthz`、`/login`、`/logout`、`/` 与 `/api/session`。
- 用 `httptest` 覆盖完整登录、错误密码、限流、CSRF、伪造/过期 session、退出和密码重置。
- 实现 store 打开、listener、context shutdown 与错误聚合；不引入日志框架。

验证：

```bash
go test ./internal/hub -race -count=1
go test ./internal/hub -race -count=10
```

## 5. systemd service

- 在 `internal/systemd` 提取可复用的精确 unit 安装内核，不改变现有 backup/verify 输出集合。
- 增加 `BuildHubUnit` / `InstallHub` 及 `ark-hub install`，只管理 `ark-hub.service`。
- 覆盖非法路径、非受管同名文件、symlink、verify 失败、rename 失败回滚和真实
  `systemd-analyze verify`。

验证：

```bash
go test ./internal/systemd ./internal/hub -race -count=1
go test -v ./internal/systemd -run '^TestGeneratedHubUnit_SystemdAnalyzeVerify$' -count=1
```

回滚点：现有 `BuildUnits` 与 `Install` 的输出和测试必须保持不变；重构未收敛时恢复为独立 hub installer。

## 6. 文档同步

- 更新 `docs/roadmap.md`、`docs/design.md`，记录密码鉴权且完全移除 TOTP。
- 更新 `docs/operations.md`，记录 auth 文件排除、初始化和 hub 重建后的密码重置步骤。
- 修正 `cmd/ark/main.go` 的旧 agent 注释，不扩展业务行为。

## 7. 最终门禁

```bash
gofmt -w .
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

- 临时/验证构建产物在检查后清理，不提交 `bin/`。
- Check-All 必须反向核对没有 P4-2 hosts/runs/alerts 或操作 API，也没有 P4-3 Vue 前端内容。
