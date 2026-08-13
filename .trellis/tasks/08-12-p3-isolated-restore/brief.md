# Brief — P3-2A 隔离恢复命名与端口映射

## Goal

- 为 `ark restore` 增加显式隔离恢复模式，在已有业务资源和端口仍被占用时自动创建可并行验收的
  恢复副本，并让 `ark verify` 复用同一隔离与清理能力。

## Scope

- 增加 `--isolate`，自动派生稳定 isolation ID、Compose project、容器、卷、网络和专用文件根目录。
- 结构化解析 `docker compose config --format json`，改写固定资源名、宿主机路径和 published ports；
  不使用 YAML 字符串替换。
- 保留原 Compose 的 host IP、协议和容器端口，由 Docker 原子分配空闲宿主机端口，启动后 inspect
  并持久化实际映射。
- 隔离副本复用现有 files、image digest、volume、database、application、health 和 marker 恢复链。
- 增加 `ark restore cleanup --host <destination> --isolation <id> [--json]`，按状态、标签和路径边界
  幂等清理隔离资源。
- 同步 restore/CLI 测试、后端规范、P3 父任务与 `p3-verify` 的复用边界。

## Non-Goals

- 不自动修改 DNS、TLS、防火墙、反向代理、生产域名或应用环境变量中的 URL、端口和回调地址。
- 不允许隔离模式覆盖原项目，也不与 `--force` 同时使用。
- 第一版不支持同一 snapshot/source/destination/project 同时创建多份副本或自定义副本名。
- 不扩展 `ark.yaml` 为 Compose 多 `-f` 文件列表，不实现 `ark verify` 的调度和结果持久化。

## Key Decisions

- 只有显式 `--isolate` 才进入隔离恢复；普通 restore 遇冲突仍 fail closed，不静默切换模式。
- 同一 manifest snapshot、source、destination 和原项目派生一个稳定 isolation ID；重复执行进入续跑。
- 自动端口保留原 Compose 的 host IP 暴露语义；具体 IP 在目标机不存在时直接失败，不回退地址。
- dry-run 保持零 SSH、零 Docker、零写入，只显示稳定派生信息和 `allocated_port: auto`。
- 普通隔离恢复成功后默认保留副本供人工验收；`ark verify` 才可编排默认自动销毁。
- 清理由 Ark 专用子命令负责，并在删除前验证 isolation state、Compose project label、隔离 label 和路径。

## Key Context

- 当前 restore Plan 是纯数据模型，真实执行位于 `internal/restore/execute.go`，CLI 位于
  `internal/cli/restore.go`。
- 当前恢复 marker 位于 `/var/lib/ark/restore`，隔离状态将放在其 `isolations/<id>` 子目录并以
  root-only 原子写方式持久化。
- 当前 `ark.yaml` 只声明一个 `compose_file`；Compose 文件自身的 include/extends 由 Compose
  canonical config 展开。
- 隔离 files 先恢复到专用根目录，完成 Compose 转换与 host IP 校验后才能创建 Docker 资源；
  原始绝对路径不得被写入。
- `p3-verify` 原有确定性端口偏移方案已改为依赖本任务的 Docker 原子分配、实际映射状态和 cleanup。

## Risks / Deferred

- Compose canonical JSON 可能含插值后的敏感值，禁止进入 CLI、JSON、错误和测试日志；生成文件必须
  保持 `0600`。
- external 资源、host namespace、未备份 bind/config/secret 路径等无法证明隔离的配置必须
  fail closed。
- 自动端口不会同步修改应用内部公开地址配置；此能力需要后续显式设计。
- Compose 多 `-f` 清单模型和自定义 isolation name 延后处理。

## Acceptance

- 已存在同名项目、固定容器名、显式卷/网络名和原端口时，隔离恢复能自动创建无冲突副本，原项目
  的容器、镜像、卷、网络、文件和端口保持不变。
- 失败续跑复用 isolation ID、名称、目录和实际端口，不创建重复副本。
- 短/长端口语法、TCP/UDP、范围、bind mount 和显式资源名均有结构化测试；不支持模式在 Docker
  创建前失败。
- 人类与 JSON 输出包含隔离资源、实际端口和精确 cleanup 命令，且不泄露敏感值。
- cleanup 只删除已证明属于 isolation ID 的资源，支持重复执行和部分失败续跑。
- `ark verify` 复用同一隔离接口；目标测试、`make check`、构建、无 CGO 构建和 `git diff --check`
  全部通过。

## Next Step

- 确认本 Brief 后启动任务，进入实现路由并先扩展可选 isolation Plan/Result 纯数据模型。
