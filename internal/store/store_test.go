package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpen_CreatesDatabaseAndSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state dir", "ark.db")
	first := openTestStore(t, path)
	second := openTestStore(t, path)

	for name, wantMode := range map[string]os.FileMode{
		filepath.Dir(path): 0o700,
		path:               0o600,
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("读取 %s 状态失败: %v", name, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Errorf("%s 权限 = %#o，期望 %#o", name, got, wantMode)
		}
	}

	if got := queryInt(t, first.db, "PRAGMA user_version"); got != currentSchemaVersion {
		t.Errorf("user_version = %d，期望 %d", got, currentSchemaVersion)
	}
	assertSchemaObjects(t, first.db)
	assertSchemaObjects(t, second.db)
}

func TestOpen_AppliesPragmasToEveryConnection(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	store.db.SetMaxOpenConns(4)

	connections := make([]*sql.Conn, 0, 4)
	for range 4 {
		conn, err := store.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("获取独立连接失败: %v", err)
		}
		connections = append(connections, conn)
	}
	t.Cleanup(func() {
		for _, conn := range connections {
			if err := conn.Close(); err != nil {
				t.Errorf("关闭测试连接失败: %v", err)
			}
		}
	})

	for i, conn := range connections {
		var foreignKeys int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("读取连接 %d 的 foreign_keys 失败: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("连接 %d 的 foreign_keys = %d，期望 1", i, foreignKeys)
		}

		var busyTimeout int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("读取连接 %d 的 busy_timeout 失败: %v", i, err)
		}
		if busyTimeout != busyTimeoutMilliseconds {
			t.Errorf("连接 %d 的 busy_timeout = %d，期望 %d", i, busyTimeout, busyTimeoutMilliseconds)
		}

		var journalMode string
		if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("读取连接 %d 的 journal_mode 失败: %v", i, err)
		}
		if !strings.EqualFold(journalMode, "wal") {
			t.Errorf("连接 %d 的 journal_mode = %q，期望 wal", i, journalMode)
		}
	}
}

func TestOpen_RejectsInvalidPathAndNewerSchema(t *testing.T) {
	if _, err := Open(context.Background(), "  "); err == nil || !strings.Contains(err.Error(), "路径不能为空") {
		t.Fatalf("空路径错误 = %v，期望包含路径不能为空", err)
	}

	path := filepath.Join(t.TempDir(), "ark.db")
	store := openTestStore(t, path)
	if _, err := store.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion+1)); err != nil {
		t.Fatalf("设置较新 schema 版本失败: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("关闭预置状态库失败: %v", err)
	}

	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("期望拒绝较新的 schema，实际成功")
	}
	if !strings.Contains(err.Error(), "高于当前程序支持") {
		t.Errorf("错误信息 %q 中未包含版本过新说明", err.Error())
	}
}

func TestOpen_MigrationFailureRollsBackPartialSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	raw := prepareV1Database(t, path)
	if _, err := raw.Exec("CREATE TABLE manual_operations (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("创建冲突表失败: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭预置数据库失败: %v", err)
	}

	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("期望迁移因冲突表失败，实际成功")
	}
	if !strings.Contains(err.Error(), "schema v2") {
		t.Errorf("迁移错误 %q 中未包含 schema 版本", err.Error())
	}

	raw, err = sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("重新打开失败数据库失败: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("关闭失败数据库连接失败: %v", err)
		}
	}()
	if got := queryInt(t, raw, "PRAGMA user_version"); got != 1 {
		t.Errorf("失败迁移后的 user_version = %d，期望 1", got)
	}
	wantTables := []string{"doctor_reports", "manual_operations", "run_targets", "runs", "verifications"}
	if got := schemaObjectNames(t, raw, "table"); !equalStrings(got, wantTables) {
		t.Errorf("失败迁移后的表 = %#v，期望仅保留 %#v", got, wantTables)
	}
}

