# 技术设计：P4-4 告警与死人开关

## 1. 架构与依赖边界

```text
ark-hub
  -> config.LoadAndValidate
  -> hub.projectHosts                    现有唯一健康投影
  -> hub.alertManager                    1 分钟串行评估
       -> store.AlertState               schema v3 静默状态
       -> monitoring.DingTalk            Markdown 批量通知

ark backup
  -> 现有 backup 状态机与 Store 收尾
  -> monitoring.Heartbeat                成功/失败双端点
  -> 人类摘要 / --json heartbeat_status  不改 run 与退出码

config -> monitoring.env_file
monitoring -> envfile + net/http
hub -> config / schedule / store / monitoring
cli -> config / monitoring / 现有 backup 编排
```

- `internal/hub` 继续拥有告警判定与生命周期，不把健康逻辑复制到通知包。
- 新增 `internal/monitoring`，只负责秘密配置解析、URL 安全校验、钉钉发送和通用心跳请求；
  它不依赖 Hub、Store、backup 或 CLI。
- `internal/store` 只保存通用告警状态并提供 SQL API，不理解 schedule 或钉钉消息。
- 前端 `/api/alerts` 仍是实时投影，不增加告警历史、确认或手工静默 UI。

## 2. 清单与秘密文件

v2 清单新增向后兼容的可选字段，不升级 `SchemaVersion`：

```yaml
monitoring:
  env_file: /etc/ark/monitoring.env
```

`Config` 新增：

```go
type Monitoring struct {
    EnvFile string `yaml:"env_file"`
}

type Config struct {
    Version    int         `yaml:"version"`
    Repo       Repo        `yaml:"repo"`
    Defaults   Defaults    `yaml:"defaults"`
    Monitoring *Monitoring `yaml:"monitoring,omitempty"`
    Hosts      []Host      `yaml:"hosts"`
}
```

- `monitoring` 缺失表示全部通知关闭，保持现有部署零网络副作用。
- 写了 `monitoring` 时 `env_file` 必填且必须是绝对路径；`config.Validate` 不访问文件系统。
- 文件沿用 `internal/envfile` 的受限语法，允许且只允许以下键：

```text
ARK_DINGTALK_WEBHOOK_URL
ARK_DINGTALK_SECRET                可选；启用加签时填写
ARK_HEARTBEAT_SUCCESS_URL
ARK_HEARTBEAT_FAILURE_URL
```

- 心跳两个 URL 必须同时存在或同时缺失；允许相同。
- `ARK_DINGTALK_SECRET` 不能脱离 webhook URL 单独存在；未知键直接拒绝，避免拼写错误静默失效。
- URL 必须是 HTTPS；仅允许 loopback 地址使用 HTTP，便于本机代理与集成测试。拒绝 userinfo、fragment、空 host。
- URL、token、secret 不进入 YAML、API、状态库、manifest、错误或日志；错误只描述配置项名称和秘密文件路径。

## 3. `internal/monitoring` 出站协议

包内公开边界：

```go
type Settings struct {
    DingTalk  *DingTalkSettings
    Heartbeat *HeartbeatSettings
}

type HeartbeatStatus string

const (
    HeartbeatDisabled HeartbeatStatus = "disabled"
    HeartbeatSent     HeartbeatStatus = "sent"
    HeartbeatFailed   HeartbeatStatus = "failed"
)

func Load(path string) (Settings, error)
func SendDingTalk(ctx context.Context, settings DingTalkSettings, message MarkdownMessage) error
func SendHeartbeat(ctx context.Context, settings HeartbeatSettings, failed bool) error
```

- 实际命名可按代码上下文微调，但导出 API 必须完整中文 Javadoc。
- HTTP 使用标准库，固定请求超时、总重试上限和 64 KiB 响应上限；只接受 2xx。
- 钉钉使用 Markdown 消息并校验 HTTP 状态与响应业务错误；配置 secret 时严格按当前官方
  Webhook 安全设置生成时间戳与签名，测试注入固定 clock。
- 同一评估周期内所有到期的活动告警和恢复事件合并为一条 Markdown，减少群内突发消息。
- 心跳使用无 body 的 GET 请求，`failed=false` 选择成功端点，`failed=true` 选择失败端点。
- 所有错误必须从构造时起避免包含完整 URL、query、secret 或响应中可能回显的请求值。

## 4. 状态库 schema v3

新增 `internal/store/schema_v3.sql`，历史 v1/v2 文件保持不变：

```sql
CREATE TABLE alert_states (
    id TEXT PRIMARY KEY,
    host TEXT NOT NULL CHECK (length(trim(host)) > 0),
    kind TEXT NOT NULL CHECK (
        kind IN ('backup_overdue', 'backup_consecutive_failures', 'verification_failed')
    ),
    active INTEGER NOT NULL CHECK (active IN (0, 1)),
    first_seen_at INTEGER NOT NULL CHECK (first_seen_at >= 0),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at >= first_seen_at),
    last_alert_sent_at INTEGER,
    resolved_at INTEGER,
    recovery_sent_at INTEGER,
    CHECK (
        (active = 1 AND resolved_at IS NULL AND recovery_sent_at IS NULL) OR
        (active = 0 AND resolved_at IS NOT NULL)
    )
);

CREATE INDEX idx_alert_states_active ON alert_states(active, host, kind);
```

Go DTO：

