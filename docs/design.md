# ark 设计文档

> 本文记录 ark 的设计目标、架构决策和取舍理由。
> 实现细节以代码为准，但**为什么这么做**只记录在这里。

## 1. 背景

生产服务用 docker compose 部署在若干台机器上。当前的风险是：

- 机器可能突然宕机、被回收、磁盘损坏，短时间内拿不回任何数据。
- 已有的主备流复制方案（如果有）解决的是「机器挂了切过去」，**解决不了误删表、
  升级写坏数据、勒索软件加密**——这类逻辑错误会实时同步到备机。
- 应用自带的数据库备份只覆盖数据库。机器整个没了之后，手上有一个 `.sql.gz`，
  但没有 compose 文件、没有 `.env`、没有数据卷、不知道当时跑的是哪个镜像，
  **新机器依然拉不起来**。

**复制不是备份。** ark 要解决的是后者：一份能回到过去某个时间点、
且独立于生产机器存在的完整副本。

## 2. 目标与非目标

### 目标

- **整机可重建**：在一台干净的新服务器上，把服务完整拉起来，
  包括数据库、数据卷、编排文件、配置密钥和镜像身份。
- **集中编排**：一台 hub 管理所有机器的备份与恢复。加一台新机器的成本
  是「在清单里加一段配置」，不是「登上去装一套东西」。
- **目标机零安装**：被备份的机器上不需要安装 ark、restic 或任何 ark 专属组件。
- **可观测**：一个 Web 页面看到所有机器的备份状态，谁多久没备份了一眼看见。
- **可验证**：备份能被自动演练恢复，而不是等到真出事那天才知道它是坏的。
- **长期可维护**：单个静态二进制，配置声明式且拒绝静默失败。

### 非目标

- **不做秒级 RPO**。默认 24 小时，可配到小时级。要做到「恢复到误删前一秒」需要
  WAL 归档（pgBackRest / wal-g），复杂度高一个量级且只覆盖 PostgreSQL，
  暂不纳入。见 §12。
- **不做自动故障切换**。ark 不参与流量调度、不做 fencing、不自动提升任何节点。
  DNS 层的故障转移由 dnsmgr 负责，见 §11。
- **不做通用备份平台**。范围严格限定在 docker compose 部署形态。
- **不做多租户**。hub 假定由一个团队独占，不设计跨团队的权限隔离。

## 3. 架构与信任边界

```
┌──────────────┐         ┌──────────────┐         ┌──────────────┐
│   机器 A     │         │   机器 B     │         │   机器 C     │
│ docker+sshd  │         │ docker+sshd  │         │ docker+sshd  │
└──────┬───────┘         └──────┬───────┘         └──────┬───────┘
       │                        │                        │
       └────────────ssh─────────┼────────ssh─────────────┘
                                │
                    ┌───────────▼────────────┐
                    │        hub             │
                    │  ark (oneshot, timer)  │  编排 / 执行
                    │  ark-hub (常驻)        │  界面 / API
                    │  restic + 全部凭证     │
                    │  /var/lib/ark/ark.db   │  状态
                    └───────────┬────────────┘
                                │ restic（加密）
                    ┌───────────▼────────────┐
                    │   对象存储 (R2/S3)     │
                    │   单一 restic 仓库     │
                    │   开启对象锁/仅追加    │
                    └────────────────────────┘
```

三方职责：

| 组件 | 职责 | 持有 |
|---|---|---|
| 目标机 | 提供 docker 和 SSH 入口，被动执行命令 | 自己的业务数据 |
| hub | 编排、执行、加密、上传、展示 | **restic 密码 + 对象存储凭证 + 各机 SSH 私钥** |
| 对象存储 | 存放加密的 restic 仓库 | — |

**hub 是刻意设计的单点。** 它持有打开一切的钥匙：能读所有备份内容，
能 SSH 进所有生产机，能发起覆盖生产数据的恢复。这是一个明确的取舍——
用「集中控制带来的运维简洁」换「集中带来的风险」，前提是这台机器本身被妥善保护。