func TestOpen_MigratesV1ToV3并保留数据(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	raw := prepareV1Database(t, path)
	startedAt := time.Date(2026, 8, 17, 4, 17, 0, 0, time.UTC)
	if _, err := raw.Exec(`
		INSERT INTO runs (id, status, started_at, ark_version, error)
		VALUES (?, ?, ?, ?, '')`, "run-v1", StatusRunning, startedAt.UnixMilli(), "v1"); err != nil {
		t.Fatalf("写入 v1 数据失败: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭 v1 数据库失败: %v", err)
	}

	state := openTestStore(t, path)
	if got := queryInt(t, state.db, "PRAGMA user_version"); got != currentSchemaVersion {
		t.Fatalf("迁移后 user_version=%d，期望 %d", got, currentSchemaVersion)
	}
	run, err := state.GetRun(context.Background(), "run-v1")
	if err != nil || run.ID != "run-v1" || run.Status != StatusRunning {
		t.Fatalf("迁移后 v1 数据=%#v err=%v", run, err)
	}
	assertSchemaObjects(t, state.db)
}

func TestOpen_MigratesV2ToV3并保留数据(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	raw := prepareV2Database(t, path)
	if _, err := raw.Exec(`
		INSERT INTO manual_operations (
			id, kind, host, status, started_at, request_json, error
		) VALUES (?, ?, ?, ?, ?, '{}', '')`,
		"operation-v2", OperationKindBackup, "web-01", OperationStatusRunning,
		time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC).UnixMilli(),
	); err != nil {
		t.Fatalf("写入 v2 数据失败: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭 v2 数据库失败: %v", err)
	}

	state := openTestStore(t, path)
	operation, err := state.GetManualOperation(context.Background(), "operation-v2")
	if err != nil || operation.Status != OperationStatusRunning {
		t.Fatalf("迁移后 v2 手工任务=%#v err=%v", operation, err)
	}
	assertSchemaObjects(t, state.db)
}

func TestOpen_V3MigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	raw := prepareV2Database(t, path)
	if _, err := raw.Exec("CREATE TABLE alert_states (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("创建冲突表失败: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭 v2 数据库失败: %v", err)
	}

	_, err := Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "schema v3") {
		t.Fatalf("v3 迁移错误 = %v", err)
	}
	raw, err = sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("重新打开失败数据库失败: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("关闭失败数据库连接失败: %v", err)
		}
	}()
	if got := queryInt(t, raw, "PRAGMA user_version"); got != 2 {
		t.Fatalf("失败迁移后的 user_version = %d，期望 2", got)
	}
	if got := queryInt(t, raw, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_alert_states_active'`); got != 0 {
		t.Fatalf("失败迁移不应遗留 v3 索引，实际 %d", got)
	}
}

