# P3-2A 隔离恢复命名与端口映射

## Goal

为 `ark restore` 增加可复用的隔离恢复能力：在已有 Compose 项目和宿主机端口仍被占用时，
自动派生独立资源名、恢复目录与端口映射，使恢复副本可与现有业务并行运行，并为
`ark verify` 复用同一套隔离转换与清理契约。

## Requirements

### R1 恢复模式边界

- 保留现有原位恢复语义：默认目标项目名、路径和端口来自恢复 Plan，冲突时继续 fail closed；
  `--force` 仍只表示对 Plan 精确声明的原项目执行破坏前备份与覆盖。
- 新增显式 `--isolate` 隔离恢复模式；普通恢复不得因检测到冲突而静默切换模式。进入该模式后，
  project、container、network、volume、files 路径和 published port 必须作为一个整体完成转换，
  不能只修改 `docker compose -p`。
- `--isolate` 可与 `--dry-run` 一起使用，但不能与 `--force` 一起使用；隔离模式永远不取得覆盖
  原项目资源的授权。
- 普通隔离恢复根据 manifest snapshot、source、destination 与原项目身份派生稳定 isolation ID。
  第一版同一组合只允许一份隔离副本；重复执行必须复用相同 ID、资源映射和 marker，不能因重试
  产生第二套资源。
- 隔离副本不得停止、删除、清空、重建或覆盖目标机上原有 Compose 项目及其文件、卷、网络和端口。

### R2 自动命名与路径隔离

- Compose project name 自动派生为 `<原项目名>-restore-<short-id>`，并满足 Compose 对项目名的
  小写字母、数字、短横线和下划线约束。
- 固定 `container_name`、显式 volume `name`、显式 network `name` 等可能绕过 project 隔离的资源，
  必须结构化移除或改写为带 isolation ID 的名称；不得用全局字符串替换 YAML。
- files target 全部恢复到 isolation 专用根目录；生成的 Compose 配置只引用映射后的路径，
  不得删除或写入原始绝对路径。
- Compose bind mount、config、secret、env file 等宿主机路径只有在能够无歧义映射到隔离恢复内容时
  才允许启动；无法证明隔离时只能清理 isolation 专用根目录内的临时材料，不得创建 Docker 资源
  或写入原始绝对路径。
- 所有创建资源都记录预期 Compose project 归属；覆盖、续跑和清理前重新 inspect 标签，名称匹配但
  标签不匹配时 fail closed。

### R3 自动端口映射

- 所有 Compose published TCP/UDP 端口自动改为目标机上的空闲宿主机端口，保留容器 target port、
  protocol 和应用协议元数据；同一容器端口的 TCP/UDP 映射分别处理。
- 自动映射保留原 Compose 的 host IP 绑定语义：原来绑定 `127.0.0.1` 时仍只允许本机访问，
  原来省略 host IP 或绑定 `0.0.0.0` 时仍对所有接口开放；隔离模式只改宿主机端口，
  不擅自收紧或扩大网络暴露范围。
- 原 Compose 指定的具体 host IP 在恢复目标机不存在时，不得自动回退到 `0.0.0.0`、回环地址或
  目标机其它地址。检查可在备份材料安全写入 isolation 专用根目录后执行，但必须早于任何 Docker
  资源创建和原始路径写入。
- 端口选择必须覆盖短语法、长语法、端口范围和显式 host IP；无法结构化保真的模式必须在启动前
  拒绝，不能把冲突留到 Compose 启动中途才发现。
- 隔离 Plan 和 CLI 人类/JSON 输出必须展示 `service/protocol/原宿主机端口/隔离宿主机端口/容器端口`
  的完整映射。只读 dry-run 将隔离宿主机端口标记为 `auto`；真实执行由 Docker 原子分配端口，
  启动后立即 inspect 并持久化实际映射，供续跑、健康检查和清理复用。
- 自动端口只处理 Docker published ports，不猜测或批量替换 `.env`、environment、命令参数中的
  URL 与端口。应用依赖自身公开地址时，由明确的后续配置能力处理。

### R4 Compose 转换与执行

