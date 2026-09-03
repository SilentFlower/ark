# Restore Plan Guidelines

> `ark restore --dry-run`、manifest/config 映射与纯数据恢复计划的可执行契约。

---

## Scenario: 新增或修改恢复计划与 dry-run

### 1. Scope / Trigger

触发本规约的改动：

- 修改 `internal/restore.Plan`、`Step`、`Phase`、`Project` 或 `Target`；
- 修改 manifest source、config source/destination 的映射与兼容规则；
- 修改 `ark restore` 的 `--host`、`--to`、`--snapshot`、`--dry-run`、`--json`；
- P3-2 执行器开始消费 Plan 字段或阶段顺序。

Plan 是 manifest 备份事实与当前 `ark.yaml` 部署定义的交集。它必须是可稳定 JSON
编码的纯值，不能持有 Repo、Runner、Store、凭证、打开的流或命令字符串。

### 2. Signatures

```text
ark restore --host <source> [--to <destination>]
  [--snapshot latest|<manifest-id>] --dry-run [--json]
```

```go
func BuildPlan(
    cfg *config.Config,
    manifest backup.Manifest,
    manifestSnapshotID string,
    sourceHost string,
    destinationHost string,
) (Plan, error)
```

`--host` 是 manifest 中的备份来源；`--to` 省略时等于 source。source 与 destination
都必须存在于当前清单，不接受临时 SSH 参数。P3-1 未带 `--dry-run` 必须在读取配置前拒绝。

### 3. Contracts

Plan 顶层 JSON 字段固定使用 snake_case：

| 字段 | 类型 | 约束 |
|---|---|---|
| `manifest_snapshot_id` | string | 精确 manifest restic snapshot ID，非空 |
| `run_id` | string | manifest 的 backup run ID |
| `source_host` / `destination_host` | string | 当前清单中存在的 host |
| `project` | object | `name`、`compose_file`、可选 `env_file` / `project_name` |
| `conflict_policy` | string | P3 MVP 固定 `refuse_existing` |
| `steps` | array | 按固定阶段与 source target 顺序 |
| `manual_checks` | array | 固定人工确认事项的副本 |

阶段顺序固定为：

```text
files -> image_digest -> volume -> database_prepare -> database_data -> application -> health
```

每个 target 步骤保存 `target_id`、`target_type`、配置副本和必要 snapshot ID；
`image_digest` 还保存完整 service/digest map。数据库 prepare 步骤不消费 snapshot，data
步骤消费同 target snapshot。application 与 health 是无 target 的项目级步骤。

manifest 提供历史事实；当前清单提供 source/destination Project、Target 与连接定义。
Plan 不复制 SSH、仓库 URL、密码文件、对象存储环境文件、调度或保留策略。source 与
destination 仅允许 host、连接方式、schedule、retention 不同，Project 与 Target 集合及字段
必须完全一致。Target 按 `Target.ID()` 与 type 映射，不按数组下标猜测。

dry-run 允许的依赖只有：`config.LoadAndValidate`、`restic.New` 和
`backup.LoadManifestSelection`。不得获取锁、运行 doctor、创建 Runner、连接 SSH/Docker、
打开 Store、创建资源、写文件或初始化仓库。

### 4. Validation & Error Matrix

| 条件 | 结果 |
|---|---|
| `cfg=nil`、manifest snapshot ID/source 为空 | 构建失败并指出字段 |
| source/destination 不在当前清单 | 一次性返回可定位 host 错误 |
| manifest 不含 source | 拒绝生成 Plan |
| Project 任一字段漂移 | 聚合错误包含 `project.<field>` 与 source/destination 值 |
| destination 缺 target、额外 target 或 target 字段漂移 | 聚合全部可定位错误 |
| manifest 缺 target、额外 target、type 改变或 target fail | 聚合错误，不生成步骤 |
| 任一 target snapshot ID 为空 | 拒绝生成 Plan，包括 fail target |
| image service 缺 digest或出现未配置 service | 聚合 `image_digests[service]` 错误 |
| 未带 `--dry-run` | 参数阶段拒绝，不读取配置或仓库 |
| latest 无 manifest | CLI 返回“仓库中不存在 manifest”错误 |
| `--json` | stdout 只包含可解码的 Plan JSON |

