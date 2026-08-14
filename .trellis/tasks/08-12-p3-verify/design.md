# 技术设计：ark verify

## 1. 职责边界

新增 `internal/verify` 承担恢复演练的生命周期、生产基线、清理决策与 detail JSON；它依赖现有
`restore`、`restic`、`sshexec` 和 `store`，但不复制 target 恢复实现。

```text
internal/cli
  -> 加载 config/repo/store/runner、选择 host、获取全局锁、输出结果
  -> internal/verify
       -> restore.BuildPlan
       -> restore.WithIsolationOptions(purpose=verify, instance=verificationID)
       -> restore.Execute
       -> restore.CleanupIsolation
       -> store.RecordVerification
```

`internal/restore` 继续只负责 Plan、隔离转换、执行与归属清理；`internal/systemd` 只生成和原子安装
oneshot service/timer；CLI 不承载基线算法或验证状态机。

## 2. 单 host 状态机

```text
select manifest/host
  -> generate verification ID
  -> BuildPlan + verify isolation transform
  -> capture production baseline
  -> restore.Execute
  -> health/digest result
  -> cleanup by default, or validate keep-on-failure ownership
  -> capture and compare production baseline
  -> RecordVerification
```

一次无 `--host` 且选择 `latest` 的命令先为当前清单中的每台 host 解析最近一次包含它的 manifest，再在
同一全局锁内按清单顺序串行执行上述状态机。显式 manifest ID 保持单 manifest 语义。某 host 失败后
继续下一 host，最终聚合为非零退出；状态写入失败同样属于该 host 的失败，不能只留在 journal。

context 取消不启动新的恢复阶段，但会用有界 `context.WithoutCancel` 收尾 cleanup、基线复核和
`RecordVerification`，避免取消把失败事实抹掉。

## 3. Verify 隔离策略

现有 `IsolationOptions` 增加明确的 published-port 策略。普通 `restore --isolate` 保持
`runtime_auto` 和原 host IP 语义；verify 固定选择 `disabled`，由同一 Compose 结构化转换删除全部
published ports。这样演练不会绑定生产 IP/公网端口，也不需要另写端口改写器。

verify 仍使用 P3-2A 的稳定 ID、project/resource 派生、files root、Compose canonical JSON、label、
state.json 和 cleanup 契约。external 资源、host namespace、越界路径和无法保真的 Compose 能力继续
在 Docker 写入前拒绝。

## 4. 生产不变证明

`internal/verify` 通过 destination runner 采集可排序、可比较的生产基线：

- Compose project 的容器 ID、状态、service 与运行镜像 digest；
- project network 的 ID、driver 与 label；
- project volume 的 name/label；Docker volume 没有独立 ID，name 即稳定身份；
- 清单声明的生产 files 路径的类型、权限、大小、时间与安全元数据摘要。

基线命令必须使用结构化 Docker 输出和固定 argv，不执行用户提供的 shell。快照比较只输出脱敏资源
标识和差异类型，不输出 `.env`、文件内容或容器环境变量。清理/保留决策后基线有任何变化都判 fail。

## 5. 结果与持久化

复用现有 `store.Verification`，不迁移 schema：

- `ID`：本次 host 演练 ID，同时作为 isolation instance key；
- `Host`：source 原 host；
- `RunID`：manifest 中的 backup run ID；
- `SnapshotID`：精确 manifest snapshot ID；
- `Status/Error`：最终聚合状态与阶段级脱敏摘要；
- `DetailJSON`：versioned verification detail。

`verifications.run_id` 只在本地状态库仍存在对应 run 时建立外键；历史 manifest 来自保留的 restic
仓库而本地 run 已缺失时写 `NULL`，来源 run ID 继续保留在 detail JSON，不能因此否定备份可恢复性。

detail 记录 target snapshot 关联、restore steps、isolation、baseline diff、cleanup 与保留状态。CLI 的
人类输出和 `--json` 都从同一结果模型生成，JSON stdout 不混入进度文本。

## 6. systemd 调度

`ark install` 在现有备份 unit 之外生成：

```text
ark-verify.service  -> ark --config <path> verify
ark-verify.timer    -> OnCalendar=weekly, Persistent=true, RandomizedDelaySec=21600
```

只生成一个 all-host timer，避免 per-host timer 在同一时刻争用 `/run/ark.lock`。service 继续使用
`CacheDirectory=ark`、0700 cache 和 `XDG_CACHE_HOME=/var/cache/ark`。所有 unit 一起暂存、执行
`systemd-analyze verify`、原子替换和失败回滚；陈旧受管 verify timer 也纳入精确回收，用户文件不动。

## 7. 测试

- `internal/verify`：表驱动 fake Runner/Repo/Store 覆盖成功、restore/health/baseline/cleanup/store
  失败、取消收尾、keep-on-failure 和多 host 继续。
- `internal/restore`：验证普通 isolation 仍自动分配端口，verify 策略删除 published ports，二者共享
  project/path/label/cleanup 实现。
- `internal/cli`：覆盖参数、全局锁、host 选择、人类/JSON 输出、退出码和无 host 串行聚合。
- `internal/systemd`：覆盖 weekly verify units、无凭证、原子安装、陈旧 timer 回收和真实
  `systemd-analyze verify`。
- Docker 集成测试使用唯一 project、`testing.Short()` 和二进制检测保护，证明生产 project 基线不变
  且清理无残留。

## 8. 已知边界

- 原 host 演练共享 Docker daemon，只能证明备份可读、恢复链可执行和资源隔离，不能替代独立机器
  灾备；该风险与 P3-3 受限验收一致并已被用户接受。
- 首版不做外部 HTTP 登录/写入探针，也不提供专用验证 host；这些能力需要新的配置和安全设计。
