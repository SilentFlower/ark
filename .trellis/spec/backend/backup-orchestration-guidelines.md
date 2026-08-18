# Backup Orchestration Guidelines

> `ark backup`、run 状态聚合、全局锁与 systemd 安装的可执行契约。

---

## Scenario: 修改 backup CLI 或 systemd 调度

### 1. Scope / Trigger

触发本规约的改动：

- 修改 `ark backup` 的参数、执行顺序、状态或退出语义；
- 修改 config、doctor、Runner、restic、manifest、store 的编排连接；
- 修改 hub 本地状态库 target 的识别、在线导出或对象锁 doctor 提示；
- 修改全局 flock、保留策略或 prune 时机；
- 修改 `ark install`、service/timer 模板、unit 校验、写入或陈旧 timer 回收。

编排位于 `internal/cli/backup.go`，systemd 文件边界位于
`internal/systemd/unit.go`。target 产流、完整性、manifest wire、SQLite 和 restic
命令仍由各自包负责，CLI 不重复实现这些底层语义。

### 2. Signatures

```text
ark backup [--host <name>] [--dry-run] [--skip-doctor] [--json]
ark install [--unit-dir <absolute-path>] [--json]
```

```go
// internal/monitoring
type HeartbeatStatus string

const (
    HeartbeatDisabled HeartbeatStatus = "disabled"
    HeartbeatSent     HeartbeatStatus = "sent"
    HeartbeatFailed   HeartbeatStatus = "failed"
)

// internal/cli
type backupRunSummary struct {
    RunID              string                     `json:"run_id"`
    Status             store.Status               `json:"status"`
    Manifest           *backup.Manifest           `json:"manifest,omitempty"`
    ManifestSnapshotID string                     `json:"manifest_snapshot_id"`
    HeartbeatStatus    monitoring.HeartbeatStatus `json:"heartbeat_status"`
    Error              string                     `json:"error"`
}
```

```go
func BuildUnits(cfg *config.Config, binaryPath, configPath string) ([]systemd.Unit, error)
func Install(ctx context.Context, cfg *config.Config, options systemd.InstallOptions) (systemd.InstallResult, error)
```

系统固定路径：

```text
/run/ark.lock
/var/lib/ark/ark.db
/etc/systemd/system
```

hub 状态库 target 的稳定输入输出：

```text
files target path: /var/lib/ark/ark.db
restic stdin filename: <host>/files/<target-name>.db
doctor check: repo.object_lock = warn
```

### 3. Contracts

非 dry-run 状态机顺序固定为：

```text
load/validate -> select host -> nonblocking flock -> local doctor
  -> open store/create running run -> restic EnsureInit
  -> hosts in manifest order: host doctor -> targets in manifest order
  -> save manifest -> forget policies without prune -> one prune
  -> FinishRun -> Close store -> unlock -> deliver heartbeat -> print summary
```

- `--dry-run` 只读取并静态校验 `ark.yaml`，输出 host、target、稳定 filename、tags、
  retention 和 manifest 信息；不得获取锁、运行 doctor/SSH/restic、打开状态库或写 unit。
- `--host` 在获取锁前验证，只保留指定 host；未知 host 是工具错误。
- 仅 `local: true` host 的 `files` target 在清理后的绝对路径精确匹配
  `/var/lib/ark/ark.db` 时调用 `Store.ExportSnapshot`，不得走普通 tar。
  状态库必须只声明一个独立 target；同 target 混入其它路径、另一 target 覆盖其父目录、子路径、
  `-wal` / `-shm` sidecar 或重复声明状态库时，都在获取锁前拒绝。
- host doctor 读取默认激活的 Compose service 后，必须要求恰有 image_digest target 覆盖全部这些
  service；缺 target、漏 service 或引用未知 service 都是 fail。profile 未激活的可选 service 不在此集合。
- 状态库导出仍走 `BackupTarget` 的 reader / Wait / Close 完整性链，稳定 filename 使用
  `.db`；其它 files、volume 和数据库 target 的流式实现不得因此改成落盘模式。
- `/run/ark.lock` 使用 `LOCK_EX|LOCK_NB`。冲突立即返回非零，不等待；全量 service、
  per-host service 和人工命令都通过同一 CLI 路径共享该锁。
- 未指定 `--skip-doctor` 时，local fail 在建库和创建快照前中止；local/host warn
  进入整体 warn。host fail 为该 host 每个 target 写 fail/skipped 结果并继续下一 host。
  doctor 失败错误只列 `Check.Name`，不得复制可能包含外部命令详情或敏感值的 `Detail`。