映射校验使用 `errors.Join`，让一次 dry-run 报出全部 Project、Target 与 manifest 漂移。
manifest schema 自身无效时由 `Manifest.Validate` 先拒绝，不用不可信字段继续映射。

### 5. Good/Base/Bad Cases

- **Good**：source 与 destination 连接信息不同，但 Project/Target 完全相同；五类 target
  生成固定七阶段 Plan，保留每个 snapshot 与 image digest。
- **Base**：省略 `--to`，恢复目标默认为 source；latest 无候选时明确失败而非输出空计划。
- **Bad**：destination 把 files path 或 compose file 改到新目录；不得把它解释成迁移意图，
  必须在任何目标机命令前拒绝。
- **Bad**：dry-run 为了“确认目标环境”运行 doctor 或创建 SSH Runner；这违反只读边界，
  环境探测属于 P3-2 执行前检查。

### 6. Tests Required

`internal/restore/plan_test.go` 至少覆盖：

- 五类 target、交错输入下的固定阶段顺序和数据库 prepare/data snapshot 语义；
- source=destination 与跨主机完全兼容；
- Project、paths、services、target fail、manifest missing 的聚合错误；
- image digest 缺失/额外 service；
- Plan 对 config slice 和 manifest map 的深拷贝；
- 重复 JSON 编码稳定，字段为 snake_case 且不含连接/凭证字段。

`internal/cli/restore_test.go` 至少覆盖：

- dry-run 只调用 load config、new repo、load manifest 三项依赖并保持顺序；
- `--host`、`--to`、`--snapshot` 默认与显式传递；
- 未带 dry-run/host 时零依赖调用；
- 人类输出含阶段、snapshot、digest、compose、冲突策略和人工确认项；
- JSON 可解码且不含 repo URL、SSH address、identity/known_hosts/password/env file。

提交前运行：

```bash
go test ./internal/restore ./internal/backup ./internal/cli -race -count=1
make check
make build
CGO_ENABLED=0 go build -o bin/ark-nocgo ./cmd/ark
git diff --check
```

### 7. Wrong vs Correct

#### Wrong

```go
type Plan struct {
    Host config.Host
    Repo *restic.Repo
    Command string
}
```

这会把 SSH/凭证路径和运行时对象混进结构化输出，也让 dry-run 与真实执行各自携带一套
不可审计语义。

#### Correct

```go
type Plan struct {
    ManifestSnapshotID string `json:"manifest_snapshot_id"`
    SourceHost          string `json:"source_host"`
    DestinationHost     string `json:"destination_host"`
    Project             Project `json:"project"`
    Steps               []Step `json:"steps"`
}
```

Plan 只保存执行所需的非敏感值副本；P3-2 结合当前配置创建 destination Runner，并严格按
Plan 阶段执行。这样 dry-run 展示与真实恢复共享同一结构化事实，而不会提前触碰目标环境。

---

## Scenario: 执行真实恢复与同 Plan 续跑

### 1. Scope / Trigger

- 修改 `internal/restore/execute.go`、真实 `ark restore` CLI、破坏前 safety backup、恢复 marker、
  Compose 启动或恢复后健康/digest 校验时，必须遵守本节。
- 这是灾难恢复写路径。目标机可能是空机，也可能保留生产资源；所有猜测、可变 tag、半完成流和
  无法证明的幂等状态都必须 fail closed。

### 2. Signatures

```text
ark restore --host <source> [--to <destination>]
  [--snapshot latest|<manifest-id>] [--force] [--skip-doctor] [--json]
```

```go
func Execute(
    ctx context.Context,
    plan Plan,
    repo *restic.Repo,
    runner sshexec.Runner,
    options ExecuteOptions,
) (Result, error)
```

真实恢复与 backup 共用 `/run/ark.lock`。destination 只运行 Docker/Compose 与标准文件命令；
restic、仓库凭证和业务明文流只存在于 hub。

### 3. Contracts

- 固定顺序是 `files -> image_digest -> volume -> database_prepare -> database_data -> application -> health`；
  target 与阶段均串行，不跨阶段并发。
