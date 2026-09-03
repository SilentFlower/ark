package verify

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

func testVerifyPlan() restore.Plan {
	return restore.Plan{
		ManifestSnapshotID: "manifest-snapshot",
		RunID:              "run-p3",
		SourceHost:         "host-01",
		DestinationHost:    "host-01",
		Project: restore.Project{
			Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod",
		},
		Steps: []restore.Step{{
			Phase: restore.PhaseFiles, TargetID: "files-app", TargetType: "files", SnapshotID: "target-snapshot",
		}},
	}
}

func TestExecute_Restore返回Fail状态时仍失败并清理(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	cleanupCalled := false
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		return restore.Result{Status: store.StatusFail}, nil
	}
	dependencies.cleanup = func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error) {
		cleanupCalled = true
		return restore.CleanupResult{Status: store.StatusOK}, nil
	}
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
	if err == nil || result.Status != store.StatusFail || !cleanupCalled || recorded.Status != store.StatusFail {
		t.Fatalf("result=%#v err=%v cleanup=%v recorded=%#v", result, err, cleanupCalled, *recorded)
	}
}

func TestExecute_Restore安全摘要传播到Verify结果(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	sensitiveErr := errors.New("底层包含敏感 stderr")
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		return restore.Result{
			Status: store.StatusFail,
			Error:  "隔离 Compose external volume 无法隔离",
		}, sensitiveErr
	}
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
	want := "隔离恢复未完成：隔离 Compose external volume 无法隔离"
	if !errors.Is(err, sensitiveErr) || result.Status != store.StatusFail || result.Error != want || recorded.Error != want {
		t.Fatalf("result=%#v err=%v recorded=%#v", result, err, *recorded)
	}
	if strings.Contains(result.Error, "敏感 stderr") {
		t.Fatalf("verify 摘要泄漏底层错误: %q", result.Error)
	}
}

func TestExecute_归属校验失败会回退清理(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	cleanupCalled := false
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		return restore.Result{Status: store.StatusFail}, errors.New("restore failed")
	}
	dependencies.validateOwnership = func(context.Context, sshexec.Runner, string, string) (restore.IsolationOwnership, error) {
		return restore.IsolationOwnership{}, errors.New("ownership failed")
	}
	dependencies.cleanup = func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error) {
		cleanupCalled = true
		return restore.CleanupResult{Status: store.StatusOK}, nil
	}
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{KeepOnFailure: true}, dependencies)
	if err == nil || result.Status != store.StatusFail || !cleanupCalled || result.KeptOnFailure ||
		result.Cleanup == nil || recorded.Status != store.StatusFail {
		t.Fatalf("result=%#v err=%v cleanup=%v recorded=%#v", result, err, cleanupCalled, *recorded)
	}
}

func TestExecute_Store失败会返回最终Fail(t *testing.T) {
	dependencies, _ := testVerifyDependencies(t)
	dependencies.record = func(context.Context, store.Verification) error { return errors.New("store failed") }
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
	if err == nil || result.Status != store.StatusFail || result.Error != "记录演练结果失败" ||
		!strings.Contains(err.Error(), "store failed") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestExecute_恢复和Store失败会保留两个失败阶段(t *testing.T) {
	dependencies, _ := testVerifyDependencies(t)
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		return restore.Result{Status: store.StatusFail}, errors.New("restore failed")
	}
	dependencies.record = func(context.Context, store.Verification) error { return errors.New("store failed") }
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
	if err == nil || result.Status != store.StatusFail ||
		result.Error != "隔离恢复未完成；记录演练结果失败" ||
		!strings.Contains(err.Error(), "restore failed") || !strings.Contains(err.Error(), "store failed") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRecordFailure_持久化TargetEvidence(t *testing.T) {
	startedAt := time.Date(2026, 8, 13, 2, 0, 0, 0, time.UTC)
	result := Result{
		ID: "verify-preflight", Host: "host-01", RunID: "run-p3", ManifestSnapshotID: "manifest-snapshot",
		StartedAt: startedAt, FinishedAt: startedAt, Status: store.StatusFail,
		Targets:  []TargetEvidence{{TargetID: "files-app", TargetType: "files", SnapshotID: "target-snapshot"}},
		Baseline: BaselineEvidence{Differences: []string{}}, Error: "本地环境检查未通过",
	}
	detail, err := verificationDetailJSON(result)
	if err != nil {
		t.Fatalf("编码 detail 失败: %v", err)
	}
	var decoded struct {
		Targets []TargetEvidence `json:"targets"`
	}
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("解码 detail 失败: %v", err)
	}
	if !reflect.DeepEqual(decoded.Targets, result.Targets) {
		t.Fatalf("targets=%#v want=%#v", decoded.Targets, result.Targets)
	}
}

