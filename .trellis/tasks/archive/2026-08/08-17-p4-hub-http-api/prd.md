# P4-2 ark-hub HTTP API

## Goal

在 P4-1 已建立的本地管理员鉴权边界内，为后续 P4-3 Web 控制台和受信系统集成提供稳定的
JSON API：读取主机、备份运行和告警状态，并安全地发起备份、恢复演练与恢复操作。

`ark-hub` 只负责读取状态、校验请求和启动独立的 `ark` 子进程，不接管 systemd 调度，
也不在 HTTP handler 内重新实现 backup、verify 或 restore 业务逻辑。

## Background

- P4-1 已实现 `ark-hub` 服务生命周期、本地管理员密码、内存会话、CSRF、登录限流和默认鉴权边界。
- 主机和 target 定义来自 v2 清单；运行记录、target 结果和演练结果来自 `store.Store`。
- `store.Store` 目前只有单条 run 查询和写入 API，没有 hosts、runs、targets、doctor reports、
  verifications 的列表读取 API；P4 调用方禁止持有 `*sql.DB` 或自行拼 SQL。
- `doctor_reports` 已有 schema 和写入 API，但当前生产 CLI 路径尚未持久化报告与
  `next_run_at`，必须补齐后主机详情和超时判定才有真实数据。
- 用户已确认手动操作状态必须持久化到 `ark.db`；Hub 重启后仍处于运行中的记录必须
  转为明确的中断状态，不能丢失恢复操作的审计事实。
- `ark backup`、`ark verify`、`ark restore` 已支持 `--json`；恢复还支持 `--dry-run` 输出完整
  `restore.Plan`。这些命令是操作 API 的唯一业务执行入口。
- Hub 当前 HTTP `WriteTimeout` 为 30 秒，而备份、演练和恢复可能运行数分钟，因此操作接口
  必须异步返回，不能占用请求直到子进程结束。

## Requirements

### R1 查询 API

- `GET /api/hosts` 返回清单中的全部 host，以及最近一次备份状态、最近成功备份时间、
  最近演练状态、下次计划时间和当前健康状态。
- `GET /api/hosts/{host}` 返回单台 host 的非敏感配置摘要、target 列表、近期 target 结果、
  近期备份运行、最新 doctor 报告和近期演练结果；未知 host 返回稳定的 `404` JSON。
- `GET /api/runs` 返回按开始时间倒序排列的运行记录，支持有上限的分页，并允许按 host 和状态筛选。
- `GET /api/alerts` 返回当前计算出的告警，不在 P4-2 引入告警确认或静默状态。至少覆盖：
  最近成功备份超过有效计划周期两倍、连续两次备份失败、最近一次恢复演练失败。
- `GET /api/operations` 和 `GET /api/operations/{id}` 返回手动操作历史、当前状态和脱敏结果，
  支持在页面刷新或 Hub 重启后继续查询；列表同样使用有上限分页。
- API 时间统一使用 UTC RFC 3339，耗时使用非负毫秒，状态值复用 `store.Status`；空集合编码为
  `[]` 而不是 `null`，可空事实必须显式为 `null`，不能用虚构零值伪装。

### R2 状态与配置边界

- `internal/store` 新增稳定的查询 DTO 和公开读取方法，持有全部 SQL、分页和 NULL 还原逻辑；
  `internal/hub` 只能依赖这些公开 API。
- 状态库新增手动操作记录，至少保存操作 ID、类型、host、脱敏请求摘要、开始/结束时间、
  状态、关联 run/verification 标识、脱敏结果和错误；不得保存确认凭据、Cookie、凭证或完整环境。
- Hub 启动时必须把数据库中遗留的运行中手动操作原子标记为中断，并保留原开始时间与请求摘要。
- Hub 从经严格校验的 v2 清单读取 host、target 和生效 schedule，但不得向 API 暴露 repo URL、
  密码文件、env 文件、SSH 私钥、known_hosts 路径或其它凭证相关字段。
- 补齐 doctor 报告和下一次计划时间的生产写入链路；写入失败必须显式影响对应命令结果，
  不能让页面长期展示空白或过期健康数据而命令仍报告完全成功。
- `ark-hub serve` 必须显式持有清单路径和 `ark` 可执行文件路径；路径在监听前验证，
  不依赖 shell、工作目录或模糊 PATH 猜测。
- 健康与告警派生逻辑只能有一个后端所有者，hosts 与 alerts API 必须复用同一计算结果。

### R3 操作 API

- `POST /api/hosts/{host}/backup` 启动 `ark backup --host <host> --json`。
- `POST /api/hosts/{host}/verify` 启动 `ark verify --host <host> --json`，允许选择 manifest snapshot；
  Web API 不暴露 `--keep-on-failure` 或 `--skip-doctor` 等应急开关。