- `RunLocal` 固定输出 `repo.object_lock=warn`，说明 provider-neutral 条件下只能人工在
  控制台核对对象锁与长期保留期。该检查不读取或输出仓库凭证，也不能伪装成 ok。
- target 启动失败由 CLI 写合成的 fail `RunTarget`；进入 `BackupTarget` 后由完整性层
  写真实结果。调用 context 已取消时，合成结果与 FinishRun 使用最多 10 秒的
  `context.WithoutCancel` 收尾，失败事实不能被取消抹掉。
- 任一 target/host/manifest/retention/prune/FinishRun 失败使 run fail；target warn 或
  doctor warn 在无失败时使 run warn。所有持久化错误文本只写阶段级脱敏摘要，返回 error
  仍保留底层错误链。
- partial 仍保存 manifest；manifest 保存失败保留已产生 target 快照并跳过 retention/prune；
  若失败返回中带 manifest snapshot ID，`SaveManifest` 必须先精确撤销该 manifest 快照。
- 只有无 fail 的 host 才应用自己的 `RetentionFor(host)`；manifest 使用 defaults retention。
  所有 `ForgetPolicy` 完成后只调用一次 `Prune`。
- `--json` 的 stdout 只含 snake_case JSON。backup 已输出成功/warn/fail 摘要后，失败通过
  `errBackupFailed` 只转换退出码，根命令不得重复打印同一错误。
- 非 dry-run 的 heartbeat 位于 `runBackup` 返回后的命令边界；此时 Store、锁、run 和 manifest 已经收尾。
  `ok` / `warn` 选择成功端点，`fail`、取消、锁失败、前置失败或空摘要选择失败端点。
- `--dry-run`、未配置 `monitoring` 或秘密文件中未启用 heartbeat 时不得发请求，状态分别不产生摘要或为 `disabled`。
- backup context 已取消时，heartbeat 使用 `context.WithoutCancel` 加 10 秒超时尽力发送最终失败终态。
- heartbeat 配置或网络失败只把摘要设为 `heartbeat_status=failed` 并输出脱敏警告；不得修改 run、manifest、
  `backupFailureSet`、`errBackupFailed` 或既有退出码。原 ok/warn 仍退出 0，原 fail 仍退出 1。
- 已形成的 backup JSON 必须总是包含 `heartbeat_status=disabled|sent|failed`；Hub 手工 backup 的
  operation 白名单和前端类型必须同步。清单本身加载失败时不能安全取得秘密路径，因此不发送 heartbeat。

systemd 合同：

- 生成 `ark-backup.service`、`ark-backup@.service` 和每 host 一个
  `ark-backup@<host>.timer`。service 固定 `Type=oneshot`；timer 使用
  `ScheduleFor(host)`、`Persistent=true`、`RandomizedDelaySec=600`。
- 同时生成单个 `ark-verify.service` 与 `ark-verify.timer`；service 不带 `--host`，timer 固定
  `OnCalendar=weekly`、`Persistent=true`、`RandomizedDelaySec=21600`，一次串行演练全部 host。
- 三种 service 都固定 `CacheDirectory=ark`、`CacheDirectoryMode=0700` 和
  `Environment=XDG_CACHE_HOME=/var/cache/ark`，由 systemd 创建受管缓存目录；不得依赖
  system service 继承交互 shell 的 `HOME`。
- unit 只含 ark 二进制绝对路径、清单绝对路径和 host 参数，不读取或嵌入密码、对象存储
  env、SSH 私钥或项目 env 内容。
- 所有 unit 先写入目标目录内的临时目录并 fsync，再整体执行
  `systemd-analyze verify`。verify 成功后逐文件 rename；任一步失败恢复旧内容和权限。
- 既有同名 unit 必须以 `ManagedMarker` 开头且是普通文件；非 ark 管理文件、符号链接、
  目录或其它类型一律拒绝覆盖。
- 只删除不再需要、带 `ManagedMarker` 且为普通文件的 `ark-backup@*.timer` 或
  `ark-verify*.timer`；用户文件和符号链接不得删除。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| dry-run + 任意 host | 只 load/validate 和输出计划，无其它调用 |
