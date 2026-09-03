# Brief — 修复 Redis readiness 多行输出误判

## Goal

- 让 Ark 在 `redis-cli PING` 成功退出且合并输出包含警告与独立 `PONG` 响应时正确判定 Redis 已就绪，避免恢复和自动演练无限轮询。

## Scope

- 在 `internal/restore/execute.go` 增加共享的包内 Redis PING 输出判定函数。
- 同时修复首次 readiness 等待和断点续跑后置条件复核，避免两处语义漂移。
- 在 `internal/restore/execute_test.go` 增加纯函数、首次等待、一次性复核、command error 和 context 取消回归。
- 更新 restore 规范，明确合并 stdout/stderr 下的成功判定与脱敏边界。
- 完成定向 race、重复回归、全项目检查、静态构建，并在 hub 重新部署后执行 `biz` latest verify、生产基线和隔离资源清理验收。

## Non-Goals

- 不修改 Redis 密码、ACL、环境变量或 Compose 配置来消除警告。
- 不把 Redis readiness 改为 TCP、Docker healthcheck 或其它探测协议。
- 不修改全局 readiness/health 超时策略、轮询间隔或调用方 context 契约。
- 不修改 `sshexec.Runner`、公开 Go API、CLI 参数、清单、manifest/schema、external network 或 cleanup 逻辑。

## Key Decisions

- 命令退出码必须为 0，并且合并输出中至少存在一行去除空白后严格等于 `PONG`；不接受子串、大小写变体、`+PONG` 或附加内容。
- 使用“任一独立行”而非“最后一行”，因为 Runner 合并 stdout/stderr 后不保证警告与响应的最终排列顺序。
- 两个调用点复用同一非导出 helper；command error 优先，输出不能覆盖非零退出。
- 完整 Redis 输出不进入 Result、CLI、JSON 或状态库，保持现有错误链和脱敏行为。
- 本任务只修复已证实的解析缺陷；全局超时策略作为独立议题延后，避免扩大恢复行为变更。

## Key Context

- `internal/restore/execute.go:1223` 当前在循环等待中使用 `strings.TrimSpace(out) == "PONG"`。
- `internal/restore/execute.go:1523` 当前在 marker 后置条件复核中重复同一整体精确匹配。
- `internal/sshexec/client.go:25` 定义 Runner；`Run` 返回 stdout/stderr 合并输出，非零退出时仍返回已产生内容。
- hub 真机输出为成功退出、AUTH 警告行和独立 `PONG` 行；前序 external network 与 `local` named volume 修复已越过原阻断点，生产二进制已回滚。
- PostgreSQL readiness 继续只按 `pg_isready` 退出码判断；verify 继续通过 `restore.Execute` 继承行为，不复制解析逻辑。

## Risks / Deferred

- 合并输出的流间顺序不稳定，因此测试必须覆盖警告在 `PONG` 前后两种情况。
- 真实 Redis 永不就绪时仍由调用方 context 控制退出；本任务不引入新的超时配置或默认值。
- hub 部署属于生产变更，必须创建即时备份、记录 SHA-256，并在失败时恢复备份；只清理可由 isolation ID 证明归属的派生资源。

## Acceptance

- 表驱动测试覆盖纯 `PONG`、空白、LF/CRLF、警告顺序，以及子串、大小写、`+PONG`、附加内容和缺失响应的拒绝。
- 首次等待和断点续跑复核都接受生产形态多行输出；命令返回错误时即使包含 `PONG` 也不成功。
- context 取消错误链保持可识别，Redis 合并输出和凭证不会进入用户可见或持久化结果。
- `go test ./internal/restore -race -count=1`、十次重复回归、verify/CLI 回归、`make check`、`go mod verify`、`git diff --check` 和静态构建全部通过。
- hub 部署后的 validate、doctor、dnsmgr AuthApi 与 `biz` latest verify 完整通过；生产 `api_shared` 和 project/container/network/volume/files 基线不变，结束后无 isolation 资源或目录残留，verify timer 正常。

## Next Step

- 确认本 Brief 后启动任务，先增加 Redis PING 多行输出与两个 readiness 调用点的回归测试。
