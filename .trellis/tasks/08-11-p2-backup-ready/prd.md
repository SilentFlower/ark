# P2 备份可用

## Goal

完成 P2-2 至 P2-8 及实机发现的流生命周期修复，使 hub 能通过 `ark backup` 把所有
机器的目标流式写入同一 restic 仓库，并用状态库、manifest、systemd 和真实环境证据
证明备份可用。

## Requirements

### R1 子任务边界

- `p2-restic`：restic 仓库与快照命令边界。
- `p2-target-executors`：五类 target 的 agentless 数据流。
- `p2-stream-integrity`：流退出状态、坏快照撤销和体积跌幅防线。
- `p2-manifest`：一次多目标运行的版本化快照清单。
- `p2-backup-cli`：备份编排、锁、状态写入和 systemd 安装。
- `p2-hub-protection`：hub 自备份示例、密钥排除和对象锁提醒。
- `p2-ssh-host-key-usability`：首次信任默认策略、密钥变化拒绝和显式刷新工具。
- `p2-stream-close-lifecycle`：修复实机暴露的 Wait 后关闭语义、失败快照精确撤销、
  systemd 缓存目录和 doctor 失败摘要脱敏。
- `p2-live-validation`：真实 hub、目标机和对象存储的最终验收。

### R2 依赖顺序

- `p2-target-executors` 依赖 `p2-restic`。
- `p2-stream-integrity` 依赖 `p2-restic` 和 `p2-target-executors`。
- `p2-manifest` 依赖 `p2-restic` 和 `p2-target-executors`。
- `p2-backup-cli` 依赖 `p2-stream-integrity` 和 `p2-manifest`。
- `p2-hub-protection` 依赖 `p2-backup-cli`。
- `p2-ssh-host-key-usability` 依赖已有 SSH 执行层和 `p2-hub-protection` 文档边界。
- `p2-stream-close-lifecycle` 来自真实环境验收的缺陷反馈，依赖既有完整性、manifest、
  CLI、restic、SSH 执行层和 systemd 契约。
- `p2-live-validation` 的最终结论依赖前述八个代码任务全部完成并通过对应复验。

### R3 Auto-loop 边界

- auto-loop 只纳入前六个代码任务，profile 固定为 `commit-only`。
- auto-loop 不 push、不归档、不执行真实生产数据、对象锁、DNS 或外部系统操作。
- `p2-ssh-host-key-usability`、`p2-live-validation` 和本父任务不进入已结束的初始队列；
  SSH 易用性由用户确认后交互实现，真实验收由用户提供环境后人工执行，父任务只在全部
  子任务完成后做集成复核。

### R4 集成复核

- 所有快照使用稳定 host/target/run 标签和稳定 stdin filename。
- 备份数据不落 hub 临时文件，凭证不进入全局环境或日志。
- 任何上游流失败都不能留下可被当作成功使用的快照。
- manifest、状态库与 restic snapshot ID 必须能相互追溯。
- 已归档子任务的 Check-All、规范、提交和 release 记录必须能支撑父任务结论；发现新的
  业务代码缺陷时重开或新建对应子任务，父任务不直接承载修复。

## Non-Goals

- 不实现 P3 恢复、P4 Web/API、P5 dnsmgr 或 P6 长期优化。
- 不在父任务中直接修改业务代码。
- 不由 auto-loop 执行真实服务器或对象存储控制台操作。

## Acceptance Criteria

- [x] 八个代码子任务均完成各自 Check-All、规范更新、提交与归档
- [x] `p2-live-validation` 的真实备份、截断、systemd、hub 自备份和对象锁验收通过
- [x] P2 集成复核确认依赖方向、流生命周期、错误传播、状态/manifest 一致性和密钥边界无漂移
- [x] `make check`、`make build` 与 `CGO_ENABLED=0 go build ./cmd/ark` 全绿
- [x] `docs/roadmap.md` 的 P2-2 至 P2-8 与 P2 阶段状态在全部验收通过后更新
