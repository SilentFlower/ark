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
const LatestManifestSelector = "latest"

func (m Manifest) Validate() error
func SaveManifest(ctx context.Context, repo *restic.Repo, manifest Manifest) (restic.Snapshot, error)
func LoadLatestManifest(ctx context.Context, repo *restic.Repo) (Manifest, bool, error)
func LoadManifestSelection(
    ctx context.Context,
    repo *restic.Repo,
    selector string,
) (Manifest, restic.Snapshot, bool, error)
func LoadLatestManifestSelections(
    ctx context.Context,
    repo *restic.Repo,
    hosts []string,
) (LatestManifestSelections, bool, error)
```

`LoadLatestManifest` 的 `bool=false, error=nil` 表示仓库里尚无 manifest，是正常首次运行
分支；存在候选快照但元数据或内容损坏必须返回错误，不能伪装成“暂无备份”。

`LoadManifestSelection` 额外返回实际读取的 `restic.Snapshot`，供恢复计划保留精确
manifest snapshot ID。`selector` 接受 `latest`、完整 ID 或唯一 ID 前缀；显式选择不存在、
匹配多个或候选损坏时必须返回错误，不能回退到最新或其它 manifest。

`LoadLatestManifestSelections` 用于 per-host backup 产生多份 manifest 的场景：按时间与 ID 从新到旧读取，
为请求的每台 host 选择最近一次包含它的 manifest，同时返回全局最新 manifest 供缺失 host 和清单漂移
记录事实。候选损坏仍 fail closed，不能为了找到更旧 host 而跳过损坏 manifest。

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
`image_digests`，以及可选 `compose_metadata`。`duration` 使用 `time.Duration.String()`，解码用
`time.ParseDuration`，避免毫秒截断丢失纳秒精度。`image_digests` 是 JSON object；
标准 `encoding/json` 会稳定排序字符串 map key。

`compose_metadata` 只允许 `image_digest` target 使用，只保存恢复计划需要的
`published_ports`。每项端口固定包含 `service`、可选 `host_ip`、可选 `published`、
`target`、`protocol`、可选 `app_protocol` 和可选 `mode`；不得保存 Compose environment、
secret、config 内容或 canonical JSON 原文。新备份必须从纯 stdout 的
`docker compose config --format json --no-env-resolution` 结构化提取这些字段。

`compose_metadata` 是 schema v1 的向后兼容可选字段：历史 manifest 缺失时仍可执行普通
原位恢复，但显式隔离恢复必须 fail closed 并要求重新备份，因为 dry-run 无法凭空还原历史
published port 声明。解码、`Result -> TargetResult -> Manifest` 传播和 Plan 内部副本均必须深拷贝，
避免调用方修改 slice 后改变已生成的恢复事实。

存储时必须使用固定 `ark-manifest.json`，不得包含日期或 run ID；tags 至少且当前固定为
`ark-manifest`、`run:<run-id>`。读取按 `ark-manifest` 查询 snapshots，使用时间升序、
同时间 ID 升序的最后一项，然后 dump 固定文件名。snapshot metadata 的唯一 path 可带
restic 的前导 `/`，归一化后必须等于固定文件名。

显式选择同样只在带 `ark-manifest` 查询结果内匹配，不能把 target snapshot ID 当作
manifest。完整 ID 优先于前缀；前缀必须唯一。选中候选后仍执行固定 path、manifest tag、
唯一 run tag、内容 schema 和 `run_id` 一致性校验，任一失败都 fail closed。

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
| 非 `image_digest` target 带 `compose_metadata` | 校验失败并指出字段路径 |
| published port 缺 service/protocol、target 为 0、host IP 非法或 published 超出 1-65535 | 校验失败 |
| 历史 `image_digest` target 缺 `compose_metadata` | manifest 仍有效；普通恢复可用，隔离 Plan 拒绝 |
| manifest backup 失败且返回 snapshot ID | 精确撤销该 ID，仍返回原始 backup error |
| manifest backup 与精确撤销同时失败 | 返回错误可分别 `errors.Is` 两条错误链 |
| manifest backup 失败且没有 snapshot ID | 返回失败，不调用 forget、不猜测候选 |
| 无 `ark-manifest` 候选 | `found=false, error=nil` |
| latest 且无候选 | `found=false, error=nil` |
| 显式 selector 为空 | 错误指出 snapshot 选择器不能为空 |
| 显式 ID/前缀无匹配 | 错误指出该 manifest snapshot 不存在 |
| 显式前缀匹配多个候选 | 错误要求提供更长 ID，不调用 Dump |
| 显式候选损坏 | 返回候选校验错误，不回退到其它 manifest |
| 候选缺 ID、时间、固定 path、manifest tag 或唯一 run tag | 读取失败 |
| dump JSON 非法、过大或 run ID 与 tag 不一致 | 读取失败 |
| restic Snapshots、Dump、Read/Close 失败 | 保留底层错误链并返回失败 |

### 5. Good/Base/Bad Cases

- **Good**：多 host、多 target，包含 ok/warn/fail 和 image digest map；JSON 往返后
  `TargetResult` 的 snapshot、bytes、duration、status、error、digest 和 Compose 端口元数据均不变。
- **Good**：同一 target 同时声明 TCP/UDP 或当前隔离模式暂不支持的协议；普通备份忠实记录，
  是否允许隔离由 restore 层按当时能力矩阵决定，备份层不篡改历史事实。
- **Good**：restic 已提交 manifest 并输出 ID 后返回非零；`SaveManifest` 只撤销该 ID，
  原始错误与可选撤销错误仍可识别。
- **Good**：`selector=def` 唯一匹配 `def222`，返回 manifest 与完整 `Snapshot{ID:"def222"}`。
- **Base**：仓库没有任何 `ark-manifest` snapshot，返回 `found=false`，调用方可继续首次备份。
- **Bad**：最新 snapshot 的 tag 是 `run:run-2`，dump 内容却写 `run_id=run-1`；必须失败，
  不能回退到更旧 manifest 掩盖仓库不一致。
- **Bad**：`selector=abc` 同时匹配 `abc111` 和 `abc222`；必须报歧义且不 dump 任一候选。
- **Bad**：`SaveManifest` 丢弃失败返回中的 ID，会让报告失败的清单仍进入最新恢复候选。
- **Bad**：为了生成隔离 dry-run，在 restore 时重新读取当前生产 Compose 端口；当前配置可能已经
  漂移，不能替代备份时事实。

### 6. Tests Required

`internal/backup/manifest_test.go` 至少覆盖：

- 完整 JSON 往返，包含纳秒 duration、status/error、多项 image digest 和 Compose 端口元数据；
- 历史 manifest 缺 `compose_metadata` 仍可解码，非 image target 携带元数据和非法端口字段必须拒绝；
- `Result -> TargetResult -> Manifest` 与恢复 Plan 对 `ComposeMetadata.PublishedPorts` 做深拷贝；
- map key 的稳定输出顺序；
- schema、主键、UTC 时间、时间倒序、负数、重复 target、类型与状态非法；
- 同 schema 未知可选字段可读取，未知 schema 明确拒绝；
- `SaveManifest` 精确 filename、tag、payload 与空 snapshot ID；失败返回带 ID 时精确撤销，
  覆盖撤销成功、双错误链和无 ID 不调用 forget；
- 多 snapshot 乱序输入下按时间和 ID 选择最新项；
- 多份 per-host manifest 下为每台请求 host 选择各自最新项，并保留全局最新项；
- 显式完整 ID、唯一前缀、无匹配、前缀歧义和选择失败零 Dump；
- 选择成功返回的 snapshot metadata 与实际 Dump ID 完全一致；
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

#### Wrong

```go
manifest, found, err := LoadLatestManifest(ctx, repo)
// 用户指定的 ID 没找到时静默使用 latest。
```

#### Correct

```go
manifest, snapshot, found, err := LoadManifestSelection(ctx, repo, selector)
if err != nil {
    return err
}
if !found {
    return fmt.Errorf("restic 仓库中不存在 manifest 快照")
}
```

恢复计划必须保存 `snapshot.ID`，且显式选择失败后立即返回；不得把 latest 当作兜底。

#### Wrong

```go
// restore 时读取当前 Compose，会把当前状态伪装成备份时事实。
ports := readCurrentComposePorts(destination)
```

#### Correct

```go
type manifestTargetWire struct {
    ImageDigests    map[string]string `json:"image_digests"`
    ComposeMetadata *ComposeMetadata  `json:"compose_metadata,omitempty"`
}
```

端口元数据必须随 `image_digest` 备份结果进入 manifest。历史字段缺失保持可读，但只有普通恢复
可以继续；隔离恢复必须明确要求重新备份。
