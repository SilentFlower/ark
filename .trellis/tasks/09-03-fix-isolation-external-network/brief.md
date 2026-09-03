# Brief — 修复 external network 隔离恢复兼容性

## Goal

- 让使用 Docker Compose external network 的真实项目完成安全隔离恢复演练，同时提供可审计、脱敏的失败阶段摘要。

## Scope

- 将 external network 转换为 Ark 派生的独立 `bridge` network，复用现有稳定命名、isolation label、state 和 cleanup 归属链。
- 兼容 Docker Compose v5 为普通 named volume 自动补出的 `driver: local`，同时保留非 local driver、driver_opts 与 external volume 的 fail-closed 边界。
- 保持 service 的逻辑网络引用、alias 与依赖拓扑，不保留原 external network 的物理身份或宿主网络参数。
- 为 Compose 隔离策略错误增加受控安全摘要，并从 restore 结果传播到 verify 与 CLI/JSON。
- 增加纯函数、restore、verify、CLI 和真实 Docker 回归测试，更新 restore/verify Trellis 规范。
- 静态构建并部署到 hub，在 `biz` 使用 latest manifest 完成真机 verify、生产基线和残留资源验收。

## Non-Goals

- 不让隔离容器加入、修改、删除或重新标记生产 external network。
- 不增加 external network allowlist、配置项、命令参数或 manifest/schema 字段。
- 不放宽 external volume/config/secret、network 非 bridge driver、任何 driver_opts、host namespace、privileged 或越界路径等隔离红线。
- 不修改 P5-3 dmonitor 维护窗口逻辑，不创建生产 dmonitor 任务，不执行原位 `--force` 恢复。

## Key Decisions

- 所有可接受形态的 external network 统一私有化为派生 bridge；不提供回退连接生产共享网络的路径。
- external network 接受 `name/external` 身份声明和 Compose canonical 自动补出的空默认字段；出现非空额外运行时参数时在 Docker 创建前 fail closed。
- 普通 named volume 只允许缺省 driver 或 canonical 默认的 `local`；非 `local` driver 与任何 `driver_opts` 继续拒绝。派生名称、isolation label、state 和 cleanup 归属验证不变。
- service attachment 保持原样，顶层 network 实体重新构造；若应用依赖共享网络中的其它服务，演练应诚实启动或健康失败。
- 复用 `restore.Result.Error` 和 `verify.Result.Error`，不新增 DTO 字段；只有实现受控摘要接口的内部错误可进入结果，任意底层 stderr 继续隐藏。
- cleanup 不增加特例，仍只删除 state 中记录且名称、Compose project label 与 isolation label 全部匹配的派生资源。

## Key Context

- 原始根因入口是 `transformIsolationCompose` 与 `transformNamedResources`：任务开始时 external network 在生成隔离 Compose 和创建 Docker 资源前被拒绝；当前已按严格输入契约转换为派生 bridge。
- 任务开始时 restore/verify 只输出通用失败摘要；当前仅由 restore 包内受控摘要补充安全阶段原因，任意底层错误文本仍不会进入结果。
- restore/verify 规范已同步为 external network 私有化、普通 named volume 缺省/`local` driver 可派生，其它共享资源和高风险参数继续 fail closed。
- 线上 hub 已运行静态 Ark `6154f63`；`biz` 的生产 Compose 使用 external network `api_shared`，此前复现确认生产基线未变化且失败清理成功。
- 首次修复部署已越过 external network，但真实 Compose v5 canonical 为三个普通 named volume 补出 `driver: local`；该次 verify 的前后基线一致、cleanup 成功，hub 已回滚到部署前 SHA-256 `8e076738...` 的二进制。
- 当前 manifest、isolation state、cleanup schema 和 CLI 接口足以承载变更，不需要迁移。

## Risks / Deferred

- 新私有 bridge 不包含原共享网络中的其它成员或 IPAM；依赖这些条件的应用可能继续在启动/health 阶段失败，但不得降低隔离边界。
- 只允许默认 `local` volume driver；依赖宿主插件、远端存储或 driver_opts 的 volume 仍无法自动隔离。
- 真实 Docker 集成测试依赖本机 Docker 与已有镜像；不可用时必须保留跳过证据，并由 `biz` 真机验收补足。
- 线上部署必须继续使用 `CGO_ENABLED=0` 静态构建，避免再次引入目标机 glibc 不兼容。
- P5-3 dmonitor 真实暂停/恢复验收继续延后，原因是当前 dnsmgr 没有可控 dmonitor task。

## Acceptance

- external network canonical 输入生成带派生名称、显式 bridge 和 isolation label 的非 external network，且生成内容不再引用原物理 network 名称。
- service 网络引用与 alias 保持，生成 Compose 可通过 `docker compose config --format json`。
- external network 额外运行时参数以及 external volume/config/secret 均在 Docker 创建前以脱敏原因 fail closed。
- 普通 named volume 的缺省/`local` driver 能完成派生、启动与清理；非 `local` driver 和任何 `driver_opts` 继续在 Docker 创建前拒绝。
- 普通 isolate 自动端口、verify 禁用端口、cleanup 归属校验、manifest/schema 兼容性均无回归。
- 安全策略摘要能出现在 restore/verify 结构化结果和 CLI/JSON 中；canonical Compose、environment、凭证与敏感 stderr 不得出现。
- 定向 race、十次重复回归、`make check`、`go mod verify`、`git diff --check` 和静态构建全部通过。
- hub 部署后 `biz` latest verify 完整通过；原 `api_shared`、生产 project/container/network/volume/files 基线不变，结束后无 isolation 资源或目录残留。

## Next Step

- 本地 Check-All 通过后重新部署静态二进制到 hub，执行 `biz` latest verify，并核对生产基线、隔离资源清理和 timer 状态。