```go
type AlertState struct {
    ID              string
    Host            string
    Kind            string
    Active          bool
    FirstSeenAt     time.Time
    LastSeenAt      time.Time
    LastAlertSentAt time.Time
    ResolvedAt      time.Time
    RecoverySentAt  time.Time
}

func (s *Store) ListAlertStates(ctx context.Context) ([]AlertState, error)
func (s *Store) SaveAlertState(ctx context.Context, state AlertState) error
```

- `SaveAlertState` 使用单条 upsert，严格校验 ID、kind、时间和 active/resolved 组合。
- 时间继续使用 UTC Unix 毫秒，查询与写入复用 `withBusyRetry`。
- 行数上限天然为“历史 host × 3”，保留已恢复行用于识别复发；本阶段不做清理任务。

## 5. Hub 告警状态机

`alertManager` 固定单 goroutine 串行运行，启动后立即评估一次，之后每分钟评估；测试注入 interval、
clock、sender 和 Store。单实例 Hub 是既有架构边界，不为多 Hub 主备引入分布式租约。

每轮流程：

1. 重新严格加载清单，并加载 `monitoring.env_file`；运行期配置损坏时报告脱敏错误并跳过本轮。
2. 调用现有 `projectHosts` 取得当前告警，绝不复制三个判定条件。
3. 读取全部 `AlertState`，按稳定 ID 对账：
   - 新告警或已恢复后复发：重置为新周期，立即到期；
   - 持续告警：`last_alert_sent_at` 为空或距现在至少 24 小时时到期；
   - 已发送告警消失：标记 resolved，恢复通知立即到期；
   - 清单删除 host：状态转为 inactive，但不发送“恢复”消息，避免把停止监控误报成健康恢复。
4. 先保存观察到的 active/resolved 状态；保存失败则不发送，避免通知与状态完全脱节。
5. 把本轮所有到期事件合并发送。发送成功后才更新 `last_alert_sent_at` 或 `recovery_sent_at`；
   发送失败不进入 24 小时静默，下一分钟重新尝试。

`alertManager` 自身 mutex 和单循环保证同进程并发评估不重复。网络发送成功后、状态提交前进程崩溃时，
重启后可能重复一条消息；选择 at-least-once 而不是“可能永久丢失”，该残余风险写入运维文档。

告警 manager 接入 `internal/hub/serve.go` 的生命周期：监听成功后启动；HTTP shutdown 后取消并等待
manager，再关闭 operation manager 和 Store。单轮失败不终止 HTTP 服务，只向 stderr 输出固定前缀的
脱敏错误，不引入日志框架。

## 6. Backup 心跳接入

心跳位于 `newBackupCmdWithDependencies` 的命令边界：

```text
load config + select hosts
  -> dry-run: 原样返回，零心跳
  -> runBackup（锁、doctor、快照、manifest、retention、Store 全部收尾）
  -> 根据最终 status/runErr 选成功或失败端点
  -> 写 summary.heartbeat_status
  -> 打印人类摘要或 JSON
  -> 按原有 runErr 返回，退出码不受心跳结果影响
```

- `ok` / `warn` 使用成功端点；`fail` 或已有运行错误使用失败端点。
- context 已取消时使用 `context.WithoutCancel` 加固定短超时，尽力报告失败终态，但不会无限阻塞退出。
- 清单加载失败时无法取得秘密文件路径，不发送；外部监控最终以缺失心跳报警。
- `--dry-run`、未配置监控和只做 `ark validate/doctor/restore/verify` 均零心跳。
- `backupRunSummary` 增加必有字段 `heartbeat_status`；Hub 的 backup operation JSON 白名单和校验同步。
- 心跳失败只得到 `HeartbeatFailed` 和一条脱敏警告，不加入 `backupFailureSet`，不修改 run、manifest、
  `errBackupFailed` 或现有退出码。

## 7. Doctor、安全与兼容性

- `doctor.RunLocal` 检查监控文件是当前用户所有的普通文件、权限不超过 `0600`、不是符号链接，
  并调用 `monitoring.Load` 校验键和 URL；失败记为 warn，不能因为辅助监控配置阻止真实备份。
- `ark-hub` 启动前若钉钉已配置但秘密文件非法则拒绝监听，避免界面看似健康但主动告警静默失效。
- backup 读取心跳配置失败时继续备份并报告 `heartbeat_status=failed`；死人开关会因缺失心跳最终报警。
- 旧清单、旧数据库和未配置部署保持兼容。v2 数据库升级 v3 只新增表，不改 runs、operations 或 API。
- 不修改 systemd timer 所有权；停止 Hub 后 backup timer 仍调用独立 `ark` 并发送心跳。

## 8. 验证与回滚

- migration 必须覆盖 v2→v3、空库直达 v3、并发迁移、失败回滚和高版本拒绝。
- alert manager 覆盖首次、24 小时边界、恢复、复发、发送失败重试、重启恢复状态、host 删除、
  schedule failure 和并发调用。
- monitoring HTTP 覆盖超时、重试、非 2xx、超大响应、钉钉业务错误、签名固定向量和秘密不泄漏。
- backup 覆盖 ok/warn/fail、取消、锁失败、配置失败、dry-run、disabled/sent/failed 与 Hub JSON 解析。
- 回滚代码时保留 schema v3 识别能力；已迁移数据库不能用不认识 v3 的旧二进制直接启动，
  上线单必须包含二进制回滚与数据库副本恢复步骤。
