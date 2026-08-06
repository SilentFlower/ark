# 路线图

> 这是一份**可以照着做**的执行计划。每个任务都写清了要动哪些文件、
> 关键设计已经定了什么、以及怎么算做完。
>
> 隔一段时间回来接手时，先读 [`design.md`](design.md) 的 §3 架构和 §4 ADR，
> 再从下面第一个未打勾的任务开始。

## 开发约定

- 提交前跑 `make check`（格式化 + vet + 测试），不绿不提交。
- 提交信息用中文，格式 `feat: / fix: / docs: / refactor: 简述`。
- **改变了某个设计决策，先更新 `design.md` 的 ADR，再改代码。**
  代码和文档不一致时，以文档为准去修代码——反过来会让这个项目在半年后失去可维护性。
- 新增外部依赖要在 `doctor` 里加对应检查。
- 涉及密钥的代码路径，错误信息里绝不能回显密钥内容。

## 阶段排序的理由

**先让 agent 能产出真实数据，再做展示。** 中心机页面展示的是 agent 上报的状态；
agent 还没跑起来时做前端只能对着假数据调样式，等真实数据结构定下来还要返工。

**P2 是整个项目的成败点。** 备份写出去容易，能在一台陌生机器上恢复回来才是目的。
P1 做完先别急着做 P3，把 P2 走通再说。

---

## P0 — 项目基础 ✅ 已完成

- [x] 项目骨架、Makefile、构建与测试流水线
- [x] 备份清单数据模型（5 种 target 类型）
- [x] `ark validate`：静态语义校验，拒绝未知字段和错位字段
- [x] `ark doctor`：运行环境检查
- [x] 设计文档与 ADR

---

## P1 — 备份可用

**阶段目标**：在一台真机上跑出第一份可以被 `restic snapshots` 列出来的真实备份，
并且由 systemd timer 自动执行。

粗估 3–4 天。

### P1-1 restic 封装 `internal/restic/`

把 restic CLI 包装成 Go 接口。

**新增文件**：`internal/restic/repo.go`、`internal/restic/repo_test.go`

**接口草案**：

```go
type Repo struct{ /* url, passwordFile, env */ }

func New(cfg *config.Repo) (*Repo, error)
func (r *Repo) EnsureInit(ctx context.Context) error          // 仓库不存在时才 init，幂等
func (r *Repo) BackupStdin(ctx context.Context, stdin io.Reader, filename string, tags []string) (Snapshot, error)
func (r *Repo) BackupPaths(ctx context.Context, paths []string, tags []string) (Snapshot, error)
func (r *Repo) Snapshots(ctx context.Context, tags []string) ([]Snapshot, error)
func (r *Repo) Forget(ctx context.Context, policy config.Retention, tags []string) error
func (r *Repo) Check(ctx context.Context) error
```

**已定的设计点**：

- 一律用 `--json` 解析输出。restic 的人类可读输出格式会变，JSON 不会。
- 对象存储凭证从 `repo.env_file` 读入后**只注入到 restic 子进程的环境**，
  不要写进 `os.Setenv`——否则会泄漏给后续所有子进程（包括 `docker compose exec`）。
- 密码用 `RESTIC_PASSWORD_FILE`，不要用 `RESTIC_PASSWORD`（会出现在 `ps` 里）。
- **stdin 备份的 `--stdin-filename` 必须跨次运行保持稳定**，
  例如固定成 `postgres/sub2api.sql`。文件名变了 restic 会当成新文件，去重效果归零。
- 包装错误时确保不回显 `env_file` 的内容。

**验收**：以 `/tmp/ark-test-repo` 本地目录作为 repo 的集成测试，
能完成 init → backup stdin → snapshots 列出 → forget，且重复 `EnsureInit` 不报错。
测试用 `testing.Short()` 跳过，避免 CI 里必须装 restic。

---

### P1-2 target 执行器 `internal/backup/`

**新增文件**：`internal/backup/executor.go`（接口 + 分发）、
`postgres.go`、`redis.go`、`volume.go`、`files.go`、`image.go`，各配测试。

**关键：不要在送进 restic 之前压缩。**

这一点和 sub2api 现有的 `pg_dump | gzip → S3` 不一样，理由是那里没有去重层，
gzip 是净收益；而 restic 自己会压缩，并且靠内容分块做去重——
预先 gzip 会让每次 dump 的字节流完全不同，去重率直接归零，
每天都在传一份全量。**送原始的 dump 流给 restic。**

各执行器的命令：

| 类型 | 命令 | 注意事项 |
|---|---|---|
| `postgres` | `docker compose exec -T <svc> pg_dump -U <user> -d <db> --no-owner --no-acl --clean --if-exists` | `-T` 必须有，否则 TTY 控制字符会污染输出流 |
| `redis` | `exec -T <svc> redis-cli BGSAVE` → 轮询 `LASTSAVE` 变化 → `exec -T <svc> cat /data/dump.rdb` | BGSAVE 是异步的，必须等 `LASTSAVE` 时间戳变化才能取文件 |
| `volume` | `docker run --rm -v <vol>:/src:ro alpine tar -cf - -C /src .` | `:ro` 不能省 |
| `files` | 直接 `restic backup <paths>` | 不走 stdin，restic 原生支持路径 |
| `image_digest` | `docker compose ps --format json` → `docker image inspect` 取 `RepoDigests` | 见下 |