- `POST /api/hosts/{host}/restore` 同时承担预览与确认执行：首次请求异步调用只读恢复预检，
  操作完成后通过 operation API 返回完整恢复计划、目标冲突和确认凭据；只有后续请求显式提交
  匹配该预检的确认凭据时才允许执行。
- 恢复请求必须明确区分来源 host、目标 host、snapshot、隔离模式和覆盖模式；`force` 与 `isolate`
  的互斥规则保持与 CLI 一致，Web 层不得放宽。
- 所有长任务都通过无 shell 的参数数组启动独立 `ark` 子进程，固定使用 `--json`，限制捕获输出大小，
  记录可展示的脱敏结果；子进程继承的环境不得新增或扩散仓库凭证。
- 操作 API 必须提供按 ID 查询持久化状态与结果的能力，使 P4-3 可以在异步启动后轮询完成状态。
- 同一个 Hub 进程内不得并发启动互相争用全局 Ark 锁的手动操作；冲突返回稳定的 `409`，
  systemd 启动的外部命令仍由既有 `/run/ark.lock` 最终裁决。

### R4 恢复确认安全

- 恢复预览必须把来源、目标、snapshot、冲突策略、隔离资源、执行步骤和潜在覆盖范围完整返回。
- 预览必须通过 SSH 对目标资源做只读冲突探测；只生成静态 `restore.Plan` 不足以完成二次确认。
- 确认凭据必须绑定规范化请求和预览得到的计划摘要，具备短时有效期并只能成功消费一次；
  参数、snapshot 或计划发生变化后，旧确认不得继续执行。
- 真实恢复必须在首次写入前重新计算预检摘要并与确认摘要比较；目标资源在预览后发生变化时，
  即使请求参数未变也必须拒绝执行并要求重新预览。
- 非隔离恢复和 `force` 恢复必须在响应中明确标记破坏性；缺少确认、确认过期、确认不匹配或重复使用
  均返回 `409` 或 `422`，且不得启动子进程。
- 预览和执行都必须遵守现有会话鉴权；所有 POST API 额外校验 session CSRF，不能仅依赖 Cookie。

### R5 HTTP 与错误契约

- 除 `/healthz`、登录入口外，全部新 API 默认要求有效会话；运行期凭证故障继续 fail closed 为 `503`。
- JSON 请求使用严格解码、拒绝未知字段并限制 body；错误响应使用统一结构和稳定错误码，
  不回显命令 stderr、请求 Cookie、清单秘密、完整子进程环境或敏感文件内容。
- 子进程非零退出、context 取消、输出损坏、JSON 契约不匹配和进程启动失败都必须形成可查询的失败状态，
  不能返回“已成功执行”。
- P4-1 的 CSP、安全响应头、Cookie、限流、优雅停止和 service/timer 隔离契约保持不变。

## Acceptance Criteria

- [ ] hosts、runs、alerts 和 operations 查询在空库、单 host、多 host、NULL 字段和分页边界下返回稳定 JSON，且不泄露任何凭证路径或值。
- [ ] hosts 与 alerts 对超时、连续两次失败和演练失败给出一致判定；计划周期变化后结果同步变化。
- [ ] `Store` 查询覆盖 run、target、doctor report 和 verification，WAL 并发写入期间可读取，context 取消及时返回。
- [ ] backup/verify 中实际执行的 doctor 能产生主机详情所需的报告与 next-run 数据，不再只存在测试构造记录。
- [ ] backup、verify POST 返回异步操作标识，子进程 argv 与 CLI 契约完全一致，重复并发操作被拒绝。
- [ ] 手动操作从启动到完成的状态持久化；Hub 重启后遗留运行中记录变为中断，历史恢复记录仍可查询。
- [ ] restore 首次请求只生成计划；没有匹配、未过期且未消费的确认凭据时，真实恢复绝不启动。
- [ ] 修改 restore 参数、snapshot、计划或目标冲突集合后旧确认失效；重复确认不能启动第二个恢复进程。
- [ ] 未认证、CSRF 缺失、未知字段、非法 host、非法状态筛选、超大 body 和子进程故障均有测试。
- [ ] Hub 关闭时不会遗留状态不明的手动操作；停止 `ark-hub` 不影响 systemd 定时备份。
- [ ] `make check`、Hub/Store race 测试、常规与 `CGO_ENABLED=0` 构建、`go mod verify` 和
  `git diff --check` 全部通过。

## Out of Scope

- P4-3 Vue 前端、页面布局、前端状态管理和操作对话框。
- P4-4 `go:embed`、pnpm 构建与 `make hub`。
- P4-5 钉钉发送、告警静默期和外部死人开关心跳。
- API token、OIDC、多管理员、角色权限和公网 TLS 终止。
- 在 Hub 内实现定时调度，或绕过现有 `ark` CLI 直接调用 backup/verify/restore 内部业务包。
- 远程取消正在执行的操作、恢复资源 cleanup UI，以及 `--skip-doctor` 等应急操作入口。