func TestStore_RunAndTargetLifecycle(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 11, 4, 17, 0, 0, time.FixedZone("CST", 8*60*60))

	runs := []struct {
		id      string
		started time.Time
		status  Status
		bytes   int64
	}{
		{id: "run-1", started: base, status: StatusOK, bytes: 100},
		{id: "run-2", started: base.Add(time.Hour), status: StatusFail, bytes: 10},
		{id: "run-3", started: base.Add(2 * time.Hour), status: StatusOK, bytes: 200},
	}
	for _, item := range runs {
		createRun(t, store, Run{
			ID:            item.id,
			RequestedHost: "web-01",
			Status:        StatusRunning,
			StartedAt:     item.started,
			ArkVersion:    "v0.2.0",
		})
		if err := store.RecordRunTarget(context.Background(), RunTarget{
			RunID:      item.id,
			Host:       "web-01",
			TargetID:   "postgres/app/main",
			TargetType: "postgres",
			Status:     item.status,
			Bytes:      item.bytes,
			Duration:   1500 * time.Millisecond,
			SnapshotID: "snapshot-" + item.id,
		}); err != nil {
			t.Fatalf("记录 %s target 失败: %v", item.id, err)
		}
	}

	bytes, found, err := store.LastSuccessfulTargetBytes(
		context.Background(), "web-01", "postgres/app/main")
	if err != nil {
		t.Fatalf("查询最近成功 bytes 失败: %v", err)
	}
	if !found || bytes != 200 {
		t.Errorf("最近成功结果 = (%d, %t)，期望 (200, true)", bytes, found)
	}
	bytes, found, err = store.LastSuccessfulTargetBytes(
		context.Background(), "web-01", "files/missing")
	if err != nil {
		t.Fatalf("查询无历史 target 失败: %v", err)
	}
	if found || bytes != 0 {
		t.Errorf("无历史结果 = (%d, %t)，期望 (0, false)", bytes, found)
	}

	if err := store.FinishRun(context.Background(), "run-3", RunResult{
		Status:     StatusWarn,
		FinishedAt: base.Add(2*time.Hour + 3*time.Second),
		Duration:   3 * time.Second,
		Error:      "快照体积低于历史值",
	}); err != nil {
		t.Fatalf("完成运行记录失败: %v", err)
	}
	got, err := store.GetRun(context.Background(), "run-3")
	if err != nil {
		t.Fatalf("查询运行记录失败: %v", err)
	}
	if got.ID != "run-3" || got.RequestedHost != "web-01" || got.Status != StatusWarn ||
		got.Duration != 3*time.Second || got.ArkVersion != "v0.2.0" ||
		got.Error != "快照体积低于历史值" {
		t.Errorf("运行记录不符合预期: %#v", got)
	}
	if !got.StartedAt.Equal(base.Add(2 * time.Hour).UTC()) {
		t.Errorf("started_at = %s，期望 %s", got.StartedAt, base.Add(2*time.Hour).UTC())
	}
	if !got.FinishedAt.Equal(base.Add(2*time.Hour + 3*time.Second).UTC()) {
		t.Errorf("finished_at = %s，期望 %s", got.FinishedAt, base.Add(2*time.Hour+3*time.Second).UTC())
	}

	_, err = store.GetRun(context.Background(), "missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("查询不存在记录错误 = %v，期望保留 sql.ErrNoRows", err)
	}
}

func TestStore_RecordsDoctorAndVerification(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	startedAt := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)
	createRun(t, store, Run{
		ID:         "run-1",
		Status:     StatusRunning,
		StartedAt:  startedAt,
		ArkVersion: "v0.2.0",
	})

	if err := store.RecordDoctorReport(context.Background(), DoctorReport{
		Scope:      DoctorScopeHost,
		Host:       "db-01",
		CreatedAt:  startedAt,
		Status:     StatusWarn,
		NextRunAt:  startedAt.Add(24 * time.Hour),
		ReportJSON: []byte(`{"checks":[{"name":"docker","status":"warn"}]}`),
	}); err != nil {
		t.Fatalf("记录 doctor 报告失败: %v", err)
	}
	if err := store.RecordVerification(context.Background(), Verification{
		ID:         "verify-1",
		Host:       "db-01",
		RunID:      "run-1",
		SnapshotID: "snapshot-1",
		StartedAt:  startedAt,
		FinishedAt: startedAt.Add(2 * time.Minute),
		Duration:   2 * time.Minute,
		Status:     StatusOK,
		DetailJSON: []byte(`{"database":"restored"}`),
	}); err != nil {
		t.Fatalf("记录 verification 失败: %v", err)
	}

	if got := queryInt(t, store.db, "SELECT COUNT(*) FROM doctor_reports"); got != 1 {
		t.Errorf("doctor_reports 数量 = %d，期望 1", got)
	}
	if got := queryInt(t, store.db, "SELECT COUNT(*) FROM verifications"); got != 1 {
		t.Errorf("verifications 数量 = %d，期望 1", got)
	}

	if _, err := store.db.Exec("DELETE FROM runs WHERE id = 'run-1'"); err != nil {
		t.Fatalf("删除 run 失败: %v", err)
	}
	if got := queryInt(t, store.db, "SELECT COUNT(*) FROM verifications WHERE run_id IS NOT NULL"); got != 0 {
		t.Errorf("run 删除后仍有 %d 条 verification 保留 run_id", got)
	}
}

