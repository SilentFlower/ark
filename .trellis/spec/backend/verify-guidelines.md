# Verify Guidelines

> `ark verify`、生产基线、隔离资源收尾、Verification 持久化与每周调度的可执行契约。

---

## Scenario: 修改自动恢复演练

### 1. Scope / Trigger

触发本规约的改动：

- 修改 `ark verify` 参数、host/manifest 选择、串行聚合或退出语义；
- 修改 `internal/verify` 的基线、状态机、失败保留、清理或 detail JSON；
- 修改 verify 使用的 `restore.IsolationOptions`、published-port 策略或归属校验；
- 修改 `ark-verify.service` / `ark-verify.timer` 的生成、安装或回收。

verify 只编排一次可销毁的恢复演练。target 数据流、数据库导入、image digest、Compose health、
隔离命名与清理由 `restore` 持有；CLI 只负责 config/repo/store/runner、全局锁、host 选择与输出，
不得复制第二套恢复实现。

### 2. Signatures

```text
ark verify [--host <source>] [--snapshot latest|<manifest-id>]
  [--keep-on-failure] [--json]
```

```go
func Execute(
    ctx context.Context,
    plan restore.Plan,
    repo *restic.Repo,
    runner sshexec.Runner,
    state *store.Store,
    options verify.Options,
) (verify.Result, error)

func RecordFailure(
    ctx context.Context,
    state *store.Store,
    failure verify.Failure,
) (verify.Result, error)

func CaptureBaseline(
    ctx context.Context,
    runner sshexec.Runner,
    plan restore.Plan,
) (verify.Baseline, error)

func CompareBaselines(before verify.Baseline, after verify.Baseline) []string
```

固定运行路径与调度：

```text
/run/ark.lock
ark-verify.service -> ark --config <path> verify
ark-verify.timer   -> OnCalendar=weekly, Persistent=true, RandomizedDelaySec=21600
```

### 3. Contracts

- 未指定 `--host` 且 selector 为 `latest` 时，按当前 config 顺序为每台 host 选择最近一次包含它的
  manifest 并串行验证；不能只使用全局最新的一份 per-host manifest。显式 manifest ID 保持单 manifest
  语义，只验证其中可匹配的 host。manifest 中已不在 config 的 host 必须写前置失败 Verification，不能
  静默忽略。显式 host 不在 config 时加锁前失败；在 config 但没有可用 manifest 时打开状态库后记录失败。
- destination 首版固定等于 source，不提供 `--to`、`--force` 或 `--skip-doctor`。每台 host 必须使用
  `restore.BuildPlan(cfg, manifest, snapshotID, host, host)`，然后由 `verify.Execute` 转成隔离 Plan。
- verify 固定调用 `WithIsolationOptions`，`Purpose=verify`、`InstanceKey=verification ID`、
  `PortAllocation=disabled`。结构化 Compose 转换必须删除整个 service `ports` 字段；普通
  `restore --isolate` 继续使用 `runtime_auto`，两者共享 project/path/label/state/cleanup 实现。
- 单 host 顺序固定为：生成 ID -> 生产基线 -> verify isolation -> `restore.Execute` -> 默认 cleanup，
  或失败时校验 keep ownership -> 生产基线复核 -> `Store.RecordVerification`。
- `--keep-on-failure` 仅在恢复已失败，且 `state.json`、destination、project、路径、container/network/
  volume isolation label 全部匹配时保留。归属无法证明时必须回退到 cleanup；不能跳过结束基线。
- 生产基线包含 Compose project 容器 ID/service/state/image ID/RepoDigests、network ID/name/driver/labels、
  volume name/driver/labels，以及声明 files 路径的 stat 和目录树递归元数据指纹。目录遍历只读取路径、类型、
  mode、UID/GID、大小、mtime 与 symlink target，不读取文件内容、环境值或业务数据。
- cleanup、结束基线和持久化是最终结果的一部分。任一失败、`restore.Result.Status=fail` 或基线差异都使
  Verification 为 fail。Store 写入失败时，`Result.Error` 必须保留已发生的阶段级脱敏摘要并追加
  “记录演练结果失败”，返回 error 使用 `errors.Join` 同时保留原阶段与 Store 错误链。context 取消后
  使用最多 30 秒的 `context.WithoutCancel` 收尾。
- detail JSON 固定带 schema version、source/host、run、manifest snapshot、target snapshot 关联、
  restore steps/isolation、baseline fingerprint/diff、cleanup、keep ownership 和阶段级错误；不得包含
  repo URL、SSH 配置、canonical Compose、env、凭证、命令输出或业务内容。
- 人类输出与 JSON 都来自同一 summary。任一 host 失败时继续后续 host，最终返回 `errVerifyFailed`；
  `--json` stdout 只能包含一份 snake_case JSON。backup、restore、verify 共用非阻塞 `/run/ark.lock`。
- `ark install` 把 verify service/timer 与全部 backup units 一起暂存、`systemd-analyze verify`、原子替换
  和回滚。只允许删除带 `ManagedMarker` 的普通 `ark-backup@*.timer` 或 `ark-verify*.timer`。

