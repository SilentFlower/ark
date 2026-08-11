# 执行计划

## 步骤

1. 核对 restic JSON 输出字段和初始化错误语义，添加最小内部模型与 fixtures。
2. 实现受限 env 文件复用/抽取、环境覆盖和脱敏命令执行器。
3. 实现 New、EnsureInit、BackupStdin、Snapshots。
4. 实现 Forget、ForgetSnapshot、Dump、Check 和流生命周期清理。
5. 补齐单元测试与本地 repo 集成测试。

## 验证

```bash
go test ./internal/restic -race -count=1
go test ./internal/restic -run Integration -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

当前环境没有 `restic`；集成测试预计 skip，必须在 `p2-live-validation` 前补跑。

## 高风险点

- repo env 与 password 任何内容都不能进入 error、stdout/stderr 或其它子进程。
- Dump/BackupStdin 的 EOF、Close、Wait 顺序不能丢失非零退出状态。
- EnsureInit 不能把权限、网络或损坏错误误判为“需要 init”。
