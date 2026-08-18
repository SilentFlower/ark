/**
 * 与 ark-hub HTTP API 的 Go DTO 一一对应的类型定义。
 *
 * 这些类型是跨语言契约的前端一侧：字段名、可空性和枚举取值都必须与
 * `internal/hub/query.go`、`internal/hub/health.go` 和
 * `internal/hub/operation.go` 保持逐字段一致。后端加字段时这里要同步，
 * 否则页面会安静地少显示一块信息——而 ark 的错误本来就是延迟暴露的。
 */

/** 运行与检查的四态状态，对应 `store.Status`。 */
export type Status = 'running' | 'ok' | 'warn' | 'fail'

/** 外部心跳投递状态，对应 `monitoring.HeartbeatStatus`。 */
export type HeartbeatStatus = 'disabled' | 'sent' | 'failed'

/** 主机健康度，由 `deriveHealth` 投影得出。`unknown` 表示无法判断，不是通过。 */
export type Health = 'ok' | 'warn' | 'fail' | 'unknown'

/** 备份目标类型，对应 `config.TargetType`。 */
export type TargetType = 'postgres' | 'redis' | 'volume' | 'files' | 'image_digest'

/** 手工操作类型，对应 `store.OperationKind`。 */
export type OperationKind = 'backup' | 'verify' | 'restore_preview' | 'restore'

/** 手工操作状态，对应 `store.OperationStatus`。`interrupted` 是 Hub 重启后的遗留态。 */
export type OperationStatus = 'running' | 'ok' | 'fail' | 'interrupted'

/** 恢复模式。`isolate` 不碰生产资源，`force` 会覆盖生产数据。 */
export type RestoreMode = 'normal' | 'force' | 'isolate'

/** 告警种类，后端保证这三个值稳定。 */
export type AlertKind = 'backup_overdue' | 'backup_consecutive_failures' | 'verification_failed'

/** 单次成功备份的体积采样点，用于总览的大小趋势。 */
export interface BackupSizePoint {
  run_id: string
  finished_at: string
  bytes: number
}

/** 主机摘要，`GET /api/hosts` 的列表元素。 */
export interface HostSummary {
  host: string
  local: boolean
  project: string
  target_count: number
  schedule: string
  last_backup_status: Status | null
  last_successful_backup_at: string | null
  last_verification_status: Status | null
  next_run_at: string | null
  /** 目前只有 `schedule_unavailable`：调度表达式无法解析，健康度因此为 unknown。 */
  diagnostics: string[]
  health: Health
  last_backup_bytes: number | null
  recent_backup_sizes: BackupSizePoint[]
}

export interface Target {
  id: string
  type: TargetType
}

export interface Run {
  id: string
  requested_host: string | null
  status: Status
  started_at: string
  finished_at: string | null
  duration_ms: number | null
  ark_version: string
  error: string
}

export interface TargetResult {
  target_id: string
  target_type: string
  status: Status
  bytes: number
  duration_ms: number
  snapshot_id: string
  error: string
}

export interface HostRun {
  run: Run
  status: Status
  targets: TargetResult[]
}

export interface DoctorCheck {
  name: string
  status: Status
  detail: string
}

export interface DoctorReport {
  created_at: string
  status: Status
  next_run_at: string | null
  report: { checks: DoctorCheck[] }
}

export interface Verification {
  id: string
  run_id: string | null
  snapshot_id: string
  started_at: string
  finished_at: string
  duration_ms: number
  status: Status
  error: string
  detail: unknown
}

/** 主机详情，`GET /api/hosts/{host}` 的响应。 */
export interface HostDetail {
  summary: HostSummary
  targets: Target[]
  runs: HostRun[]
  doctor: DoctorReport | null
  verifications: Verification[]
}

/**
 * 告警。注意这是**实时投影**而不是历史流水：`created_at` 是投影时刻，
 * 不是告警首次出现的时间（见 `internal/hub/health.go` 的 deriveAlerts）。
 */
export interface Alert {
  id: string
  host: string
  kind: AlertKind | string
  severity: string
  message: string
  created_at: string
}

/** 手工操作。`confirmation_token` 只在恢复预检成功后的第一次读取时出现。 */
export interface Operation {
  id: string
  kind: OperationKind
  host: string
  status: OperationStatus
  started_at: string
  finished_at: string | null
  duration_ms: number | null
  request: unknown
  result: unknown
  error: string
  exit_code: number | null
  parent_operation_id: string | null
  confirmation_token?: string
}

/** 恢复目标上发现的既有资源冲突，对应 `restore.Conflict`。 */
export interface RestoreConflict {
  resource: string
  detail: string
  force_allowed: boolean
}

export interface RestoreStep {
  phase: string
  target_id?: string
  target_type?: TargetType
  snapshot_id?: string
  [key: string]: unknown
}

export interface RestorePlan {
  manifest_snapshot_id: string
  run_id: string
  source_host: string
  destination_host: string
  project: unknown
  steps: RestoreStep[]
  manual_checks: string[]
  isolation?: unknown
}

/** 恢复预检结果，字段白名单见 `operationResultFields`。 */
export interface RestorePreviewResult {
  plan: RestorePlan
  force: boolean
  resume: boolean
  destructive: boolean
  conflicts: RestoreConflict[]
  digest: string
}

/** 恢复执行结果。 */
export interface RestoreResult {
  manifest_snapshot_id: string
  run_id: string
  source_host: string
  destination_host: string
  status: Status
  steps: RestoreStep[]
  manual_checks: string[]
  error?: string
  isolation?: unknown
}

/** 备份操作结果。 */
export interface BackupResult {
  run_id: string
  status: Status
  manifest?: unknown
  manifest_snapshot_id: string
  heartbeat_status: HeartbeatStatus
  error: string
}

/** 演练操作结果。 */
export interface VerifyResult {
  manifest_snapshot_id: string
  status: Status
  results: unknown[]
  error?: string
}

/** 会话信息，`GET /api/session` 的响应。 */
export interface SessionInfo {
  authenticated: boolean
  username: string
  csrf_token: string
}

/** keyset 分页列表响应。 */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
}

/** 后端稳定的错误码，见 `internal/hub/api.go` 与 `api_action.go`。 */
export type ApiErrorCode =
  | 'invalid_request'
  | 'not_found'
  | 'conflict'
  | 'confirmation_required'
  | 'confirmation_expired'
  | 'operation_failed'
  | 'service_unavailable'
  | 'unknown'