- 使用 `docker compose config --format json` 获取合并、插值后的规范化模型，再结构化生成独立
  Compose 文件；不得依赖文本替换。当前 `ark.yaml` 仍只声明一个 `compose_file`，但该文件内部的
  include/extends 必须按 Compose 自身语义展开；多 `-f` 清单模型不在本任务扩展。
- 生成配置、恢复文件和 marker 放在 root-only isolation 根目录中，并纳入恢复结果与清理清单。
- image digest 覆盖、target 恢复顺序、流式 `restic dump -> Runner.Feed`、数据库 readiness、
  application/health 校验继续复用现有 restore Executor 契约，不实现第二套恢复引擎。
- `network_mode: host`、无法隔离的外部资源、越过 isolation 根目录的路径或其它会直接共享生产资源的
  Compose 配置必须 fail closed。
- `ark verify` 后续必须调用同一隔离转换能力，只允许在端口策略、生命周期和验证记录上增加编排，
  不得复制命名、路径或 Compose 改写逻辑。

### R5 清理与可审计性

- 恢复结果必须输出 isolation ID、生成配置路径、资源清单、端口映射、访问提示和精确清理命令。
- 清理只处理记录在隔离清单中且标签、名称、路径均匹配的资源；任何归属无法证明的对象均不删除，
  并显式报告清理失败。
- 提供 Ark 专用清理子命令，按目标 host 与 isolation ID 定位隔离清单；命令必须先校验 Compose 标签、
  资源名称和路径边界，再删除容器、网络、卷、恢复文件、生成配置与 marker。命令形态为
  `ark restore cleanup --host <destination> --isolation <id> [--json]`。
- 普通隔离恢复成功后默认保留副本供人工验收，不自动销毁。
- `ark verify` 可在其自身编排中使用自动销毁策略，但不得改变普通隔离恢复的资源归属校验。
- 人类与 JSON 错误只暴露阶段级脱敏信息，不输出 Compose 插值后的密钥、环境变量或仓库凭证。

## Non-Goals

- 不自动修改 DNS、TLS、防火墙、反向代理、生产域名或应用内部服务发现配置。
- 不猜测任意环境变量中的端口、URL、主机名和回调地址。
- 不把已有原项目转换成隔离项目，也不允许隔离模式借用 `--force` 覆盖归属不明资源。
- 不在第一版支持同一 snapshot/source/destination/project 同时创建多份隔离副本或自定义副本名。
- 不扩展 `ark.yaml` 为 Compose 多 `-f` 文件列表；原 Compose 文件自身的 include/extends 仍需支持。
- 不在本任务实现 `ark verify` 的调度、生产基线对比和 `store.Verification` 持久化。

## Acceptance Criteria

- [ ] 隔离模式能在目标机已存在同名 Compose project、固定容器名、显式卷名和原 published ports 时，
      自动生成无冲突资源并完成恢复
- [ ] 同一 Plan 失败续跑复用原 isolation ID、名称、目录和实际端口，不创建重复副本
- [ ] `--isolate --dry-run` 保持现有零 SSH/零 Docker/零写入契约，输出稳定 ID、派生资源和 `auto` 端口；
      `--isolate --force` 在参数阶段拒绝
- [ ] 原项目的容器 ID/state/digest、volume、network、文件元数据和 published ports 在恢复前后不变
- [ ] 生成配置来自结构化 Compose 模型，覆盖短/长端口语法、TCP/UDP、范围、bind mount、显式资源名
- [ ] 无法隔离的 host network、外部资源、未映射路径和标签冲突均在破坏性写入前失败
- [ ] CLI 人类与 JSON 输出包含完整资源及端口映射、精确清理命令，且不泄露插值后的敏感值
- [ ] 清理只删除已证明属于 isolation ID 的资源，标签或路径不匹配时 fail closed
- [ ] `ark verify` 可复用隔离转换接口，不存在第二套命名、端口和路径改写实现
- [ ] `go test ./internal/restore ./internal/cli -race -count=1`、`make check`、`make build`、
      `CGO_ENABLED=0 go build ./cmd/ark` 与 `git diff --check` 通过
