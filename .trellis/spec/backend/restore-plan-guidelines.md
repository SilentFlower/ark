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
