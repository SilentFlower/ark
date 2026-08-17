// Package store 管理 ark 的本地 SQLite 状态库。
//
// 本包只负责连接、schema 迁移、数据校验和状态读写，不依赖 config、doctor、
// backup 等业务包。调用方负责把业务结果转换为这里的稳定记录类型，并确保
// 错误文本已经脱敏；未来 ark-hub 也通过本包读取同一份 WAL 数据库。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	// DefaultPath 是 ark 状态库的默认路径。
	DefaultPath = "/var/lib/ark/ark.db"
	// currentSchemaVersion 是当前程序支持的 schema 版本。
	currentSchemaVersion = 2
	// busyTimeoutMilliseconds 是 SQLite 等待短暂写锁竞争的最长时间。
	busyTimeoutMilliseconds = 5000
	// busyRetryIntervalMilliseconds 把长 busy handler 拆成短等待，确保 context
	// 取消不必等满默认的 5 秒。
	busyRetryIntervalMilliseconds = 25
	// connectionRestoreTimeout 限制归还连接前恢复默认 PRAGMA 的等待时间。
	connectionRestoreTimeout = time.Second
)

//go:embed schema.sql
var schemaV1 string

//go:embed schema_v2.sql
var schemaV2 string

var migrations = []string{schemaV1, schemaV2}

// Status 是状态库中记录的运行结果状态。
type Status string

const (
	// StatusRunning 表示任务仍在执行。
	StatusRunning Status = "running"
	// StatusOK 表示任务成功完成。
	StatusOK Status = "ok"
	// StatusWarn 表示任务完成，但存在需要关注的风险。
	StatusWarn Status = "warn"
	// StatusFail 表示任务失败。
	StatusFail Status = "fail"
)

// DoctorScope 是 doctor 报告的检查范围。
type DoctorScope string

const (
	// DoctorScopeLocal 表示报告针对 hub 本机。
	DoctorScopeLocal DoctorScope = "local"
	// DoctorScopeHost 表示报告针对清单中的一台 host。
	DoctorScopeHost DoctorScope = "host"
)

// Store 持有状态库连接池，并封装全部 SQL 与迁移细节。
type Store struct {
	db *sql.DB
}

// Run 表示一次整体 ark backup 运行。
type Run struct {
	ID            string
	RequestedHost string
	Status        Status
	StartedAt     time.Time
	FinishedAt    time.Time
	Duration      time.Duration
	ArkVersion    string
	Error         string
}

// RunResult 是完成一次 run 时写入的最终结果。
type RunResult struct {
	Status     Status
	FinishedAt time.Time
	Duration   time.Duration
	Error      string
}

// RunTarget 是一台 host 上一个 target 的最终备份结果。
type RunTarget struct {
	RunID      string
	Host       string
	TargetID   string
	TargetType string
	Status     Status
	Bytes      int64
	Duration   time.Duration
	SnapshotID string
	Error      string
}

// DoctorReport 是一次 local 或 host 范围的 doctor 报告。
type DoctorReport struct {
	Scope      DoctorScope
	Host       string
	CreatedAt  time.Time
	Status     Status
	NextRunAt  time.Time
	ReportJSON json.RawMessage
}

// Verification 是一次恢复演练的最终结果。
type Verification struct {
	ID         string
	Host       string
	RunID      string
	SnapshotID string
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration
	Status     Status
	Error      string
	DetailJSON json.RawMessage
}

// Open 打开状态库，并在首次运行时创建目录、数据库和 schema。
// @param ctx 控制连接、WAL 配置和迁移的取消与超时。
// @param path 数据库文件路径；测试可传临时路径，生产通常使用 DefaultPath。
// @return *Store 已完成初始化且可并发使用的状态库。
// @return error 路径、权限、连接、WAL 或迁移失败时的错误。
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("打开状态库失败: 数据库路径不能为空")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析状态库路径 %q 失败: %w", path, err)
	}
	if err := prepareDatabaseFile(absolutePath); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dataSourceName(absolutePath))
	if err != nil {
		return nil, fmt.Errorf("创建状态库连接失败: %w", err)
	}
	closeOnError := func(openErr error) (*Store, error) {
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(openErr, fmt.Errorf("关闭初始化失败的状态库连接失败: %w", closeErr))
		}
		return nil, openErr
	}

	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("连接状态库失败: %w", err))
	}
	if err := enableWAL(ctx, db); err != nil {
		return closeOnError(err)
	}
	if err := migrate(ctx, db); err != nil {
		return closeOnError(err)
	}

	return &Store{db: db}, nil
}

