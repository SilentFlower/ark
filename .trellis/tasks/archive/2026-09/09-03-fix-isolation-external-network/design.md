# external network 隔离恢复技术设计

## 1. 边界与已确认决策

本任务只修复 Docker Compose external network 对隔离恢复和 `ark verify` 的阻断，并补充可安全展示的失败阶段摘要。

已确认的策略是：所有 external network 都转换为 Ark 派生的独立 `bridge` network，隔离容器永远不连接原生产共享网络。若应用健康依赖只存在于原共享网络中的其它服务，演练允许在 health 或启动阶段诚实失败，不以降低隔离边界换取通过。

以下边界不变：

- external volume、config、secret 继续在 Docker 创建前 fail closed。
- network 非 `bridge` driver、任何 `driver_opts`、host/container/service namespace、privileged、越界 bind path 等现有红线不放宽。
- verify 继续删除全部 service `ports`；普通 `restore --isolate` 继续使用 Docker 运行时自动端口。
- 不修改备份 manifest schema、ComposeMetadata、命名算法、isolation state schema 或 cleanup 命令接口。
- P5-3 dmonitor 真实任务验收不属于本任务；线上当前没有可供验收的 dmonitor task，不在本任务中创建生产任务。

## 2. 当前故障链

线上 `biz` 的 Compose canonical 配置包含 external network `api_shared`。`internal/restore/isolation.go` 的 `transformIsolationCompose` 先处理 service，再由 `transformNamedResources` 处理 `volumes` 和 `networks`；当前实现对两类资源统一调用 `externalResource` 并直接拒绝，导致失败发生在生成隔离 Compose 文件和创建 Docker 资源之前。

`internal/restore/execute.go` 的 `failResult` 当前把所有错误统一折叠成“恢复未完成”，`internal/verify/verify.go` 又折叠成“隔离恢复未完成”。底层错误链没有直接输出，因此没有泄露，但也无法从 verify 结果识别是 Compose 安全策略拒绝。

清理链已经具备所需归属契约：转换阶段返回派生 network 名称，`state.json` 保存到 `Networks`，`CleanupIsolation` 同时校验 Compose project label、isolation label、状态清单和名称后才允许删除。因此 external network 只要被转换为现有命名与标签体系下的新 network，无需修改 cleanup schema。

## 3. external network 转换

### 3.1 分离 volume 与 network 策略

保留 `transformNamedResources` 的现有入口和排序行为，但在资源类型内区分 external 处理：

- `volumes`：external 仍返回隔离策略错误；普通 named volume 允许缺省 driver 或 canonical 默认的 `local`，其它 driver 与任何非空 `driver_opts` 继续拒绝。
- `networks`：普通 network 沿用现有无 driver 或 `bridge`、无 `driver_opts` 的校验和改名逻辑；external network 进入专用转换分支。

这样不会把“network 可安全复制为私有 bridge”的结论错误扩散到 volume 等持久化共享资源。

Docker Compose v5 会把未显式声明 driver 的普通 named volume canonical 化为 `driver: local`。`local` 是当前缺省 named-volume driver，转换仍会覆盖物理名称、添加 isolation label，并由 state/cleanup 只管理派生 volume，因此允许该标准默认值不扩大到共享 volume。非 `local` driver 可能依赖宿主插件或外部存储，`driver_opts` 可能挂载宿主路径或远端资源，二者继续 fail closed。

### 3.2 external network 的输入与输出

external network 只允许携带外部身份声明，即 canonical 形式中的 `name` 与 `external`。现有 `externalResource` 支持布尔和对象两种 external 表示；对象形式只接受其中的名称信息。Docker Compose v5 会为最小 external network 自动补出空 `ipam`，因此转换允许并丢弃值为空的 canonical 默认字段。若 external network 同时携带非空 driver、driver_opts、IPAM、internal、attachable 或其它运行时参数，转换在 Docker 创建前 fail closed，避免静默继承或丢弃无法证明等价的宿主网络语义。

转换结果重新构造为最小独立 network：