func TestStore_RecordVerification允许历史Run缺失(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	startedAt := time.Date(2026, 8, 13, 4, 17, 0, 0, time.UTC)
	detail := `{"run_id":"historical-run"}`
	if err := state.RecordVerification(context.Background(), Verification{
		ID: "verify-history", Host: "db-01", RunID: "historical-run", SnapshotID: "manifest-1",
		StartedAt: startedAt, FinishedAt: startedAt.Add(time.Minute), Duration: time.Minute,
		Status: StatusOK, DetailJSON: []byte(detail),
	}); err != nil {
		t.Fatalf("记录历史 verification 失败: %v", err)
	}
	var runID sql.NullString
	var gotDetail string
	if err := state.db.QueryRow(
		"SELECT run_id, detail_json FROM verifications WHERE id = 'verify-history'",
	).Scan(&runID, &gotDetail); err != nil {
		t.Fatalf("查询历史 verification 失败: %v", err)
	}
	if runID.Valid || gotDetail != detail {
		t.Fatalf("run_id=%#v detail=%q", runID, gotDetail)
	}
}

func TestStore_ForeignKeyCascadeAndDatabaseChecks(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	now := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)
	createRun(t, store, Run{ID: "run-1", Status: StatusRunning, StartedAt: now, ArkVersion: "dev"})
	if err := store.RecordRunTarget(context.Background(), RunTarget{
		RunID:      "run-1",
		Host:       "hub",
		TargetID:   "files/etc-ark",
		TargetType: "files",
		Status:     StatusOK,
		Bytes:      42,
		Duration:   time.Second,
	}); err != nil {
		t.Fatalf("记录 target 失败: %v", err)
	}
	if _, err := store.db.Exec("DELETE FROM runs WHERE id = 'run-1'"); err != nil {
		t.Fatalf("删除 run 失败: %v", err)
	}
	if got := queryInt(t, store.db, "SELECT COUNT(*) FROM run_targets"); got != 0 {
		t.Errorf("级联删除后仍有 %d 条 target", got)
	}

	_, err := store.db.Exec(`
		INSERT INTO doctor_reports (scope, host, created_at, status, report_json)
		VALUES ('local', NULL, 1, 'ok', 'not-json')`)
	if err == nil {
		t.Error("数据库 CHECK 未拒绝非法 JSON")
	}
	_, err = store.db.Exec(`
		INSERT INTO run_targets (
			run_id, host, target_id, target_type, status, bytes, duration_ms
		) VALUES ('missing', 'hub', 'files/etc-ark', 'files', 'ok', 1, 1)`)
	if err == nil {
		t.Error("foreign key 未拒绝不存在的 run")
	}
}

