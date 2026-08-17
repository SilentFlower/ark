package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultListLimit = 50
	maximumListLimit = 100
)

// RunListOptions 定义整体运行记录的筛选、游标和数量限制。
type RunListOptions struct {
	Host     string
	Status   Status
	BeforeAt time.Time
	BeforeID string
	Limit    int
}

// HostRun 表示一次整体运行中指定 host 的 target 结果及其聚合状态。
type HostRun struct {
	Run     Run
	Status  Status
	Targets []RunTarget
}

// ListRuns 按开始时间和 ID 倒序查询整体运行记录。
// @param ctx 控制数据库查询的取消与超时。
// @param options host、状态、keyset 游标和数量限制。
// @return []Run 当前页运行记录。
// @return bool 是否仍有下一页。
// @return error 参数校验或数据库查询失败时的错误。
func (s *Store) ListRuns(ctx context.Context, options RunListOptions) ([]Run, bool, error) {
	limit, err := normalizeListLimit(options.Limit)
	if err != nil {
		return nil, false, fmt.Errorf("查询运行记录失败: %w", err)
	}
	if err := validateRunListOptions(options); err != nil {
		return nil, false, err
	}

	query := `
		SELECT r.id, r.requested_host, r.status, r.started_at, r.finished_at,
			r.duration_ms, r.ark_version, r.error
		FROM runs AS r
		WHERE 1 = 1`
	args := make([]any, 0, 6)
	if options.Host != "" {
		query += ` AND EXISTS (
			SELECT 1 FROM run_targets AS rt WHERE rt.run_id = r.id AND rt.host = ?
		)`
		args = append(args, options.Host)
	}
	if options.Status != "" {
		query += " AND r.status = ?"
		args = append(args, options.Status)
	}
	if !options.BeforeAt.IsZero() {
		beforeAt, conversionErr := requiredUnixMilli(options.BeforeAt, "run_list.before_at")
		if conversionErr != nil {
			return nil, false, conversionErr
		}
		query += " AND (r.started_at < ? OR (r.started_at = ? AND r.id < ?))"
		args = append(args, beforeAt, beforeAt, options.BeforeID)
	}
	query += " ORDER BY r.started_at DESC, r.id DESC LIMIT ?"
	args = append(args, limit+1)

	runs := make([]Run, 0, limit+1)
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		rows, queryErr := conn.QueryContext(ctx, query, args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			run, scanErr := scanRun(rows)
			if scanErr != nil {
				return scanErr
			}
			runs = append(runs, run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("查询运行记录失败: %w", err)
	}

	hasMore := len(runs) > limit
	if hasMore {
		runs = runs[:limit]
	}
	return runs, hasMore, nil
}

// ListHostRuns 查询指定 host 最近已完成的运行及 target 结果。
// @param ctx 控制数据库查询的取消与超时。
// @param host 清单中的 host 标识。
// @param limit 返回的最大运行数量，零值使用默认值。
// @return []HostRun 按开始时间倒序的 host 运行记录。
// @return error 参数校验或数据库查询失败时的错误。
func (s *Store) ListHostRuns(ctx context.Context, host string, limit int) ([]HostRun, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("查询 host 运行记录失败: host 不能为空")
	}
	normalizedLimit, err := normalizeListLimit(limit)
	if err != nil {
		return nil, fmt.Errorf("查询 host 运行记录失败: %w", err)
	}

	result := make([]HostRun, 0, normalizedLimit)
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		rows, queryErr := conn.QueryContext(ctx, `
			WITH recent_runs AS (
				SELECT r.id, r.requested_host, r.status, r.started_at, r.finished_at,
					r.duration_ms, r.ark_version, r.error
				FROM runs AS r
				WHERE r.status != ? AND EXISTS (
					SELECT 1 FROM run_targets AS candidate
					WHERE candidate.run_id = r.id AND candidate.host = ?
				)
				ORDER BY r.started_at DESC, r.id DESC
				LIMIT ?
			)
			SELECT r.id, r.requested_host, r.status, r.started_at, r.finished_at,
				r.duration_ms, r.ark_version, r.error,
				t.run_id, t.host, t.target_id, t.target_type, t.status,
				t.bytes, t.duration_ms, t.snapshot_id, t.error
			FROM recent_runs AS r
			JOIN run_targets AS t ON t.run_id = r.id AND t.host = ?
			ORDER BY r.started_at DESC, r.id DESC, t.target_id ASC`,
			StatusRunning, host, normalizedLimit, host,
		)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()

		for rows.Next() {
			run, target, scanErr := scanHostRunRow(rows)
			if scanErr != nil {
				return scanErr
			}
			if len(result) == 0 || result[len(result)-1].Run.ID != run.ID {
				result = append(result, HostRun{
					Run: run, Status: StatusOK, Targets: make([]RunTarget, 0, 1),
				})
			}
			current := &result[len(result)-1]
			current.Targets = append(current.Targets, target)
			current.Status = aggregateTargetStatus(current.Status, target.Status)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("查询 host %q 的运行记录失败: %w", host, err)
	}
	return result, nil
}