```json
{
  "name": "<Ark 派生名称>",
  "driver": "bridge",
  "labels": {
    "io.ark.restore.isolation": "<完整 isolation ID>"
  }
}
```

派生名称继续复用 `isolationResourceName(original, purpose, shortID)`。原名称优先取 canonical 顶层 `name`，对象形式可取 `external.name`，均不存在时回退到现有 `effectiveProjectResourceName`，保证同一 Plan 重试时名称稳定。

转换必须删除 `external` 并覆盖原 `name`，不能把原外部名称留在生成文件的任何 network 资源字段中。

### 3.3 service 引用与 alias

Compose service 通过顶层 network 的逻辑 key 建立引用。转换只替换 `document.networks[key]` 对应的物理资源声明，不改 service 的 `networks` map/list，因此以下非共享语义原样保留：

- service 连接哪些逻辑 network；
- service 级 alias；
- priority、gw_priority、link_local_ips 等 service attachment 字段；
- service 之间通过同一逻辑 key 形成的网络拓扑。

静态地址或健康检查若依赖原 external network 的 IPAM/其它成员，可能在生成配置校验、Compose 启动或 health 阶段失败。这是可接受的真实演练结论，不能因此连接生产 network。

## 4. 安全失败摘要

`restore.Result.Error` 和 `verify.Result.Error` 已明确是脱敏结果字段，不新增 DTO 字段。新增仅包内使用的安全摘要错误契约，例如由错误类型提供固定的 `ResultSummary()`；摘要必须由代码选择，不能从任意底层 `err.Error()` 自动复制。

错误传播规则：

1. Compose 隔离策略拒绝点返回带安全摘要的内部错误，摘要只包含阶段、资源类别和固定原因，例如“隔离 Compose network 包含不支持的运行时参数”。资源 key 可作为已允许的安全资源摘要，但不包含完整 Compose、environment、凭证或命令输出。
2. `failResult` 使用 `errors.As` 查找安全摘要；找到时写入 `restore.Result.Error`，否则继续使用“恢复未完成”。
3. `isolationComposeCommandError` 和 SSH/Docker/restic 等外部命令错误不实现安全摘要接口，底层 stdout/stderr 仍只保留在返回 error 链中，不进入结构化结果或 CLI 输出。
4. verify 在 restore 失败时，若 `restore.Result.Error` 比通用文案更具体，则生成“隔离恢复未完成：<安全摘要>”；否则保持“隔离恢复未完成”。后续 cleanup、baseline、store 失败继续沿用现有追加与覆盖规则。

该设计保持 CLI 现有安全边界：人类输出与 JSON 只读取 Result 字段，Cobra 继续静默底层 error 文本。

## 5. 失败矩阵

| 条件 | 必须行为 |
|---|---|
| external network 只有 `name/external` | 转换为派生私有 bridge，保留 service 引用与 alias |
| external network 使用对象形式名称 | 稳定提取来源名称并转换为派生私有 bridge |
| external network 含 Compose 自动补出的空默认字段 | 忽略空默认值并转换，不把字段带入派生 network |
| external network 含额外运行时参数 | Docker 创建前 fail closed，Result 写安全策略摘要 |
| external volume/config/secret | 继续 Docker 创建前 fail closed，不改变原资源 |
| 普通 named volume 无 driver 或 driver=local | 派生独立名称并添加 isolation label |
| 普通 named volume 使用非 local driver 或 driver_opts | Docker 创建前 fail closed，Result 写安全策略摘要 |
| 普通 network 无 driver 或 driver=bridge | 沿用现有转换与标签逻辑 |
| 普通 network 非 bridge 或带 driver_opts | 继续 fail closed |
| Compose canonical 命令失败且 stderr 含敏感值 | Result 保持通用摘要，CLI/JSON 不出现敏感值 |
| 新私有 network 启动后 health 失败 | 按现有 verify 失败路径清理或显式保留，不连接原 external network |
| cleanup 发现 network 标签或名称不匹配 | 拒绝删除并保留 state/root 供同 ID 重试 |

## 6. 测试设计

### 6.1 纯函数与恢复结果

