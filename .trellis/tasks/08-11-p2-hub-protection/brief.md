# Brief — P2-7 hub 自备份保护

## Goal

- 安全备份 hub 清单与一致性 SQLite 状态，并持续提醒对象锁和离线密钥前提。

## Scope

- 增加 store 在线一致性导出、backup 特殊接入、hub 示例 targets、doctor warn 与运维文档。
- 明确排除 repo password/env 和 SSH 私钥，验证导出副本 integrity_check。

## Non-Goals

- 不修改真实 bucket、不执行删除测试、不实现 provider-specific 对象锁 API。

## Key Decisions

- WAL 数据库不能走普通 files tar；一致性导出由 store 持有并由 backup 精确识别。
- 无 provider-neutral 证据时 doctor 只能 warn，不能伪装为已验证 ok。

## Key Context

- 依赖 p2-backup-cli；受 ADR-006/012 与 database guidelines 约束。

## Risks / Deferred

- modernc v1.36.1 的 Online Backup API 已通过真实 WAL 并发写入、独立打开和清理测试；
  对象锁真实控制台验收仍留给人工任务。

## Acceptance

- WAL 副本一致、临时权限/清理安全、示例不含密钥、doctor warn 和文档测试通过。

## Next Step

- 由 auto-loop 完成 full Check-All、规范同步和 commit-only；不触碰真实 bucket。