// LatestDoctorReport 查询指定范围最近一份 doctor 报告。
// @param ctx 控制数据库查询的取消与超时。
// @param scope local 或 host 检查范围。
// @param host host 范围的清单标识；local 范围必须为空。
// @return DoctorReport 最近一份报告；其 next_run_at 缺失时回退到同范围最近一次已知值。
// @return bool 是否存在报告。
// @return error 参数校验或数据库查询失败时的错误。
func (s *Store) LatestDoctorReport(
	ctx context.Context,
	scope DoctorScope,
	host string,
) (DoctorReport, bool, error) {
	if err := validateDoctorIdentity(scope, host); err != nil {
		return DoctorReport{}, false, err
	}

	var report DoctorReport
	var storedHost sql.NullString
	var createdAt int64
	var nextRunAt sql.NullInt64
	var reportJSON string
	err := withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
				SELECT latest.scope, latest.host, latest.created_at, latest.status,
					COALESCE(latest.next_run_at, (
						SELECT previous.next_run_at
						FROM doctor_reports AS previous
						WHERE previous.scope = latest.scope
							AND previous.host IS latest.host
							AND previous.next_run_at IS NOT NULL
						ORDER BY previous.created_at DESC, previous.id DESC
						LIMIT 1
					)),
					latest.report_json
				FROM doctor_reports AS latest
				WHERE latest.scope = ? AND latest.host IS ?
				ORDER BY latest.created_at DESC, latest.id DESC
				LIMIT 1`, scope, nullableString(host),
		).Scan(
			&report.Scope, &storedHost, &createdAt, &report.Status,
			&nextRunAt, &reportJSON,
		)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DoctorReport{}, false, nil
	}
	if err != nil {
		return DoctorReport{}, false, fmt.Errorf("查询 doctor 报告失败: %w", err)
	}
	report.Host = storedHost.String
	report.CreatedAt = time.UnixMilli(createdAt).UTC()
	report.ReportJSON = []byte(reportJSON)
	if nextRunAt.Valid {
		report.NextRunAt = time.UnixMilli(nextRunAt.Int64).UTC()
	}
	return report, true, nil
}

// ListVerifications 查询指定 host 最近的恢复演练记录。
// @param ctx 控制数据库查询的取消与超时。
// @param host 清单中的 host 标识。
// @param limit 返回的最大记录数，零值使用默认值。
// @return []Verification 按开始时间和 ID 倒序的演练记录。
// @return error 参数校验或数据库查询失败时的错误。
func (s *Store) ListVerifications(
	ctx context.Context,
	host string,
	limit int,
) ([]Verification, error) {
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("查询演练记录失败: host 不能为空")
	}
	normalizedLimit, err := normalizeListLimit(limit)
	if err != nil {
		return nil, fmt.Errorf("查询演练记录失败: %w", err)
	}

	verifications := make([]Verification, 0, normalizedLimit)
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		rows, queryErr := conn.QueryContext(ctx, `
			SELECT id, host, run_id, snapshot_id, started_at, finished_at,
				duration_ms, status, error, detail_json
			FROM verifications
			WHERE host = ?
			ORDER BY started_at DESC, id DESC
			LIMIT ?`, host, normalizedLimit,
		)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			verification, scanErr := scanVerification(rows)
			if scanErr != nil {
				return scanErr
			}
			verifications = append(verifications, verification)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("查询 host %q 的演练记录失败: %w", host, err)
	}
	return verifications, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(scanner rowScanner) (Run, error) {
	var run Run
	var requestedHost sql.NullString
	var startedAt int64
	var finishedAt sql.NullInt64
	var durationMilliseconds sql.NullInt64
	if err := scanner.Scan(
		&run.ID, &requestedHost, &run.Status, &startedAt, &finishedAt,
		&durationMilliseconds, &run.ArkVersion, &run.Error,
	); err != nil {
		return Run{}, err
	}
	run.RequestedHost = requestedHost.String
	run.StartedAt = time.UnixMilli(startedAt).UTC()
	if finishedAt.Valid {
		run.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	if durationMilliseconds.Valid {
		duration, err := durationFromMilliseconds(durationMilliseconds.Int64)
		if err != nil {
			return Run{}, err
		}
		run.Duration = duration
	}
	return run, nil
}

func scanHostRunRow(scanner rowScanner) (Run, RunTarget, error) {
	var run Run
	var target RunTarget
	var requestedHost sql.NullString
	var startedAt int64
	var finishedAt sql.NullInt64
	var runDurationMilliseconds sql.NullInt64
	var targetDurationMilliseconds int64
	if err := scanner.Scan(
		&run.ID, &requestedHost, &run.Status, &startedAt, &finishedAt,
		&runDurationMilliseconds, &run.ArkVersion, &run.Error,
		&target.RunID, &target.Host, &target.TargetID, &target.TargetType,
		&target.Status, &target.Bytes, &targetDurationMilliseconds,
		&target.SnapshotID, &target.Error,
	); err != nil {
		return Run{}, RunTarget{}, err
	}
	run.RequestedHost = requestedHost.String
	run.StartedAt = time.UnixMilli(startedAt).UTC()
	if finishedAt.Valid {
		run.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	var err error
	if runDurationMilliseconds.Valid {
		run.Duration, err = durationFromMilliseconds(runDurationMilliseconds.Int64)
		if err != nil {
			return Run{}, RunTarget{}, err
		}
	}
	target.Duration, err = durationFromMilliseconds(targetDurationMilliseconds)
	if err != nil {
		return Run{}, RunTarget{}, err
	}
	return run, target, nil
}

func scanVerification(scanner rowScanner) (Verification, error) {
	var verification Verification
	var runID sql.NullString
	var startedAt int64
	var finishedAt int64
	var durationMilliseconds int64
	var detailJSON string
	if err := scanner.Scan(
		&verification.ID, &verification.Host, &runID, &verification.SnapshotID,
		&startedAt, &finishedAt, &durationMilliseconds, &verification.Status,
		&verification.Error, &detailJSON,
	); err != nil {
		return Verification{}, err
	}
	verification.RunID = runID.String
	verification.StartedAt = time.UnixMilli(startedAt).UTC()
	verification.FinishedAt = time.UnixMilli(finishedAt).UTC()
	duration, err := durationFromMilliseconds(durationMilliseconds)
	if err != nil {
		return Verification{}, err
	}
	verification.Duration = duration
	verification.DetailJSON = []byte(detailJSON)
	return verification, nil
}

func validateRunListOptions(options RunListOptions) error {
	if (options.BeforeAt.IsZero()) != (options.BeforeID == "") {
		return fmt.Errorf("查询运行记录失败: before_at 和 before_id 必须同时提供")
	}
	if options.Status != "" {
		switch options.Status {
		case StatusRunning, StatusOK, StatusWarn, StatusFail:
		default:
			return fmt.Errorf("查询运行记录失败: status %q 非法", options.Status)
		}
	}
	return nil
}

func validateDoctorIdentity(scope DoctorScope, host string) error {
	switch scope {
	case DoctorScopeLocal:
		if host != "" {
			return fmt.Errorf("查询 doctor 报告失败: local 范围不能指定 host")
		}
	case DoctorScopeHost:
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("查询 doctor 报告失败: host 范围必须指定 host")
		}
	default:
		return fmt.Errorf("查询 doctor 报告失败: scope %q 非法", scope)
	}
	return nil
}

func normalizeListLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultListLimit, nil
	}
	if limit < 0 || limit > maximumListLimit {
		return 0, fmt.Errorf("limit 必须在 1 到 %d 之间", maximumListLimit)
	}
	return limit, nil
}

func aggregateTargetStatus(current Status, candidate Status) Status {
	if current == StatusFail || candidate == StatusFail {
		return StatusFail
	}
	if current == StatusWarn || candidate == StatusWarn {
		return StatusWarn
	}
	return StatusOK
}
