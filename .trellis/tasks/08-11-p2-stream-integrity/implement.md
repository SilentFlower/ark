# 执行计划

## 步骤

1. 在 backup 包建立单 target 流消费与结果模型。
2. 实现 counting reader、统一 Close/Wait 和错误组合。
3. 接入 restic BackupStdin 与失败后的 ForgetSnapshot。
4. 接入 store 历史查询、50% 边界和 RunTarget 持久化。
5. 补齐调用顺序、资源清理、错误链和阈值表驱动测试。

## 验证

```bash
go test ./internal/backup ./internal/store -race -count=1
make check
make build
CGO_ENABLED=0 go build ./cmd/ark
git diff --check
```

## 高风险点

- restic 成功不等于上游成功，Wait 必须是成功判定的一部分。
- 坏快照撤销失败必须暴露，不能留下“失败但看起来可恢复”的仓库状态。
- 阈值计算不能因整数溢出或零基线产生错误告警。
