# 技术设计：P2 备份可用

## 1. 任务拓扑

```text
p2-restic
    -> p2-target-executors
        -> p2-stream-integrity
        -> p2-manifest
             \          /
              p2-backup-cli
                    -> p2-hub-protection
                              -> p2-ssh-host-key-usability（交互）
                                      -> p2-live-validation（人工）
                                             -> p2-stream-close-lifecycle（实机缺陷修复）
                                                    -> p2-live-validation 最终复验
```

父任务只维护范围、依赖和最终集成结论，不承载实现。auto-loop 的任务顺序不等于依赖，
启动时必须把上图边显式传给 `--depends-on`。

## 2. 数据主链

```text
config.Target
  -> sshexec.Runner.Stream
  -> backup executor
  -> restic.BackupStdin
  -> stream Wait / integrity decision
  -> store.RunTarget + manifest target
  -> unified forget/prune
```

数据只经过内存流和子进程管道，不落 hub 临时文件。stderr、凭证环境和业务数据流
必须保持隔离。

## 3. 完成策略

- 代码子任务可以由 auto-loop 独立实现、检查、更新规范并本地提交。
- 初始 auto-loop 归档后新增的 SSH 主机密钥易用性任务按交互流程完成，再进入真实验收。
- 真实环境验收独立成任务，避免外部凭证和生产副作用阻塞整条无人值守队列。
- 实机发现的跨包缺陷独立进入 `p2-stream-close-lifecycle` 修复任务；修复完成后回到
  `p2-live-validation` 汇总复验与完整证据，父任务不直接修改业务代码。
- auto-loop 结束后由用户显式决定 push；每个 completed 子任务仍需显式归档。
- 所有子任务归档后，再对父任务执行集成复核和归档。

## 4. 风险

- 当前本地环境可能没有 `restic`，相关集成测试可按规范跳过；父任务必须同时核对已归档
  `p2-live-validation/validation.md` 的真实 restic 与对象存储证据，不能只看本地测试。
- P2-6 使用 per-host timer 兑现 schedule 覆盖；实例碰撞由全局 flock 明确失败，
  需要在真实 systemd 验收中验证可观测性。
- P2-7 的对象锁验证需要具体存储提供商和有权限的测试凭证。
- SSH 主机重装或轮换时不能靠删除整份信任库绕过；真实验收必须保留首次建立信任、
  变化拒绝和带外核对后刷新三类证据。
- 对象锁残留和管理密码轮换属于 release 运维事项，不影响代码 strict pass，但必须在
  最终 release audit 中保持可见。
