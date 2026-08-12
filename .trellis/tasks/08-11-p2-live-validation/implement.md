# 执行计划

## 前置

1. 取得对隔离资源创建、故障注入和清理的明确授权。
2. 确认七个代码子任务已完成并已部署到测试 hub。
3. 只读记录现有容器、systemd unit、监听端口、磁盘/内存和目标目录基线。
4. 准备脱敏场景表、基线 snapshots、资源命名表和逐项回滚步骤。
5. 确认 `/var/lib/ark/ark.db`、`/run/ark.lock` 和 `/etc/systemd/system/ark-*` 未被既有
   ark 部署占用；任一冲突都停止环境写入并报告 blocked。

## 环境准备

1. 构建无 CGO ark 二进制并上传到 `/opt/ark-live-validation/bin/ark`。
2. 安装 restic；创建权限为 `0600` 的专用 repo password、MinIO env、SSH identity 和
   known_hosts 文件，秘密值不进入 shell argv 或任务记录。
3. 创建 `ark-live-validation` Docker Compose 项目，包含 PostgreSQL、Redis、可检查镜像
   digest 的服务和专用 volume/files 数据。
4. 创建仅监听 localhost 高端口的独立 sshd，使用独立 host key 与临时公钥认证。
5. 启动隔离 MinIO，创建启用 object lock 的专用 bucket 和专用 restic repository。
6. 生成独立 ark 清单并依次运行 `validate`、`doctor --all` 和 `backup --dry-run`。
   清单放在 `/etc/ark-live-validation/ark.yaml`，但状态库与全局锁仍使用产品固定路径。

## 执行

1. 验证 SSH 默认首次信任、密钥变化拒绝、刷新预览和带外核对后的显式应用。
2. 正常执行 validate、doctor、install、verify 和完整 backup。
3. 核对 restic snapshots/dump、manifest、store 和 systemd 证据。
4. 执行 flock、kill pg_dump、体积腰斩三类故障注入。
5. 验证 hub 状态库一致性备份和密钥排除。
6. 在 provider 控制台/API 验证对象锁删除拒绝。
7. 恢复测试环境并记录残留、blocked 和后续修复任务。

每行场景执行前后都检查资源前缀和 PID/container/unit 归属；目标不属于专用测试资源时
立即停止，不执行 kill、delete、restart 或覆盖操作。

## 验证方式

本任务执行时使用 `trellis-run-full-chain` 的场景表流程，跨 CLI、restic、SQLite、systemd
和对象存储逐行记录 pass/fail。不能用单元测试结果替代真实证据。

## 高风险点

- kill、删除和 systemd 写入只允许作用于明确授权的测试资源。
- 对象锁数据可能在保留期内无法清理，这是预期且需提前确认成本。
- 任何凭证和业务数据样本都不得写入 Git 任务文件。
- root 密码只用于管理连接，不写入命令行、环境文件或 ark 清单；验收结束后提醒用户轮换。
- MinIO object lock 可能使少量测试对象在保留期内不可删除；执行前记录容量上限，禁止用
  缩短保留期或删除后端文件的方式绕过验证。
