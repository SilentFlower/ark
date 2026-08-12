# Backup Manifest Guidelines

> restic 中备份产物清单的 schema、校验与读取一致性规约。

---

## Scenario: 新增或修改备份 manifest 契约

### 1. Scope / Trigger

触发本规约的改动：

- 修改 `internal/backup.Manifest`、`ManifestHost` 或 manifest target 字段；
- 修改 manifest schema version、JSON 编解码或校验规则；
- 修改 manifest 在 restic 中的 filename、tag、查询或 dump 流程；
- restore、CLI 或 API 开始消费 manifest。

备份 manifest 是 P3 restore 的输入事实，不是 `ark.yaml` 用户配置。前者定义在
`internal/backup/manifest.go`，后者遵循 `manifest-guidelines.md`，二者不可复用
schema version 或错误语义。

### 2. Signatures

```go
const ManifestSchemaVersion = 1
const ManifestFilename = "ark-manifest.json"
const ManifestTag = "ark-manifest"

func (m Manifest) Validate() error
func SaveManifest(ctx context.Context, repo *restic.Repo, manifest Manifest) (restic.Snapshot, error)
func LoadLatestManifest(ctx context.Context, repo *restic.Repo) (Manifest, bool, error)
```

`LoadLatestManifest` 的 `bool=false, error=nil` 表示仓库里尚无 manifest，是正常首次运行
分支；存在候选快照但元数据或内容损坏必须返回错误，不能伪装成“暂无备份”。

### 3. Contracts

schema v1 顶层 JSON 固定字段：

| 字段 | 类型 | 约束 |
|---|---|---|
| `schema_version` | integer | 必须等于 `ManifestSchemaVersion` |
| `run_id` | string | 非空，并与 snapshot 的唯一 `run:<id>` tag 一致 |
| `ark_version` | string | 非空，记录执行备份的 hub 版本 |
| `started_at` / `finished_at` | RFC3339Nano | UTC，结束时间不得早于开始时间 |
| `hosts` | array | 保留清单执行顺序 |

每个 host 使用 `host` 和 `targets`；target 直接来自 `TargetResult`，wire 字段固定为
`id`、`type`、`snapshot_id`、`bytes`、`duration`、`status`、`error`、
`image_digests`。`duration` 使用 `time.Duration.String()`，解码用
`time.ParseDuration`，避免毫秒截断丢失纳秒精度。`image_digests` 是 JSON object；
标准 `encoding/json` 会稳定排序字符串 map key。

存储时必须使用固定 `ark-manifest.json`，不得包含日期或 run ID；tags 至少且当前固定为
`ark-manifest`、`run:<run-id>`。读取按 `ark-manifest` 查询 snapshots，使用时间升序、
同时间 ID 升序的最后一项，然后 dump 固定文件名。snapshot metadata 的唯一 path 可带
restic 的前导 `/`，归一化后必须等于固定文件名。

`SaveManifest` 消费 `BackupStdin` 的 `(Snapshot, error)` 组合契约：备份失败但返回 ID 时，
必须用原 context 精确 `ForgetSnapshot(id)`，避免失败清单成为 `LoadLatestManifest` 的最新候选；
撤销失败与原始备份错误使用 `errors.Join` 并列返回。没有精确 ID 时不得按 tag 猜测删除。

同 schema 新增可选字段时，旧程序应忽略未知字段以保持向后兼容；未知或高于当前支持的
`schema_version` 必须拒绝，并同时报告实际版本与支持版本。破坏性字段变更必须升 schema。

### 4. Validation & Error Matrix

