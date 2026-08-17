# Brief — P4-2 ark-hub HTTP API

## Goal

- 在 P4-1 的本地管理员鉴权边界内提供稳定 JSON API，使后续 Web 控制台能够查看备份健康状态，并安全发起备份、恢复演练和恢复操作。

## Scope

- 实现 hosts、host detail、runs、alerts、operations 查询 API，使用有上限的 keyset 分页和稳定 JSON 契约。
- 新增状态库 schema v2，持久化手动 backup、verify、restore preview 和 restore 操作；Hub 重启时把遗留 running 记录标记为 interrupted。
- 为 Store 增加 runs、host runs/targets、doctor、verification 和 operations 的公开查询 API，Hub 不接触 SQL。
- 新增统一 schedule 探测边界，通过 `systemd-analyze calendar --iterations=2` 得到下次运行时间和有效周期；补齐 backup/verify 的 doctor 报告持久化。
- 实现唯一的 host 健康与告警派生逻辑，覆盖备份超时、连续两次失败和最近演练失败。
- 所有手动长任务通过独立 `ark --json` 子进程异步执行，POST 返回 `202` 和 operation ID，operation API 提供持久化状态与脱敏结果。
- 扩展恢复 CLI 的只读目标冲突预检和 preview digest；HTTP 恢复先异步预览，再使用同 session、短时、单次确认 token 执行。
- 真实恢复绑定精确 manifest snapshot，并在首次预检及 safety backup 后复核 preview digest；目标状态变化时在首次恢复写入前拒绝执行。
- 扩展 `ark-hub serve/install` 与 systemd service，显式配置清单和 Ark 二进制绝对路径。
- 同步 roadmap、设计、运维说明、README 和相关后端规范。

## Non-Goals

- 不实现 P4-3 Vue 前端、P4-4 `go:embed`/pnpm 打包或 P4-5 钉钉告警与静默期。
- 不在 Hub 内实现定时调度，也不绕过 `ark` CLI 直接执行 backup、verify 或 restore 业务包。
- 不增加 API token、OIDC、多管理员、角色权限或公网 TLS 终止。
- 不提供远程取消、restore cleanup UI、`--skip-doctor` 或 `--keep-on-failure` 等应急 Web 入口。

## Key Decisions

- 用户确认手动操作状态持久化到 `ark.db`；恢复操作保留审计记录，Hub 重启后的未完成操作明确标记为 interrupted。
- 备份、演练、恢复预览和恢复执行全部采用异步 operation 模型，避免受 Hub 30 秒 HTTP 写超时影响。
- `ark-hub` 只校验请求和启动无 shell argv 的 Ark 子进程；现有 systemd timer 与 oneshot 调度保持独立。
- 恢复确认不只绑定静态 Plan，还绑定 SSH 只读预检得到的目标冲突集合；执行参数、snapshot、清单或目标状态变化都会使旧确认失效。
- 确认 token 只保存在内存并绑定 session，数据库只保存 preview/result 白名单数据；Hub 重启或 token 首次展示丢失后必须重新预览。
- 任意 OnCalendar 周期只委托 systemd 计算，不实现近似 cron parser；无法分析时健康状态为 unknown，不伪造超时结论。

## Key Context

- P4-1 已提供默认鉴权、session CSRF、限流、安全响应头、优雅停止和独立 `ark-hub.service`。
- 当前 Store schema v1 有 runs、run_targets、doctor_reports、verifications，但缺少列表读取和手动操作表；doctor 报告目前仅有写入 API，没有生产调用链。
- `ark backup`、`verify`、`restore` 已有纯 JSON 输出；现有 restore dry-run 只构建静态 Plan，真实冲突当前在 Execute 内部才发现。
- 状态库必须继续保持 WAL、短 busy retry、context 优先取消、顺序迁移和无 CGO 构建能力。
- API 不直接序列化 `config.Host`，避免泄露 repo、env、SSH identity、known_hosts 和其它敏感路径。
- 详细技术契约见 `design.md`，代码证据与 schedule 实测见 `research/code-evidence.md`。

## Risks / Deferred

- schema v2 属于向前迁移；旧二进制会按现有高版本保护拒绝打开，回滚程序前必须恢复 v1 数据库快照。
- force restore 的 safety backup 可能耗时较长；设计通过 safety backup 后再次预检降低确认与写入之间的状态漂移风险。
- 手动操作不提供远程取消；graceful shutdown 会取消并记录 interrupted，异常退出依赖 Pdeathsig 与下次启动恢复状态。
- 确认 token 不持久化是刻意的安全取舍，浏览器丢失首次 token 或 Hub 重启后需要重新预览。

## Acceptance

- 查询 API 在空库、多 host、NULL、分页和并发写入下返回稳定结果，且不泄露凭证或敏感配置路径。
- hosts 与 alerts 使用同一判定，对备份超时、连续失败、演练失败和 schedule unavailable 给出一致事实。
- schema v1 可安全迁移到 v2；操作从 running 到 ok/fail/interrupted 的全部路径可查询且无状态丢失。
- backup/verify doctor 产生真实报告和 next-run 数据；Store 查询在 WAL 写期间可用并及时响应 context 取消。
- backup、verify、restore preview、restore 的 argv、JSON、互斥、失败和 shutdown 路径均有 race 测试。
- 没有匹配、未过期、未消费且同 session 的确认时，真实恢复绝不启动；preview digest 变化时零恢复写入。
- `ark-hub.service` 新参数通过真实 systemd verify，停止 Hub 不影响现有 backup/verify timer。
- `make check`、重点包 `-race -count=10`、常规/无 CGO 双二进制构建、`go mod verify` 和 `git diff --check` 全部通过。

## Next Step

- 用户确认本 Brief 后运行 `task.py start`，再通过 `trellis-route(target=implement)` 选择实现执行模式并从 schema v2 与 Store 查询开始。
