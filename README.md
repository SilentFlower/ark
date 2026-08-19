<div align="center">

# ark

**Docker Compose 整机备份与重建**

把一台机器的数据库、数据卷、配置和镜像身份打包成快照，
在任意一台干净的新机器上重建出来。

</div>

---

## 这是什么

如果你的服务用 docker compose 部署，而你担心某天服务器突然没了——
ark 要保证那天你手上有的不只是一个 `.sql.gz`，而是**一整套能直接拉起来的东西**：

- 数据库（`pg_dump` 逻辑备份，不是不可靠的 PGDATA 热拷贝）
- 数据卷
- `docker-compose.yml`、`.env`、反代配置
- 当时运行的镜像 digest（不是会漂移的 `:latest`）

一台 hub 通过 SSH 管理所有机器。**被备份的机器上不装任何东西**——
只要有 docker 和 sshd 就行。加一台新机器的成本是在清单里加一段配置。

## 这不是什么

- **不是主备复制的替代品**。复制解决「机器挂了切过去」；备份解决「数据被删/被写坏」。
  逻辑错误会同步到备机，只有备份能救。两者互补，不互相替代。
- **不做秒级 RPO**。默认 24 小时，可配到小时级。
- **不做自动故障切换**。ark 只备份和恢复，不碰流量调度。

## 架构

```
机器 A ─┐
机器 B ─┼─ ssh ─►  hub  ─ restic(加密) ─►  对象存储 (R2/S3)
机器 C ─┘         编排 / 界面              开启对象锁
                  全部凭证
```

hub 是刻意设计的单点：它持有 restic 密码、对象存储凭证和各机 SSH 私钥，
能读所有备份、能登所有机器、能发起恢复。换来的是运维上的简洁——
一处配置、一处升级、一个界面。

代价用两条兜底摁住：**对象存储桶开启对象锁**（攻陷 hub 也删不掉备份），
**restic 密码另存离线副本**（hub 磁盘挂了也解得开）。详见 [设计文档](docs/design.md)。

## 当前状态

**P4 界面与告警阶段。** 备份、恢复、恢复演练、`ark-hub` 鉴权、HTTP API、Vue 控制台、
带 24 小时静默期的钉钉告警与外部心跳死人开关均已实现。进度见 [路线图](docs/roadmap.md)。

| 命令 | 状态 | 说明 |
|---|---|---|
| `ark validate` | ✅ | 校验多机清单的语法语义，不碰 docker 和网络 |
| `ark doctor` | ✅ | 检查 hub、指定 host 或清单中的全部运行环境 |
| `ark backup` | ✅ | 执行多 host 备份并持久化运行、target 与 doctor 结果 |
| `ark restore` | ✅ | 恢复 / 跨机重建 / 隔离恢复 / 只读目标预检 |
| `ark verify` | ✅ | 自动隔离恢复演练 |
| `ark-hub` | ✅ P4 | 鉴权、HTTP API、内嵌 Web 控制台与主动告警 |

> ⚠️ 清单格式已是 `version: 2`——**一份清单描述全部机器**，只放在 hub 上。
> P0 时期按单机写的 v1 清单需要迁移，`ark validate` 会直接给出迁移提示。
> `doctor` 无范围标志时只检查 hub；使用 `--host <name>` 检查指定目标机，
> 使用 `--all` 先检查 hub，再按清单顺序检查全部 host。

## 快速开始

```bash
# 构建
make build

# 准备清单。它只放在 hub 上，一份描述全部要备份的机器。
sudo mkdir -p /etc/ark
sudo cp examples/ark.yaml /etc/ark/ark.yaml
sudo vim /etc/ark/ark.yaml

# 校验清单本身（不碰 docker 和网络，任何机器都能跑）
./bin/ark validate -c /etc/ark/ark.yaml

# 校验 hub 自身是否真的具备执行条件
./bin/ark doctor -c /etc/ark/ark.yaml

# 校验指定目标机
./bin/ark doctor -c /etc/ark/ark.yaml --host web-01

# 完整体检：hub + 清单中的全部 host
./bin/ark doctor -c /etc/ark/ark.yaml --all
```

`validate` 通过时会逐台列出摘要：

```
清单校验通过: /etc/ark/ark.yaml
  3 台机器 / 12 个备份目标

  hub-01（本机）项目 ark-hub，3 个目标，*-*-* 04:17:00
  web-01（ssh 10.0.0.11:22）项目 sub2api，5 个目标，*-*-* 04:17:00
  db-01（ssh 10.0.0.12:22）项目 pgcluster，4 个目标，*-*-* 00,06,12,18:23:00
```

`doctor` 支持 `--json` 输出合并后的结构化报告。退出码：`0` 全部通过，
`1` 工具本身出错，`2` 检查未通过。
方便直接挂到监控上。