- `image_digests` 的值必须严格是 `repo@sha256:<64位小写十六进制>`。Plan 构建与 Execute 都校验；
  禁止接受 tag、短 digest 或回退到 Compose 原始 image。
- 默认激活的每个 Compose service 都必须出现在 image digest 映射中。拉取后写 root-only override，
  所有 Compose `up` 固定使用 `--no-build --pull never`；应用启动前再次核对覆盖，防止 files 恢复后
  Compose 定义与 Plan 不一致。
- preflight 默认拒绝既有同项目容器、volume 和目标路径。`--force` 只授权 Plan 精确声明且 Compose
  label 归属一致的资源；任何 stop、rm、volume 清空或数据导入前必须先完成 destination safety backup。
- 同 Plan 续跑依赖目标机 root-only marker。marker 只说明某一步曾成功，跳过前仍重验后置条件：
  files 比较路径 `stat` 元数据指纹；image 比较本地 digest 与 override；volume 比较存在性和标签；
  database 比较 readiness；application/health 比较全部服务状态、health 和实际容器 digest。
- files tar 直接由 `restic.Dump` 流入 destination `tar -xpf -`；hub 不落业务临时文件。hub 状态库
  的 `.db` 原始流先写同目录临时文件、收紧到 `0600` 后原子切换，再删除旧 WAL/SHM。
- PostgreSQL 使用 `psql --set ON_ERROR_STOP=1`；Redis 必须停止 service，在 volume 内写隔离临时 RDB
  并原子切换后再启动。Redis readiness 的 `redis-cli PING` 只有在命令成功退出，且 Runner 合并输出中
  存在去除空白后严格等于 `PONG` 的独立行时才算成功；警告可位于响应前后，但子串、大小写变体、
  `+PONG` 或附加内容不能通过。首次等待与 marker 后置条件复核必须复用同一判定。所有
  Dump/Feed/Close、Runner 和 context 错误保留 `errors.Is` 错误链。
- CLI 的 `Result.Error` 和步骤 Detail 只保留阶段级脱敏文本；底层错误链只用于退出和诊断，
  `errRestoreFailed` 负责转换退出码，不能让根命令重复回显敏感外部错误。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| context、repo、runner、Plan 或显式 `project_name` 缺失 | 任何目标命令前失败 |
| Plan 缺 image step、存在多个 image step或 digest 格式无效 | 任何目标命令前失败 |
| Compose 默认活跃 service 缺 digest | 拉镜像/启动应用前失败，不使用 tag 补救 |
| 既有资源且未指定 `--force` | 零目标写入，返回冲突摘要 |
| `--force` 但资源标签不属于目标 Project | 永远拒绝，不因 force 扩大授权 |
| safety backup 失败或未生成完整 manifest | 不停容器、不删文件、不导入数据 |
| files marker 存在但 stat 元数据漂移 | 不跳过，按该 step 重做 |
| step marker 与 Plan/snapshot 不匹配 | 不跳过，按该 step 重做 |
| Feed 与 Dump Close 同时失败 | 两条错误都可 `errors.Is`，阶段失败且不写完成 marker |
| Redis PING 成功退出且合并输出含警告与独立 `PONG` 行 | readiness 成功；完整输出不进入 Result、CLI 或状态库 |
| Redis PING 非零退出，即使输出含独立 `PONG` 行 | 继续等待或复核失败，输出不能覆盖退出状态 |
| context 在 readiness/health 轮询中取消 | 立即结束轮询并保留取消错误链 |
| service exited/unhealthy 或实际容器 digest 不一致 | health 阶段 fail；缺 healthcheck 只 warn |
| 人类 Plan 输出失败 | 在首次目标 marker/写入前中止 |
| 结果输出与执行同时失败 | 返回值同时保留 `errRestoreFailed`、执行错误和 writer 错误 |

### 5. Good / Base / Bad Cases

- **Good**：第一次执行完成 files 后在 image 阶段失败；重跑读取同 Plan marker，files 元数据一致则
  skipped，从 image 继续，最终实际容器 digest 全部等于 Plan。
- **Base**：空 destination 无冲突，按 digest 拉取全部默认活跃服务，`--pull never` 启动成功；
  没有 healthcheck 的 service 产生 warn 和人工确认项。
