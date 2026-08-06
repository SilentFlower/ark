# 路线图

分阶段推进，每个阶段结束时都应该是**可用且被验证过的**，而不是半成品堆积。

排序原则：**先让 agent 能产出真实数据，再做展示。** 中心机页面展示的是
agent 上报的状态，agent 还没跑起来时做前端只能对着假数据调样式，
等真实数据出来还得重做一遍。

---

## P0 — 项目基础 ✅ 已完成

- [x] 项目骨架、Makefile、构建与测试流水线
- [x] 备份清单数据模型（5 种 target 类型）
- [x] `ark validate`：静态语义校验，拒绝未知字段和错位字段
- [x] `ark doctor`：运行环境检查（外部命令、文件权限、compose 资源存在性）
- [x] 设计文档与 ADR

**验收**：`make check` 全绿；`ark validate -c examples/ark.yaml` 通过。

---

## P1 — 备份可用

- [ ] restic 仓库封装（init / backup --stdin / snapshots / forget / check）
- [ ] 五种 target 的执行器
- [ ] `ark backup`：完整备份流程 + 快照清单
- [ ] `ark snapshots`：列出本机快照
- [ ] `ark install`：生成并安装 systemd service + timer
- [ ] 状态文件 `_status/<host>.json` 写入

**验收**：在一台真实机器上完成一次全量备份，对象存储中能看到加密对象，
`restic snapshots` 能列出内容。

---

## P2 — 恢复可用（最关键的一步）

- [ ] `ark restore --dry-run`：只读，输出完整恢复计划
- [ ] `ark restore`：按 §7 顺序执行，幂等，拒绝隐式覆盖
- [ ] 跨机器恢复：在一台干净的新机器上从零重建
- [ ] `ark verify`：自动恢复演练到隔离的 compose 项目并跑健康检查

**验收**：在一台全新的 VPS 上，只用「对象存储凭证 + restic 密码 + `ark restore`」
把服务完整拉起来，且业务数据完整。**这一步不通过，前面所有工作都没有意义。**

---

## P3 — 中心机与 Web 页面

- [ ] `ark-hub` 后端：聚合 `_status/*.json`，提供只读 API
- [ ] 前端页面（Vue 3 + Vite + TS + Pinia + Tailwind）
  - [ ] 总览：机器卡片，最近备份时间 / 状态 / 大小趋势 / 下次计划
  - [ ] 详情：target 明细、历史快照、doctor 报告、演练结果
  - [ ] 告警视图：超时未备份、连续失败、演练失败
- [ ] 前端产物 `go:embed` 进单二进制
- [ ] 钉钉机器人告警推送

**验收**：打开页面能一眼看出所有机器的备份健康度；
人为让一台机器备份失败，页面和钉钉都能在下一个周期内报出来。

---

## P4 — 运维打磨

- [ ] `pg_dump` 与服务端版本比对检查
- [ ] 单机多 compose 项目支持
- [ ] 第二备份后端（防止单一存储商账号被封）
- [ ] 恢复后的人工确认清单（DNS、证书、防火墙、端口）
- [ ] 发布流程：goreleaser + 多架构二进制

---

## 暂不做

- **WAL 归档 / PITR**：秒级 RPO 需要 pgBackRest 或 wal-g，复杂度高一个量级
  且只覆盖 PostgreSQL。等 P2 稳定运行一段时间后再评估是否需要。
- **自动故障切换**：ark 不参与流量调度，不做 fencing，不自动提升节点。
- **非 compose 部署形态**（k8s、裸机 systemd 服务）：范围外。
