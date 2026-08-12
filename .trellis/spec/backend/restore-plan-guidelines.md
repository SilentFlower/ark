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
CGO_ENABLED=0 go build ./cmd/ark
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
  并原子切换后再启动。所有 Dump/Feed/Close、Runner 和 context 错误保留 `errors.Is` 错误链。
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
