# Journal - silentflower (Part 1)

> AI development session journal
> Started: 2026-08-11

---



## Session 1: Bootstrap backend spec from real code

**Date**: 2026-08-11
**Task**: Bootstrap backend spec from real code
**Branch**: `main`

### Summary

从 ark 现有 Go 代码提炼 backend 开发规约，替换 trellis init 的占位模板：新增 external-command-guidelines.md（子进程与凭证红线），database-guidelines.md 改写为备份/恢复数据库规约，其余四篇按 config/doctor/cli 实际写法记录。make check 全绿。

### Git Commits

| Hash | Message |
|------|---------|
| `43faa59` | (see git log) |

### Status

[OK] **Completed**


## Session 2: P1-1 清单模型升级到多机（v2）

**Date**: 2026-08-11
**Task**: P1-1 清单模型升级到多机（v2）
**Branch**: `main`

### Summary

ark.yaml 从单机模型升级为 hub 编排的多机 v2 模型：顶层拆为 repo/defaults/hosts，新增 Host 与 SSH（known_hosts_file 必填、绝对路径），local 与 ssh 互斥、host 名全局唯一；默认值继承改为指针 + ScheduleFor/RetentionFor，删除 retention 三项全 0 的启发式；Load 改两遍解析，v1 清单给出迁移提示。validate 改为逐 host 摘要，doctor 过渡适配多机遍历，examples/ark.yaml 重写为多机清单，config 测试整体重写覆盖全部新增失败路径，README/design/roadmap 同步并新增 spec backend/manifest-guidelines.md。归档时生成 release.md，记录 v1→v2 人工迁移与 repo.url 不再按机器分路径两项配置事项。

### Git Commits

| Hash | Message |
|------|---------|
| `ca1078d` | (see git log) |

### Status

[OK] **Completed**


## Session 3: 完成 P1-2 SSH 执行层

**Date**: 2026-08-11
**Task**: 完成 P1-2 SSH 执行层
**Branch**: `main`

### Summary

新增本地与系统 OpenSSH 统一 Runner，覆盖 Run、Stream、Feed 生命周期与远程参数安全转义；doctor 增加 ssh 运行时依赖检查，并通过 Full Check-All、竞态测试和真实 localhost SSH 注入验证。

### Git Commits

| Hash | Message |
|------|---------|
| `fd53cabd1308bb4bc37e91c137a2f296353899a2` | (see git log) |

### Status

[OK] **Completed**


## Session 4: 完成 P1-3 远程 doctor

**Date**: 2026-08-11
**Task**: 完成 P1-3 远程 doctor
**Branch**: `main`

### Summary

完成 hub 与 host doctor 拆分、远程环境检查和 CLI 范围选择；Full Check-All 通过并归档任务。

### Main Changes

- 新增 RunLocal 与 RunHost，统一 local/SSH Runner 检查与依赖降级
- 新增 doctor --host/--all，并保持 JSON 与退出码兼容
- 修复远程 stat 跟随符号链接语义并沉淀 code-spec

### Git Commits

| Hash | Message |
|------|---------|
| `6e77007` | (see git log) |

### Testing

- [OK] make check、race 定向测试、CLI 实际命令与 git diff 检查通过

### Status

[OK] **Completed**

### Next Steps

- 开始 roadmap 下一项任务
