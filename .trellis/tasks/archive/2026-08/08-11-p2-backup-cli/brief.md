# Brief — P2-6 backup CLI 与 systemd

## Goal

- 用 `ark backup` 和 `ark install` 串起完整备份编排，并交给 systemd 安全调度。

## Scope

- 实现全流程、--host/--dry-run/--skip-doctor、全局 flock、partial 结果和统一 prune。
- 生成手动全量 service、per-host service/timer、原子安装、陈旧 timer 回收与 verify。

## Non-Goals

- 不执行真实 systemctl/对象存储，不实现 restore、hub 服务或外部心跳 provider。

## Key Decisions

- 每 host timer 使用 `ScheduleFor(host)` 并执行 `ark backup --host`。
- 所有实例共享 `/run/ark.lock`；碰撞时后启动者明确失败，不排队。

## Key Context

- 依赖 stream integrity 与 manifest；沿用 doctor CLI 的包内依赖注入模式。

## Risks / Deferred

- 长任务可能与其它 timer 碰撞；失败必须由 systemd 和后续死人开关可见。

## Acceptance

- 主流程、失败隔离、flock、状态一致性、per-host schedule、unit 回滚与 verify 测试通过。

## Next Step

- 启动任务后先建立可测试的 backup 状态机与 dry-run 计划。