| 未知 `--host` | 获取锁前返回错误 |
| flock 冲突 | 立即非零，无 doctor/store/repo 调用 |
| local doctor fail | 创建 run/快照前中止；错误只列失败检查项名称，不含 Detail |
| local/host doctor warn | 继续执行，最终至少为 warn |
| hub 状态库独占 files target | 使用 Online Backup，stdin filename 以 `.db` 结尾 |
| hub 状态库与其它 path 混在同一 target | 获取锁前 fail closed，不执行 doctor/store/restic |
| 其它 files target 覆盖状态库父/子路径、WAL/SHM 或重复状态库 target | 获取锁前 fail closed |
| 默认活跃 Compose service 未被 image_digest 完整覆盖 | host doctor fail，不产生新恢复点 |
| 远程 host 恰有同名路径 | 仍按普通远程 files target 处理，不误用 hub store |
| 对象锁无法 provider-neutral 核验 | `repo.object_lock` 返回 warn，不阻断 doctor/backup |
| host doctor fail | 错误只列失败检查项名称；该 host target 全部记录 fail/skipped，继续后续 host |
| target Execute/BackupTarget fail | 记录 fail，继续同 host 后续 target |
| manifest 保存失败 | run fail，不执行 forget/prune，target 快照保留 |
| 某 host 有 target fail | 该 host 不执行 retention；成功 host 可执行 |
| retention 失败 | 继续其它 retention 和最终单次 prune，run fail |
| FinishRun 失败 | 返回非零，摘要降为 fail，不伪造成成功 |
| dry-run 或未配置 heartbeat | 零网络请求；dry-run 保持原计划输出，正常摘要为 `heartbeat_status=disabled` |
| backup 终态 ok/warn | 调成功端点；成功后为 `sent` |
| backup 终态 fail、取消、锁失败或其它运行错误 | 调失败端点；heartbeat 结果不覆盖原错误 |
| heartbeat 配置或网络失败 | 摘要为 `failed`，stderr 只输出脱敏警告，run/manifest/退出码不变 |
| 清单加载失败 | 不读取 monitoring 文件、不发送请求，由外部监控宽限期发现缺失 heartbeat |
| unit verify 失败 | 旧 unit 完全不变 |
| system service 没有 HOME | 使用 `/var/cache/ark` 作为 `XDG_CACHE_HOME`，restic 可启动 |
| rename/delete/fsync 失败 | 回滚全部目标 unit；回滚错误与原错误组合 |
| 同名非受管 unit 或 symlink | fail closed，不覆盖、不删除 |

### 5. Good/Base/Bad Cases

- **Good**：web-01 的一个 target warn、其余成功；manifest 写入，web-01 retention 与
  manifest retention 完成后只 prune 一次，run/JSON/store 都是 warn。
- **Base**：`ark backup --host web-01 --dry-run --json` 输出两个 target 的稳定路径和
  retention，不创建 `/run/ark.lock`，也不读取 repo env/password。
- **Base**：hub 状态库 target 输出 `hub-01/files/ark-state.db`，而远程同名路径仍输出
  普通 files tar；识别不跟随符号链接，也不靠 target name 猜测。
- **Base**：systemd 在没有 `HOME` 的 system service 中创建 `/var/cache/ark`，权限 0700，
  restic 使用 `XDG_CACHE_HOME` 正常执行。
- **Good**：warn 备份完成并持久化后成功调用 heartbeat 成功端点，JSON 为 `heartbeat_status=sent` 且退出 0。
- **Base**：未配置 monitoring 的旧清单继续完成备份，JSON 为 `disabled`，没有新增网络副作用。
- **Bad**：heartbeat 超时后把已成功的 run 改为 fail，或把 URL/token 写进 stderr；监控故障不能改写备份事实。
- **Bad**：web-01 target 失败后仍对 web-01 执行 retention，可能在没有新可用备份时
  删除旧恢复点。
- **Bad**：install 看到用户自建的同名 timer 后直接 rename 覆盖，或跟随 symlink
  读取并在回滚时替换成普通文件。
- **Bad**：把 `/var/lib/ark/ark.db` 和 `/etc/ark/ark.yaml` 放进同一个 files target，
  让状态库退回普通 tar，得到缺 WAL 页的“成功”坏备份。
- **Bad**：状态库单独声明后又备份 `/var/lib/ark` 或 `ark.db-wal`；第二个 tar 仍会带入不一致的
  SQLite 文件，并可能在恢复时覆盖 Online Backup 产物。

### 6. Tests Required

`internal/cli/backup_test.go` 至少覆盖：