func TestStore_ValidationErrors(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	now := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)

	tests := []struct {
		name    string
		run     func() error
		wantSub string
	}{
		{name: "run ID 为空", run: func() error {
			return store.CreateRun(context.Background(), Run{Status: StatusRunning, StartedAt: now, ArkVersion: "dev"})
		}, wantSub: "run.id"},
		{name: "run 状态不是 running", run: func() error {
			return store.CreateRun(context.Background(), Run{ID: "run", Status: StatusOK, StartedAt: now, ArkVersion: "dev"})
		}, wantSub: "期望 \"running\""},
		{name: "run 版本为空", run: func() error {
			return store.CreateRun(context.Background(), Run{ID: "run", Status: StatusRunning, StartedAt: now})
		}, wantSub: "ark_version"},
		{name: "run 提前包含完成字段", run: func() error {
			return store.CreateRun(context.Background(), Run{
				ID: "run", Status: StatusRunning, StartedAt: now, FinishedAt: now, ArkVersion: "dev",
			})
		}, wantSub: "不能包含完成时间"},
		{name: "完成状态非法", run: func() error {
			return store.FinishRun(context.Background(), "run", RunResult{Status: StatusRunning, FinishedAt: now})
		}, wantSub: "必须是"},
		{name: "完成耗时为负", run: func() error {
			return store.FinishRun(context.Background(), "run", RunResult{
				Status: StatusOK, FinishedAt: now, Duration: -time.Second,
			})
		}, wantSub: "不能为负数"},
		{name: "target host 为空", run: func() error {
			return store.RecordRunTarget(context.Background(), RunTarget{
				RunID: "run", TargetID: "files/etc", TargetType: "files", Status: StatusOK,
			})
		}, wantSub: "run_target.host"},
		{name: "target bytes 为负", run: func() error {
			return store.RecordRunTarget(context.Background(), RunTarget{
				RunID: "run", Host: "hub", TargetID: "files/etc", TargetType: "files",
				Status: StatusOK, Bytes: -1,
			})
		}, wantSub: "bytes"},
		{name: "doctor local 带 host", run: func() error {
			return store.RecordDoctorReport(context.Background(), DoctorReport{
				Scope: DoctorScopeLocal, Host: "hub", CreatedAt: now, Status: StatusOK,
				ReportJSON: []byte(`{}`),
			})
		}, wantSub: "不能指定 host"},
		{name: "doctor JSON 非法", run: func() error {
			return store.RecordDoctorReport(context.Background(), DoctorReport{
				Scope: DoctorScopeLocal, CreatedAt: now, Status: StatusOK,
				ReportJSON: []byte(`{`),
			})
		}, wantSub: "不是合法 JSON"},
		{name: "verification 快照为空", run: func() error {
			return store.RecordVerification(context.Background(), Verification{
				ID: "verify", Host: "hub", StartedAt: now, FinishedAt: now,
				Status: StatusOK, DetailJSON: []byte(`{}`),
			})
		}, wantSub: "snapshot_id"},
		{name: "verification 结束早于开始", run: func() error {
			return store.RecordVerification(context.Background(), Verification{
				ID: "verify", Host: "hub", SnapshotID: "snapshot", StartedAt: now,
				FinishedAt: now.Add(-time.Second), Status: StatusOK, DetailJSON: []byte(`{}`),
			})
		}, wantSub: "不能早于"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 中未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}

	err := store.FinishRun(context.Background(), "missing", RunResult{
		Status: StatusOK, FinishedAt: now, Duration: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("完成不存在 run 的错误 = %v，期望包含不存在", err)
	}
}

func TestStore_WALAllowsReadDuringWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	writer := openTestStore(t, path)
	reader := openTestStore(t, path)
	now := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)
	createRun(t, writer, Run{ID: "run-1", Status: StatusRunning, StartedAt: now, ArkVersion: "dev"})

	conn, err := writer.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("获取 writer 连接失败: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("关闭 writer 连接失败: %v", err)
		}
	}()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("开始写事务失败: %v", err)
	}
	defer rollbackTestTransaction(t, conn)
	if _, err := conn.ExecContext(context.Background(),
		"UPDATE runs SET status = 'fail' WHERE id = 'run-1'"); err != nil {
		t.Fatalf("写入未提交状态失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := reader.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("写事务期间读取已提交数据失败: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("reader 看到了未提交状态 %q，期望 %q", got.Status, StatusRunning)
	}
}

