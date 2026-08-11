# 执行计划

## 步骤

1. 研究并验证 modernc v1.36.1 可用的 SQLite 在线备份机制，记录选择与失败语义。
2. 在 store 增加一致性导出 API、权限/清理逻辑和并发 WAL 测试。
3. 在 backup 流程精确识别 hub 状态库 target，并接入在线导出而非普通 tar。
4. 更新 examples/ark.yaml 的 hub targets 与密钥排除注释。
5. 增加 doctor 对象锁人工确认 warn 和对应 CLI/JSON 测试。
6. 补充 hub 自备份、离线密钥、对象锁与恢复材料文档。

## 验证

```bash
go test ./internal/store ./internal/backup ./internal/doctor ./internal/cli -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- WAL 数据库不能只复制主文件，导出副本必须用独立连接验证。
- 凭证路径排除必须具体，不能用容易误收密钥的目录级 target。
- 对象锁未自动验证时只能 warn，不能给出虚假的安全保证。

## 实现记录

- 采用 modernc v1.36.1 的 `NewBackup` / `Step` / `Finish` Online Backup API。
- 在同一 SQLite 连接上先建立 WAL 读快照，再按页分批导出；持续 writer 不阻塞且导出可收敛。
- 导出文件归一化为 DELETE journal 单文件，并执行 `integrity_check` 与
  `foreign_key_check` 后才交给 restic。
- 仅本地 host 上清理后的绝对路径精确匹配状态库时走导出；混合路径在获取锁前拒绝。

## 验证记录

- `make check`、`make build`、无 CGO 构建、`go mod verify`、`git diff --check` 通过。
- `go test ./internal/store -race -count=10` 通过，覆盖并发 WAL writer、独立副本和临时清理。