- **Bad**：只给应用 service 记录 digest，却让 Compose 的 Postgres/Redis 按 tag 拉取；这既不能在
  空机恢复，也无法证明数据库容器版本与备份一致。
- **Bad**：只凭 files 路径存在就跳过；外部修改权限、属主、大小或 mtime 后会把漂移状态误报成功。

### 6. Tests Required

- fake Runner 精确断言七阶段顺序、五类 target argv、`--no-build --pull never`、Feed 字节与 marker 时机。
- 表驱动覆盖默认冲突零写入、force safety backup 失败、七阶段中断不写 complete、context 取消、
  health 失败、digest 不一致和多错误链。
- 同 Plan 用例必须至少含两个 step：已验证 step skipped，后续未完成 step 继续执行；另测 files stat
  元数据漂移时不 skipped，以及存在未完成数据 step 时先停止项目容器。
- Plan/Execute 双层覆盖 mutable tag、短/非十六进制 digest、缺 image step；doctor 覆盖 image target
  漏默认活跃 service 和完全缺 target。
- CLI 覆盖锁/doctor 顺序、JSON 单输出、人类 Plan writer 失败零写入、失败哨兵与 writer 组合错误。
- 提交前运行 `go test ./internal/restore ./internal/cli ./internal/doctor ./internal/backup -race -count=10`、
  `make check`、`make build`、无 CGO 构建、`go mod verify` 和 `git diff --check`。

### 7. Wrong vs Correct

#### Wrong

```go
if pathExists(target) {
    return skipped
}
dockerComposeUp("-d") // 允许 build 或按 tag pull
```

路径存在不能证明内容或元数据仍是该 snapshot；普通 `up -d` 也可能绕开 manifest digest。

#### Correct

```go
if markerMatches(step) && metadataFingerprintMatches(target) {
    return skipped
}
validateComposeImageCoverage(plan)
dockerComposeUp("-d", "--no-build", "--pull", "never")
```

marker 绑定 Plan/snapshot，后置条件再次验证实际状态；Compose 只使用已拉取且由 override 固定的 digest。

---

## Scenario: 显式隔离恢复、自动端口与精确清理

### 1. Scope / Trigger

- 修改 `restore.WithIsolation`、`WithIsolationOptions`、`IsolationSpec`、Compose 结构化转换、
  Docker 自动端口、隔离状态或 `CleanupIsolation` 时，必须遵守本节。
- 隔离模式只由显式 `ark restore --isolate` 进入；普通恢复遇冲突仍 fail closed，不能自动切换。
- 后续 `ark verify` 通过 `WithIsolationOptions` 复用同一命名、路径、端口、归属和清理实现，
  不复制第二套转换逻辑。

### 2. Signatures

```text
ark restore --host <source> [--to <destination>]
  [--snapshot latest|<manifest-id>] --isolate [--dry-run] [--json]

ark restore cleanup --host <destination> --isolation <64-hex-id> [--json]
```

```go
func WithIsolation(plan Plan) (Plan, error)
func WithIsolationOptions(plan Plan, options IsolationOptions) (Plan, error)
func CleanupIsolation(
    ctx context.Context,
    runner sshexec.Runner,
    destinationHost string,
    isolationID string,
) (CleanupResult, error)
```

`IsolationSpec` 至少保存 schema、完整/短 ID、purpose、可选 instance key、派生 project、
root/files/generated compose 路径、原项目定位、路径/volume 映射、`runtime_auto` 端口策略和
完整 `IsolationPort`。`IsolationPort` 固定包含 service、host IP、原 published、allocated、
target、protocol、可选 app protocol 和 mode。

### 3. Contracts

- 普通 restore 使用 manifest snapshot、source、destination、原 project 身份和 purpose 派生稳定
  SHA-256 isolation ID；相同事实重复执行复用同一 ID、project、root、资源名和实际端口。
- `--isolate` 与 `--force` 互斥。隔离路径不运行 destination safety backup，也不得停止、删除、
  覆盖或清空原项目资源。
- files target 全部恢复到 `/var/lib/ark/restore/isolations/<id>/files`。compose/env、bind、config、
  secret 等宿主机路径只有被成功 files target 覆盖时才能映射；无法证明时在创建 Docker 资源前失败。