对应的兜底见 ADR-006 和 §8：**对象存储桶必须开启对象锁 / 仅追加保留**，
**restic 密码必须另存一份离线副本**。这两条让 hub 整台丢失或被攻陷时，
备份数据本身不会跟着消失。

## 4. 关键决策

### ADR-001：用 restic，不自己写备份引擎

自己实现增量、去重、加密、完整性校验和保留策略，等于把 restic 重写一遍且没经过考验。
ark 真正的价值在**编排层**：知道该备份什么、按什么顺序恢复。

restic 直接提供：内容寻址去重、客户端加密（对象存储被拖库也读不了）、
快照语义、多后端（S3/R2/SFTP/本地）、`restic check` 完整性校验、`forget --prune` 保留策略。

代价：多一个外部二进制依赖。可接受——它只需要装在 hub 一台机器上，`doctor` 会检查。

### ADR-002：hub 集中编排，目标机 agentless

**取代了早期的「每台机器装 agent 主动 push」方案。**

hub 通过 SSH 在目标机上执行命令，把输出流拉回本地送进 restic：

```
ssh web-01 'docker compose exec -T postgres pg_dump ...' | restic backup --stdin
```

目标机上因此**只需要 docker 和 sshd**，不装 ark、不装 restic、不配 systemd timer、
不放任何密钥。加一台机器 = 在清单里加一段配置；升级 ark = 只升 hub 一台。

早期方案（每机 agent + push 到自己的 prefix）的好处是权限最小化：
hub 不持有任何生产机凭证，被攻陷也只能看到元数据。它被放弃的原因是
**运维成本和能力上限**——N 台机器意味着 N 份安装、N 次升级、N 个密钥副本，
而且 hub 退化成纯观测面之后，「在页面上点一下恢复」这类需求根本无法实现。

这个取舍的代价是真实的，不要粉饰：**拿下 hub 等于拿下全部生产机和全部备份。**
接受它的前提是 hub 被当作最高等级的资产来保护，且 ADR-006 的两条兜底必须落实。

一个反直觉的收益：早期方案里「机器没了，新开一台空白 VPS」这个核心场景
hub 帮不上忙——新机器不认识 hub。现在方向反过来了，**是 hub 认识新机器**，
只要能 SSH 进去就能全自动重建。

### ADR-003：数据库走逻辑备份，绝不打包 PGDATA 卷

运行中的 PostgreSQL 数据目录做热拷贝，得到的是撕裂的、不一致的快照，
恢复时可能根本起不来，而且**在恢复之前你不会知道**。

因此 postgres 类型固定走 `docker compose exec -T <service> pg_dump`。
Redis 同理，先 `BGSAVE` 再取 RDB。

附带的好处：`pg_dump` 在数据库容器内部执行，客户端与服务端是同一个镜像，
版本天然一致，不存在「客户端版本低于服务端导致 dump 失败」的问题。

只有确定与数据库无关的卷（上传文件、静态资源）才允许直接打包。

### ADR-004：镜像用 digest 不用 tag

备份时记录容器实际运行镜像的 `RepoDigests`（`repo@sha256:...`），恢复时按 digest 拉取。

tag 是可变的。半年后的 `myapp:latest` 可能已经是三个大版本之后，
数据库 schema 与备份内容不兼容，恢复出来的服务会以难以诊断的方式失败。

硬性契约：必须从**正在运行的容器**的镜像 ID 反查 digest，不能用 compose 里写的 tag；
`RepoDigests` 为空或匹配到多个仓库时**必须失败**，不能猜。

### ADR-005：备份仍由 systemd timer 触发 oneshot 进程，只是 timer 搬到了 hub

agent 模式取消之后，调度自然要集中，但**调度方式不变**：hub 上一个
systemd timer 触发 `ark backup`，跑完退出。

