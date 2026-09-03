# 修复 Redis readiness 多行输出误判

## Goal

让 Ark 在 `redis-cli PING` 成功退出且合并输出同时包含警告与独立 `PONG` 响应时，正确判定 Redis 已就绪，避免恢复和自动演练无限轮询。

同时保证失败退出、非精确响应和敏感命令输出仍不会被误判为成功或进入结构化结果。

## Background

- `sshexec.Runner.Run` 明确返回 stdout 与 stderr 的合并输出；非零退出时仍返回已经产生的内容。
- `internal/restore/execute.go:1223` 的首次 readiness 等待和 `internal/restore/execute.go:1523` 的断点续跑复核都要求整个输出 `TrimSpace` 后严格等于 `PONG`。
- hub 真机验收中，`redis-cli PING` 退出码为 0，合并输出先出现 Redis AUTH 警告，随后出现独立 `PONG`；当前实现因此持续轮询，直到人工取消。
- 该问题与 external network、named volume 转换无关；前序修复已经越过这些阶段，生产二进制和隔离资源均已安全回滚、清理。

## Requirements

- Redis readiness 只在命令退出码为 0，且合并输出中至少存在一行去除首尾空白后严格等于 `PONG` 时成功。
- 支持纯 `PONG`、前后空行、LF/CRLF，以及警告出现在 `PONG` 前后；不能依赖 stdout/stderr 的合并顺序。
- 不接受 `PONG` 子串、大小写变体、`+PONG`、`PONG` 后附加内容或只有警告没有独立响应的输出。
- 首次等待 `waitDatabaseReady` 与断点续跑复核 `waitDatabaseReadyOnce` 必须复用同一非导出判定函数，避免两处语义再次漂移。
- 即使输出包含独立 `PONG`，命令返回非 nil error 时仍必须失败或继续等待；不得用输出覆盖退出状态。
- 保持现有 context 取消、轮询间隔、错误链、步骤状态和脱敏输出语义；不得把 Redis 警告、环境变量、密码或完整命令输出写入 `Result`、CLI 或状态库。
- 不修改公开 Go API、CLI 参数、清单字段、manifest/schema、`sshexec.Runner` 接口或 Redis 认证配置。
- 更新 restore 规范，明确 `Runner.Run` 合并输出下的 Redis readiness 判定契约。
- 完成本地定向测试、重复 race、全项目检查和静态构建后，重新部署 hub 并执行 `biz` latest verify。

## Acceptance Criteria

- [ ] 表驱动测试覆盖纯 `PONG`、警告加 `PONG`、CRLF、空行、警告顺序变化，以及子串、大小写、`+PONG`、附加内容和缺失响应的拒绝场景。
- [ ] `waitDatabaseReady` 对生产形态的多行成功输出立即返回，不进入下一次轮询。
- [ ] `waitDatabaseReadyOnce` 对相同输出成功，确保同 Plan 断点续跑不会误判 marker 后置条件失效。
- [ ] 命令返回错误时，即使输出中存在独立 `PONG` 也不判定成功；context 取消仍可由 `errors.Is` 识别。
- [ ] 人类输出、JSON、步骤 Detail 和错误摘要均不包含 Redis 合并输出或凭证内容。
- [ ] `go test ./internal/restore -race -count=1` 与至少十次重复回归通过。
- [ ] `make check`、`go mod verify`、`git diff --check` 和 `CGO_ENABLED=0` 静态构建通过。
- [ ] hub 部署后 `ark validate`、doctor 与 dnsmgr AuthApi 检查通过。
- [ ] `ark verify --host biz --snapshot latest --json` 完整通过；生产 `api_shared` 与 project/container/network/volume/files 基线不变，结束后无 isolation 资源或目录残留，verify timer 状态正常。

## Out of Scope

- 修改 Redis 服务的密码、ACL、环境变量或 Compose 配置来消除警告。
- 把 Redis readiness 改为 TCP 探测、Docker healthcheck 或其它协议。
- 修改所有恢复/健康轮询的全局超时策略；本任务继续由调用方 context 控制取消与超时。
- 修改 `sshexec.Runner.Run` 的合并输出契约，或把本次短命令改为流式执行。
- 重做 external network、named volume、cleanup 或生产基线逻辑。
