# 执行计划

## 步骤

1. 在 `internal/config` 增加主机密钥策略类型、默认访问器、静态校验和配置测试。
2. 修改 `internal/sshexec`，由统一生效策略构造 `StrictHostKeyChecking` 参数并补齐测试。
3. 新建 `internal/hostkey`，实现无 shell 的扫描、SHA256 指纹、预览和原子刷新。
4. 在 `internal/cli` 增加 `ark host-key refresh --host <name> [--apply]` 及依赖注入测试。
5. 调整 `internal/doctor` 对 strict 和 accept-new 的 known_hosts 检查语义。
6. 更新 `examples/ark.yaml`、`docs/design.md`、`docs/operations.md`、`docs/roadmap.md` 和相关 backend specs。
7. 运行全量检查，并把 P2 真实环境验收前置依赖更新为本任务完成。

## 验证

```bash
go test ./internal/config ./internal/sshexec ./internal/hostkey ./internal/doctor ./internal/cli -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- 默认值从严格变为 accept-new 是有意的兼容行为变化，文档、示例和测试必须显式记录。
- `ssh-keyscan` 不能证明服务器身份；不得把扫描成功写成安全验证成功。
- known_hosts 更新必须在同目录原子替换，拒绝符号链接，不能在失败时截断原文件；rename 后目录 sync 失败时恢复更新前状态并再次同步目录。
- refresh 命令不得被 backup、doctor 或 systemd 自动调用，变化密钥仍需人工信任。

## 完成前检查

- 所有层只从 `internal/config` 获取生效策略。
- 全仓不存在生成 `StrictHostKeyChecking=no` 的路径。
- 预览默认零写入，`--apply` 只修改选中 host 的记录。
- 旧 schema v2 清单无需改动即可加载，显式 strict 恢复原行为。