func TestStore_ExportSnapshot_并发WAL写入时保持一致且自动清理(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	source := openTestStore(t, path)
	writer := openTestStore(t, path)
	now := time.Date(2026, 8, 12, 4, 17, 0, 0, time.UTC)
	createRun(t, source, Run{
		ID: "committed-run", Status: StatusRunning, StartedAt: now, ArkVersion: "dev",
	})
	if _, err := source.db.Exec(`
		CREATE TABLE export_payload (id INTEGER PRIMARY KEY, content TEXT NOT NULL);
		WITH RECURSIVE sequence(value) AS (
			SELECT 1
			UNION ALL
			SELECT value + 1 FROM sequence WHERE value < 1024
		)
		INSERT INTO export_payload (content)
		SELECT printf('%.*c', 4096, 'x') FROM sequence;
	`); err != nil {
		t.Fatalf("准备导出数据失败: %v", err)
	}

	stop := make(chan struct{})
	started := make(chan struct{})
	writerDone := make(chan struct{})
	writerErrors := make(chan error, 1)
	go func() {
		defer close(writerDone)
		first := true
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := writer.db.Exec(
				"UPDATE runs SET error = ? WHERE id = 'committed-run'", fmt.Sprintf("write-%d", index),
			); err != nil {
				writerErrors <- err
				return
			}
			if first {
				close(started)
				first = false
			}
		}
	}()
	<-started

	exportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	reader, err := source.ExportSnapshot(exportCtx)
	cancel()
	close(stop)
	<-writerDone
	if err != nil {
		t.Fatalf("在线导出状态库失败: %v", err)
	}
	select {
	case err := <-writerErrors:
		t.Fatalf("并发 writer 失败: %v", err)
	default:
	}

	exported, ok := reader.(*exportReadCloser)
	if !ok {
		t.Fatalf("导出 reader 类型 = %T，期望 *exportReadCloser", reader)
	}
	directoryInfo, err := os.Stat(exported.directory)
	if err != nil {
		t.Fatalf("读取导出目录权限失败: %v", err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("导出目录权限 = %o，期望 700", got)
	}
	fileInfo, err := exported.file.Stat()
	if err != nil {
		t.Fatalf("读取导出文件权限失败: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("导出文件权限 = %o，期望 600", got)
	}

	copyDB, err := sql.Open("sqlite", readOnlyDataSourceName(exported.file.Name()))
	if err != nil {
		t.Fatalf("独立打开导出数据库失败: %v", err)
	}
	var integrity string
	if err := copyDB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("执行 integrity_check 失败: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q，期望 ok", integrity)
	}
	var runCount int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM runs WHERE id = 'committed-run'").Scan(&runCount); err != nil {
		t.Fatalf("查询导出数据库已提交数据失败: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("导出数据库已提交 run 数量 = %d，期望 1", runCount)
	}
	if err := copyDB.Close(); err != nil {
		t.Fatalf("关闭独立导出数据库失败: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(exported.file.Name() + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("导出数据库不应依赖 %s 文件: %v", suffix, err)
		}
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("读取导出数据流失败: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("关闭导出数据流失败: %v", err)
	}
	if _, err := os.Stat(exported.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("关闭后导出目录仍存在: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("重复关闭导出数据流失败: %v", err)
	}
}

func TestStore_ExportSnapshot_取消和导出失败不残留临时文件(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "ark.db")
	state, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开状态库失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := state.exportSnapshot(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消导出错误 = %v，期望保留 context.Canceled", err)
	}
	assertDirectoryEmpty(t, root)

	if err := state.Close(); err != nil {
		t.Fatalf("关闭状态库失败: %v", err)
	}
	if _, err := state.exportSnapshot(context.Background(), root); err == nil {
		t.Fatal("关闭状态库后导出应失败")
	}
	assertDirectoryEmpty(t, root)
}

func TestStore_ContextCancellationStopsBusyWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	locker := openTestStore(t, path)
	contender := openTestStore(t, path)

	conn, err := locker.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("获取锁连接失败: %v", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("关闭锁连接失败: %v", err)
		}
	}()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("取得写锁失败: %v", err)
	}
	transactionActive := true
	defer func() {
		if transactionActive {
			rollbackTestTransaction(t, conn)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	err = contender.CreateRun(ctx, Run{
		ID: "blocked", Status: StatusRunning, StartedAt: time.Now(), ArkVersion: "dev",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("锁等待错误 = %v，期望保留 context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("context 取消后仍等待了 %s，期望 1 秒内返回", elapsed)
	}

	rollbackTestTransaction(t, conn)
	transactionActive = false
	if err := contender.CreateRun(context.Background(), Run{
		ID: "after-cancel", Status: StatusRunning, StartedAt: time.Now(), ArkVersion: "dev",
	}); err != nil {
		t.Fatalf("context 取消后复用连接写入失败: %v", err)
	}
	if got := queryInt(t, contender.db, "PRAGMA busy_timeout"); got != busyTimeoutMilliseconds {
		t.Errorf("操作完成后的 busy_timeout = %d，期望 %d", got, busyTimeoutMilliseconds)
	}
}

func TestOpen_ConcurrentFirstMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ark.db")
	const workers = 8
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup

	for i := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			<-start
			store, err := Open(context.Background(), path)
			if err != nil {
				errorsByWorker <- fmt.Errorf("worker %d 打开失败: %w", worker, err)
				return
			}
			if err := store.Close(); err != nil {
				errorsByWorker <- fmt.Errorf("worker %d 关闭失败: %w", worker, err)
			}
		}(i)
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Error(err)
	}

	store := openTestStore(t, path)
	if got := queryInt(t, store.db, "PRAGMA user_version"); got != currentSchemaVersion {
		t.Errorf("并发迁移后的 user_version = %d，期望 %d", got, currentSchemaVersion)
	}
	assertSchemaObjects(t, store.db)
}

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("打开测试状态库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("关闭测试状态库失败: %v", err)
		}
	})
	return store
}