// Close 关闭状态库连接池。
// @return error 等待中的数据库资源无法关闭时的错误。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("关闭状态库失败: %w", err)
	}
	return nil
}

// CreateRun 创建一条处于 running 状态的整体备份运行记录。
// @param ctx 控制数据库写入的取消与超时。
// @param run 待创建的运行记录；完成字段必须为空。
// @return error 字段校验、唯一约束或数据库写入失败时的错误。
func (s *Store) CreateRun(ctx context.Context, run Run) error {
	if err := validateNewRun(run); err != nil {
		return err
	}
	startedAt, err := requiredUnixMilli(run.StartedAt, "run.started_at")
	if err != nil {
		return err
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO runs (
				id, requested_host, status, started_at, finished_at,
				duration_ms, ark_version, error
			) VALUES (?, ?, ?, ?, NULL, NULL, ?, '')`,
			run.ID,
			nullableString(run.RequestedHost),
			StatusRunning,
			startedAt,
			run.ArkVersion,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("创建运行记录 %q 失败: %w", run.ID, err)
	}
	return nil
}

// GetRun 查询一条整体备份运行记录。
// @param ctx 控制数据库查询的取消与超时。
// @param id 运行记录 ID。
// @return Run 完整运行记录；可空数据库字段还原为对应零值。
// @return error 查询失败时的错误；不存在时保留 sql.ErrNoRows 错误链。
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	if strings.TrimSpace(id) == "" {
		return Run{}, fmt.Errorf("查询运行记录失败: run.id 不能为空")
	}

	var run Run
	var requestedHost sql.NullString
	var startedAt int64
	var finishedAt sql.NullInt64
	var durationMilliseconds sql.NullInt64
	err := withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
			SELECT id, requested_host, status, started_at, finished_at,
				duration_ms, ark_version, error
			FROM runs
			WHERE id = ?`, id,
		).Scan(
			&run.ID,
			&requestedHost,
			&run.Status,
			&startedAt,
			&finishedAt,
			&durationMilliseconds,
			&run.ArkVersion,
			&run.Error,
		)
	})
	if err != nil {
		return Run{}, fmt.Errorf("查询运行记录 %q 失败: %w", id, err)
	}

	run.RequestedHost = requestedHost.String
	run.StartedAt = time.UnixMilli(startedAt).UTC()
	if finishedAt.Valid {
		run.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	if durationMilliseconds.Valid {
		run.Duration, err = durationFromMilliseconds(durationMilliseconds.Int64)
		if err != nil {
			return Run{}, fmt.Errorf("读取运行记录 %q 的 duration 失败: %w", id, err)
		}
	}
	return run, nil
}

// FinishRun 写入一条 run 的最终状态和完成时间。
// @param ctx 控制数据库写入的取消与超时。
// @param id 要完成的运行记录 ID。
// @param result 最终状态、完成时间、耗时和已脱敏错误。
// @return error 字段校验、记录不存在或数据库写入失败时的错误。
func (s *Store) FinishRun(ctx context.Context, id string, result RunResult) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("完成运行记录失败: run.id 不能为空")
	}
	if err := validateFinalStatus(result.Status, "run.status"); err != nil {
		return err
	}
	finishedAt, err := requiredUnixMilli(result.FinishedAt, "run.finished_at")
	if err != nil {
		return err
	}
	durationMilliseconds, err := durationMilliseconds(result.Duration, "run.duration")
	if err != nil {
		return err
	}

	var resultSQL sql.Result
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		var execErr error
		resultSQL, execErr = conn.ExecContext(ctx, `
			UPDATE runs
			SET status = ?, finished_at = ?, duration_ms = ?, error = ?
			WHERE id = ?`,
			result.Status,
			finishedAt,
			durationMilliseconds,
			result.Error,
			id,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("完成运行记录 %q 失败: %w", id, err)
	}
	if err := requireOneRow(resultSQL, fmt.Sprintf("运行记录 %q 不存在", id)); err != nil {
		return err
	}
	return nil
}

