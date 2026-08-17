package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const interruptedOperationError = "Hub 上次运行异常终止"

// OperationKind 是 Hub 手工任务的业务类型。
type OperationKind string

const (
	// OperationKindBackup 表示手工备份。
	OperationKindBackup OperationKind = "backup"
	// OperationKindVerify 表示手工恢复演练。
	OperationKindVerify OperationKind = "verify"
	// OperationKindRestorePreview 表示恢复预检。
	OperationKindRestorePreview OperationKind = "restore_preview"
	// OperationKindRestore 表示正式恢复。
	OperationKindRestore OperationKind = "restore"
)

// OperationStatus 是 Hub 手工任务的生命周期状态。
type OperationStatus string

const (
	// OperationStatusRunning 表示任务仍在运行。
	OperationStatusRunning OperationStatus = "running"
	// OperationStatusOK 表示任务成功完成。
	OperationStatusOK OperationStatus = "ok"
	// OperationStatusFail 表示任务执行失败。
	OperationStatusFail OperationStatus = "fail"
	// OperationStatusInterrupted 表示任务因 Hub 退出或异常重启而中断。
	OperationStatusInterrupted OperationStatus = "interrupted"
)

// ManualOperation 表示一条可轮询的 Hub 手工任务记录。
type ManualOperation struct {
	ID                string
	Kind              OperationKind
	Host              string
	Status            OperationStatus
	StartedAt         time.Time
	FinishedAt        time.Time
	Duration          time.Duration
	RequestJSON       json.RawMessage
	ResultJSON        json.RawMessage
	Error             string
	ExitCode          *int
	ParentOperationID string
}

// ManualOperationResult 是完成手工任务时写入的终态结果。
type ManualOperationResult struct {
	Status     OperationStatus
	FinishedAt time.Time
	Duration   time.Duration
	ResultJSON json.RawMessage
	Error      string
	ExitCode   *int
}

// OperationListOptions 定义手工任务的筛选、游标和数量限制。
type OperationListOptions struct {
	Host     string
	Kind     OperationKind
	Status   OperationStatus
	BeforeAt time.Time
	BeforeID string
	Limit    int
}

