# Redis readiness 多行输出修复实施计划

## 1. 锁定回归测试

- [x] 在 `internal/restore/execute_test.go` 增加 `redisPingReady` 表驱动测试，覆盖成功多行、CRLF、警告顺序与严格拒绝场景。
- [x] 增加 `waitDatabaseReady` 回归，使用生产形态的警告加独立 `PONG` 输出，断言首次调用即成功。
- [x] 增加 `waitDatabaseReadyOnce` 回归，证明断点续跑后置条件使用相同语义。
- [x] 覆盖 command error 与 `PONG` 同时存在时仍失败，并保留 context 取消错误链。

定向验证：

```bash
go test ./internal/restore -run 'TestRedisPingReady|TestWaitDatabaseReady' -race -count=1
```

## 2. 实现共享判定

- [x] 在 `internal/restore/execute.go` 增加包内非导出纯函数，按独立整行严格识别 `PONG`。
- [x] 让 `waitDatabaseReady` 在 Redis 分支同时要求命令成功和 helper 成功。
- [x] 让 `waitDatabaseReadyOnce` 保留 command error 优先级，并复用同一 helper。
- [x] 添加中文 Why 注释，解释 Runner 合并 stdout/stderr 且顺序不稳定，不能继续整体匹配或只取最后一行。
- [x] 不修改 PostgreSQL、轮询间隔、context、Result、CLI 或 Runner 接口。

## 3. 同步规范

- [x] 更新 `.trellis/spec/backend/restore-plan-guidelines.md` 的数据库 readiness 契约与错误矩阵。
- [x] 明确命令非零退出不能被 `PONG` 输出覆盖，完整 Redis 输出不得进入 Result/CLI。

## 4. 本地质量门

- [x] 对变更 Go 文件执行 `gofmt`。
- [x] 运行 restore 定向 race 与十次重复回归。
- [x] 运行 verify/CLI 相关包回归，确认跨层行为无变化。
- [x] 运行 `make check`、`go mod verify` 和 `git diff --check`。
- [x] 使用 `CGO_ENABLED=0` 构建部署二进制并确认静态链接。

```bash
gofmt -w internal/restore/execute.go internal/restore/execute_test.go
go test ./internal/restore -race -count=1
go test ./internal/restore -race -count=10
go test ./internal/verify ./internal/cli -race -count=1
make check
go mod verify
git diff --check
CGO_ENABLED=0 go build -o bin/ark-nocgo ./cmd/ark
file bin/ark-nocgo
ldd bin/ark-nocgo
```

## 5. hub 部署与真机验收

- [ ] 记录本地 commit、二进制 SHA-256 和 `biz` 生产基线，在 hub 为当前 `/usr/local/bin/ark` 创建新的带时间戳即时备份。
- [ ] 原子部署静态二进制，运行 validate、完整 doctor 与 dnsmgr AuthApi 检查。
- [ ] 执行 `ark verify --host biz --snapshot latest --json` 并保存结构化验收证据。
- [ ] 核对 Redis readiness 不再因警告加 `PONG` 持续轮询。
- [ ] 核对演练容器未连接 `api_shared`，生产 project/container/network/volume/files 基线前后一致。
- [ ] 核对无 isolation 容器、network、volume 或 restore root 残留，verify service/timer 状态正常。

## 回滚

1. 停止新的手工 verify，不删除无法证明归属的资源。
2. 恢复本次部署前即时备份并校验 SHA-256。
3. 重新执行 validate、doctor 与 timer 核对。
4. 只使用结构化 cleanup command 或匹配 isolation ID 的 `ark restore cleanup` 清理派生资源。
