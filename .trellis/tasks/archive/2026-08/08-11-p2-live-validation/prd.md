# P2 真实环境验收

## Goal

在隔离且可清理的真实 hub、目标机和对象存储环境中验证 P2 备份链，证明代码不仅单测通过，
而且能生成可检查、可撤销、受对象锁保护的真实加密快照。

## Confirmed Environment

- 已提供一台可通过 SSH 管理的候选测试服务器；访问凭证不写入任务文件、命令记录或 Git。
- 当前管理地址的 SSH 主机指纹已由用户明确确认可信；管理登录可使用现有 root 密码，
  但 ark 的被测 SSH 链仍保持 `BatchMode=yes`，只使用本次生成的临时密钥。
- 服务器为 Debian 10、2 vCPU、约 1 GiB 内存、约 22 GiB 可用磁盘，systemd 与 Docker 可用。
- 服务器当前运行 dnsmgr、MySQL、代理和 nginx 等现有服务；这些服务及其数据禁止用于故障注入。
- 当前未安装 restic、ark、PostgreSQL 客户端和 sqlite3；如使用该服务器，测试 target 必须使用隔离资源。
- `p2-ssh-host-key-usability` 已完成并发布到 `main`：默认策略为 `accept-new`，保留显式
  `strict`，密钥变化仍拒绝连接并通过 `ark host-key refresh` 显式处理。

## Requirements

### R1 环境与安全

- 使用专用测试 bucket/repository、非生产目标机或明确维护窗口，不触碰未授权生产数据。
- 测试前记录现有 snapshots、systemd units、bucket lock 和目标服务状态，测试后恢复。
- 凭证只通过现有 password/env/SSH 文件提供，不写入任务文档、命令行或日志。
- 全部测试资源使用 `ark-live-validation` 前缀和独立目录；不得停止、重启、重建或注入故障到
  dnsmgr、MySQL、代理、nginx、系统 sshd 及其它既有容器或 unit。
- 主机密钥变化测试使用仅监听 localhost 高端口的独立 sshd、独立 host key 和临时
  authorized key；不得修改系统 sshd 的 host key、配置或监听端口。
- 对象存储优先使用同机隔离 MinIO bucket 并启用 object lock；若最短保留期导致对象暂时
  无法清理，必须在验收记录中列出预计释放时间和磁盘占用。
- 用户已明确授权上述隔离资源的安装、创建、故障注入和验收后清理。

### R2 正常链路

- 默认 `accept-new` 在空信任库上完成首次连接并留下记录；随后模拟主机密钥变化，
  普通 doctor/backup 必须拒绝且给出刷新提示。管理员带外核对后用
  `ark host-key refresh --host <name> --apply` 恢复连接，不能使用关闭校验的参数。
- 安装 restic，执行 validate、doctor、ark install、systemd-analyze verify 和一次完整 backup。
- 核对每类 target 的非空 snapshot、host/target/run tags、稳定 filename、manifest 和状态库记录。
- 手动启动 service 与 timer 触发都能工作，flock 冲突不会并发执行第二轮。
- PostgreSQL、Redis、volume、files 和 image_digest 均来自专用 Docker Compose 项目；
  hub 状态库和清单使用专用 ark 路径，不能引用服务器现有业务目录。

### R3 破坏性故障注入

- 备份中在测试目标机 kill 远程 pg_dump，target 必须失败。
- 仓库中不得残留该截断 snapshot；若对象锁使物理删除延迟，逻辑快照必须不可用并记录原因。
- 制造低于历史 50% 的确定性数据流，结果必须 warn 且 snapshot/bytes 可追溯。

### R4 Hub 与对象锁

- hub 清单和 SQLite 一致性副本可从仓库取回，副本 integrity_check 通过。
- 在测试 bucket 开启对象锁/immutability，并验证保留期内删除失败。
- 核对 restic 密码与 SSH 私钥有独立离线副本，且未出现在 snapshots 中。

## Non-Goals

- 不由 auto-loop 执行本任务。
- 不测试 P3 跨机恢复、P4 页面或 P5 DNS/证书联动。
- 验收发现代码缺陷时创建/重开对应子任务，不在操作记录中临时修代码。
- 不在本验收任务中临时修改 SSH 主机密钥产品策略；如行为不符合已完成的易用性任务，
  回到对应代码任务修复并重新 Check-All。
- 不把管理登录的 root 密码改造成 ark 产品能力；密码只用于本次环境编排，ark 仍仅支持
  非交互式身份文件认证。

## Acceptance Criteria

- [x] 完整 backup 产生全部 target snapshot、manifest 和一致状态记录
- [x] 默认策略完成首次信任；密钥变化被拒绝并提示刷新；带外核对后显式刷新恢复连接
- [x] pg_dump 被 kill 后 target 失败且仓库无可用截断快照
- [x] 体积腰斩产生 warn，历史基线和当前 bytes 正确
- [x] systemd unit/timer 可验证、可触发，flock 阻止重入
- [x] hub 状态库副本一致，清单恢复材料齐全且密钥未进入备份
- [x] 对象锁保留期内删除测试失败，清理与恢复记录完整