刻意**不**把调度做进常驻的 `ark-hub` 进程。常驻进程会引入一整类故障：
进程自己崩了、内存泄漏、时钟漂移导致漏跑，而且**没有任何外部信号**——
你以为它在跑，其实三个月前就死了。把调度留在 systemd 手里意味着：
`ark-hub` 挂掉只是看不到界面，**备份照常执行**。

配套决定：调度表达式用 systemd `OnCalendar` 语法，
语法校验直接委托给 `systemd-analyze calendar`，避免自己写一个近似的解析器。

`ark-hub` 需要触发临时操作时，起一个 `ark` 子进程，而不是自己执行长任务。

### ADR-006：hub 持有全部凭证，代价用对象锁和离线密钥兜底

**取代了早期的「中心机只读状态元数据、不持有 restic 密码」方案。**

hub 持有 restic 仓库密码、对象存储读写凭证、各目标机的 SSH 私钥。
这是 ADR-002 的必然结果，也是「在页面上发起恢复」的前提。

被放弃的隔离性必须用别的方式补回来，以下两条是**设计的一部分，不是建议**：

1. **对象存储桶开启对象锁 / 仅追加保留（Object Lock / Immutability）。**
   保留期内的对象无法被删除或覆盖，即使用的是 hub 上那把有写权限的凭证。
   这直接掐断了「攻陷 hub → 删光备份 → 加密生产」这条勒索路径。
   代价是 `forget --prune` 无法真正回收保留期内的空间，需要把保留期
   和保留策略配成一致的量级。
2. **restic 密码在 hub 之外另存一份离线副本**（密码管理器 / 离线介质）。
   只存在于 hub 上的仓库密码，在 hub 磁盘损毁时会让所有备份一起
   变成无法解密的废数据。

第二条与 ADR-012（hub 自身状态的保全）配套。

### ADR-007：清单里的未知字段一律报错

YAML 解析开启 `KnownFields(true)`。写错一个字段名会立刻失败，而不是被静默忽略。

对普通应用，静默忽略未知配置只是小问题；对备份工具，它意味着
**你以为某个目标在备份、实际它从未被执行过**，而这个事实只会在
真正需要恢复的那天暴露。

同理，`Validate` 也会拒绝「字段填在了错误的类型下」，例如给 `redis` 目标写 `database:`。

### ADR-008：校验分三层——静态、hub 本地、目标机远程

- `ark validate` 只检查清单本身，全程不碰文件系统、不连网络、不 SSH。
- `ark doctor --local` 检查 hub 自己：restic / ssh 客户端是否存在、
  密钥文件权限是否为 0600、对象存储是否可达、仓库是否能解锁。
- `ark doctor --host <name>` 通过 SSH 检查目标机：能否登录、
  docker 与 compose v2 是否可用、清单里引用的 service / volume / 路径是否真实存在。

备份系统最常见的失败模式不是代码写错，而是**三个月前某个前提条件悄悄变了而没人发现**。
`doctor` 把这些前提条件变成可以定期自动检查的断言。远程检查尤其重要——
目标机上的 compose 文件被人改了服务名，只有 doctor 会告诉你。

### ADR-009：所有机器共用一个 restic 仓库，按 tag 区分

早期每机一个 prefix 的设计是为了配合「各机凭证只能写自己 prefix」的隔离模型。
该模型随 ADR-002 一起消失，因此改为单一共享仓库。

收益是**跨机去重**：多台机器跑着相同的基础镜像、相同的配置模板、
相同的系统文件，共享仓库能把这些只存一份。机器越多收益越大。

每个快照打三类 tag：`host:<name>`、`target:<id>`、`run:<run-id>`。
按 host 检索、按 host 应用保留策略都通过 tag 过滤完成。

代价与约束：