- `internal/restore/isolation_test.go`：external network 转换成功，断言派生名称、显式 bridge、isolation label、无 external/原物理名称、service alias 不变。
- 覆盖布尔/object external 表示、无显式名称回退、额外运行时参数拒绝，以及 external volume/config/secret 继续拒绝。
- 覆盖普通 named volume 缺省/`local` driver 成功，非 `local` driver 与 `driver_opts` 继续拒绝。
- `internal/restore/execute_test.go`：带安全摘要的策略错误进入 `Result.Error`；普通底层错误仍只显示“恢复未完成”；嵌套敏感 stderr 不进入结果。

### 6.2 verify 与 CLI

- `internal/verify/verify_test.go`：restore 安全摘要进入 verify 顶层阶段文案，cleanup/store 错误仍按现有规则合并；通用 restore 失败文案不重复。
- `internal/cli/restore_test.go` 与 `internal/cli/verify_test.go`：人类输出和 JSON 能看到安全阶段原因，但看不到注入的敏感 stderr、canonical Compose 或凭证。

### 6.3 Docker 集成

扩展 `internal/restore/isolation_docker_test.go` 的真实 Docker 用例，先创建一个代表生产共享资源的 external bridge，再让生产 Compose 通过 `external: true` 引用它。隔离转换、启动和 cleanup 后必须同时证明：

- 隔离容器连接的是派生 network，不是原 external network；
- 原 external network ID、driver、labels 和已连接生产容器不变；
- verify 容器没有 published ports；
- 派生容器、network、volume 和 isolation root 全部清理。

集成测试继续受 `testing.Short()`、`ARK_DOCKER_INTEGRATION=1` 和本地镜像检测保护，不自动拉取镜像。

## 7. 规范与兼容性

更新 `.trellis/spec/backend/restore-plan-guidelines.md`：把“所有 external 资源拒绝”收窄为“external 持久化/文件资源拒绝，external network 按严格输入转换为 Ark 私有 bridge”，并在错误矩阵中记录额外参数的 fail-closed 行为。

同时把普通 named volume 契约从“任意显式 driver 均拒绝”调整为“缺省或 `local` 可派生，非 `local` 与 `driver_opts` 拒绝”，记录这是 Compose v5 canonical 默认兼容，不是 external volume 放行。

更新 `.trellis/spec/backend/verify-guidelines.md`：要求 verify 继承相同 external network 转换、不得连接生产共享网络，并允许传播由 restore 提供的安全阶段摘要。

该变更不修改命令行参数、YAML 配置、公开 Go API、JSON 字段或持久化 schema。旧 manifest 可直接使用；如果应用确实依赖共享网络成员，结果从“转换前失败”变为“隔离环境启动/健康失败”，但生产隔离边界不变。

## 8. 部署验证与回滚

本地质量门通过后使用 `CGO_ENABLED=0` 构建静态二进制，确认 `file`/`ldd` 不依赖远端较新 glibc。部署前在 hub 为当前 `/usr/local/bin/ark` 创建新的带时间戳备份并记录 SHA-256，再原子替换。

部署后顺序执行：

1. `ark validate`、本地 doctor 与 dnsmgr AuthApi 检查。
2. 记录 `biz` 生产 Compose project、容器、network、volume 和 files 基线。
3. 执行 `ark verify --host biz --snapshot latest`；确认完整通过或得到新的、安全且可操作的失败阶段。
4. 核对演练容器没有连接 `api_shared`，原 external network 与生产基线前后一致。
5. 核对无 isolation 容器、network、volume、root 目录残留，verify service/timer 状态正常。

首次部署验证已经证明 external network 转换生效，同时暴露 canonical 默认 `driver: local` 兼容缺口；该次部署已回滚到 SHA-256 为 `8e076738...` 的旧二进制，失败 isolation 已幂等清理，timer 保持 active。

后续若部署验证出现回归，停止继续演练，恢复本次部署前备份的二进制并重新执行 validate/doctor。已有 `/usr/local/bin/ark.backup-42a8936-20260903` 可作为更早版本兜底，但优先使用本次部署即时生成的备份，避免跨版本回退范围扩大。
