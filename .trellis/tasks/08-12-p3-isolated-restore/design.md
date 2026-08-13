# 技术设计：P3-2A 隔离恢复命名与端口映射

## 1. 总体边界

隔离恢复复用现有 `restore.Plan` 与 Executor 的 target 恢复、digest、数据库和 health 语义，只在
执行模型中增加隔离意图、运行时 Compose 转换、资源状态与 cleanup：

```text
manifest + ark.yaml
  -> restore.Plan + IsolationSpec（纯数据，可 dry-run）
  -> files 恢复到 isolation root
  -> docker compose config --format json
  -> IsolationRuntime（结构化改写并持久化）
  -> 现有 image/volume/database/application/health 执行链
  -> inspect 实际端口并原子更新 isolation state
  -> 显式 cleanup
```

普通恢复保持原语义。CLI 只有显式 `--isolate` 才构建 `IsolationSpec`；`--force` 与隔离模式互斥。

## 2. 数据模型与稳定身份

在 Plan 中增加可选的纯值 `IsolationSpec`，至少包含 schema version、完整 isolation ID、短 ID、
purpose、隔离 project name、root、files root、generated compose path，以及端口分配策略 `runtime_auto`。
不把 Runner、Compose 原文、插值后的环境变量或目标机探测结果放进 Plan。

普通 restore 的 isolation ID 使用以下稳定输入做 SHA-256：

```text
schema + manifest_snapshot_id + source_host + destination_host
  + project.name + project.project_name + project.compose_file
```

状态目录使用完整摘要，项目名使用短摘要。项目前缀先规范为 Compose 允许的小写字符，并为后缀
保留长度；空前缀回退为 `ark`。同一输入只对应一份副本，因此重复命令天然进入续跑。

目标机状态固定在：

```text
/var/lib/ark/restore/isolations/<isolation-id>/
  state.json
  files/
  compose.generated.json
  compose-images.json
  steps/
  complete
```

`state.json` 使用 schema 化结构保存原 Plan 执行身份、预期资源、路径映射、原端口声明和实际端口。
所有文件通过现有 root-only 原子写 helper 落盘。状态存在但执行身份不一致时拒绝接管。

为让后续 `ark verify` 复用，内部构造器接受 purpose/instance key；普通 restore 使用稳定默认 key，
verify 可传 verification ID，但二者共用转换、标签、状态和 cleanup 实现。

## 3. 安全恢复与 Compose 转换

files tar 不再解到 `/`，而是解到 `<root>/files`。tar 中原绝对路径去掉前导 `/` 后映射为：

```text
/srv/app/compose.yaml -> <root>/files/srv/app/compose.yaml
```

所有 files target、原始单文件 target、project compose/env 路径都按同一规则映射。原路径不删除、
不覆盖。Compose 文件未包含在成功 files target 中时，隔离恢复在 Docker 创建前失败。

恢复完 files 后，对映射后的 Compose 文件运行现有三件套定位参数及
`docker compose config --format json`，让 Docker Compose 负责 include、extends、变量、路径和端口
规范化。输出只在进程内解析，不进入 CLI 日志；生成配置以 `0600` 写入 isolation root。

结构化转换规则：

- project name 改为派生名称；移除 service `container_name`，由 Compose 按隔离 project 生成。
- top-level volume/network 的实际 `name` 改为带 short ID 的唯一名称，并增加
  `io.ark.restore.isolation` label；service/container 同样增加该 label。
- bind mount、config、secret 的宿主机源路径必须被已恢复 files path 覆盖，并改到 files root。
- 非 external 的命名资源全部隔离；external volume/network/config/secret 一律拒绝。
- `network_mode: host`、`ipc: host`、`pid: host`、`container:` namespace、外部 container 引用、
  无法隔离的 device 与其它直接共享宿主或生产资源的配置一律拒绝。
- Plan 中 volume target 的物理名称和 files target 路径同步替换，后续继续交给现有 Executor。

转换失败只允许删除 isolation root 内本次产生的临时材料，不得创建 Compose 资源或访问原路径。

## 4. 端口分配

Canonical Compose 模型会把短语法、长语法和范围展开为端口对象。转换为每个端口保留：

- service、target、protocol、app_protocol、mode；
- 原 published 端口或范围；
- 原 host IP 语义。

