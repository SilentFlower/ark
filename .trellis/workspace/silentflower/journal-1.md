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


## Session 5: 完成并归档 P2-1 状态库

**Date**: 2026-08-11
**Task**: 完成并归档 P2-1 状态库
**Branch**: `main`

### Summary

完成 internal/store SQLite WAL 状态库、schema v1、并发安全迁移和状态读写 API；通过 race、无 CGO 与构建检查，更新数据库规范并归档 P2-1。

### Git Commits

| Hash | Message |
|------|---------|
| `3735182` | (see git log) |

### Status

[OK] **Completed**


## Session 6: 归档 P2 备份可用 wave 子任务

**Date**: 2026-08-12
**Task**: 归档 P2 备份可用 wave 子任务
**Branch**: `main`

### Summary

P2-2 至 P2-7 已完成质量检查和本地业务提交；本次完成决策审计、当前任务上线审计并归档 6 个子任务，保留父任务与人工真实环境验收任务。

### Git Commits

| Hash | Message |
|------|---------|
| `8421ea6826b4c87cf2cea11118bc5b178f5892a6` | (see git log) |
| `ed9a8acc78d860904c0b1f6285a6088ff36a613d` | (see git log) |
| `62d9e4ead415365c9e1abc523b54fc0a0f47d70e` | (see git log) |
| `717e9e8adfd5f820de5602be2193677980655bf8` | (see git log) |
| `c33dbbe45d343b4e1d9bf10445b7ef90e19fb1ce` | (see git log) |
| `e7d17008eecdbf61bae248ff58b8faff976dd86a` | (see git log) |

### Status

[OK] **Completed**


## Session 7: 完成 P2 SSH 主机密钥易用性

**Date**: 2026-08-12
**Task**: 完成 P2 SSH 主机密钥易用性
**Branch**: `main`

### Summary

实现默认 accept-new 与显式 strict、主机密钥预览/刷新、doctor 策略检查和原子失败回滚；通过 Check-All 后提交推送并归档任务。

### Main Changes

- 新增 ark host-key refresh 安全预览与显式应用流程
- 统一 config、sshexec 和 doctor 的主机密钥策略
- 补齐 known_hosts 原子更新与目录同步失败回滚

### Git Commits

| Hash | Message |
|------|---------|
| `2ff5a01` | (see git log) |
| `8a17ed9` | (see git log) |

### Testing

- [OK] make check、make build、CGO_ENABLED=0 go build ./cmd/ark、git diff --check

### Status

[OK] **Completed**

### Next Steps

- 继续 p2-live-validation，在真实服务器验证首次接受、变更拒绝和显式刷新


## Session 8: 完成 P2 真实流 Close 生命周期修复与实机验收

**Date**: 2026-08-12
**Task**: 完成 P2 真实流 Close 生命周期修复与实机验收
**Branch**: `main`

### Summary

修复真实 StdoutPipe Wait 后 Close 误报、restic summary 后失败快照在 target 与 manifest 两个入口的精确撤销、systemd restic 缓存目录和 doctor 失败项脱敏；完成真实服务器全量备份、故障注入、恢复与对象锁验收，Full Check-All 与全部质量门禁通过。归档后回到 p2-live-validation 收尾；对象锁测试 volume 待保留期结束删除，管理口令需轮换。

### Git Commits

| Hash | Message |
|------|---------|
| `b76f930` | (see git log) |
| `3f84631` | (see git log) |

### Status

[OK] **Completed**


## Session 9: 完成 P2 真实环境验收

**Date**: 2026-08-12
**Task**: 完成 P2 真实环境验收
**Branch**: `main`

### Summary

完成隔离实机备份链验收、缺陷修复复验、Full Check-All 与任务归档准备；验收覆盖 SSH 主机密钥、systemd、flock、截断快照撤销、体积告警、hub 自备份和对象锁。

### Main Changes

- 归档 p2-live-validation，并生成包含部署刷新、密码轮换和对象锁残留清理的 release.md

### Git Commits

| Hash | Message |
|------|---------|
| `b76f930` | (see git log) |
| `f19fcdc` | (see git log) |
| `b2454a2` | (see git log) |

### Testing

- [OK] make check、关键包 10 轮 race、普通/静态构建和实机场景全部通过

### Status

[OK] **Completed**

### Next Steps

- 进入父任务 p2-backup-ready 执行 P2 集成复核


## Session 10: 完成并归档 P2 备份可用

**Date**: 2026-08-12
**Task**: 完成并归档 P2 备份可用
**Branch**: `main`

### Summary

完成 P2-2 至 P2-8、实机缺陷修复与真实环境验收的父任务集成复核；通过 Full Check-All，更新 roadmap 和上线操作单，并归档父任务。

### Git Commits

| Hash | Message |
|------|---------|
| `cc80f4b` | (see git log) |
| `f57069f` | (see git log) |

### Status

[OK] **Completed**


## Session 11: 完成 P3 Wave A 恢复计划与执行

**Date**: 2026-08-12
**Task**: 完成 P3 Wave A 恢复计划与执行
**Branch**: `main`

### Summary

P3-1 恢复 dry-run 与 P3-2 可重跑恢复执行已完成全量检查并归档；修复 P3-2 auto-loop 完成态落盘缺失，父任务保留 P3-3 至 P3-5 待推进。

### Main Changes

- 实现并提交恢复 Plan、只读 dry-run 与严格 manifest/digest 校验
- 实现带冲突保护、安全备份、幂等续跑和健康校验的恢复执行
- 归档 P3-1 与 P3-2，保留 P3 父任务及后续验收任务

### Git Commits

| Hash | Message |
|------|---------|
| `39206c8a4b47adbd7ecc7abd5d66a57c3a113f99` | (see git log) |
| `ed05aa83d849a58ea3b613717d72cb2c49b6b125` | (see git log) |

### Testing

- [OK] make check、make build、无 CGO 构建、go mod verify、git diff --check 均通过
- [OK] restore/cli/doctor/backup race 测试 count=10 通过

### Status

[OK] **Completed**

### Next Steps

- 启动 P3-3 跨机重建实测，使用已提供的隔离服务器完成真实恢复验收


## Session 12: 完成 P3-2A 隔离恢复并归档

**Date**: 2026-08-13
**Task**: 完成 P3-2A 隔离恢复并归档
**Branch**: `main`

### Summary

完成隔离恢复命名、路径、资源和 Docker 自动端口映射，修复 SSH 流生命周期清理，Full Check-All 通过并归档任务。

### Main Changes

- 增加隔离 Compose 项目、资源、路径与自动端口映射
- 增加安全 cleanup、续跑状态和 Compose 端口元数据契约
- 统一 SSH stdout 读取与异常资源回收

### Git Commits

| Hash | Message |
|------|---------|
| `226156d` | (see git log) |
| `c1c24a4` | (see git log) |

### Testing

- [OK] make check 通过
- [OK] 关键包 race 测试连续 10 次通过
- [OK] 真实 Docker 共存、端口映射与清理验证通过

### Status

[OK] **Completed**

### Next Steps

- 进入下一个 P3 子任务


## Session 13: 完成 P3-3 跨主机重建受限验收

**Date**: 2026-08-13
**Task**: 完成 P3-3 跨主机重建受限验收
**Branch**: `main`

### Summary

完成隔离跨机恢复、中断续跑、源业务停止后重建与归属清理；Check-All 通过且用户接受 CHK-001/FBK-001 风险，任务已归档。

### Git Commits

| Hash | Message |
|------|---------|
| `c58a952` | (see git log) |

### Status

[OK] **Completed**