| 条件 | 结果 |
|---|---|
| schema 缺失、未知或更高 | 错误包含实际版本和当前支持版本 |
| run ID、ark version、host、target ID/type 缺失 | 校验失败并指出字段路径 |
| 时间非 UTC、结束早于开始 | 校验失败 |
| bytes 或 duration 为负 | 校验失败 |
| 同一 `(host,target_id)` 重复 | 校验失败 |
| ok/warn target 缺少 snapshot ID | 校验失败；fail 可保留空 ID 或已撤销 ID |
| target 的 Host 与所属 ManifestHost 不一致 | 校验失败 |
| manifest backup 失败且返回 snapshot ID | 精确撤销该 ID，仍返回原始 backup error |
| manifest backup 与精确撤销同时失败 | 返回错误可分别 `errors.Is` 两条错误链 |
| manifest backup 失败且没有 snapshot ID | 返回失败，不调用 forget、不猜测候选 |
| 无 `ark-manifest` 候选 | `found=false, error=nil` |
| 候选缺 ID、时间、固定 path、manifest tag 或唯一 run tag | 读取失败 |
| dump JSON 非法、过大或 run ID 与 tag 不一致 | 读取失败 |
| restic Snapshots、Dump、Read/Close 失败 | 保留底层错误链并返回失败 |

### 5. Good/Base/Bad Cases

- **Good**：多 host、多 target，包含 ok/warn/fail 和 image digest map；JSON 往返后
  `TargetResult` 的 snapshot、bytes、duration、status、error 均不变。
- **Good**：restic 已提交 manifest 并输出 ID 后返回非零；`SaveManifest` 只撤销该 ID，
  原始错误与可选撤销错误仍可识别。
- **Base**：仓库没有任何 `ark-manifest` snapshot，返回 `found=false`，调用方可继续首次备份。
- **Bad**：最新 snapshot 的 tag 是 `run:run-2`，dump 内容却写 `run_id=run-1`；必须失败，
  不能回退到更旧 manifest 掩盖仓库不一致。
- **Bad**：`SaveManifest` 丢弃失败返回中的 ID，会让报告失败的清单仍进入最新恢复候选。

### 6. Tests Required

`internal/backup/manifest_test.go` 至少覆盖：

- 完整 JSON 往返，包含纳秒 duration、status/error 和多项 image digest；
- map key 的稳定输出顺序；
- schema、主键、UTC 时间、时间倒序、负数、重复 target、类型与状态非法；
- 同 schema 未知可选字段可读取，未知 schema 明确拒绝；
- `SaveManifest` 精确 filename、tag、payload 与空 snapshot ID；失败返回带 ID 时精确撤销，
  覆盖撤销成功、双错误链和无 ID 不调用 forget；
- 多 snapshot 乱序输入下按时间和 ID 选择最新项；
- 无 snapshot 正常分支，以及 path/tag/run/dump/JSON 各类不一致失败；
- `make check`、相关包 `-race`、常规构建与 `CGO_ENABLED=0` 构建。

### 7. Wrong vs Correct

#### Wrong

```go
type Manifest struct {
    Duration time.Duration `json:"duration_ms"`
}

decoder.DisallowUnknownFields()
```

直接把 `time.Duration` 塞进毫秒字段会隐式写纳秒或在手工转换时截断精度；拒绝同 schema
未知字段又会让新增可选字段破坏旧 restore 读取。

#### Correct

```go
type manifestTargetWire struct {
    Duration string `json:"duration"`
}

wire.Duration = target.Duration.String()
duration, err := time.ParseDuration(wire.Duration)
```

先宽松读取 `schema_version` 并拒绝不支持版本，再按已知 v1 字段解码和显式 `Validate`；
同 schema 的额外可选字段由旧程序忽略。

#### Wrong

```go
snapshot, err := repo.BackupStdin(ctx, payload, ManifestFilename, tags)
if err != nil {
    return restic.Snapshot{}, err // 丢失可能已经提交的 snapshot.ID
}
```

#### Correct

```go
snapshot, backupErr := repo.BackupStdin(ctx, payload, ManifestFilename, tags)
if backupErr != nil {
    var forgetErr error
    if snapshot.ID != "" {
        forgetErr = repo.ForgetSnapshot(ctx, snapshot.ID)
    }
    return restic.Snapshot{}, errors.Join(backupErr, forgetErr)
}
```

失败 manifest 不能成为恢复输入；只有 `BackupStdin` 返回的精确 ID 可以用于撤销。