### 4. Validation & Error Matrix

| 条件 | 必须行为 |
|---|---|
| 未知显式 host | 获取锁前失败，不访问 repo/store/runner |
| 显式 host 不在 manifest | 写前置 fail Verification，命令非零 |
| manifest host 已不在 config | 写该 host 的前置 fail，继续可匹配 host |
| `latest` 的某 config host 没有任何 manifest | 写该 host 的前置 fail，继续其它 host |
| flock 冲突 | 立即非零，无 doctor/repo/store/runner 调用 |
| local doctor fail | 每个待验证 host 写前置 fail，不启动恢复 |
| BuildPlan 漂移、host doctor fail、Runner 创建失败 | 写当前 host 前置 fail，继续下一 host |
| baseline 前置采集失败 | 零隔离写入，记录 fail |
| 隔离转换或原始单文件映射失败 | 零 Docker 写入，记录 fail |
| Dump/Feed/database/digest/health 失败或 restore 返回 fail | 默认 cleanup，记录 fail |
| keep-on-failure 归属校验失败 | 不保留，执行 cleanup，记录 fail |
| cleanup、结束基线或 Store 写入失败 | 最终 fail，不输出成功 |
| Store 写入失败且已有阶段失败 | 保留原阶段摘要并追加记录失败；返回 error 可识别两条错误链 |
| 生产容器/network/volume/files 任一变化 | differences 记录类别，最终 fail |
| context 取消 | 不启动下一 host；当前 host 有界 cleanup/baseline/store 收尾 |
| JSON 输出写失败 | 返回值同时保留 `errVerifyFailed`、执行错误和 writer 错误 |

### 5. Good / Base / Bad Cases

- **Good**：web-01 恢复失败并完成 cleanup，web-02 随后成功；两台都写 Verification，summary fail，
  JSON 仍包含两条结果，生产基线均不变。
- **Base**：`ark verify --host web-01 --snapshot latest` 在原 host 创建独立 verify project，容器只有
  Compose 内部网络、不发布宿主机端口；health 成功后清理全部资源并记录 ok/warn。
- **Good**：显式 `--keep-on-failure` 且完整 ownership 校验通过，输出精确
  `ark restore cleanup --host ... --isolation ...`；结束生产基线仍完全一致。
- **Bad**：为避开端口冲突只把生产端口加偏移。它仍会意外暴露公网端口，且复制了普通 isolation
  的端口语义；verify 必须直接删除全部 published ports。
- **Bad**：manifest 中旧 host 不在 config 就直接跳过，会让“没有被验证”看起来像“全部通过”。
- **Bad**：只检查目录根节点 stat 或只检查容器是否 running，会漏掉配置树、镜像或资源身份漂移。

### 6. Tests Required

- `internal/verify` 覆盖基线排序/指纹、RepoDigests、目录递归元数据、成功、restore fail/status fail、
  cleanup fail、baseline diff、store fail、restore 与 store 同时失败时的摘要/错误链聚合、keep ownership、
  取消后收尾和 target snapshot detail。
- `internal/restore` 覆盖普通 isolation 仍为 runtime-auto，verify 删除全部 `ports`，并共享 project/path/
  label/state/cleanup。真实 Docker 用例必须由 `testing.Short()`、`ARK_DOCKER_INTEGRATION=1` 和本地镜像
  检测保护，并断言生产 project 前后基线一致、verify 容器 `docker port` 为空、清理无残留。
- `internal/cli` 覆盖未知/漂移 host、锁顺序、doctor/plan/runner 前置失败落库、全 host 串行继续、
  每 host latest manifest 选择、显式 snapshot 单 manifest、JSON 单输出、脱敏、`errVerifyFailed` 与
  close/unlock 错误聚合。
- `internal/systemd` 覆盖 weekly all-host service/timer、21600 秒随机延迟、CacheDirectory、无凭证、
  真实 `systemd-analyze verify`、原子回滚和受管陈旧 timer 精确回收。

提交前运行：

```bash
go test ./internal/verify ./internal/restore ./internal/cli ./internal/store ./internal/systemd -race -count=1
go test ./internal/verify ./internal/restore ./internal/cli ./internal/systemd -race -count=10
make check
make build
CGO_ENABLED=0 go build -o bin/ark-nocgo ./cmd/ark
go mod verify
git diff --check
```

### 7. Wrong vs Correct

#### Wrong

```go
plan, _ := restore.BuildPlan(cfg, manifest, snapshotID, host, host)
plan.Project.ProjectName += "-verify"
runRestore(plan) // 没有路径、label、端口和 cleanup 归属契约
```

#### Correct

```go
plan, err := restore.BuildPlan(cfg, manifest, snapshotID, host, host)
if err != nil {
    return recordPreflightFailure(err)
}
result, err := verify.Execute(ctx, plan, repo, runner, state, verify.Options{
    KeepOnFailure: keepOnFailure,
})
```

verify 只接收 source=destination 的未隔离完整 Plan，并通过现有 isolation/restore/cleanup 边界完成演练；
所有无法证明的资源、路径和最终状态都 fail closed。