// RecordRunTarget 记录一台 host 上一个 target 的最终备份结果。
// @param ctx 控制数据库写入的取消与超时。
// @param target 待记录的 target 结果。
// @return error 字段校验、外键或唯一约束、数据库写入失败时的错误。
func (s *Store) RecordRunTarget(ctx context.Context, target RunTarget) error {
	if err := validateRunTarget(target); err != nil {
		return err
	}
	durationMilliseconds, err := durationMilliseconds(target.Duration, "run_target.duration")
	if err != nil {
		return err
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO run_targets (
				run_id, host, target_id, target_type, status,
				bytes, duration_ms, snapshot_id, error
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			target.RunID,
			target.Host,
			target.TargetID,
			target.TargetType,
			target.Status,
			target.Bytes,
			durationMilliseconds,
			target.SnapshotID,
			target.Error,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("记录 host %q 的 target %q 结果失败: %w", target.Host, target.TargetID, err)
	}
	return nil
}

// LastSuccessfulTargetBytes 查询指定 host 和 target 最近一次成功结果的字节数。
// @param ctx 控制数据库查询的取消与超时。
// @param host 清单中的 host 标识。
// @param targetID config.Target.ID() 生成的稳定 target 标识。
// @return bytes 最近一次成功结果的字节数。
// @return found 存在历史成功结果时为 true，无历史时为 false。
// @return error 字段校验或数据库查询失败时的错误。
func (s *Store) LastSuccessfulTargetBytes(
	ctx context.Context,
	host string,
	targetID string,
) (bytes int64, found bool, err error) {
	if strings.TrimSpace(host) == "" {
		return 0, false, fmt.Errorf("查询 target 历史失败: host 不能为空")
	}
	if strings.TrimSpace(targetID) == "" {
		return 0, false, fmt.Errorf("查询 target 历史失败: target_id 不能为空")
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, `
			SELECT rt.bytes
			FROM run_targets AS rt
			JOIN runs AS r ON r.id = rt.run_id
			WHERE rt.host = ? AND rt.target_id = ? AND rt.status = ?
			ORDER BY r.started_at DESC, rt.run_id DESC
			LIMIT 1`, host, targetID, StatusOK,
		).Scan(&bytes)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("查询 host %q 的 target %q 历史失败: %w", host, targetID, err)
	}
	return bytes, true, nil
}

// RecordDoctorReport 追加一份 doctor 报告。
// @param ctx 控制数据库写入的取消与超时。
// @param report 待记录的检查范围、状态、计划时间和完整 JSON 报告。
// @return error 字段校验或数据库写入失败时的错误。
func (s *Store) RecordDoctorReport(ctx context.Context, report DoctorReport) error {
	if err := validateDoctorReport(report); err != nil {
		return err
	}
	createdAt, err := requiredUnixMilli(report.CreatedAt, "doctor_report.created_at")
	if err != nil {
		return err
	}
	nextRunAt, err := nullableUnixMilli(report.NextRunAt, "doctor_report.next_run_at")
	if err != nil {
		return err
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO doctor_reports (
				scope, host, created_at, status, next_run_at, report_json
			) VALUES (?, ?, ?, ?, ?, ?)`,
			report.Scope,
			nullableString(report.Host),
			createdAt,
			report.Status,
			nextRunAt,
			string(report.ReportJSON),
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("记录 doctor 报告失败: %w", err)
	}
	return nil
}

// RecordVerification 追加一次恢复演练的最终结果。
// @param ctx 控制数据库写入的取消与超时。
// @param verification 待记录的演练标识、快照、时间和完整 JSON 详情。
// @return error 字段校验或数据库写入失败时的错误。
func (s *Store) RecordVerification(ctx context.Context, verification Verification) error {
	if err := validateVerification(verification); err != nil {
		return err
	}
	startedAt, err := requiredUnixMilli(verification.StartedAt, "verification.started_at")
	if err != nil {
		return err
	}
	finishedAt, err := requiredUnixMilli(verification.FinishedAt, "verification.finished_at")
	if err != nil {
		return err
	}
	durationMilliseconds, err := durationMilliseconds(verification.Duration, "verification.duration")
	if err != nil {
		return err
	}

	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO verifications (
				id, host, run_id, snapshot_id, started_at, finished_at,
				duration_ms, status, error, detail_json
			) VALUES (?, ?, (SELECT id FROM runs WHERE id = ?), ?, ?, ?, ?, ?, ?, ?)`,
			verification.ID,
			verification.Host,
			verification.RunID,
			verification.SnapshotID,
			startedAt,
			finishedAt,
			durationMilliseconds,
			verification.Status,
			verification.Error,
			string(verification.DetailJSON),
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("记录演练 %q 结果失败: %w", verification.ID, err)
	}
	return nil
}

func prepareDatabaseFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("创建状态库目录 %q 失败: %w", parent, err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("创建状态库文件 %q 失败: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		chmodErr := fmt.Errorf("设置状态库文件 %q 权限失败: %w", path, err)
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(chmodErr,
				fmt.Errorf("关闭状态库文件 %q 失败: %w", path, closeErr))
		}
		return chmodErr
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭预创建的状态库文件 %q 失败: %w", path, err)
	}
	return nil
}

func dataSourceName(path string) string {
	query := url.Values{}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMilliseconds))
	query.Add("_pragma", "foreign_keys(ON)")
	return (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: query.Encode(),
	}).String()
}

func enableWAL(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := withBusyRetry(ctx, db, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode)
	}); err != nil {
		return fmt.Errorf("启用状态库 WAL 模式失败: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("启用状态库 WAL 模式失败: 期望 wal，实际 %q", mode)
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	return withConfiguredConnection(ctx, db, func(conn *sql.Conn) error {
		// 两个进程可能同时首次启动。先取得唯一写锁，再读取版本，等待者才能看到
		// 先行进程已经提交的 user_version，避免双方都按旧版本重复建表。
		if err := retryBusy(ctx, func() error {
			_, execErr := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
			return execErr
		}); err != nil {
			return fmt.Errorf("开始状态库迁移失败: %w", err)
		}

		if err := migrateLocked(ctx, conn); err != nil {
			return rollbackMigration(conn, err)
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return rollbackMigration(conn, fmt.Errorf("提交状态库迁移失败: %w", err))
		}
		return nil
	})
}

func migrateLocked(ctx context.Context, conn *sql.Conn) error {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取状态库 schema 版本失败: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("状态库 schema 版本 %d 高于当前程序支持的 %d", version, currentSchemaVersion)
	}

	for nextVersion := version + 1; nextVersion <= currentSchemaVersion; nextVersion++ {
		migration := migrations[nextVersion-1]
		if _, err := conn.ExecContext(ctx, migration); err != nil {
			return fmt.Errorf("执行状态库 schema v%d 迁移失败: %w", nextVersion, err)
		}
		if _, err := conn.ExecContext(ctx,
			fmt.Sprintf("PRAGMA user_version = %d", nextVersion)); err != nil {
			return fmt.Errorf("更新状态库 schema 版本到 %d 失败: %w", nextVersion, err)
		}
	}
	return nil
}

func rollbackMigration(conn *sql.Conn, migrationErr error) error {
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		return errors.Join(migrationErr, fmt.Errorf("回滚状态库迁移失败: %w", err))
	}
	return migrationErr
}

func withBusyRetry(ctx context.Context, db *sql.DB, operation func(*sql.Conn) error) error {
	return withConfiguredConnection(ctx, db, func(conn *sql.Conn) error {
		return retryBusy(ctx, func() error {
			return operation(conn)
		})
	})
}

func withConfiguredConnection(
	ctx context.Context,
	db *sql.DB,
	operation func(*sql.Conn) error,
) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取状态库连接失败: %w", err)
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), connectionRestoreTimeout)
		defer cancel()
		if restoreErr := setBusyTimeout(restoreCtx, conn, busyTimeoutMilliseconds); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("恢复状态库 busy timeout 失败: %w", restoreErr))
		}
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭状态库连接失败: %w", closeErr))
		}
	}()

	// modernc 的 sqlite3_busy_timeout 不会在等待期间响应 sqlite3_interrupt。
	// 临时使用短等待并由 Go 层重试，才能同时保留 5 秒总等待与即时 context 取消。
	if err := setBusyTimeout(ctx, conn, busyRetryIntervalMilliseconds); err != nil {
		return fmt.Errorf("配置状态库 context 感知的 busy timeout 失败: %w", err)
	}
	return operation(conn)
}

func setBusyTimeout(ctx context.Context, conn *sql.Conn, milliseconds int) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", milliseconds))
	return err
}

func retryBusy(ctx context.Context, operation func() error) error {
	deadline := time.Now().Add(time.Duration(busyTimeoutMilliseconds) * time.Millisecond)
	for {
		err := operation()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(err, ctxErr) {
				return err
			}
			return errors.Join(ctxErr, err)
		}
		if !isBusyError(err) || !time.Now().Before(deadline) {
			return err
		}
	}
}

func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

