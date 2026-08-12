# P2 真实环境验收记录

## 执行摘要

- 日期：2026-08-12（Asia/Shanghai）
- 基线代码：`main` / `79ef5f4`；验收二进制为当次工作区无 CGO 静态构建
- 实机验收二进制 SHA256：`ab3823cb254b4188133f1d33e19d172bd79ba6c0998b7e7cef27639e00373e98`
- restic：`0.19.1`，官方发布包 SHA256 校验通过
- 环境：Debian 10、systemd 241、Docker 26.1.4、Docker Compose 5.1.0
- 隔离资源：`ark-live-validation` Compose 项目、localhost 独立 sshd、Object Lock bucket
- 结果：PASS，场景表全部完成；实机发现的缺陷均已修复并在同一环境复验通过

## 场景结果

| 场景 | 结果 | 证据摘要 |
|---|---|---|
| 隔离与碰撞门禁 | PASS | 固定状态库、锁、ark unit 和同名前缀 Docker 资源初始均未被占用；既有业务服务未操作 |
| validate / dry-run / doctor | PASS | 2 台 host、7 个 target；最终 doctor 为 29 ok、1 warn、0 fail，warn 仅为对象锁人工确认 |
| SSH 首次信任 | PASS | 空 known_hosts 下默认 `accept-new` 建立记录，后续连接复用 |
| SSH 密钥变化 | PASS | 独立 sshd key 变化后 doctor 退出 2 并提示 refresh；预览不落盘，带外 SHA256 指纹一致后显式 apply 恢复，known_hosts 仍为 0600 |
| 正常全量备份 | PASS | run `20260812T043315.972990601Z-897eefefdbe5121c`，7 个 target 全部 ok，另有 manifest snapshot `be3045292628...` |
| systemd service | PASS | 模板 service `Result=success`、`ExecMainStatus=0`；systemd 创建 `/var/cache/ark`，权限 0700 root:root |
| systemd timer | PASS | runtime drop-in 设 2 秒触发，timer 实际拉起 service，run 数从 7 增至 8；drop-in 随后删除并恢复原计划 |
| flock 冲突 | PASS | 持有 `/run/ark.lock` 时第二轮退出 1，run 数保持 4；释放后可立即重取锁 |
| pg_dump 中断 | PASS | 只 kill 隔离 PostgreSQL 容器内的 `pg_dump`；run `20260812T042758.414730727Z-5ca539d73f82c839` target fail，审计 ID `26af6813e90c...`，仓库按 run+target 精确查询为 0 |
| 体积腰斩 | PASS | 基线 52,690,257 bytes；缩至 1,000 行后为 262,255 bytes，target warn，snapshot `143e0fe36edd...` 保留；数据恢复至 200,000 行 |
| hub 状态库恢复 | PASS | 快照恢复后的 SQLite `integrity_check=ok`，`runs=2`、`run_targets=5`；manifest run/host/target 一致 |
| hub 公共材料与密钥排除 | PASS | 归档只含 ark.yaml、known_hosts、compose.yaml、公开测试文件；未包含 password/env/SSH identity 等秘密文件 |
| 对象锁 | PASS | 哨兵数据版本设置 1 天 GOVERNANCE；精确版本删除退出 1，MinIO 明确返回 WORM protected |

## 最终全量快照

| host / target | bytes | snapshot |
|---|---:|---|
| hub-live / files/ark-state | 65,536 | `4047c5c888a9...` |
| hub-live / files/hub-public | 10,240 | `f18103372b92...` |
| target-live / postgres/postgres/arklive | 52,690,257 | `3889145206b3...` |
| target-live / redis/redis | 142,873 | `a23e6327fea6...` |
| target-live / volume/ark-live-validation-payload-data | 8,391,680 | `b79b2f192b69...` |
| target-live / files/target-files | 10,240 | `02b765ee4980...` |
| target-live / image_digest | 90 | `c40cda82dc5a...` |

该 run 的状态库记录为 7 个 target，全部 `ok` 且 bytes/snapshot ID 非空；仓库按 run tag
精确列出 8 个快照，包括 7 个 target 和 1 个 manifest。整体状态为 `warn`，仅因为
provider-neutral 的 `repo.object_lock` 检查按设计保持人工确认告警。

## 实机发现与修复

1. 真实 `exec.Cmd.StdoutPipe` 在 `Wait` 后由 Go 关闭，随后 `Close` 的预期已关闭错误曾把
   四类完整流误报为 fail。修复为只在 Wait 已接管时归一化 `os.ErrClosed`，其它 Close 错误仍可见。
2. systemd system service 不提供 `HOME`，restic 0.19.1 因无法定位缓存目录拒绝启动。
   service 现使用 `CacheDirectory=ark`、`CacheDirectoryMode=0700` 和
   `XDG_CACHE_HOME=/var/cache/ark`。
3. 上游流中断时 restic 可能先提交 snapshot 并输出 summary，再返回非零退出。`BackupStdin`
   现保留已解析 ID，`BackupTarget` 发现失败且 ID 非空时精确 forget；实机重跑后仓库无截断快照。
4. local/host doctor 失败现在列出失败检查项名称，不输出可能包含外部命令详情的 detail。

实机环境清理后，Full Check-All 又补齐 manifest 保存入口对同一失败 snapshot ID 契约的消费：
备份返回错误且带 ID 时精确 forget，撤销失败与原始错误并列返回。该后续改动未重新部署到已清理
服务器；它不改变上述实机场景的成功路径，并由失败注入单元测试、10 轮 race 与完整构建门禁覆盖。

## 质量门禁

- `make check`：PASS
- `go test ./internal/backup ./internal/restic ./internal/sshexec ./internal/systemd ./internal/cli -race -count=10`：PASS
- 常规 `go build ./cmd/ark`：PASS
- `CGO_ENABLED=0 go build ./cmd/ark`：PASS，产物为静态 ELF
- `git diff --check`：PASS
- 真实 `systemd-analyze verify`：Ark unit 无告警；仅出现服务器既有 fail2ban unit 的无关旧式 PIDFile 提示

## 清理与残留

- 已删除隔离 PostgreSQL、Redis、image 容器，普通 volumes、networks、独立 sshd、测试用户、
  ark units、`/var/lib/ark`、`/run/ark.lock`、`/var/cache/ark`、隔离目录和本次安装的 restic。
- 既有 nginx、Docker 和系统 sshd 清理后仍为 active；未修改 dnsmgr、MySQL、代理或其它业务资源。
- 唯一残留为 Docker volume `ark-live-validation-minio-data`，约 13 MB。内部未受保留的数据已清空，
  只剩 48 B 哨兵版本；保留截止时间为 `2026-08-13T04:31:01Z`，到期后可删除该 volume。
- 未使用 `--bypass`、缩短保留期或删除 MinIO 后端文件绕过对象锁。
- 管理密码、对象存储密钥、restic 密码、SSH 私钥、公钥原文和测试数据未写入本文件或 Git。
- 本次使用过 root 管理密码，验收结束后应立即轮换。
