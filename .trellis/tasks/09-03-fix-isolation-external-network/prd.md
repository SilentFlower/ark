# 修复 external network 隔离恢复兼容性

## Goal

让使用 Docker Compose external network 的真实项目可以完成安全的隔离恢复演练，且演练容器不接入、修改或依赖生产共享网络。

同时保留一条可审计的脱敏失败原因，避免 `ark verify` 只报告“隔离恢复未完成”而无法定位隔离策略拒绝项。

## Background

- 2026-09-03 已在 hub 部署静态构建的 Ark `6154f63`，部署后 `validate`、本地 doctor 与 dnsmgr AuthApi 检查通过。
- 线上 `biz` 的最近两次定时 verify 均失败；手工执行最新版 verify 可稳定复现，生产基线前后指纹一致且隔离资源清理成功。
- 失败发生在 files target 恢复完成后、生成隔离 Compose 配置之前。保留隔离目录并复现 canonical Compose 后确认，项目声明了 external network `api_shared`。
- 当前 `internal/restore/isolation.go:1129` 的 Compose 转换在 `internal/restore/isolation.go:1290` 对所有 external network/volume fail closed；`.trellis/spec/backend/restore-plan-guidelines.md:334` 也明确要求 external 资源全部拒绝。
- `internal/restore/execute.go:376` 和 `internal/verify/verify.go:228` 分别把失败折叠为通用 restore/verify 摘要，导致结构化结果无法定位 Compose 隔离策略拒绝项。
- 生产 Compose 还包含普通 bridge network 与三个普通 named volume；本次没有发现 external volume、host namespace、privileged、越界 bind path 或不支持的 network driver。
- 首次修复部署后的真实 verify 已越过 external network 转换，但 Compose v5 canonical 为三个普通 named volume 全部补出 `driver: local`，现有“任意非空 volume driver 均拒绝”规则再次在 Docker 创建前阻断。生产基线前后一致，cleanup 成功，现网已回滚到部署前二进制。

## Requirements

- 隔离恢复不得把演练容器连接到原 external network，不得修改、删除或重新标记生产 network。
- 所有 external network 统一转换为 Ark 派生的独立 bridge network；不提供复用生产共享网络的开关或 allowlist。
- external volume、external config、external secret 继续 fail closed；本任务只处理 external network，不放宽其它共享宿主资源边界。
- external network 的处理必须保持 Compose service 的网络引用、别名和依赖拓扑可解析，并派生独立名称及 isolation label，使 cleanup 仍能精确证明归属。
- external network 只把原声明视为逻辑拓扑入口，不继承生产 network 的运行时身份或宿主网络参数；若业务健康依赖原共享网络中的其它服务，演练应诚实失败，不得回退连接生产网络。
- 接受 Docker Compose canonical 自动补出的空默认字段（当前 Compose v5 会为 external network 生成空 `ipam`）；任何非空额外运行时参数仍 fail closed。
- 普通 named volume 接受缺省 driver 或 Docker Compose canonical 默认的 `driver: local`，但必须继续派生独立名称并添加 isolation label；任何非 `local` driver 或非空 `driver_opts` 继续 fail closed。
- 普通 isolation 的自动端口与 verify 的禁用端口语义保持不变；生产 project、容器、network、volume、files 基线前后必须一致。
- 备份 manifest 的 ComposeMetadata 兼容不变，不新增必须迁移的 schema 字段。
- 隔离转换失败时，restore/verify 结果必须记录可安全展示的阶段原因；不得输出 canonical Compose、environment、凭证、完整外部命令输出或业务数据。
- 更新 `.trellis/spec/backend/restore-plan-guidelines.md` 与 `.trellis/spec/backend/verify-guidelines.md`，使规范与新的 external network 契约一致。
- 增加纯函数、restore、verify 与 CLI 回归测试，并在部署后重新执行 `ark verify --host biz --snapshot latest`。

## Acceptance Criteria

- [ ] 含 external network 的 Compose canonical 输入可转换为带派生名称和 isolation label 的独立非 external bridge network。
- [ ] 演练 Compose 不再引用原 external network 名称，且任何 external volume/config/secret 仍在创建 Docker 资源前失败。
- [ ] service 网络引用及 alias 等非共享语义保持，转换后的 Compose 可通过 `docker compose config --format json`。
- [ ] external network 含无法安全迁移的额外运行时参数时，在创建 Docker 资源前给出脱敏策略原因并 fail closed。
- [ ] Compose canonical 的空默认字段不阻断转换，也不会被带入生成的私有 network。
- [ ] 普通 named volume 的缺省/`local` driver 可完成派生与清理，非 `local` driver、`driver_opts` 和 external volume 仍在 Docker 创建前拒绝。
- [ ] cleanup 只删除带匹配 isolation label 的派生资源；原 external network 与生产基线不发生变化。
- [ ] 不支持的隔离资源失败时，结构化结果包含脱敏的阶段原因，而不是只有“恢复未完成”。
- [ ] `go test ./internal/restore ./internal/verify ./internal/cli -race -count=1`、重复回归、`make check`、静态构建与 `git diff --check` 全部通过。
- [ ] 线上 `biz` verify 使用最新版 manifest 完整通过，结束后无 isolation 容器、network、volume 或目录残留。

## Out of Scope

- 让演练容器加入生产 external network。
- 为 external network 增加用户 allowlist 或新的清单字段。
- 放宽 external volume、config、secret、非 bridge driver、driver_opts、host namespace 或其它现有隔离红线。
- 修改 P5-3 dmonitor 维护窗口逻辑、创建生产 dmonitor 任务或执行原位 `--force` 恢复。