生成配置删除 `published`，保留 `host_ip`。Docker 在容器创建时原子选择空闲宿主机端口，避免
“先扫描空闲端口、后启动”之间的竞争。原来未写 host IP 或写 `0.0.0.0` 的仍对所有接口绑定；
回环和具体地址保持不变。

若配置写了具体 host IP，转换阶段通过 `ip -j address` 读取目标机地址并结构化校验；命令缺失、
输出无效或 IP 不存在均 fail closed，不回退地址。

application 启动后用容器 inspect 获取 `NetworkSettings.Ports`，按 container/service/target/protocol/
host IP 保存实际映射，并原子更新 `state.json`。若进程在启动后、落状态前中断，续跑根据隔离标签和
project 重新 inspect，恢复端口状态后再继续。dry-run 不连接目标机，只显示 `allocated_port: auto`。

## 5. 执行与幂等

隔离执行在现有固定顺序中增加内部 `isolation_prepare` 边界：

```text
files -> isolation_prepare -> image_digest -> volume
  -> database_prepare -> database_data -> application -> health
```

原位 Plan 不包含该阶段，原有顺序和 JSON 兼容性保持不变。隔离 marker 的 execution identity 包含
`IsolationSpec` 与持久化 runtime schema。每一步跳过前继续复验文件元数据、资源 label、数据库
readiness、容器 digest 与 health；端口状态缺失时必须重建，不因 application marker 存在而跳过。

隔离模式不运行 destination safety backup，因为它没有原资源覆盖授权。任何将要 stop/remove 原项目
资源的路径都视为实现错误并在测试中禁止。

## 6. Cleanup

CLI 增加：

```text
ark restore cleanup --host <destination> --isolation <id> [--json]
```

cleanup 加载当前清单、取得全局锁并连接目标 host，不需要 restic 仓库。它读取 root-only state，
先校验完整 isolation ID、状态 schema、路径必须位于 isolation base、project name，以及每个容器、
network、volume 的 `com.docker.compose.project` 和 `io.ark.restore.isolation` label。

删除顺序固定为 container -> network -> volume -> generated/files/markers -> isolation root。只删除状态中
记录且归属完全匹配的对象；发现未记录对象、标签漂移、软链接越界或路径越界时停止并报告失败。
资源已不存在视为幂等成功。状态目录最后删除，部分清理失败可用同一命令续跑。

普通隔离恢复成功后保留资源，并在 Result/人类输出中给出 isolation ID、实际端口和上述 cleanup
命令。verify 编排可调用同一 cleanup API 实现默认销毁。

## 7. CLI 与兼容性

- `ark restore ... --isolate --dry-run` 仍只调用 config、repo、manifest 三项只读依赖。
- dry-run 输出稳定 ID、派生 project/root、路径映射规则和端口 `auto`，不伪造实际端口。
- `--isolate --force` 在 PreRunE 阶段拒绝；`--skip-doctor` 不能跳过隔离转换自身的 IP、路径、资源
  归属检查。
- JSON 只输出结构化映射与脱敏错误，不输出 canonical Compose 内容、environment 或命令输出。
- 原位恢复 Plan 没有 isolation 字段时继续走现有逻辑，避免改变 P3-2 已验收行为。

## 8. 测试策略

- 纯函数测试覆盖 ID、项目名规范化、路径映射、资源名、端口对象和 unsupported matrix。
- fake Runner 覆盖 files 安全根、Compose config、host IP、标签、Docker 分配、inspect 恢复与续跑。
- cleanup 测试覆盖完整删除、重复删除、部分失败续跑、未记录对象、标签漂移和软链接/路径越界。
- CLI 测试覆盖参数互斥、dry-run 零副作用、普通/JSON 输出、cleanup 路由和脱敏。
- 可选 Docker 集成测试使用唯一 project，验证真实随机端口、TCP/UDP、container_name、显式 volume/
  network name 与原项目并存；通过 `testing.Short()` 和 Docker 可用性保护。

## 9. 风险与延后

- Canonical Compose 可能包含插值后的敏感值；不得记录原文，生成配置必须是 root-only，错误必须脱敏。
- 当前 config 只有单个 `compose_file`；多 `-f` 清单模型和自定义 isolation name 延后。
- 自动端口不会修改应用内部 URL、回调、反代或 environment 里的端口；需要此能力时另建显式配置任务。