**`image_digest` 的硬性契约**（这是 sub2api 容灾方案里已经踩过的坑）：

- 必须从**正在运行的容器**的镜像 ID 反查 digest，不能直接用 compose 里写的 tag。
- `RepoDigests` 为空、或匹配到多个仓库时**必须失败**，不能猜。
  拿不到确定的 digest，就意味着这份备份恢复时无法保证跑的是同一个版本。

**验收**：每个执行器有单测（用假 exec 注入）；
在装了 docker 的机器上能对真实 compose 项目产出非空输出流。

---

### P1-3 快照清单 `internal/backup/manifest.go`

一次备份 = 多个 restic 快照（每个 target 一个）+ 一份把它们串起来的 manifest。

**manifest 内容**：schema 版本、host、project、ark 版本、开始/结束时间、
每个 target 的 `{id, type, snapshot_id, size, duration, error}`、镜像 digest 映射。

manifest 本身也作为一个 restic 快照存入，打 tag `ark-manifest`。
恢复时第一步就是取最新的 manifest 快照。

**验收**：manifest 能序列化/反序列化往返；能从 repo 里按 tag 取回最新一份。

---

### P1-4 `ark backup` 命令

**新增文件**：`internal/cli/backup.go`

流程：

```
加载清单 → doctor 前置检查 → EnsureInit → 逐个 target 执行
  → 写 manifest → forget --prune → 写状态文件
```

**已定的设计点**：

- **单实例锁**：`flock` 于 `/run/ark.lock`。手动触发和 timer 撞车会让两个
  `pg_dump` 同时跑，白白加倍数据库压力。拿不到锁就直接退出，不排队。
- **前置 doctor 失败即中止**，不产生半成品快照。提供 `--skip-doctor` 用于应急。
- **单个 target 失败不中止其余**。半份备份也比没有强；但整体退出码非零，
  且失败信息进 manifest 和状态文件。
- `--dry-run` 打印将要执行的命令，不实际执行。

**验收**：在真机上执行一次，对象存储里能看到加密对象，
`restic snapshots` 能列出全部 target 和 manifest。

---

### P1-5 状态上报 `internal/status/`

**⚠️ 这里有个设计待定，动手前先决定。**

状态文件不能放进 restic 仓库——restic 是加密的，中心机没有密码就读不了。
所以 `_status/<host>.json` 必须走**原生 S3 API 直传**，和 restic 是两条独立路径。

由此带来两个待定问题：

1. **配置怎么写**：是复用 `repo.env_file` 里的 S3 凭证 + 从 `repo.url` 解析出
   endpoint/bucket，还是在清单里新增一段独立的 `status:` 配置？
   前者少配一次、但要解析 restic 的 URL 格式（`s3:https://host/bucket/path`）；
   后者更直白、但配置重复。**倾向后者**，因为显式优于隐式，而且未来
   状态文件可能想放在和备份不同的桶里。
2. **多一个依赖**：需要引入 `aws-sdk-go-v2`。

**内容**：见 `design.md` §9。

**验收**：备份跑完后对象存储 `_status/<host>.json` 存在且内容正确；
用只读凭证能读到它。

---

### P1-6 `ark install`

生成并安装 systemd unit。

**新增文件**：`internal/cli/install.go`、`internal/systemd/unit.go`（模板）

生成 `ark-backup.service`（`Type=oneshot`）和 `ark-backup.timer`：

- `OnCalendar` 取自清单
- **`Persistent=true`**：机器关机错过了执行窗口，开机后补跑一次。
  没有这行，关机一晚就等于跳过一天备份。
- **`RandomizedDelaySec=600`**：多台机器同时打对象存储容易触发限流。
- `--dry-run` 打印 unit 内容不写盘。

**验收**：`ark install` 后 `systemctl list-timers` 能看到，
`systemd-analyze verify` 无告警，手动 `systemctl start ark-backup.service` 能跑通。

---

## P2 — 恢复可用（成败点）

**阶段目标**：在一台**全新的、什么都没有的** VPS 上，只用
「对象存储凭证 + restic 密码 + `ark restore`」把服务完整拉起来。

粗估 3–5 天。**这一步不通过，P1 的所有工作都没有意义。**

### P2-1 恢复计划 `internal/restore/plan.go`

从 manifest 生成一个纯数据结构的 `Plan`：要恢复哪些 target、恢复到什么位置、
要拉哪些镜像 digest、按什么顺序执行。

`Plan` 必须能被完整打印。`ark restore --dry-run` 就是「生成 Plan 并打印」，
全程只读，不创建容器、不写文件、不拉镜像。

**验收**：对一份真实 manifest 能输出完整可读的计划；`--dry-run` 跑完
`docker ps -a` 和文件系统均无变化。

---

### P2-2 恢复执行 `internal/restore/execute.go`

严格按 `design.md` §7 的顺序执行：