- 仓库损坏会影响全部机器。用 `restic check` 定期校验，用对象锁防删除。
- `prune` 需要仓库排他锁，因此**所有 host 的备份串行执行，全部完成后统一 prune 一次**。
  串行也顺带避免了多台机器同时 dump 打爆 hub 带宽。

### ADR-010：数据流式经过 hub，不落 hub 磁盘

`ssh ... | restic backup --stdin` 是纯流式的，hub 不需要与备份总量相当的临时空间。
这一点必须在实现中守住：**任何「先下载到本地文件再上传」的写法都是错的**，
它会让 hub 的磁盘容量成为备份能力的上限。

由此产生一个与早期设计的差异：`files` 类型**也必须走 stdin**。
早期方案里 agent 在本地跑 `restic backup <paths>`，agentless 之后 hub 上没有这些路径，
因此改为 `ssh host 'tar -cpf - <paths>' | restic backup --stdin`。

代价：restic 从「文件级去重」退化为「tar 流的块级去重」。
tar 头部的时间戳变化会影响边界，但内容相同的大块仍然能去重。
对 compose 文件、`.env` 这类小文件完全无所谓；
如果将来出现体积很大的 `files` 目标，再考虑单独处理。

`-p` 不能省，否则恢复出来的文件权限和属主全丢。

### ADR-011：SSH 流截断必须被主动检出

这是 agentless 方案**最危险的地方**，早期的本地 pipe 方案没有这个问题。

`restic backup --stdin` 在 stdin 提前 EOF 时会正常结束并生成一个快照。
如果 SSH 连接中途断开、远程 `pg_dump` 中途失败、或者目标机 OOM 杀掉了进程，
**restic 会心安理得地存下一个被截断的 dump 并报告成功**。
这种快照在恢复那天才会暴露，而那正是最不能出问题的时刻。

因此：

1. `restic` 完成后**必须检查 SSH 子进程的退出码**。非零则本次 target 判定为失败，
   并把已经产生的快照 `forget` 掉——宁可没有，不要有一个假的。
2. 记录每次备份的字节数并与上一次比对，跌幅超过阈值（如 50%）时告警。
   截断未必伴随非零退出码（例如远程进程被 SIGKILL 而 shell 仍返回 0 的边缘情况），
   体积突变是第二道防线。

恢复方向（`restic dump | ssh host 'psql'`）同样适用这两条。

**不需要 `set -o pipefail`。** 五种 target 的远程命令全都是单条命令，
远程侧没有管道——管道在 hub 本地（ssh 的 stdout 接 restic 的 stdin），
退出码由 Go 侧的 `Wait` 拿。将来真出现远程管道时再单独论证，
不要为了「保险」提前套一层 shell：`ssh` 的远程端本来就会经登录 shell 解析，
多包一层只增加转义面而不增加安全性。相关约定见
`.trellis/spec/backend/external-command-guidelines.md`。

### ADR-012：hub 自身的状态必须能在别处重建

hub 持有全部清单、状态库和密钥。**hub 的磁盘挂掉比任何一台生产机挂掉都严重。**

因此 hub 把自己也作为一个备份目标（清单中 `local: true` 的条目），
备份 `/etc/ark/ark.yaml` 和 `/var/lib/ark/ark.db`，
以及 hub 上其它服务的数据（例如 dnsmgr 的 MySQL）。

**但密钥不进备份。** SSH 私钥和 restic 密码不作为备份目标——
把开锁的钥匙和锁着的箱子放在同一个地方没有意义。它们走离线介质。

hub 的重建路径因此是：新机器 → 装 ark 和 restic → 从离线介质取回
restic 密码和对象存储凭证 → `restic restore` 取回清单和状态库 →
从离线介质取回 SSH 私钥 → 恢复完毕。这条路径必须在 P3 的实测中走一遍。

### ADR-013：ark 不做流量调度，与 dnsmgr 分进程协作

