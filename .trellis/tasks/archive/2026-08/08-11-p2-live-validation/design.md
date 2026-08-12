# 验收设计：P2 真实环境验收

## 1. 隔离拓扑

```text
管理机（当前工作区）
  -> 现有 SSH 管理端口（root 密码，仅用于环境编排）
      -> 测试服务器 / hub
          -> /opt/ark-live-validation/bin/ark
          -> /etc/ark-live-validation/{ark.yaml,known_hosts,credentials}
          -> /opt/ark-live-validation/{compose,data,evidence}
          -> /var/lib/ark/ark.db 与 /run/ark.lock（ark 固定路径，先做占用门禁）
          -> /etc/systemd/system/ark-*（先做同名 unit 门禁）
          -> 127.0.0.1:<专用高端口> 独立 sshd
              -> 临时 root authorized key + 独立 host key
              -> 专用 Docker Compose 项目
                  -> PostgreSQL / Redis / image service / test volumes
          -> 隔离 MinIO + object-lock bucket
```

- 独立 sshd 只监听 localhost，供同机 hub 模拟 agentless 远程目标；主机密钥轮换只替换
  该实例的测试 host key，不触碰系统 sshd。
- Compose project、container、volume、network、目录和 unit 统一使用
  `ark-live-validation` 前缀，避免与现有业务资源同名。
- 管理密码不写文件；ark 清单只引用临时 SSH 私钥、known_hosts、restic password file
  和 MinIO env file，权限固定为 `0600`。
- ark 的状态库、全局锁和真实 systemd unit 路径是产品固定契约，不能通过测试配置改写。
  基线发现 `/var/lib/ark/ark.db`、`/run/ark.lock` 或同名 `ark-*` unit 已被既有部署使用时，
  本任务立即记为 blocked，不覆盖、不迁移、不借用该资源。

## 2. 场景表

| 场景 | 路径 | 期望 |
|---|---|---|
| 正常全量 | timer/service → backup → restic/store | 全 target、manifest、run 成功 |
| SSH 首次信任 | 空 known_hosts → doctor/backup | 默认策略建立记录，后续连接复用 |
| SSH 密钥变化 | 替换测试主机 key → doctor/backup → refresh | 先拒绝并提示；带外核对和 apply 后恢复 |
| 锁冲突 | 同时启动两轮 backup | 第二轮立即失败，无重复 dump |
| SSH 截断 | pg_dump 运行中 kill | target fail，截断 snapshot 不可用 |
| 体积异常 | 当前 bytes < 历史 50% | target warn，snapshot 保留 |
| hub 自备份 | 在线导出 ark.db | 副本独立完整且可查询 |
| 对象锁 | 删除保留期内对象 | provider 拒绝删除 |

## 3. 数据恢复

所有故障注入使用专用测试数据。执行前保存服务状态与仓库快照清单；执行后恢复服务、
删除允许删除的测试快照、容器、网络、volume、临时 sshd 和 ark unit，不修改对象锁保留期
来强行清理。仍在保留期内的 MinIO 对象保留到自然到期，并记录路径、体积和预计清理时间。

## 4. 证据

保留脱敏的命令、退出码、snapshot ID、tags、manifest run ID、状态库查询、
systemd verify 输出和 provider 删除拒绝结果到任务内 `validation.md`。不得记录密码、
AK/SK、私钥、原始公钥、数据库内容或完整环境变量。

## 5. 失败处理

环境/权限不足记为 blocked，不伪造通过。代码缺陷回到所属子任务修复并重新 Check-All；
外部 provider 行为差异记录到本任务和运维文档，经用户确认后再调整验收。