func prepareV1Database(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := prepareDatabaseFile(path); err != nil {
		t.Fatalf("预创建数据库失败: %v", err)
	}
	raw, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		t.Fatalf("打开预置数据库失败: %v", err)
	}
	if _, err := raw.Exec(schemaV1); err != nil {
		_ = raw.Close()
		t.Fatalf("创建 schema v1 失败: %v", err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		_ = raw.Close()
		t.Fatalf("设置 schema v1 版本失败: %v", err)
	}
	return raw
}

func prepareV2Database(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw := prepareV1Database(t, path)
	if _, err := raw.Exec(schemaV2); err != nil {
		_ = raw.Close()
		t.Fatalf("创建 schema v2 失败: %v", err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 2"); err != nil {
		_ = raw.Close()
		t.Fatalf("设置 schema v2 版本失败: %v", err)
	}
	return raw
}

func createRun(t *testing.T, store *Store, run Run) {
	t.Helper()
	if err := store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("创建运行记录 %q 失败: %v", run.ID, err)
	}
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("读取目录 %s 失败: %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("目录 %s 仍有残留: %#v", path, entries)
	}
}

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var value int
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("执行查询 %q 失败: %v", query, err)
	}
	return value
}

func rollbackTestTransaction(t *testing.T, conn *sql.Conn) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Errorf("回滚测试事务失败: %v", err)
	}
}

func assertSchemaObjects(t *testing.T, db *sql.DB) {
	t.Helper()
	wantTables := []string{"alert_states", "doctor_reports", "manual_operations", "run_targets", "runs", "verifications"}
	wantIndexes := []string{
		"idx_alert_states_active",
		"idx_doctor_reports_scope_host_created",
		"idx_manual_operations_host_started",
		"idx_manual_operations_started",
		"idx_manual_operations_status_started",
		"idx_run_targets_lookup",
		"idx_runs_started_at",
		"idx_runs_status_started_at",
		"idx_verifications_host_started",
	}
	if got := schemaObjectNames(t, db, "table"); !equalStrings(got, wantTables) {
		t.Errorf("schema 表 = %#v，期望 %#v", got, wantTables)
	}
	if got := schemaObjectNames(t, db, "index"); !equalStrings(got, wantIndexes) {
		t.Errorf("schema 索引 = %#v，期望 %#v", got, wantIndexes)
	}
}

func schemaObjectNames(t *testing.T, db *sql.DB, objectType string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT name
		FROM sqlite_master
		WHERE type = ? AND name NOT LIKE 'sqlite_%'
		ORDER BY name`, objectType)
	if err != nil {
		t.Fatalf("查询 schema %s 失败: %v", objectType, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("关闭 schema 查询结果失败: %v", err)
		}
	}()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读取 schema %s 名称失败: %v", objectType, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 schema %s 失败: %v", objectType, err)
	}
	sort.Strings(names)
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