func validateNewRun(run Run) error {
	if strings.TrimSpace(run.ID) == "" {
		return fmt.Errorf("创建运行记录失败: run.id 不能为空")
	}
	if run.Status != StatusRunning {
		return fmt.Errorf("创建运行记录失败: run.status 期望 %q，实际 %q", StatusRunning, run.Status)
	}
	if strings.TrimSpace(run.ArkVersion) == "" {
		return fmt.Errorf("创建运行记录失败: run.ark_version 不能为空")
	}
	if !run.FinishedAt.IsZero() || run.Duration != 0 || run.Error != "" {
		return fmt.Errorf("创建运行记录失败: running 状态不能包含完成时间、耗时或错误")
	}
	return nil
}

func validateRunTarget(target RunTarget) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "run_target.run_id", value: target.RunID},
		{name: "run_target.host", value: target.Host},
		{name: "run_target.target_id", value: target.TargetID},
		{name: "run_target.target_type", value: target.TargetType},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("记录 target 结果失败: %s 不能为空", field.name)
		}
	}
	if err := validateFinalStatus(target.Status, "run_target.status"); err != nil {
		return err
	}
	if target.Bytes < 0 {
		return fmt.Errorf("记录 target 结果失败: run_target.bytes 不能为负数，实际 %d", target.Bytes)
	}
	return nil
}

func validateDoctorReport(report DoctorReport) error {
	switch report.Scope {
	case DoctorScopeLocal:
		if report.Host != "" {
			return fmt.Errorf("记录 doctor 报告失败: local 范围不能指定 host")
		}
	case DoctorScopeHost:
		if strings.TrimSpace(report.Host) == "" {
			return fmt.Errorf("记录 doctor 报告失败: host 范围必须指定 host")
		}
	default:
		return fmt.Errorf("记录 doctor 报告失败: doctor_report.scope %q 非法", report.Scope)
	}
	if err := validateFinalStatus(report.Status, "doctor_report.status"); err != nil {
		return err
	}
	if !json.Valid(report.ReportJSON) {
		return fmt.Errorf("记录 doctor 报告失败: report_json 不是合法 JSON")
	}
	return nil
}

func validateVerification(verification Verification) error {
	required := []struct {
		name  string
		value string
	}{
		{name: "verification.id", value: verification.ID},
		{name: "verification.host", value: verification.Host},
		{name: "verification.snapshot_id", value: verification.SnapshotID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("记录演练结果失败: %s 不能为空", field.name)
		}
	}
	if err := validateFinalStatus(verification.Status, "verification.status"); err != nil {
		return err
	}
	if verification.FinishedAt.Before(verification.StartedAt) {
		return fmt.Errorf("记录演练结果失败: verification.finished_at 不能早于 started_at")
	}
	if !json.Valid(verification.DetailJSON) {
		return fmt.Errorf("记录演练结果失败: detail_json 不是合法 JSON")
	}
	return nil
}

func validateFinalStatus(status Status, field string) error {
	switch status {
	case StatusOK, StatusWarn, StatusFail:
		return nil
	default:
		return fmt.Errorf("%s 必须是 %q、%q 或 %q，实际 %q",
			field, StatusOK, StatusWarn, StatusFail, status)
	}
}

func requiredUnixMilli(value time.Time, field string) (int64, error) {
	if value.IsZero() {
		return 0, fmt.Errorf("%s 不能为空", field)
	}
	milliseconds := value.UTC().UnixMilli()
	if milliseconds < 0 {
		return 0, fmt.Errorf("%s 不能早于 Unix epoch", field)
	}
	return milliseconds, nil
}

func nullableUnixMilli(value time.Time, field string) (any, error) {
	if value.IsZero() {
		return nil, nil
	}
	milliseconds, err := requiredUnixMilli(value, field)
	if err != nil {
		return nil, err
	}
	return milliseconds, nil
}

func durationMilliseconds(value time.Duration, field string) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s 不能为负数，实际 %s", field, value)
	}
	return value.Milliseconds(), nil
}

func durationFromMilliseconds(value int64) (time.Duration, error) {
	if value < 0 {
		return 0, fmt.Errorf("毫秒值不能为负数，实际 %d", value)
	}
	if value > math.MaxInt64/int64(time.Millisecond) {
		return 0, fmt.Errorf("毫秒值 %d 超出 time.Duration 范围", value)
	}
	return time.Duration(value) * time.Millisecond, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func requireOneRow(result sql.Result, notFoundMessage string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取数据库变更行数失败: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("%s", notFoundMessage)
	}
	return nil
}
