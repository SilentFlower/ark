# Brief — P2 SSH 主机密钥易用性

## Goal

- 降低首次接入和服务器重装后的 SSH 主机密钥维护成本，同时继续阻止未确认的密钥变更。

## Scope

- 在清单 SSH 配置中新增 `host_key_policy`，默认 `accept-new`，支持显式 `strict`。
- 让 SSH Runner、doctor 和配置校验统一消费同一生效策略。
- 新增 `ark host-key refresh --host <name> [--apply]`，默认预览旧、新 SHA256 指纹，显式应用时原子更新指定 known_hosts 记录。
- 同步示例、设计、运维、roadmap 和 backend specs，并补齐配置、Runner、CLI、doctor 与原子写入测试。

## Non-Goals

- 不支持 SSH 密码登录。
- 不自动接受已经变化的主机密钥。
- 不提供 `StrictHostKeyChecking=no` 或其它完全关闭校验的模式。
- 不修改真实服务器主机密钥、生产 known_hosts、备份数据流、restic、systemd 或对象存储行为。

## Key Decisions

- 保持清单 schema v2；新增字段可选，旧清单默认进入 `accept-new`。
- `known_hosts_file` 继续必填且使用绝对路径，作为可备份、可审计的信任库。
- 首次连接可自动写入；密钥变化仍阻止连接，只能经刷新命令预览后用 `--apply` 显式信任。
- 刷新命令不使用交互式 prompt，不经过 shell，并明确声明 `ssh-keyscan` 不能证明服务器身份。
- known_hosts 采用同目录 `0600` 临时文件和原子 rename，拒绝符号链接；rename 后目录 sync 失败时回滚原内容/权限或删除首次创建的文件。

## Key Context

- 配置契约：`internal/config/config.go`、`internal/config/config_test.go`。
- SSH 参数与流边界：`internal/sshexec/client.go`、`internal/sshexec/client_test.go`。
- 环境检查：`internal/doctor/doctor.go`。
- CLI 入口：`internal/cli/root.go`，刷新逻辑由新增 `internal/hostkey` 包持有。
- manifest、external-command、logging、quality 与目录规范已同步新的策略和刷新边界。

## Risks / Deferred

- 默认从 strict 变为 accept-new 是有意的行为变化，必须在文档和测试中明确。
- `ssh-keyscan` 结果需通过云控制台或服务器本地指纹核对；扫描成功不等于身份已验证。
- 本任务完成后才能继续 `p2-live-validation`，并在真实环境验证首次接受、变更拒绝和显式刷新。

## Acceptance

- 默认和 strict 策略均通过配置与完整 SSH argv 测试，非法策略在 validate 阶段失败。
- refresh 默认零写入，`--apply` 只替换选中 host，权限、原子性、回滚和临时清理均有测试。
- doctor 正确区分 strict 缺文件、accept-new 首次连接和不可写目录。
- 输出不泄露凭证或原始密钥内容，全仓不存在主机密钥校验关闭路径。
- `make check`、`make build`、无 CGO 构建及 `git diff --check` 全部通过。

## Next Step

- 处理 Check-All 发现并完成 full 重检，再进入规范更新与提交阶段。