```
files → 镜像 digest → volume → 起数据库 → 灌数据 → 起应用 → 健康检查
```

**已定的硬性要求**：

- **幂等**：每步先检查当前状态再决定做不做。中途失败后重跑同一条命令
  必须能继续，而不是留下一个半坏的环境。
- **拒绝隐式覆盖**：目标机器上已存在同名 volume 或容器时必须报错退出，
  除非显式加 `--force`。恢复操作把生产数据覆盖掉是不可逆的。
- **恢复顺序不能并行化**。数据库没起来就灌数据只会失败得很难诊断。
- 结束时输出**人工确认清单**：DNS 指向、TLS 证书、防火墙端口、
  以及 `.env` 里需要按新环境调整的项。

**验收**：见 P2-3。

---

### P2-3 跨机重建实测（一次性任务，但最重要）

开一台干净的 VPS，完整走一遍恢复，把踩到的每个坑写回 `design.md`。

**必须验证的点**：

- [ ] 业务数据完整（抽查若干张表的行数和最新记录）
- [ ] 应用能正常登录、正常读写
- [ ] 加密字段能正常解密（验证 `.env` 里的密钥确实被正确恢复）
- [ ] 恢复出来的镜像 digest 和备份时一致
- [ ] 全程只用了「对象存储凭证 + restic 密码」，没有依赖原机器上的任何东西

最后一条最容易翻车：过程中一旦发现「还需要从老机器上拷点什么」，
说明备份范围有缺口，回头补进清单。

---

### P2-4 `ark verify` 自动演练

定期挑一个快照恢复到隔离环境，跑健康检查，通过后销毁。

- compose 项目名加 `-verify` 后缀，端口整体偏移，volume 独立命名
- **绝对不能碰生产项目的任何资源**——所有操作前校验
  `com.docker.compose.project` 标签
- 结果写入状态文件，在中心机页面作为一等公民展示

**验收**：`ark verify` 跑完后生产环境无任何变化；
人为破坏一份快照后 verify 能报失败。

---

## P3 — 中心机与 Web 页面

**阶段目标**：打开一个页面，一眼看出所有机器的备份健康度。

粗估 3–4 天。

### P3-1 hub 后端 `cmd/ark-hub/` + `internal/hub/`

- 用只读凭证列出并读取 `_status/*.json`，聚合成内存视图，定时刷新
- API：`GET /api/hosts`（列表）、`GET /api/hosts/:host`（详情）、`GET /api/alerts`
- **hub 不持有 restic 密码，不调用 restic**（ADR-006）。
  它只会读状态元数据，拿不到任何备份内容。
- 健康判定：`最近成功备份时间 > 计划周期 × 2` 判为超时告警

### P3-2 前端 `web/`

Vue 3 + Vite + TypeScript + Pinia + Tailwind（与 sub2api 同栈，便于长期维护）。

页面：

- **总览**：每台机器一张卡片——最近备份时间、状态、大小趋势、下次计划时间
- **主机详情**：target 明细、历史快照、doctor 报告、演练结果
- **告警**：超时未备份、连续失败、演练失败

### P3-3 内嵌打包

`go:embed` 把 `web/dist` 打进 `ark-hub` 单二进制，部署时不需要 node 环境。
Makefile 增加 `make hub`（先 pnpm build 再 go build）。

### P3-4 钉钉告警

复用现有钉钉机器人。触发条件：超时未备份、连续两次失败、演练失败。
**告警要有静默期**，否则一台机器坏掉会每轮刷一次群。

**验收**：人为让一台机器备份失败，页面和钉钉都能在下一个周期内报出来。

---

## P4 — 运维打磨

- [ ] `pg_dump` 与 PostgreSQL 服务端版本比对（客户端版本必须 ≥ 服务端），加进 `doctor`
- [ ] 单机多 compose 项目支持（当前清单假设一机一项目）
- [ ] 第二备份后端（防止单一存储商账号被封）
- [ ] goreleaser 发布多架构二进制
- [ ] `ark` 自身的升级流程（新版本不能读不了旧 manifest —— manifest 要有 schema 版本兼容策略）

---

## 暂不做

- **WAL 归档 / PITR**：秒级 RPO 需要 pgBackRest 或 wal-g，复杂度高一个量级
  且只覆盖 PostgreSQL。等 P2 稳定运行一段时间后再评估。
- **自动故障切换**：ark 不参与流量调度，不做 fencing，不自动提升节点。
- **非 compose 部署形态**（k8s、裸机 systemd 服务）：范围外。

---

## 待决问题汇总

动手到对应阶段时需要先拍板：

| 问题 | 阶段 | 现在的倾向 |
|---|---|---|
| 状态文件的 S3 配置是复用 repo 还是独立配置段 | P1-5 | 独立配置段，显式优于隐式 |
| `ark verify` 的频率和触发方式（独立 timer？） | P2-4 | 独立 timer，每周一次 |
| hub 是否需要鉴权（内网 only 还是公网暴露） | P3-1 | 先做内网 only，公网需求出现再加 |
| manifest schema 的向后兼容策略 | P4 | 新版必须能读旧 manifest，写入始终用最新版 |
