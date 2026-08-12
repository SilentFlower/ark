# P2-6 backup CLI 与 systemd

## Goal

新增 `ark backup` 与 `ark install`，把 config、doctor、Runner、restic、backup、manifest
和 store 串成可由 systemd 安全触发的一次完整备份运行。

## Requirements

### R1 CLI 与流程

- 新增 `internal/cli/backup.go`、`internal/systemd/unit.go`、`internal/cli/install.go` 及测试。
- backup 固定流程：加载清单 → 本地 doctor → EnsureInit → host 串行 doctor/targets
  → manifest → 统一 forget/prune → 状态库完成记录。
- `--host <name>` 只处理指定 host；`--dry-run` 只打印脱敏计划且无锁、网络、文件、
  数据库或仓库副作用；`--skip-doctor` 仅用于明确应急。

### R2 失败隔离

- 本地 doctor fail 时整轮中止且不创建快照。
- 单 host doctor fail 时跳过该 host 并继续后续 host；单 target fail 不阻止同 host 其它 target。
- 任一 host/target 失败使整体退出非零，同时完整写入 manifest 和状态库；错误不得重复打印。
- manifest 或状态库关键写入失败必须可见，不能以零退出码结束。

### R3 单实例与顺序

- 使用 `/run/ark.lock` 的非阻塞 flock；拿不到锁立即失败，不等待。
- host 按清单顺序串行，全部目标处理后再统一 prune，禁止并发打爆仓库或数据库。
- run、target、manifest 的 ID/snapshot/bytes/status 必须一致可追溯。

### R4 systemd 安装

- `ark install` 生成手动全量 `ark-backup.service`，以及执行
  `ark backup --host <host>` 的 `ark-backup@.service` 模板。
- 每个 host 生成一个 `ark-backup@<host>.timer`，使用 `Config.ScheduleFor(host)` 的
  `OnCalendar`，并包含 `Persistent=true`、`RandomizedDelaySec=600`。
- 所有全量/单 host service 共用 `/run/ark.lock`；实例重叠时后启动者明确失败，
  不排队、不并发执行，也不伪造成成功。
- install 只清理带 ark 管理标记且已不在当前清单中的旧 host timer，不能删除用户 unit。
- 全部 unit 必须通过 `systemd-analyze verify`。
- 写系统路径前使用显式权限和原子替换；失败不留下半份 unit。
- 不在 unit 或命令行中写入 restic 密码、对象存储凭证或 SSH 私钥内容。

### R5 输出与测试

- 正常输出走 `cmd.OutOrStdout()`；脚本消费的结果提供纯 JSON 模式或稳定结构化状态。
- 测试通过依赖注入替换 doctor、Runner、Repo、Store、flock 和文件系统边界，
  不需要真实对象存储或 root。
- 外部心跳的 provider/config 由 P4-5 统一实现；本任务保留完整 run 结果和清晰扩展点，
  不提前发明新的清单字段。

## Non-Goals

- 不执行真实 systemctl、对象存储备份或生产 target；归 `p2-live-validation`。
- 不实现 P3 restore、P4 hub 服务或告警发送。
- 不在本任务修改 schedule 的清单数据模型。

## Acceptance Criteria

- [x] backup 主流程、`--host`、`--dry-run`、`--skip-doctor` 和失败隔离均有测试
- [x] flock 冲突立即非零退出，host/target 始终串行且统一 prune
- [x] run、target、manifest、store 状态在成功/warn/partial/fail 下保持一致
- [x] 每 host timer 使用自己的有效 schedule，unit 包含 oneshot、Persistent 和随机延迟
- [x] 多 timer 重叠时全局 flock 使后启动实例明确失败，手动全量 service 仍可用
- [x] install 只回收 ark 管理的陈旧 timer，`systemd-analyze verify` 无告警
- [x] install 失败不留下半份 unit，输出和 unit 不含凭证内容
- [x] make check、build、无 CGO 构建和 git diff 检查通过