hub 上同时运行着 dnsmgr（DNS 聚合管理 + DNS 层故障转移 + 证书签发部署）。
两者**同机部署，但不同进程、不共享代码**。

理由：dnsmgr 是 ThinkPHP + MySQL 的上游开源项目 fork，
执行模型（PHP-FPM 请求 / CLI 检测进程）与 ark 需要的 GB 级流式长任务不匹配；
且往 fork 里塞功能会让每次追上游都变成冲突处理。

协作方式与冲突见 §11。

## 5. 备份清单

hub 上一份 `/etc/ark/ark.yaml` 描述所有机器。

```yaml
version: 2

# 全局唯一的 restic 仓库
repo:
  type: restic
  url: "s3:https://<account>.r2.cloudflarestorage.com/<bucket>"
  password_file: /etc/ark/repo.pass
  env_file: /etc/ark/repo.env

# 各 host 未显式覆盖时套用
defaults:
  schedule:
    on_calendar: "*-*-* 04:17:00"
  retention:
    daily: 7
    weekly: 4
    monthly: 6

hosts:
  - host: web-01
    ssh:
      address: 10.0.0.11:22
      user: root
      identity_file: /etc/ark/ssh/web-01.key
      known_hosts_file: /etc/ark/ssh/known_hosts
    project:
      name: sub2api
      compose_file: /root/sub2api/deploy/docker-compose.yml
      env_file: /root/sub2api/deploy/.env
      project_name: deploy
    targets:
      - {type: postgres, service: postgres, database: sub2api, user: sub2api}
      - {type: redis, service: redis}
      - {type: volume, name: sub2api_data}
      - {type: files, name: deploy-config, paths: [...]}
      - {type: image_digest, services: [sub2api]}

  # hub 自己，不走 SSH
  - host: hub
    local: true
    project: {...}
    targets: [...]
```

`version` 从 1 升到 2：单机清单到多机清单是不兼容变更，
`Load` 遇到 version 1 的清单应当给出明确的迁移提示而不是含糊报错。

五种备份目标不变：

| 类型 | 做什么 | 必填字段 |
|---|---|---|
| `postgres` | `pg_dump` 逻辑备份 | `service`, `database` |
| `redis` | `BGSAVE` 后取 RDB | `service` |
| `volume` | 打包整个 docker volume | `name` |
| `files` | 打包宿主机路径 | `name`, `paths` |
| `image_digest` | 记录运行镜像 digest | `services` |

**SSH 配置的硬性要求**：`known_hosts_file` 必填，严格校验主机指纹。
绝不允许 `StrictHostKeyChecking=no`——hub 会把生产数据流经这条连接，
中间人劫持意味着数据泄露和恢复投毒。

## 6. 备份流程

```
ark backup [--host <name>]
  ├─ 加载并校验清单
  ├─ doctor --local（失败即中止）
  ├─ EnsureInit 仓库
  ├─ 逐个 host 串行执行（ADR-009）
  │    ├─ doctor --host（失败则跳过该 host，不中止其余）
  │    └─ 逐个 target：
  │         postgres     → ssh 'compose exec -T pg_dump'    → restic --stdin
  │         redis        → ssh 'BGSAVE; 轮询 LASTSAVE; cat' → restic --stdin
  │         volume       → ssh 'docker run --rm -v v:/src:ro alpine tar -cpf -'
  │                                                          → restic --stdin
  │         files        → ssh 'tar -cpf - <paths>'          → restic --stdin
  │         image_digest → ssh 'compose ps --format json' + 'image inspect'
  │                                                          → JSON → restic --stdin
  │         └─ 校验 SSH 退出码与体积（ADR-011）
  ├─ 写 manifest（本次的 run-id、各 target 的 snapshot id、镜像 digest 映射）
  ├─ 全部 host 完成后统一 restic forget --prune
  └─ 写入本地状态库 /var/lib/ark/ark.db
```

关键约束：

