package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_ManualOperation生命周期与分页(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	for index, id := range []string{"operation-1", "operation-2", "operation-3"} {
		if err := state.CreateManualOperation(context.Background(), ManualOperation{
			ID: id, Kind: OperationKindBackup, Host: "web-01", Status: OperationStatusRunning,
			StartedAt: base.Add(time.Duration(index) * time.Minute), RequestJSON: []byte(`{}`),
		}); err != nil {
			t.Fatalf("创建手工任务 %s 失败: %v", id, err)
		}
		if id != "operation-3" {
			exitCode := 0
			if err := state.FinishManualOperation(context.Background(), id, ManualOperationResult{
				Status: OperationStatusOK, FinishedAt: base.Add(time.Duration(index)*time.Minute + time.Second),
				Duration: time.Second, ResultJSON: []byte(`{"status":"ok"}`), ExitCode: &exitCode,
			}); err != nil {
				t.Fatalf("完成手工任务 %s 失败: %v", id, err)
			}
		}
	}

	first, more, err := state.ListManualOperations(context.Background(), OperationListOptions{Limit: 2})
	if err != nil || !more || len(first) != 2 || first[0].ID != "operation-3" || first[1].ID != "operation-2" {
		t.Fatalf("第一页 = %#v more=%t err=%v", first, more, err)
	}
	second, more, err := state.ListManualOperations(context.Background(), OperationListOptions{
		BeforeAt: first[1].StartedAt, BeforeID: first[1].ID, Limit: 2,
	})
	if err != nil || more || len(second) != 1 || second[0].ID != "operation-1" {
		t.Fatalf("第二页 = %#v more=%t err=%v", second, more, err)
	}

	interrupted, err := state.InterruptRunningOperations(context.Background(), base.Add(10*time.Minute))
	if err != nil || interrupted != 1 {
		t.Fatalf("中断数量 = %d err=%v", interrupted, err)
	}
	operation, err := state.GetManualOperation(context.Background(), "operation-3")
	if err != nil || operation.Status != OperationStatusInterrupted || operation.Error != interruptedOperationError {
		t.Fatalf("中断任务 = %#v err=%v", operation, err)
	}
	if err := state.FinishManualOperation(context.Background(), "operation-3", ManualOperationResult{
		Status: OperationStatusOK, FinishedAt: base.Add(11 * time.Minute), Duration: time.Second,
	}); err == nil {
		t.Fatal("重复完成中断任务应失败")
	}
	if _, err := state.GetManualOperation(context.Background(), "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("不存在任务错误 = %v，期望保留 sql.ErrNoRows", err)
	}
}

func TestStore_InterruptRunningOperations_时钟回拨仍完成中断(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	startedAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if err := state.CreateManualOperation(context.Background(), ManualOperation{
		ID: "operation-future", Kind: OperationKindBackup, Host: "web-01",
		Status: OperationStatusRunning, StartedAt: startedAt, RequestJSON: []byte(`{}`),
	}); err != nil {
		t.Fatalf("创建未来时间任务失败: %v", err)
	}

	count, err := state.InterruptRunningOperations(context.Background(), startedAt.Add(-time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("时钟回拨中断 count=%d err=%v", count, err)
	}
	operation, err := state.GetManualOperation(context.Background(), "operation-future")
	if err != nil || operation.Status != OperationStatusInterrupted ||
		!operation.FinishedAt.Equal(startedAt) || operation.Duration != 0 {
		t.Fatalf("时钟回拨中断结果=%#v err=%v", operation, err)
	}
}

func TestStore_HostRunDoctor与Verification查询(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	createRun(t, state, Run{
		ID: "run-1", Status: StatusRunning, StartedAt: base, ArkVersion: "test",
	})
	for _, target := range []RunTarget{
		{RunID: "run-1", Host: "web-01", TargetID: "files/config", TargetType: "files", Status: StatusOK, Duration: time.Second},
		{RunID: "run-1", Host: "web-01", TargetID: "volume/data", TargetType: "volume", Status: StatusWarn, Duration: time.Second},
	} {
		if err := state.RecordRunTarget(context.Background(), target); err != nil {
			t.Fatalf("记录 target 失败: %v", err)
		}
	}
	if err := state.FinishRun(context.Background(), "run-1", RunResult{
		Status: StatusWarn, FinishedAt: base.Add(time.Minute), Duration: time.Minute,
	}); err != nil {
		t.Fatalf("完成 run 失败: %v", err)
	}
	if err := state.RecordDoctorReport(context.Background(), DoctorReport{
		Scope: DoctorScopeHost, Host: "web-01", CreatedAt: base,
		Status: StatusWarn, NextRunAt: base.Add(24 * time.Hour), ReportJSON: []byte(`{"checks":[]}`),
	}); err != nil {
		t.Fatalf("记录 doctor 失败: %v", err)
	}
	if err := state.RecordVerification(context.Background(), Verification{
		ID: "verify-1", Host: "web-01", RunID: "run-1", SnapshotID: "snapshot-1",
		StartedAt: base, FinishedAt: base.Add(time.Second), Duration: time.Second,
		Status: StatusOK, DetailJSON: []byte(`{"status":"ok"}`),
	}); err != nil {
		t.Fatalf("记录 verification 失败: %v", err)
	}

	runs, err := state.ListHostRuns(context.Background(), "web-01", 10)
	if err != nil || len(runs) != 1 || runs[0].Status != StatusWarn || len(runs[0].Targets) != 2 {
		t.Fatalf("host runs = %#v err=%v", runs, err)
	}
	report, found, err := state.LatestDoctorReport(context.Background(), DoctorScopeHost, "web-01")
	if err != nil || !found || report.Status != StatusWarn || report.NextRunAt.IsZero() {
		t.Fatalf("doctor = %#v found=%t err=%v", report, found, err)
	}
	verifications, err := state.ListVerifications(context.Background(), "web-01", 10)
	if err != nil || len(verifications) != 1 || verifications[0].ID != "verify-1" {
		t.Fatalf("verifications = %#v err=%v", verifications, err)
	}
}

func TestStore_LatestDoctorReport回退最后已知NextRun(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	lastKnown := base.Add(24 * time.Hour)
	for _, report := range []DoctorReport{
		{
			Scope: DoctorScopeHost, Host: "web-01", CreatedAt: base,
			Status: StatusOK, NextRunAt: lastKnown, ReportJSON: []byte(`{"checks":[{"name":"old"}]}`),
		},
		{
			Scope: DoctorScopeHost, Host: "web-01", CreatedAt: base.Add(time.Hour),
			Status: StatusWarn, ReportJSON: []byte(`{"checks":[{"name":"latest"}]}`),
		},
	} {
		if err := state.RecordDoctorReport(context.Background(), report); err != nil {
			t.Fatalf("记录 doctor 报告失败: %v", err)
		}
	}

	report, found, err := state.LatestDoctorReport(context.Background(), DoctorScopeHost, "web-01")
	if err != nil || !found || report.Status != StatusWarn || !report.CreatedAt.Equal(base.Add(time.Hour)) ||
		!report.NextRunAt.Equal(lastKnown) || !strings.Contains(string(report.ReportJSON), "latest") {
		t.Fatalf("latest doctor=%#v found=%t err=%v", report, found, err)
	}
}
