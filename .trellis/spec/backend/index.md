# Backend Development Guidelines

> ark 的后端编码规约。所有内容来自本仓库的真实代码，不是通用建议。

---

## Overview

ark 是一个 Go 编写的 Docker Compose 整机备份与重建工具，
产出 `ark`（hub 上的编排 CLI，oneshot）和 `ark-hub`（常驻的界面与 API）两个二进制。
被备份的目标机上不安装任何 ark 组件，只需要 docker 和 sshd（ADR-002）。

写代码前请先理解一条贯穿全项目的前提：
**ark 的失败是延迟暴露的**。一个写错的备份工具会连续三个月「成功」运行，
直到需要恢复的那天才暴露。下面所有规约里偏严的部分，理由都源于此。

架构背景与 13 条 ADR 见 `docs/design.md`，分阶段任务见 `docs/roadmap.md`。

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Directory Structure](./directory-structure.md) | 包划分、依赖方向、命名约定 | Filled |
| [Manifest Guidelines](./manifest-guidelines.md) | 清单模型演进、校验规约、默认值继承 | Filled |
| [Backup Manifest Guidelines](./backup-manifest-guidelines.md) | 备份产物 manifest schema、restic 存取与恢复一致性 | Filled |
| [Restore Plan Guidelines](./restore-plan-guidelines.md) | 恢复 Plan、manifest/config 映射与只读 dry-run 契约 | Filled |
| [DNSMgr Integration Guidelines](./dnsmgr-integration-guidelines.md) | dmonitor 维护窗口、恢复后 DNS 切换、凭证与补偿契约 | Filled |
| [Verify Guidelines](./verify-guidelines.md) | 自动恢复演练、生产基线、清理、结果与 weekly 调度 | Filled |
| [Backup Orchestration Guidelines](./backup-orchestration-guidelines.md) | backup 状态机、全局锁、保留阶段与 systemd 安装 | Filled |
| [External Command Guidelines](./external-command-guidelines.md) | 子进程调用与凭证注入红线 | Filled |
| [Database Guidelines](./database-guidelines.md) | ark 自身 SQLite 状态库，以及业务数据库备份/恢复边界 | Filled |
| [Error Handling](./error-handling.md) | 错误传播、聚合与退出码 | Filled |
| [Logging Guidelines](./logging-guidelines.md) | 输出通道、结构化上报、密钥红线 | Filled |
| [Hub Guidelines](./hub-guidelines.md) | ark-hub 本地鉴权、HTTP 会话、限流、生命周期与常驻 service | Filled |
| [Quality Guidelines](./quality-guidelines.md) | 提交门槛、测试要求、注释标准 | Filled |

### 与模板的差异

本项目对 Trellis 默认模板做了三处调整，理由如下：

- **新增 `external-command-guidelines.md`**。ark 的功能几乎全部由调用
  `restic` / `docker compose` / `pg_dump` 实现，子进程调用是本项目
  最主要的代码形态和最集中的风险点，需要独立成篇。
- **新增 `manifest-guidelines.md`**。清单是 ark 唯一的用户输入契约，
  hub 的全部行为都由它决定，而它的每一次结构变更都会横穿
  config / doctor / cli 三层，需要一份独立的演进与校验规约。
- **`database-guidelines.md` 明确了双重边界**。ark 自身状态只允许
  `internal/store` 管理 SQLite；被备份的业务数据库仍只通过容器内 CLI
  交互，不引入业务数据库驱动或 ORM。

---

## Reading Order

首次进入本项目，按这个顺序读效率最高：

1. `docs/design.md` 第 4 节的 13 条 ADR — 理解为什么是现在这个形状。
2. [Directory Structure](./directory-structure.md) — 知道代码该往哪放。
3. [Manifest Guidelines](./manifest-guidelines.md) — 清单是唯一的用户输入契约，几乎所有功能都从它读起。
4. [External Command Guidelines](./external-command-guidelines.md) — 本项目最容易出事的地方。
5. 其余三篇按需查阅。

动手前再扫一眼 [Quality Guidelines](./quality-guidelines.md) 末尾的
Code Review Checklist。

---

**Language**: 文档标题保持英文，正文用中文。代码注释、错误信息、
用户可见文本一律中文。