- **单实例锁**：`flock` 于 `/run/ark.lock`。手动触发和 timer 撞车会让两个
  `pg_dump` 同时跑，白白加倍数据库压力。拿不到锁直接退出，不排队。
- **stdin 备份的 `--stdin-filename` 必须跨次运行保持稳定**，
  例如固定成 `web-01/postgres/sub2api.sql`。文件名变了 restic 会当成新文件，
  去重效果归零。
- **不要在送进 restic 之前压缩。** restic 自己会压缩，并且靠内容分块去重；
  预先 gzip 会让每次 dump 的字节流完全不同，去重率直接归零。
- **SSH 关闭压缩**（`-o Compression=no`）。restic 会压，SSH 再压一遍
  是纯浪费 CPU，而 hub 的 CPU 是所有机器共用的。
- **单个 target 失败不中止其余**。半份备份也比没有强；但整体退出码非零，
  失败信息进 manifest 和状态库。

一次备份 = 多个 restic 快照（每个 target 一个）+ 一份把它们串起来的 manifest。
manifest 自身也作为快照存入，打 tag `ark-manifest`。恢复第一步就是取最新的 manifest。

## 7. 恢复流程

恢复的顺序不能错，否则会得到一个看似启动成功、实际数据不完整的服务：

```
ark restore --host web-01 [--to <target-host>] [--snapshot latest] [--dry-run]
  1. 取 manifest，生成 Plan 并展示将要恢复的内容和目标位置
  2. 恢复 files（compose 文件、.env、反代配置）  hub → ssh 'tar -xpf -'
  3. 按记录的 digest 拉取镜像                     ssh 'docker pull repo@sha256:...'
  4. 创建 volume 并恢复卷数据                     hub → ssh 'tar -xpf -'
  5. 只启动数据库服务，等待健康
  6. 灌入数据                                     hub → ssh 'compose exec -T psql'
  7. 启动应用服务
  8. 健康检查 + 输出人工确认清单
```

`--to` 允许把 A 机器的备份恢复到 B 机器，这是跨机重建的入口：
目标机只要有 docker 和 sshd，且已加入清单的 `hosts`（或通过命令行临时指定 SSH 参数）。

强制要求：

- **`--dry-run` 必须只读**：生成 Plan 并打印，不创建容器、不写文件、不拉镜像。
- **幂等**：每步先检查当前状态再决定做不做。中途失败后重跑同一条命令必须能继续，
  而不是留下一个半坏的环境。
- **拒绝隐式覆盖**：目标机上已存在同名 volume 或容器时必须报错退出，除非显式 `--force`。
  恢复操作把生产数据覆盖掉是不可逆的。
- **恢复顺序不能并行化**。数据库没起来就灌数据只会失败得很难诊断。
- **破坏性恢复前先给现状打一份快照**。目标机上有正在运行的服务时，
  恢复前自动备份当前状态，失败则中止。这让误操作从不可逆变成可回滚。
- ADR-011 的流完整性校验同样适用于恢复方向。

第 8 步的人工确认清单包括：DNS 指向、TLS 证书、防火墙端口、
以及 `.env` 里需要按新环境调整的项。其中前两项可以与 dnsmgr 联动自动化，见 §11。

## 8. 密钥与信任边界

这是整个系统**最容易致命**的部分。

需要在 hub 之外独立保管的密钥：

| 密钥 | 丢失后果 |
|---|---|
| restic 仓库密码 | **所有备份变成无法解密的废数据** |
| 对象存储凭证 | 无法访问备份（可在控制台重新签发，不致命） |
| 各机 SSH 私钥 | 无法编排（可重新分发公钥，不致命） |
| 应用 `.env` 中的加密密钥 | 数据库能恢复，但加密字段全部读不出来 |

规则：

1. `repo.password_file` 的内容必须同时存在于密码管理器或离线介质中。
   只存在于 hub 上的仓库密码，在 hub 损毁时和没有备份是等价的。