- dry-run 保持零 SSH、零 Docker、零目标写入。Plan 输出稳定 ID、文件/volume 映射和每条
  `service / host IP / 原 published / auto / target / protocol`；不得读取当前生产 Compose 补事实。
- 端口声明来自备份 manifest 的 `ComposeMetadata`。历史 manifest 缺该字段时普通恢复仍可用，
  `WithIsolation*` 明确要求重新备份。真实执行再次读取恢复材料的 canonical Compose，并与备份端口
  声明比较；漂移时在 Docker 创建前拒绝。
- Compose 转换只解析 `docker compose config --format json --no-env-resolution` 的纯 stdout。
  移除固定 `container_name`，为 service、volume、network 增加 isolation label，改写显式资源名和
  安全路径，删除 published 让 Docker 原子分配宿主机端口；保留 host IP、target、TCP/UDP、
  app protocol 和 mode。
- 普通 network 只允许默认 driver 或无 `driver_opts` 的 `bridge`。external network 只把 `name/external`
  身份声明和 Compose canonical 自动补出的空默认字段作为输入，重新构造为带派生名称及 isolation label
  的独立 `bridge`，不得连接、修改或删除原共享 network；非空 IPAM、driver、driver_opts、internal、
  attachable 等额外运行时参数在 Docker 创建前 fail closed。
- 普通 named volume 允许缺省 driver 或 Compose canonical 默认的 `local`，但物理名称和 isolation label
  仍必须重写；非 `local` driver 与任何非空 `driver_opts` 继续在 Docker 创建前 fail closed。
- external volume/config/secret、host/container namespace、非 bridge driver、越界路径和其它共享宿主资源
  配置继续全部 fail closed。隔离端口第一版只接受 TCP/UDP，重复声明和无法结构化保真的协议在启动前拒绝。
- 具体 host IP 必须存在于 destination；不得回退到 `0.0.0.0`、回环或其它地址。Docker 启动后立即
  inspect 实际端口并原子持久化到 root-only `state.json`，续跑从受标签保护的容器重建缺失状态。
- cleanup 只删除 state 中记录且 project/isolation label、名称和路径全部匹配的 container、network、
  volume 与 isolation root。root 不存在时仍按 isolation label 扫描孤立 Docker 资源；发现孤儿必须
  报错，只有 state 与带标签资源都不存在才算幂等成功。
- state/root 是部分清理失败后证明归属并按同一 ID 重试的最后凭据。删除 container、network、volume
  后必须再次按 isolation label 扫描；只有确认无残留资源后才能删除 root。扫描失败或仍有残留时
  返回失败并保留 state/root，不能先删状态再报告孤儿。
- CLI 人类与 JSON 输出只包含安全资源摘要、端口、访问地址和精确 cleanup 命令；Compose 隔离策略
  可通过 restore 包内受控摘要进入 `Result.Error`，任意外部错误文本不得自动复制。canonical Compose、
  environment、secret、仓库凭证和底层外部命令诊断不得进入输出。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| `--isolate --force` | 参数阶段拒绝，零依赖调用 |
| dry-run | 只读取 config/repo/manifest；端口 `allocated_port=auto` |
| Plan 已隔离、purpose/instance key 非法或 compose/env 未被 files 覆盖 | 构建隔离 Plan 失败 |
| manifest 缺 Compose 端口元数据 | 普通 Plan 可构建；隔离 Plan 要求重新备份 |
| 备份端口与恢复材料 canonical 端口漂移 | 创建 Docker 资源前失败 |
| host IP 在 destination 不存在 | 不回退地址，不创建 Docker 资源 |
| external network 只有身份字段或空 canonical 默认字段 | 转换为派生私有 bridge，不连接原共享 network |
| external network 含非空运行时参数 | fail closed，不创建 Docker 资源，Result 记录脱敏策略摘要 |
| 普通 named volume 无 driver 或 driver=local | 派生独立名称和 isolation label，按现有 state/cleanup 管理 |
| 普通 named volume 使用非 local driver 或 driver_opts | fail closed，不创建 Docker 资源，Result 记录脱敏策略摘要 |
| external volume/config/secret、host namespace、未映射路径或不支持 driver/protocol | fail closed，不触碰原项目 |
| 名称存在但 Compose/isolation label 不匹配 | 续跑与 cleanup 均拒绝接管或删除 |
| 启动成功但端口 inspect/状态持久化失败 | 当前阶段失败；续跑按标签重建，不创建第二套副本 |
| state/root 不存在且无带标签资源 | cleanup 幂等成功 |
| state/root 不存在但仍有带 isolation 标签资源 | cleanup 失败并列出孤儿，不静默成功 |
| cleanup 中任一资源归属或路径无法证明 | 停止删除并保留 state 供同 ID 重试 |
| Docker 删除后标签复扫失败或仍有残留 | cleanup 失败并保留 state/root，不删除最后归属凭据 |