func testVerifyDependencies(t *testing.T) (executeDependencies, *store.Verification) {
	t.Helper()
	times := []time.Time{
		time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 13, 1, 2, 0, 0, time.UTC),
	}
	timeIndex := 0
	recorded := &store.Verification{}
	return executeDependencies{
		now: func() time.Time {
			value := times[timeIndex]
			if timeIndex < len(times)-1 {
				timeIndex++
			}
			return value
		},
		newID: func(time.Time) (string, error) { return "verify-test", nil },
		captureBaseline: func(context.Context, sshexec.Runner, restore.Plan) (Baseline, error) {
			return Baseline{Fingerprint: "same"}, nil
		},
		isolate: func(plan restore.Plan, options restore.IsolationOptions) (restore.Plan, error) {
			if options.Purpose != restore.IsolationPurposeVerify || options.PortAllocation != restore.IsolationPortDisabled {
				t.Fatalf("isolation options=%#v", options)
			}
			plan.Isolation = &restore.IsolationSpec{ID: strings.Repeat("a", 64), ProjectName: "app-verify"}
			return plan, nil
		},
		executeRestore: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusOK}, nil
		},
		cleanup: func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error) {
			return restore.CleanupResult{Status: store.StatusOK}, nil
		},
		validateOwnership: func(context.Context, sshexec.Runner, string, string) (restore.IsolationOwnership, error) {
			return restore.IsolationOwnership{}, errors.New("测试不应保留")
		},
		record: func(_ context.Context, verification store.Verification) error {
			*recorded = verification
			return nil
		},
	}, recorded
}

func TestExecute_成功后清理复核并记录(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
	if err != nil {
		t.Fatalf("execute 失败: %v", err)
	}
	if result.Status != store.StatusOK || result.Cleanup == nil || result.Baseline.BeforeFingerprint != "same" ||
		result.Baseline.AfterFingerprint != "same" || recorded.Status != store.StatusOK || len(recorded.DetailJSON) == 0 {
		t.Fatalf("result=%#v recorded=%#v", result, *recorded)
	}
}

func TestExecute_Restore失败仅保留已证明归属资源(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	cleanupCalled := false
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		return restore.Result{Status: store.StatusFail, Error: "恢复失败"}, errors.New("restore failed")
	}
	dependencies.validateOwnership = func(context.Context, sshexec.Runner, string, string) (restore.IsolationOwnership, error) {
		return restore.IsolationOwnership{ProjectName: "app-verify", CleanupCommand: "ark restore cleanup"}, nil
	}
	dependencies.cleanup = func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error) {
		cleanupCalled = true
		return restore.CleanupResult{}, nil
	}
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{KeepOnFailure: true}, dependencies)
	if err == nil || result.Status != store.StatusFail || !result.KeptOnFailure || result.KeptOwnership == nil ||
		cleanupCalled || recorded.Status != store.StatusFail {
		t.Fatalf("result=%#v err=%v cleanup=%v recorded=%#v", result, err, cleanupCalled, *recorded)
	}
}

func TestExecute_Cleanup或基线变化都记录失败(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*executeDependencies)
		wantError string
	}{
		{
			name: "cleanup",
			configure: func(dependencies *executeDependencies) {
				dependencies.cleanup = func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error) {
					return restore.CleanupResult{Status: store.StatusFail}, errors.New("cleanup failed")
				}
			},
			wantError: "清理演练资源失败",
		},
		{
			name: "baseline",
			configure: func(dependencies *executeDependencies) {
				calls := 0
				dependencies.captureBaseline = func(context.Context, sshexec.Runner, restore.Plan) (Baseline, error) {
					calls++
					return Baseline{Fingerprint: string(rune('a' + calls)), Containers: []ContainerBaseline{{ID: string(rune('a' + calls))}}}, nil
				}
			},
			wantError: "生产资源基线发生变化",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dependencies, recorded := testVerifyDependencies(t)
			test.configure(&dependencies)
			runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
			result, err := execute(context.Background(), testVerifyPlan(), nil, runner, Options{}, dependencies)
			if err == nil || result.Status != store.StatusFail || result.Error != test.wantError || recorded.Status != store.StatusFail {
				t.Fatalf("result=%#v err=%v recorded=%#v", result, err, *recorded)
			}
		})
	}
}

func TestExecute_取消后仍用可用Context清理和记录(t *testing.T) {
	dependencies, recorded := testVerifyDependencies(t)
	ctx, cancel := context.WithCancel(context.Background())
	dependencies.executeRestore = func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
		cancel()
		return restore.Result{Status: store.StatusFail}, context.Canceled
	}
	dependencies.cleanup = func(ctx context.Context, _ sshexec.Runner, _, _ string) (restore.CleanupResult, error) {
		if ctx.Err() != nil {
			t.Fatalf("cleanup context 已取消: %v", ctx.Err())
		}
		return restore.CleanupResult{Status: store.StatusOK}, nil
	}
	dependencies.record = func(ctx context.Context, verification store.Verification) error {
		if ctx.Err() != nil {
			t.Fatalf("record context 已取消: %v", ctx.Err())
		}
		*recorded = verification
		return nil
	}
	runner := &fakeRunner{run: func(context.Context, ...string) (string, error) { return "", nil }}
	result, err := execute(ctx, testVerifyPlan(), nil, runner, Options{}, dependencies)
	if !errors.Is(err, context.Canceled) || result.Status != store.StatusFail || recorded.Status != store.StatusFail {
		t.Fatalf("result=%#v err=%v recorded=%#v", result, err, *recorded)
	}
}