2. 所有密钥文件权限必须是 `0600`，`ark doctor --local` 强制检查。
3. 密钥本身不进备份（ADR-012）。
4. 清单文件不含密钥（只含文件路径），但含仓库地址和内网拓扑，
   因此 `.gitignore` 排除真实清单，只提交 `examples/`。
5. 对象存储凭证按最小权限签发：hub 需要读写，但**不需要删除权限**——
   配合对象锁，`prune` 由生命周期规则在保留期后执行。
6. SSH 私钥每台机器一把，不复用。一把泄露不等于全线失守。

## 9. hub 的状态与界面

早期设计里 agent 需要把状态写到对象存储的 `_status/<host>.json`，
因为中心机没有别的途径知道 agent 干了什么。**hub 自己就是执行者之后，
这条路径整个消失了**——状态直接落在本地。

状态库 `/var/lib/ark/ark.db`（SQLite，WAL 模式）记录：

- 每次运行（run）的 host、开始/结束时间、结果、耗时
- 每个 target 的结果、字节数、restic snapshot id、错误信息
- 每次 doctor 的报告
- 每次演练的结果
- 下一次计划运行时间（来自 `systemd-analyze calendar`）

选 SQLite 而不是 MySQL/Postgres：单文件便于整体备份（ADR-012），
零运维，且这个数据量下性能完全不是问题。`ark`（oneshot）写入，
`ark-hub`（常驻）读取，WAL 模式下并发安全。

`ark-hub` 提供：

- **总览**：每台机器一张卡片——最近备份时间、状态、大小趋势、下次计划时间
- **主机详情**：target 明细、历史快照、doctor 报告、演练结果
- **告警**：超时未备份、连续失败、演练失败。对接钉钉机器人，**告警要有静默期**，
  否则一台机器坏掉会每轮刷一次群。
- **操作**：触发备份、触发演练、发起恢复（起 `ark` 子进程执行）
- **HTTP API**：供 dnsmgr 之外的系统集成

健康判定：`最近成功备份时间 > 计划周期 × 2` 判为超时告警。

**`ark-hub` 必须有鉴权。** 它能发起覆盖生产数据的恢复，内网部署也不例外。

前端 Vue 3 + Vite + TypeScript + Pinia + Tailwind（与现有项目同栈），
构建产物用 `go:embed` 打进 `ark-hub` 单二进制，部署时不需要 node 环境。

**死人开关**：`ark-hub` 是常驻进程，它自己哑火了不会有人知道。
备份调度不依赖它（ADR-005），但告警依赖它。因此 `ark backup` 每次跑完
向外部监控（healthchecks.io 之类）推一次心跳——由外部系统发现「ark 整个没动静了」。

## 10. 恢复演练

**没有验证过的备份等于没有备份。**

`ark verify` 定期（建议每周）挑一个快照，恢复到一个隔离环境，跑健康检查，通过后销毁：

- compose 项目名加 `-verify` 后缀，端口整体偏移，volume 独立命名
- **绝对不能碰生产项目的任何资源**——所有操作前校验 `com.docker.compose.project` 标签
- 演练环境不绑定生产域名和生产 IP，避免被 dnsmgr 的健康检测扫到（见 §11）
- 结果写入状态库，在 hub 页面作为一等公民展示

不做这件事，最可能的结局是：`pg_dump` 三个月前就在报错，
而你在需要恢复的那天才发现。

演练可以在专用的隔离机器上执行，也可以在原机器上执行。前者更接近真实重建，
后者省一台机器但只能验证「数据能读出来」，验证不了「换台机器也能跑」。

## 11. 与 dnsmgr 的联动

hub 上同时运行 dnsmgr（`SilentFlower/dnsmgr`，上游 `netcccyun/dnsmgr`）。
它提供 DNS 聚合管理、**DNS 层故障转移**（ping/tcp/http 检测 → 自动切换解析记录）、
证书签发与自动部署。