### 5. Good/Base/Bad Cases

- **Good**：目标机保留同名 production project、固定容器名、显式 volume/network 和原 TCP/UDP 端口；
  隔离副本自动派生名称和端口，启动与清理前后原容器 ID/state、volume、network 和端口不变。
- **Good**：production Compose 通过 external network 连接共享 bridge；隔离转换保留 service 的逻辑引用
  与 alias，但容器只连接 Ark 派生 network，启动与清理前后原 external network ID、label 和成员不变。
- **Good**：Compose canonical 为普通 named volume 补出 `driver: local`；隔离转换仍覆盖物理名称并添加
  isolation label，cleanup 只删除派生 volume，不接管或删除生产 volume。
- **Good**：首次启动后、写端口状态前中断；相同命令复用 isolation ID，inspect 已有受标签保护容器，
  恢复实际端口后继续，不创建新 project。
- **Base**：`--isolate --dry-run` 只显示稳定映射和 `auto`，管理员确认后真实执行，成功副本默认保留。
- **Bad**：检测到端口冲突后静默换 project 或扫描空闲端口再启动；前者扩大用户授权，后者存在竞争窗口。
- **Bad**：state root 已丢失就直接报告 cleanup 成功；带标签容器、network 或 volume 会成为不可审计孤儿。

### 6. Tests Required

- Plan/CLI：稳定 ID、purpose/instance key、深拷贝、普通 Plan JSON 兼容、`--force` 互斥、dry-run
  零副作用，以及人类/JSON 完整路径、volume 和端口映射。
- Compose 纯函数：短/长端口、范围、TCP/UDP、IPv4/IPv6 host IP、app protocol、固定名称、bind/config/
  secret 映射；external network 布尔/对象形式、空 canonical 默认字段、service alias、额外运行时参数；
  普通 named volume 缺省/`local` driver；external volume/config/secret、host namespace、非 `local` driver、
  driver_opts、重复/未知协议矩阵。
- fake Runner：canonical stdout/stderr 隔离、备份端口漂移、host IP、root-only state、标签冲突、
  inspect 端口恢复、同 ID 续跑和阶段错误脱敏。
- cleanup：精确删除顺序、部分失败续跑、重复执行、state/标签/名称/路径漂移、root 缺失且有/无孤儿，
  以及删除后标签复扫发现残留时 root 未被删除。
- 可选真实 Docker 集成测试使用唯一 project 和临时目录，覆盖 production 与隔离副本并存、external
  network 私有化、TCP/UDP 自动或禁用端口、显式资源名、原项目基线不变和被测 cleanup API；失败清理
  只能使用精确 ID/名称。
- 提交前运行关键包十轮 race、`make check`、双构建、`go mod verify`、`git diff --check`，并在有
  Docker 环境时运行 `ARK_DOCKER_INTEGRATION=1` 隔离集成测试。

### 7. Wrong vs Correct

#### Wrong

```go
if portConflict {
    project.Name += "-restore"
    port.Published = findFreePort()
}
```

这既会静默改变普通 restore 模式，也在端口扫描和容器启动之间留下竞争窗口；固定资源名、路径和
归属标签仍可能覆盖原项目。

#### Correct

```go
isolated, err := restore.WithIsolation(plan)
if err != nil {
    return err
}
result, err := restore.Execute(ctx, isolated, repo, runner, options)
```

只有显式隔离 Plan 才进入统一结构化转换。Docker 负责原子分配端口，实际映射与资源归属写入
root-only state，后续 verify 和 cleanup 复用同一接口。
