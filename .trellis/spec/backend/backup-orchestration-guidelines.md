# Backup Orchestration Guidelines

> `ark backup`、run 状态聚合、全局锁与 systemd 安装的可执行契约。

---

## Scenario: 修改 backup CLI 或 systemd 调度

### 1. Scope / Trigger

触发本规约的改动：

- 修改 `ark backup` 的参数、执行顺序、状态或退出语义；
- 修改 config、doctor、Runner、restic、manifest、store 的编排连接；
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
func BuildUnits(cfg *config.Config, binaryPath, configPath string) ([]systemd.Unit, error)
func Install(ctx context.Context, cfg *config.Config, options systemd.InstallOptions) (systemd.InstallResult, error)
```

系统固定路径：

```text
/run/ark.lock
/var/lib/ark/ark.db
/etc/systemd/system
```

### 3. Contracts

非 dry-run 状态机顺序固定为：

```text
load/validate -> select host -> nonblocking flock -> local doctor
  -> open store/create running run -> restic EnsureInit
  -> hosts in manifest order: host doctor -> targets in manifest order
  -> save manifest -> forget policies without prune -> one prune
  -> FinishRun -> Close store -> unlock
```

- `--dry-run` 只读取并静态校验 `ark.yaml`，输出 host、target、稳定 filename、tags、
  retention 和 manifest 信息；不得获取锁、运行 doctor/SSH/restic、打开状态库或写 unit。
- `--host` 在获取锁前验证，只保留指定 host；未知 host 是工具错误。
- `/run/ark.lock` 使用 `LOCK_EX|LOCK_NB`。冲突立即返回非零，不等待；全量 service、
  per-host service 和人工命令都通过同一 CLI 路径共享该锁。
- 未指定 `--skip-doctor` 时，local fail 在建库和创建快照前中止；local/host warn
  进入整体 warn。host fail 为该 host 每个 target 写 fail/skipped 结果并继续下一 host。
- target 启动失败由 CLI 写合成的 fail `RunTarget`；进入 `BackupTarget` 后由完整性层
  写真实结果。调用 context 已取消时，合成结果与 FinishRun 使用最多 10 秒的
  `context.WithoutCancel` 收尾，失败事实不能被取消抹掉。
- 任一 target/host/manifest/retention/prune/FinishRun 失败使 run fail；target warn 或
  doctor warn 在无失败时使 run warn。所有持久化错误文本只写阶段级脱敏摘要，返回 error
  仍保留底层错误链。
- partial 仍保存 manifest；manifest 保存失败保留已产生 target 快照并跳过 retention/prune。
- 只有无 fail 的 host 才应用自己的 `RetentionFor(host)`；manifest 使用 defaults retention。
  所有 `ForgetPolicy` 完成后只调用一次 `Prune`。
- `--json` 的 stdout 只含 snake_case JSON。backup 已输出成功/warn/fail 摘要后，失败通过
  `errBackupFailed` 只转换退出码，根命令不得重复打印同一错误。

systemd 合同：

- 生成 `ark-backup.service`、`ark-backup@.service` 和每 host 一个
  `ark-backup@<host>.timer`。service 固定 `Type=oneshot`；timer 使用
  `ScheduleFor(host)`、`Persistent=true`、`RandomizedDelaySec=600`。
- unit 只含 ark 二进制绝对路径、清单绝对路径和 host 参数，不读取或嵌入密码、对象存储
  env、SSH 私钥或项目 env 内容。
- 所有 unit 先写入目标目录内的临时目录并 fsync，再整体执行
  `systemd-analyze verify`。verify 成功后逐文件 rename；任一步失败恢复旧内容和权限。
- 既有同名 unit 必须以 `ManagedMarker` 开头且是普通文件；非 ark 管理文件、符号链接、
  目录或其它类型一律拒绝覆盖。
- 只删除 `ark-backup@*.timer` 中不再属于当前 host 且带 `ManagedMarker` 的普通文件；
  用户文件和符号链接不得删除。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| dry-run + 任意 host | 只 load/validate 和输出计划，无其它调用 |
| 未知 `--host` | 获取锁前返回错误 |
| flock 冲突 | 立即非零，无 doctor/store/repo 调用 |
| local doctor fail | 创建 run/快照前中止 |
| local/host doctor warn | 继续执行，最终至少为 warn |
| host doctor fail | 该 host target 全部记录 fail/skipped，继续后续 host |
| target Execute/BackupTarget fail | 记录 fail，继续同 host 后续 target |
| manifest 保存失败 | run fail，不执行 forget/prune，target 快照保留 |
| 某 host 有 target fail | 该 host 不执行 retention；成功 host 可执行 |
| retention 失败 | 继续其它 retention 和最终单次 prune，run fail |
| FinishRun 失败 | 返回非零，摘要降为 fail，不伪造成成功 |
| unit verify 失败 | 旧 unit 完全不变 |
| rename/delete/fsync 失败 | 回滚全部目标 unit；回滚错误与原错误组合 |
| 同名非受管 unit 或 symlink | fail closed，不覆盖、不删除 |

### 5. Good/Base/Bad Cases

- **Good**：web-01 的一个 target warn、其余成功；manifest 写入，web-01 retention 与
  manifest retention 完成后只 prune 一次，run/JSON/store 都是 warn。
- **Base**：`ark backup --host web-01 --dry-run --json` 输出两个 target 的稳定路径和
  retention，不创建 `/run/ark.lock`，也不读取 repo env/password。
- **Bad**：web-01 target 失败后仍对 web-01 执行 retention，可能在没有新可用备份时
  删除旧恢复点。
- **Bad**：install 看到用户自建的同名 timer 后直接 rename 覆盖，或跟随 symlink
  读取并在回滚时替换成普通文件。

### 6. Tests Required

`internal/cli/backup_test.go` 至少覆盖：

- host/target 串行顺序，manifest 后才 forget，全部 forget 后只 prune 一次；
- ok、doctor warn、target warn、host fail、target 启动 fail、TargetResult fail；
- partial 继续后续 target、完整 manifest/RunTarget、非零错误链；
- manifest 保存失败跳过 retention，FinishRun 失败不可伪装成功；
- `--host`、`--skip-doctor`、未知 host、dry-run 零副作用和 snake_case JSON；
- flock 冲突立即失败、释放后可重取；取消后的合成结果使用收尾 context；
- partial 输出后错误可被 `errors.Is(err, errBackupFailed)` 识别。

`internal/systemd/unit_test.go` 至少覆盖：

- 全量 service、模板 service、per-host schedule、Persistent 和随机延迟；
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