注意它做的是 DNS 故障转移，不是负载均衡：没有流量分发，切换生效受 TTL
和各地 DNS 缓存制约。ark 不重复造这块能力，也不承担流量调度职责（ADR-013）。

### 11.1 恢复完成后的自动化

`design.md` §7 第 8 步的人工确认清单里，前两项可以交给 dnsmgr：

- **DNS 指向** → 调 dnsmgr 的 `POST /api/record/update/:id`，把域名切到新机器 IP。
  该路由在 `Route::group('api', ...)` 里、由 `AuthApi` 中间件保护，ark 用 API token 即可调用。
- **TLS 证书** → dnsmgr 的证书自动部署支持 `ssh`、`local` 等 40+ 目标，
  可以把证书推到重建后的机器。

恢复流程的最后一公里因此从「打印一张清单让人去点」变成「调两个 API」。

### 11.2 一个会真打架的地方

**dnsmgr 的健康检测会把 ark 正在恢复的机器判死。**

恢复过程中服务必然有一段不可用，检测到了就自动切走解析；恢复完再切回来——
如果检测间隔配得短，中间可能来回震荡。

因此 ark 在发起恢复前应当暂停对应的 dmtask，完成后恢复。

**现存障碍**：dnsmgr 的 `/dmonitor/task/:action` 路由挂在 `CheckLogin` 会话组里，
**不在 `AuthApi` 组**，ark 用 API token 调不到。打通需要给 dnsmgr fork
加一个小补丁，把 dmtask 的启停暴露到 API 组。

在补丁落地之前，恢复流程把「请先暂停 dnsmgr 对该主机的检测」放进人工确认清单。

演练（`ark verify`）不受此影响，因为演练环境不绑定生产域名和 IP。

## 12. 已知风险与未决问题

| 风险 | 现状 | 缓解 |
|---|---|---|
| **hub 是单点，被攻陷等于全线失守** | 已知取舍（ADR-002/006） | 对象锁 + 离线密钥 + hub 按最高等级资产保护 |
| **SSH 流截断产生「成功的坏备份」** | 设计已覆盖（ADR-011） | 退出码校验 + 体积突变告警 + 演练 |
| hub 带宽成为总量上限 | 未处理 | 串行执行；机器数量增长时评估分流或改回直传 |
| RPO 最坏等于一个备份周期 | 默认 24h | 可配到小时级；需要更小则要上 WAL 归档 |
| 大库全量逻辑备份耗时长 | 未处理 | restic 去重只减少传输量，不减少 dump 时间 |
| 恢复到新机器后 IP/域名变化 | 部分处理 | 人工确认清单；后续与 dnsmgr 联动自动化（§11） |
| 对象存储账号本身被封 | 未处理 | 后续支持同时写入第二个后端 |
| 单机多 compose 项目 | 未支持 | 清单当前假设一台机器一个项目 |
| 共享仓库损坏影响全部机器 | 已知取舍（ADR-009） | `restic check` 定期校验 + 对象锁 |

待决问题：

| 问题 | 阶段 | 现在的倾向 |
|---|---|---|
| 对象锁保留期与 restic 保留策略如何对齐 | P2 | 保留期 = 月备保留时长，prune 交给生命周期规则 |
| `ark verify` 的频率与执行位置（原机 / 专用隔离机） | P3 | 每周一次；先在原机跑通，有条件再上专用机 |
| `ark-hub` 的鉴权形式（本地账号 / OIDC / 反代托管） | P4 | 先做本地账号 + TOTP，与 dnsmgr 保持一致的体验 |
| dnsmgr fork 的 API 补丁何时提交 | P5 | 与联动功能同批做，尽量做成能回馈上游的形态 |
| manifest schema 的向后兼容策略 | P6 | 新版必须能读旧 manifest，写入始终用最新版 |
