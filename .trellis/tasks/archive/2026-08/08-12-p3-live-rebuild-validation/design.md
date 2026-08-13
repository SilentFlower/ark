# 验收设计：跨主机重建

## 1. 环境拓扑

```text
hub（ark + restic + repo credentials）
  -> object storage snapshots
  -> SSH
clean destination VPS（docker + compose + sshd）
```

源主机不在恢复数据链上，只作为恢复后业务结果的基准来源。若必须读取源主机文件或数据才能
完成恢复，说明备份范围不完整，验收直接失败。

## 2. 证据分层

- 基线：目标机现有资源、安装软件、端口和文件。
- 计划：dry-run 的 manifest/Plan 标识与目标资源。
- 执行：每阶段结果和失败/重跑记录。
- 数据：表统计、最新记录、可回滚写入、加密字段、Redis/volume/files 抽查。
- 身份：实际容器 digest 与 Compose project label。
- 独立性：目标机软件清单、凭证缺失与源主机零读取证据。
- 清理：测试写入、临时连接和目标机处置。

## 3. 缺陷闭环

发现缺陷后停止对应验收项，记录最小复现与影响，回到所属代码任务修复、Check-All、本地提交，
再从失败步骤之前重新执行。`validation.md` 同时保留首次失败与最终复验，不能覆盖历史事实。

## 4. 安全

- 不在命令行直接写密码或对象存储密钥。
- 不更改生产 DNS、证书、dnsmgr 或源主机服务。
- 清理前校验资源属于 destination 测试项目，避免按名称误删其它 Docker 资源。
