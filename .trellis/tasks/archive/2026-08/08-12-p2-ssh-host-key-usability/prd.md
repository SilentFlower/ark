# P2 SSH 主机密钥易用性

## Goal

降低首次接入和服务器重装后的 SSH 主机密钥维护成本，同时继续阻止未确认的主机密钥变更，避免备份数据被中间人窃取或恢复链被投毒。

## Background

- 当前清单强制填写 `known_hosts_file`，SSH Runner 固定使用 `StrictHostKeyChecking=yes`。
- 首次连接必须由用户提前执行 `ssh-keyscan`；服务器重装后只能手工处理旧记录，错误虽然安全但操作链过于生硬。
- 用户已确认采用“首次连接自动接受、变更密钥显式刷新、保留严格模式、不允许完全关闭校验”的方案。
- 清单仍使用 schema v2；新增可选字段必须保持现有清单可加载。

## Requirements

### R1 主机密钥策略

- `ssh` 配置新增 `host_key_policy`，只允许 `accept-new` 和 `strict`。
- 未填写时默认 `accept-new`；已有清单不需要增加字段即可继续加载。
- `accept-new` 只允许 OpenSSH 自动记录首次见到的主机密钥，已存在主机的密钥发生变化时仍必须拒绝连接。
- `strict` 保持当前 `StrictHostKeyChecking=yes` 行为。
- `known_hosts_file` 继续必填且必须是绝对路径；不提供 `no`、`off`、`insecure` 或其它跳过校验模式。

### R2 SSH Runner

- Runner 根据生效策略生成 `StrictHostKeyChecking=accept-new` 或 `StrictHostKeyChecking=yes`，其它隔离、BatchMode、身份文件和参数转义约束不变。
- SSH 失败必须保留 OpenSSH stderr；主机密钥冲突错误需给出运行 `ark host-key refresh --host <name>` 的操作指引。
- `Run`、`Stream`、`Feed` 三条路径必须使用同一策略来源，不能各自拼接参数。

### R3 显式刷新命令

- 新增 `ark host-key refresh --host <name>`，只接受清单中的单个远程 host；未知 host 或 `local: true` 必须失败。
- 默认只扫描并展示已记录与远端当前主机密钥的 SHA256 指纹，不修改文件。
- 只有显式增加 `--apply` 才替换该 host 对应记录；`--apply` 表示操作者已通过控制台等可信渠道核对指纹。
- 刷新使用系统 `ssh-keyscan` 和 `ssh-keygen` 的 argv 调用，不经过 shell，不读取身份私钥，也不发起账号认证。
- 写入必须在目标目录创建 `0600` 临时文件，同目录原子替换，拒绝符号链接和非普通文件；失败时保留原文件并清理临时产物。
- 命令输出不得包含密码、私钥、环境变量或完整凭证文件内容。

### R4 Doctor 行为

- `strict` 模式下，`known_hosts_file` 不存在仍为 fail。
- `accept-new` 模式下，文件存在时按普通文件检查；文件尚不存在但父目录存在且可写时为 warn，说明首次连接将创建记录。
- 父目录不存在、不是目录或不可写时为 fail，避免把错误推迟到无人值守备份。

### R5 兼容与文档

- 示例清单、设计文档和运维文档必须说明默认 `accept-new`、严格模式、刷新流程及 `ssh-keyscan` 本身不能证明远端身份。
- 更新 manifest、外部命令、日志和质量规范，使配置、CLI、doctor 与 Runner 契约一致。
- P2 真实环境验收在本任务完成后验证首次连接、变更拒绝和显式刷新三条链路。

## Non-Goals

- 不支持 SSH 密码登录，ark 仍使用身份文件和 `BatchMode=yes`。
- 不自动接受或静默替换已经变化的主机密钥。
- 不提供完全关闭主机密钥校验的配置。
- 不在本任务中修改真实服务器的 SSH 主机密钥或生产 known_hosts。
- 不改变备份数据流、restic、systemd 或对象存储行为。

## Acceptance Criteria

- [x] 未填写 `host_key_policy` 时使用 `accept-new`，显式 `strict` 时保持旧行为
- [x] 非法策略在 `ark validate` 阶段返回带 host 路径的中文错误
- [x] Runner 三种执行方式共享正确的 StrictHostKeyChecking 参数，且不存在 `no`
- [x] `ark host-key refresh --host <name>` 只预览旧、新 SHA256 指纹，不改文件
- [x] `--apply` 只替换目标 host 记录，原子写入、权限为 `0600`，失败可回滚且无临时残留
- [x] 变化密钥在普通 backup/doctor 路径仍被拒绝，并提供刷新指引
- [x] doctor 正确区分 strict 缺文件、accept-new 首次连接和不可写目录
- [x] 示例、设计、运维文档及 backend specs 同步更新
- [x] `make check`、`make build`、`CGO_ENABLED=0 go build ./cmd/ark` 和 `git diff --check` 通过
