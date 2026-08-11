# 技术设计：P2-4 流完整性保障

## 1. 数据流

```text
executor result.Reader
  -> counting reader
  -> restic.BackupStdin
  -> executor Wait
  -> optional ForgetSnapshot
  -> history comparison
  -> store.RecordRunTarget
```

只有 restic、Wait、必要的撤销和状态记录全部完成后，target 才能返回成功或 warn。

## 2. 错误组合

- restic 失败：关闭 reader并执行 Wait 回收，组合可用错误；无 snapshot 不撤销。
- restic 成功 + Wait 失败：撤销 snapshot；撤销失败与 Wait 错误并列返回。
- 写状态库失败：返回持久化错误，不改变已经完成的仓库事实。
- context 取消：保留 `errors.Is` 语义并完成资源回收。

## 3. 跌幅算法

避免浮点比较，使用整数交叉乘法判断 `current * 100 < previous * 50`，并处理溢出边界。
恰好 50% 不告警；只有低于阈值才 warn。历史查询的 `found=false` 是正常首次备份。

## 4. 测试

使用假 executor、假 restic 和临时 SQLite Store 组合测试，不依赖真实 SSH/restic。
测试必须断言调用顺序、Wait 次数、Forget snapshot ID、状态库最终记录和错误链。