- host/target 串行顺序，manifest 后才 forget，全部 forget 后只 prune 一次；
- ok、doctor warn、target warn、host fail、target 启动 fail、TargetResult fail；
- partial 继续后续 target、完整 manifest/RunTarget、非零错误链；
- manifest 保存失败跳过 retention，FinishRun 失败不可伪装成功；
- `--host`、`--skip-doctor`、未知 host、dry-run 零副作用和 snake_case JSON；
- flock 冲突立即失败、释放后可重取；取消后的合成结果使用收尾 context；
- partial 输出后错误可被 `errors.Is(err, errBackupFailed)` 识别。
- hub 状态库精确路径走 `exportState` 而不调用普通 `executeTarget`，filename 稳定为 `.db`；
- 混合、父/子路径、WAL/SHM 与重复状态库 target 均在加锁前拒绝，远程同名路径不误判；
- image_digest target 缺失、漏默认活跃 service 或包含未知 service 时 doctor fail；
- doctor 的 `repo.object_lock` 在人类和 JSON 输出中都是 warn；local/host fail 摘要只包含
  失败项名称，不复制 Detail。
- ok/warn 调成功端点，fail/取消/锁失败调失败端点；配置失败和网络失败保持原退出码；
- disabled/sent/failed 的人类输出与 JSON，Hub operation 对必填字段的解析；
- dry-run、未配置和清单加载失败零请求；取消路径使用独立有界收尾 context；
- URL query、签名密钥和完整端点不出现在 warning、error、JSON、Store 或 manifest。

`internal/systemd/unit_test.go` 至少覆盖：

- 全量 service、模板 service、per-host schedule、Persistent 和随机延迟；
- backup/verify service 都包含 `CacheDirectory=ark`、0700 mode 和 `/var/cache/ark` 环境；
- verify service 不带 host，weekly timer 使用 21600 秒随机延迟；
- unit 不含密码/env/私钥内容；
- 真实 `systemd-analyze verify`，由 `testing.Short` 和 `LookPath` 保护；
- verify 前不改旧文件，rename 中途失败完整回滚；
- 只清理受管陈旧 timer，保留用户文件和 symlink；
- 拒绝覆盖非受管同名 unit、symlink 和非普通文件。

提交门禁：`make check`、相关包 `-race -count=10`、常规构建、无 CGO 构建和
`git diff --check`。

### 7. Wrong vs Correct

#### Wrong

```go
for _, host := range hosts {
    runTargets(host)
    repo.Forget(ctx, cfg.RetentionFor(host), []string{"host:" + host.Host})
}
```

每台 host 完成后立即 `forget --prune` 会反复获取仓库排他锁；失败 host 仍清理旧快照，
还可能在后续 host 尚未备份时占满 I/O。

#### Correct

```go
for _, host := range successfulHosts {
    err := repo.ForgetPolicy(ctx, cfg.RetentionFor(host), []string{"host:" + host.Host})
    // 聚合错误并继续其它 policy。
}
err := repo.ForgetPolicy(ctx, cfg.RetentionFor(nil), []string{backup.ManifestTag})
err = errors.Join(err, repo.Prune(ctx))
```

target 全部结束并成功写入 manifest 后，先按分组标记删除，最后只 prune 一次；失败 host
不进入 `successfulHosts`。

#### Wrong

```go
source, err := backup.Execute(ctx, host, target, runner) // 本地 ark.db 被 tar
```

#### Correct

```go
if isStateDatabaseTarget(host, target, store.DefaultPath) {
    reader, err := state.ExportSnapshot(ctx)
    // 构造稳定 .db source，后续仍交给 BackupTarget。
}
```

状态库识别必须同时依赖 local host、files 类型、单一路径和清理后的绝对路径；只看文件名
或 target name 会误伤远程机器，只看 `paths` 包含关系会放过混合 tar。

#### Wrong

```go
if err := monitoring.SendHeartbeat(ctx, settings, failed); err != nil {
    summary.Status = store.StatusFail
    runErr = errors.Join(runErr, err)
}
```

#### Correct

```go
heartbeatStatus, heartbeatErr := deliverBackupHeartbeat(ctx, cfg, summary, runErr, dependencies)
summary.HeartbeatStatus = heartbeatStatus
if heartbeatErr != nil {
    cmd.PrintErrf("警告: %v\n", heartbeatErr)
}
```

heartbeat 是已完成备份事实之外的可观测性投递；失败必须可见，但不能污染 run 状态或退出语义。