// CreateManualOperation 创建一条 running 状态的手工任务记录。
// @param ctx 控制数据库写入的取消与超时。
// @param operation 待创建的任务；完成字段必须为空。
// @return error 参数校验、唯一约束或数据库写入失败时的错误。
func (s *Store) CreateManualOperation(ctx context.Context, operation ManualOperation) error {
	if err := validateNewManualOperation(operation); err != nil {
		return err
	}
	startedAt, err := requiredUnixMilli(operation.StartedAt, "manual_operation.started_at")
	if err != nil {
		return err
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO manual_operations (
				id, kind, host, status, started_at, finished_at, duration_ms,
				request_json, result_json, error, exit_code, parent_operation_id
			) VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, NULL, '', NULL, ?)`,
			operation.ID, operation.Kind, operation.Host, OperationStatusRunning,
			startedAt, string(operation.RequestJSON), nullableString(operation.ParentOperationID),
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("创建手工任务 %q 失败: %w", operation.ID, err)
	}
	return nil
}

// FinishManualOperation 把一条 running 手工任务原子转换为终态。
// @param ctx 控制数据库写入的取消与超时。
// @param id 手工任务 ID。
// @param result 终态、完成时间、耗时和白名单结果。
// @return error 参数校验、任务不存在、重复完成或数据库写入失败时的错误。
func (s *Store) FinishManualOperation(
	ctx context.Context,
	id string,
	result ManualOperationResult,
) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("完成手工任务失败: id 不能为空")
	}
	if err := validateManualOperationResult(result); err != nil {
		return err
	}
	finishedAt, err := requiredUnixMilli(result.FinishedAt, "manual_operation.finished_at")
	if err != nil {
		return err
	}
	durationMilliseconds, err := durationMilliseconds(result.Duration, "manual_operation.duration")
	if err != nil {
		return err
	}

	var resultSQL sql.Result
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		var execErr error
		resultSQL, execErr = conn.ExecContext(ctx, `
			UPDATE manual_operations
			SET status = ?, finished_at = ?, duration_ms = ?, result_json = ?,
				error = ?, exit_code = ?
			WHERE id = ? AND status = ?`,
			result.Status, finishedAt, durationMilliseconds,
			nullableJSON(result.ResultJSON), result.Error, nullableInt(result.ExitCode),
			id, OperationStatusRunning,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("完成手工任务 %q 失败: %w", id, err)
	}
	if err := requireOneRow(resultSQL, fmt.Sprintf("手工任务 %q 不存在或已完成", id)); err != nil {
		return err
	}
	return nil
}

// InterruptRunningOperations 把 Hub 启动时遗留的 running 任务标记为 interrupted。
// @param ctx 控制数据库写入的取消与超时。
// @param finishedAt 本次中断清理的统一完成时间。
// @return int64 被中断的任务数量。
// @return error 参数校验或数据库写入失败时的错误。
func (s *Store) InterruptRunningOperations(ctx context.Context, finishedAt time.Time) (int64, error) {
	finishedMilliseconds, err := requiredUnixMilli(finishedAt, "manual_operation.finished_at")
	if err != nil {
		return 0, err
	}

	var resultSQL sql.Result
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		var execErr error
		resultSQL, execErr = conn.ExecContext(ctx, `
			UPDATE manual_operations
			SET status = ?, finished_at = MAX(?, started_at),
				duration_ms = MAX(0, ? - started_at), error = ?
			WHERE status = ?`,
			OperationStatusInterrupted, finishedMilliseconds, finishedMilliseconds,
			interruptedOperationError, OperationStatusRunning,
		)
		return execErr
	})
	if err != nil {
		return 0, fmt.Errorf("清理遗留手工任务失败: %w", err)
	}
	count, err := resultSQL.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取遗留手工任务清理数量失败: %w", err)
	}
	return count, nil
}

// GetManualOperation 查询一条手工任务记录。
// @param ctx 控制数据库查询的取消与超时。
// @param id 手工任务 ID。
// @return ManualOperation 完整任务记录。
// @return error 参数校验或查询失败时的错误；不存在时保留 sql.ErrNoRows。
func (s *Store) GetManualOperation(ctx context.Context, id string) (ManualOperation, error) {
	if strings.TrimSpace(id) == "" {
		return ManualOperation{}, fmt.Errorf("查询手工任务失败: id 不能为空")
	}

	var operation ManualOperation
	err := withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		var queryErr error
		operation, queryErr = scanManualOperation(conn.QueryRowContext(ctx, `
			SELECT id, kind, host, status, started_at, finished_at, duration_ms,
				request_json, result_json, error, exit_code, parent_operation_id
			FROM manual_operations
			WHERE id = ?`, id))
		return queryErr
	})
	if err != nil {
		return ManualOperation{}, fmt.Errorf("查询手工任务 %q 失败: %w", id, err)
	}
	return operation, nil
}

// ListManualOperations 按开始时间和 ID 倒序查询手工任务。
// @param ctx 控制数据库查询的取消与超时。
// @param options host、类型、状态、keyset 游标和数量限制。
// @return []ManualOperation 当前页手工任务。
// @return bool 是否仍有下一页。
// @return error 参数校验或数据库查询失败时的错误。
func (s *Store) ListManualOperations(
	ctx context.Context,
	options OperationListOptions,
) ([]ManualOperation, bool, error) {
	limit, err := normalizeListLimit(options.Limit)
	if err != nil {
		return nil, false, fmt.Errorf("查询手工任务失败: %w", err)
	}
	if err := validateOperationListOptions(options); err != nil {
		return nil, false, err
	}

	query := `
		SELECT id, kind, host, status, started_at, finished_at, duration_ms,
			request_json, result_json, error, exit_code, parent_operation_id
		FROM manual_operations
		WHERE 1 = 1`
	args := make([]any, 0, 8)
	if options.Host != "" {
		query += " AND host = ?"
		args = append(args, options.Host)
	}
	if options.Kind != "" {
		query += " AND kind = ?"
		args = append(args, options.Kind)
	}
	if options.Status != "" {
		query += " AND status = ?"
		args = append(args, options.Status)
	}
	if !options.BeforeAt.IsZero() {
		beforeAt, conversionErr := requiredUnixMilli(options.BeforeAt, "operation_list.before_at")
		if conversionErr != nil {
			return nil, false, conversionErr
		}
		query += " AND (started_at < ? OR (started_at = ? AND id < ?))"
		args = append(args, beforeAt, beforeAt, options.BeforeID)
	}
	query += " ORDER BY started_at DESC, id DESC LIMIT ?"
	args = append(args, limit+1)

	operations := make([]ManualOperation, 0, limit+1)
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		rows, queryErr := conn.QueryContext(ctx, query, args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			operation, scanErr := scanManualOperation(rows)
			if scanErr != nil {
				return scanErr
			}
			operations = append(operations, operation)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("查询手工任务失败: %w", err)
	}

	hasMore := len(operations) > limit
	if hasMore {
		operations = operations[:limit]
	}
	return operations, hasMore, nil
}

func scanManualOperation(scanner rowScanner) (ManualOperation, error) {
	var operation ManualOperation
	var finishedAt sql.NullInt64
	var durationMilliseconds sql.NullInt64
	var resultJSON []byte
	var requestJSON string
	var resultJSONString sql.NullString
	var exitCode sql.NullInt64
	var parentOperationID sql.NullString
	var startedAt int64
	if err := scanner.Scan(
		&operation.ID, &operation.Kind, &operation.Host, &operation.Status,
		&startedAt, &finishedAt, &durationMilliseconds, &requestJSON,
		&resultJSONString, &operation.Error, &exitCode, &parentOperationID,
	); err != nil {
		return ManualOperation{}, err
	}
	operation.StartedAt = time.UnixMilli(startedAt).UTC()
	operation.RequestJSON = []byte(requestJSON)
	if finishedAt.Valid {
		operation.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	if durationMilliseconds.Valid {
		duration, err := durationFromMilliseconds(durationMilliseconds.Int64)
		if err != nil {
			return ManualOperation{}, err
		}
		operation.Duration = duration
	}
	if resultJSONString.Valid {
		resultJSON = []byte(resultJSONString.String)
		operation.ResultJSON = json.RawMessage(resultJSON)
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		operation.ExitCode = &value
	}
	operation.ParentOperationID = parentOperationID.String
	return operation, nil
}

func validateNewManualOperation(operation ManualOperation) error {
	if strings.TrimSpace(operation.ID) == "" {
		return fmt.Errorf("创建手工任务失败: id 不能为空")
	}
	if !validOperationKind(operation.Kind) {
		return fmt.Errorf("创建手工任务失败: kind %q 非法", operation.Kind)
	}
	if strings.TrimSpace(operation.Host) == "" {
		return fmt.Errorf("创建手工任务失败: host 不能为空")
	}
	if operation.Status != OperationStatusRunning {
		return fmt.Errorf("创建手工任务失败: status 期望 %q，实际 %q", OperationStatusRunning, operation.Status)
	}
	if !json.Valid(operation.RequestJSON) {
		return fmt.Errorf("创建手工任务失败: request_json 不是合法 JSON")
	}
	if !operation.FinishedAt.IsZero() || operation.Duration != 0 || operation.ResultJSON != nil ||
		operation.Error != "" || operation.ExitCode != nil {
		return fmt.Errorf("创建手工任务失败: running 状态不能包含完成字段")
	}
	return nil
}

func validateManualOperationResult(result ManualOperationResult) error {
	if !validFinalOperationStatus(result.Status) {
		return fmt.Errorf("完成手工任务失败: status %q 非法", result.Status)
	}
	if result.ResultJSON != nil && !json.Valid(result.ResultJSON) {
		return fmt.Errorf("完成手工任务失败: result_json 不是合法 JSON")
	}
	return nil
}

func validateOperationListOptions(options OperationListOptions) error {
	if (options.BeforeAt.IsZero()) != (options.BeforeID == "") {
		return fmt.Errorf("查询手工任务失败: before_at 和 before_id 必须同时提供")
	}
	if options.Kind != "" && !validOperationKind(options.Kind) {
		return fmt.Errorf("查询手工任务失败: kind %q 非法", options.Kind)
	}
	if options.Status != "" && !validOperationStatus(options.Status) {
		return fmt.Errorf("查询手工任务失败: status %q 非法", options.Status)
	}
	return nil
}

func validOperationKind(kind OperationKind) bool {
	switch kind {
	case OperationKindBackup, OperationKindVerify, OperationKindRestorePreview, OperationKindRestore:
		return true
	default:
		return false
	}
}

func validOperationStatus(status OperationStatus) bool {
	return status == OperationStatusRunning || validFinalOperationStatus(status)
}

func validFinalOperationStatus(status OperationStatus) bool {
	switch status {
	case OperationStatusOK, OperationStatusFail, OperationStatusInterrupted:
		return true
	default:
		return false
	}
}

func nullableJSON(value json.RawMessage) any {
	if value == nil {
		return nil
	}
	return string(value)
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
