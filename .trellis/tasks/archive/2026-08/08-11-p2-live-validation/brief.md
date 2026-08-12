# Brief — P2 真实环境验收

## Goal

- 在一台已授权的真实服务器上，以完全隔离且可清理的资源验证 P2 备份链，证明 ark 能产生可追溯、可撤销并具备对象锁证据的真实加密快照。

## Scope

- 使用现有 SSH 管理端口和 root 密码编排环境；ark 被测链只使用临时 SSH 密钥和 `BatchMode=yes`。
- 在服务器创建 `ark-live-validation` 前缀的专用目录、Docker Compose 项目、PostgreSQL、Redis、MinIO、volume、files 数据和镜像服务。
- 创建仅监听 `127.0.0.1` 专用高端口的独立 sshd，用独立 host key 验证首次 `accept-new`、密钥变化拒绝、刷新预览与显式应用。
- 安装 restic，上传本次构建的 ark，运行 validate、doctor、install、systemd verify、手动 backup、service 和 timer 触发。
- 验证 PostgreSQL、Redis、volume、files、image_digest、manifest、状态库、稳定 tags/filename 和快照内容。
- 仅对测试资源执行 flock 冲突、kill `pg_dump`、低于历史 50% 数据流和对象锁删除拒绝测试。
- 恢复并清理可删除资源，把脱敏退出码、snapshot ID、run ID、状态查询和残留记录写入 `validation.md`。

## Non-Goals

- 不停止、重启、重建或注入故障到 dnsmgr、MySQL、代理、nginx、系统 sshd、既有容器或用户 unit。
- 不把 root 密码认证加入 ark 产品；管理密码仅用于本次环境编排。
- 不测试 P3 跨机恢复、P4 页面、P5 DNS/证书或修改 SSH 主机密钥产品策略。
- 发现代码缺陷时不在验收记录中临时修代码，而是回到所属代码任务重新修复和 Check-All。

## Key Decisions

- 用户已明确授权安装依赖、创建隔离容器/目录/sshd/systemd unit，以及仅针对这些资源的故障注入和清理。
- 主机密钥轮换使用独立高端口 sshd，不修改系统 sshd 的配置、host key 或端口。
- 对象存储使用同机隔离 MinIO bucket；保留期内对象不强制清理，记录预计释放时间和磁盘占用。
- ark 的 `/var/lib/ark/ark.db`、`/run/ark.lock` 和 `/etc/systemd/system/ark-*` 是固定产品路径；基线发现既有占用时立即 blocked，绝不覆盖。
- 全部秘密仅进入权限为 `0600` 的临时文件或受控 stdin，不写入任务文件、Git、命令行参数或验收输出。

## Key Context

- 候选服务器是 Debian 10，2 vCPU、约 1 GiB 内存、约 22 GiB 可用磁盘，systemd 与 Docker 可用，但运行着现有业务服务。
- 七个 P2 代码子任务已完成；SSH 策略现在默认 `accept-new`，支持显式 `strict` 和 `ark host-key refresh`。
- 备份编排固定使用 `/var/lib/ark/ark.db`、`/run/ark.lock` 与 systemd system unit；测试不能伪造可配置路径。
- 执行按 `trellis-run-full-chain` 场景表逐行记录 pass/fail，单元测试不能替代真实证据。

## Risks / Deferred

- 服务器内存较小，容器与测试必须串行启动、限制资源，并持续检查磁盘和内存；资源不足记为 blocked。
- MinIO object lock 可能留下暂时不可删除的小规模测试对象，不能缩短保留期或直接删除后端文件绕过验证。
- root 密码已经出现在历史对话中，验收结束后必须提醒立即轮换。
- 本任务只证明 P2 备份链，真正跨机恢复仍留给 P3。

## Acceptance

- 完整 backup 产生全部 target snapshot、manifest 和一致的状态库记录，tags、run ID、filename 与内容可相互追溯。
- 首次连接建立信任；独立 sshd host key 变化后 doctor/backup 拒绝并提示刷新；带外核对后显式应用恢复。
- `pg_dump` 被 kill 后 target 失败且仓库没有可用截断快照；低于历史 50% 的结果为 warn 且 bytes 可追溯。
- systemd unit/timer 可验证和触发，全局 flock 阻止第二轮并发执行。
- 导出的 ark 状态库可从仓库取回并通过 SQLite integrity check，密码和 SSH 私钥未进入快照。
- MinIO bucket 开启 object lock，保留期内测试对象删除被拒绝；清理结果和所有残留均有记录。
- 全程未修改任何既有业务服务、系统 sshd 或非 ark 管理的 systemd unit。

## Next Step

- 实机验收与缺陷复验已经完成；通过 Full Check-All 后进入规范更新、提交和归档，随后由父任务执行 P2 集成复核。