## 主动告警与死人开关

清单可选配置 `monitoring.env_file`，指向当前运行用户所有、权限不超过 `0600` 的普通文件：

```yaml
monitoring:
  env_file: /etc/ark/monitoring.env
```

秘密文件只允许以下四个键；不要把真实 URL 或签名密钥提交到仓库：

```dotenv
ARK_DINGTALK_WEBHOOK_URL=https://oapi.dingtalk.com/robot/send?access_token=REDACTED
ARK_DINGTALK_SECRET=REDACTED
ARK_HEARTBEAT_SUCCESS_URL=https://monitor.example/ping/REDACTED
ARK_HEARTBEAT_FAILURE_URL=https://monitor.example/ping/REDACTED/fail
```

钉钉告警复用控制台的三类健康投影，首次立即发送，持续故障每 24 小时最多重发一次，恢复时发送一次。
`ark backup` 的 `ok`/`warn` 调成功端点，`fail` 调失败端点；`--json` 和人类摘要都会显示
`heartbeat_status=disabled|sent|failed`。心跳失败不会改写备份 run、manifest 或既有退出码。
`ark-hub` 与 backup timer 仍彼此独立，停止 Hub 不影响定时备份和外部心跳。

## 恢复维护窗口与自动切换 DNS

同机或跨机原位恢复可以在目标 host 下关联 dnsmgr dmonitor 任务；跨机恢复还可以关联 A/AAAA
记录。顶层只保存服务地址和受限凭证文件路径，host 保存任务 ID、目标 IP、dnsmgr domain ID 与
provider record ID：

```yaml
dnsmgr:
  base_url: https://dns.example.com
  env_file: /etc/ark/dnsmgr.env

hosts:
  - host: web-02
    dnsmgr:
      task_ids: [21, 34]
      value: 203.0.113.10
      records:
        - domain_id: 12
          record_id: "provider-record-id"
```

`/etc/ark/dnsmgr.env` 必须由运行 ark 的用户所有、权限不超过 `0600`，且只包含
`ARK_DNSMGR_UID` 与 `ARK_DNSMGR_API_KEY`。维护与 DNS 计划都会进入 dry-run、inspect 和 preview
digest，但这些只读模式不会打开凭证文件或发 HTTP。真实恢复在安全备份完成后按顺序暂停任务，
在数据恢复和 DNS 切换全部结束后逆序恢复；暂停失败不会开始目标写入。配置的任务应在恢复前处于
启用状态，结束目标固定为 `active=1`。DNS 只在跨机恢复 completion marker 成功后切换，多记录失败
会逆序补偿，补偿不完整时结果会列出需人工核对的记录。

普通错误、取消和中断会执行恢复任务的兜底逻辑；`SIGKILL`、宿主机断电等无法运行 defer 的场景，
必须按结构化结果和清单人工把关联任务恢复为 `active=1`。详细发布、验收和回滚步骤见
[运维说明](docs/operations.md)。

## ⚠️ 关于密钥

**restic 仓库密码必须另外保存一份在 hub 之外**（密码管理器 / 离线介质）。

只存在于 hub 上的仓库密码，在 hub 损毁时会让所有备份一起变成无法解密的废数据——
这是备份系统最常见、也最致命的失败方式。

SSH 私钥同理，且**密钥本身不进备份**：把开锁的钥匙和锁着的箱子放在同一个地方没有意义。
`monitoring.env` 同样不得进入备份，否则群机器人与外部监控凭证会随快照扩散。
`dnsmgr.env` 也不得进入备份；它包含可修改 DNS 记录的 AuthApi 凭证。

## 开发

```bash
make check      # 格式化 + vet + 测试（纯 Go，不需要 node）
make build      # 编译 ark 到 bin/
make test       # 只跑测试

make hub        # 构建前端并编译 ark-hub 到 bin/
make web-check  # 前端 lint + 类型检查 + 单测
```

要求 Go 1.22+。改动 `web/` 还需要 Node 20+ 与 pnpm。

**发布 `ark-hub` 必须用 `make hub`。** 前端产物不进版本库，直接 `go build` 得到的
二进制里只有一页占位提示（API、登录和备份调度不受影响，但界面是空的）。

运行时依赖分两侧：

- **hub**：`restic`、`ssh`、`systemd`、`docker`（hub 自己也被备份时需要）
- **目标机**：`docker`、`docker compose v2`、`sshd`——**不需要装 ark 或 restic**

`ark-hub` 的界面随二进制分发，**部署环境不需要 node**。

## 文档

- [设计文档](docs/design.md) — 架构、ADR、取舍理由
- [路线图](docs/roadmap.md) — 分阶段计划与验收标准
- [清单示例](examples/ark.yaml) — 带注释的完整配置

## License

MIT
