package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

const (
	integrityStreamHelperEnv     = "ARK_INTEGRITY_STREAM_HELPER"
	integrityStreamHelperPayload = "ARK_INTEGRITY_STREAM_PAYLOAD"
)

func TestIntegrityStreamHelper(t *testing.T) {
	if os.Getenv(integrityStreamHelperEnv) != "1" {
		return
	}
	if _, err := io.WriteString(os.Stdout, os.Getenv(integrityStreamHelperPayload)); err != nil {
		os.Exit(125)
	}
	// 子进程必须直接退出，避免测试框架额外输出混入备份数据流。
	os.Exit(0)
}

type integrityHarness struct {
	events         []string
	snapshot       restic.Snapshot
	backupErr      error
	forgetErr      error
	historyBytes   int64
	historyFound   bool
	historyErr     error
	recordErr      error
	recordCtxErr   error
	backupFilename string
	backupTags     []string
	records        []store.RunTarget
	times          []time.Time
}

func (h *integrityHarness) dependencies() targetDependencies {
	return targetDependencies{
		backupStdin: func(_ context.Context, reader io.Reader, filename string, tags []string) (restic.Snapshot, error) {
			h.events = append(h.events, "backup")
			h.backupFilename = filename
			h.backupTags = append([]string(nil), tags...)
			if h.backupErr == nil {
				if _, err := io.Copy(io.Discard, reader); err != nil {
					return restic.Snapshot{}, err
				}
			}
			return h.snapshot, h.backupErr
		},
		forgetSnapshot: func(_ context.Context, id string) error {
			h.events = append(h.events, "forget:"+id)
			return h.forgetErr
		},
		lastSuccessfulTargetBytes: func(_ context.Context, host, targetID string) (int64, bool, error) {
			h.events = append(h.events, "history:"+host+":"+targetID)
			return h.historyBytes, h.historyFound, h.historyErr
		},
		recordRunTarget: func(ctx context.Context, record store.RunTarget) error {
			h.events = append(h.events, "record")
			h.recordCtxErr = ctx.Err()
			h.records = append(h.records, record)
			return h.recordErr
		},
		now: func() time.Time {
			if len(h.times) == 0 {
				return time.Time{}
			}
			value := h.times[0]
			h.times = h.times[1:]
			return value
		},
	}
}

type integritySourceProbe struct {
	events     *[]string
	reader     io.Reader
	closeErr   error
	closeCalls int
	waitErr    error
	waitCalls  int
}

func (p *integritySourceProbe) Read(buffer []byte) (int, error) {
	return p.reader.Read(buffer)
}

func (p *integritySourceProbe) Close() error {
	*p.events = append(*p.events, "close")
	p.closeCalls++
	return p.closeErr
}

func newIntegritySource(
	events *[]string,
	payload string,
	waitErr error,
	closeErr error,
) (*Result, *integritySourceProbe) {
	probe := &integritySourceProbe{
		events:   events,
		reader:   strings.NewReader(payload),
		waitErr:  waitErr,
		closeErr: closeErr,
	}
	target := config.Target{Type: config.TargetImageDigest, Services: []string{"api"}}
	result := streamResult(testHost(), target, ".json", probe, func() error {
		*events = append(*events, "wait")
		probe.waitCalls++
		return probe.waitErr
	})
	result.ImageDigests = map[string]string{"api": "ghcr.io/acme/app@sha256:111"}
	result.ComposeMetadata = &ComposeMetadata{PublishedPorts: []PublishedPort{
		{Service: "api", Published: "8080", Target: 8080, Protocol: "tcp"},
	}}
	return result, probe
}

func testIntegrityTimes() []time.Time {
	start := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)
	return []time.Time{start, start.Add(3 * time.Second)}
}

func TestBackupTarget_SuccessPersistsExactResult(t *testing.T) {
	harness := &integrityHarness{
		snapshot: restic.Snapshot{ID: "snapshot-1"},
		times:    testIntegrityTimes(),
	}
	source, probe := newIntegritySource(&harness.events, "payload", nil, nil)

	result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
	if err != nil {
		t.Fatalf("backupTarget 失败: %v", err)
	}
	wantEvents := []string{
		"backup",
		"wait",
		"close",
		"history:web-01:image_digest",
		"record",
	}
	if !reflect.DeepEqual(harness.events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", harness.events, wantEvents)
	}
	if harness.backupFilename != "web-01/image_digest.json" {
		t.Errorf("filename = %q", harness.backupFilename)
	}
	wantTags := []string{"host:web-01", "target:image_digest", "run:run-1"}
	if !reflect.DeepEqual(harness.backupTags, wantTags) {
		t.Errorf("tags = %#v，期望 %#v", harness.backupTags, wantTags)
	}
	if result.Status != store.StatusOK || result.Bytes != int64(len("payload")) ||
		result.Duration != 3*time.Second || result.SnapshotID != "snapshot-1" || result.Error != "" {
		t.Errorf("TargetResult = %#v", result)
	}
	if len(harness.records) != 1 {
		t.Fatalf("状态库记录数 = %d", len(harness.records))
	}
	wantRecord := store.RunTarget{
		RunID:      "run-1",
		Host:       "web-01",
		TargetID:   "image_digest",
		TargetType: "image_digest",
		Status:     store.StatusOK,
		Bytes:      int64(len("payload")),
		Duration:   3 * time.Second,
		SnapshotID: "snapshot-1",
	}
	if !reflect.DeepEqual(harness.records[0], wantRecord) {
		t.Errorf("RunTarget = %#v，期望 %#v", harness.records[0], wantRecord)
	}
	source.ImageDigests["api"] = "changed"
	if result.ImageDigests["api"] != "ghcr.io/acme/app@sha256:111" {
		t.Errorf("TargetResult 复用了可变 image digest map: %#v", result.ImageDigests)
	}
	source.ComposeMetadata.PublishedPorts[0].Published = "changed"
	if result.ComposeMetadata.PublishedPorts[0].Published != "8080" {
		t.Errorf("TargetResult 复用了可变 Compose metadata: %#v", result.ComposeMetadata)
	}
	if probe.waitCalls != 1 || probe.closeCalls != 1 {
		t.Errorf("Wait=%d Close=%d，期望均为 1", probe.waitCalls, probe.closeCalls)
	}
}

func TestBackupTarget_RealStdoutPipeWaitThenCloseSucceeds(t *testing.T) {
	const payload = "real command stdout"
	t.Setenv(integrityStreamHelperEnv, "1")
	t.Setenv(integrityStreamHelperPayload, payload)

	reader, wait, err := sshexec.NewLocal().Stream(
		context.Background(),
		os.Args[0],
		"-test.run=^TestIntegrityStreamHelper$",
	)
	if err != nil {
		t.Fatalf("启动真实本地流失败: %v", err)
	}
	target := config.Target{Type: config.TargetFiles, Name: "etc", Paths: []string{"/etc/hosts"}}
	source := streamResult(testHost(), target, ".tar", reader, wait)
	harness := &integrityHarness{
		snapshot: restic.Snapshot{ID: "snapshot-real-pipe"},
		times:    testIntegrityTimes(),
	}

	result, err := backupTarget(context.Background(), "run-real-pipe", source, harness.dependencies())
	if err != nil {
		t.Fatalf("真实 StdoutPipe 备份失败: %v", err)
	}
	if result.Status != store.StatusOK || result.Bytes != int64(len(payload)) ||
		result.SnapshotID != "snapshot-real-pipe" {
		t.Errorf("TargetResult = %#v", result)
	}
	for _, event := range harness.events {
		if strings.HasPrefix(event, "forget:") {
			t.Errorf("完整快照不应被撤销，调用事件 = %#v", harness.events)
		}
	}
}

func TestBackupTarget_RejectsInvalidDependenciesAndSource(t *testing.T) {
	sourceEvents := []string{}
	validSource, _ := newIntegritySource(&sourceEvents, "payload", nil, nil)
	validHarness := &integrityHarness{snapshot: restic.Snapshot{ID: "snapshot-1"}, times: testIntegrityTimes()}
	validDependencies := validHarness.dependencies()

	if _, err := BackupTarget(context.Background(), "run-1", validSource, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "restic repo") {
		t.Fatalf("nil repo 错误 = %v", err)
	}
	repo, err := restic.New(&config.Repo{
		Type: config.DefaultRepoType, URL: "local:/repo", PasswordFile: "/tmp/repo.pass",
	})
	if err != nil {
		t.Fatalf("构造测试 repo 失败: %v", err)
	}
	if _, err := BackupTarget(context.Background(), "run-1", validSource, repo, nil); err == nil ||
		!strings.Contains(err.Error(), "store") {
		t.Fatalf("nil store 错误 = %v", err)
	}

	tests := []struct {
		name         string
		ctx          context.Context
		runID        string
		source       *Result
		dependencies targetDependencies
		wantSub      string
	}{
		{name: "context 为空", runID: "run-1", source: validSource, dependencies: validDependencies, wantSub: "context"},
		{name: "run ID 为空", ctx: context.Background(), source: validSource, dependencies: validDependencies, wantSub: "run ID"},
		{name: "source 为空", ctx: context.Background(), runID: "run-1", dependencies: validDependencies, wantSub: "source"},
		{
			name: "source 字段不完整", ctx: context.Background(), runID: "run-1",
			source: &Result{}, dependencies: validDependencies, wantSub: "host、target ID",
		},
		{
			name: "source 流不完整", ctx: context.Background(), runID: "run-1",
			source:       &Result{Host: "web-01", TargetID: "files/etc", TargetType: config.TargetFiles},
			dependencies: validDependencies, wantSub: "filename、Reader",
		},
		{
			name: "内部依赖不完整", ctx: context.Background(), runID: "run-1",
			source: validSource, dependencies: targetDependencies{}, wantSub: "内部依赖",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := backupTarget(tc.ctx, tc.runID, tc.source, tc.dependencies)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("backupTarget 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestBackupTarget_ByteDropThreshold(t *testing.T) {
	tests := []struct {
		name          string
		current       int
		previous      int64
		found         bool
		wantStatus    store.Status
		wantErrorPart string
	}{
		{name: "没有历史", current: 1, previous: 0, found: false, wantStatus: store.StatusOK},
		{name: "零基线", current: 0, previous: 0, found: true, wantStatus: store.StatusOK},
		{name: "高于一半", current: 60, previous: 100, found: true, wantStatus: store.StatusOK},
		{name: "恰好一半", current: 50, previous: 100, found: true, wantStatus: store.StatusOK},
		{name: "低于一半", current: 49, previous: 100, found: true, wantStatus: store.StatusWarn, wantErrorPart: "低于上次成功值"},
		{name: "奇数基线低于一半", current: 50, previous: 101, found: true, wantStatus: store.StatusWarn, wantErrorPart: "低于上次成功值"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			harness := &integrityHarness{
				snapshot:     restic.Snapshot{ID: "snapshot-1"},
				historyBytes: tc.previous,
				historyFound: tc.found,
				times:        testIntegrityTimes(),
			}
			source, _ := newIntegritySource(
				&harness.events,
				strings.Repeat("x", tc.current),
				nil,
				nil,
			)

			result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
			if err != nil {
				t.Fatalf("backupTarget 失败: %v", err)
			}
			if result.Status != tc.wantStatus || result.Bytes != int64(tc.current) {
				t.Errorf("结果 = %#v", result)
			}
			if !strings.Contains(result.Error, tc.wantErrorPart) {
				t.Errorf("Error = %q，期望包含 %q", result.Error, tc.wantErrorPart)
			}
			if len(harness.records) != 1 || harness.records[0].Status != tc.wantStatus ||
				harness.records[0].Error != result.Error {
				t.Errorf("RunTarget = %#v", harness.records)
			}
		})
	}
}

func TestBackupTarget_WaitFailureForgetsExactSnapshotAndJoinsErrors(t *testing.T) {
	const secret = "SECRET_FROM_UPSTREAM_STDERR"
	waitErr := errors.New("upstream failed " + secret)
	closeErr := errors.New("close failed")
	forgetErr := errors.New("forget failed")
	harness := &integrityHarness{
		snapshot:  restic.Snapshot{ID: "snapshot-bad"},
		forgetErr: forgetErr,
		times:     testIntegrityTimes(),
	}
	source, probe := newIntegritySource(&harness.events, "partial", waitErr, closeErr)

	result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
	for _, wantErr := range []error{waitErr, closeErr, forgetErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("错误 %v 未保留 %v", err, wantErr)
		}
	}
	wantEvents := []string{"backup", "wait", "close", "forget:snapshot-bad", "record"}
	if !reflect.DeepEqual(harness.events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", harness.events, wantEvents)
	}
	if result.Status != store.StatusFail || result.SnapshotID != "snapshot-bad" ||
		!strings.Contains(result.Error, "撤销坏快照") {
		t.Errorf("TargetResult = %#v", result)
	}
	if len(harness.records) != 1 {
		t.Fatalf("状态库记录数 = %d，期望 1", len(harness.records))
	}
	if strings.Contains(result.Error, secret) || strings.Contains(harness.records[0].Error, secret) {
		t.Errorf("脱敏错误泄漏了上游 stderr: result=%q record=%q", result.Error, harness.records[0].Error)
	}
	if harness.records[0].Status != store.StatusFail ||
		harness.records[0].SnapshotID != "snapshot-bad" {
		t.Errorf("RunTarget = %#v", harness.records)
	}
	if probe.waitCalls != 1 || probe.closeCalls != 1 {
		t.Errorf("Wait=%d Close=%d", probe.waitCalls, probe.closeCalls)
	}
}

func TestBackupTarget_ResticFailureCleansUpWithoutForgetting(t *testing.T) {
	backupErr := errors.New("restic failed")
	waitErr := errors.New("upstream failed")
	closeErr := errors.New("close failed")
	harness := &integrityHarness{backupErr: backupErr, times: testIntegrityTimes()}
	source, _ := newIntegritySource(&harness.events, "payload", waitErr, closeErr)

	result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
	for _, wantErr := range []error{backupErr, waitErr, closeErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("错误 %v 未保留 %v", err, wantErr)
		}
	}
	wantEvents := []string{"backup", "close", "wait", "record"}
	if !reflect.DeepEqual(harness.events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", harness.events, wantEvents)
	}
	if result.Status != store.StatusFail || result.SnapshotID != "" {
		t.Errorf("TargetResult = %#v", result)
	}
}

func TestBackupTarget_ResticFailureForgetsReturnedSnapshot(t *testing.T) {
	backupErr := errors.New("restic failed after summary")
	forgetErr := errors.New("forget failed")
	tests := []struct {
		name      string
		forgetErr error
		wantStage string
	}{
		{name: "撤销成功"},
		{name: "撤销失败可见", forgetErr: forgetErr, wantStage: "撤销坏快照失败"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			harness := &integrityHarness{
				snapshot:  restic.Snapshot{ID: "snapshot-partial"},
				backupErr: backupErr,
				forgetErr: tc.forgetErr,
				times:     testIntegrityTimes(),
			}
			source, _ := newIntegritySource(&harness.events, "partial", nil, nil)

			result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
			if !errors.Is(err, backupErr) {
				t.Fatalf("错误 = %v，期望保留 %v", err, backupErr)
			}
			if tc.forgetErr != nil && !errors.Is(err, tc.forgetErr) {
				t.Fatalf("错误 = %v，期望保留 %v", err, tc.forgetErr)
			}
			wantEvents := []string{"backup", "close", "wait", "forget:snapshot-partial", "record"}
			if !reflect.DeepEqual(harness.events, wantEvents) {
				t.Fatalf("调用顺序 = %#v，期望 %#v", harness.events, wantEvents)
			}
			if result.Status != store.StatusFail || result.SnapshotID != "snapshot-partial" ||
				!strings.Contains(result.Error, tc.wantStage) {
				t.Errorf("TargetResult = %#v", result)
			}
		})
	}
}

func TestBackupTarget_MissingSnapshotIDFailsWithoutForgetting(t *testing.T) {
	harness := &integrityHarness{times: testIntegrityTimes()}
	source, _ := newIntegritySource(&harness.events, "payload", nil, nil)

	result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "未返回 snapshot ID") {
		t.Fatalf("错误 = %v", err)
	}
	wantEvents := []string{"backup", "wait", "close", "record"}
	if !reflect.DeepEqual(harness.events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", harness.events, wantEvents)
	}
	if result.Status != store.StatusFail || result.SnapshotID != "" {
		t.Errorf("TargetResult = %#v", result)
	}
}

func TestBackupTarget_ContextCancellationIsRecognizableAndCleansUp(t *testing.T) {
	harness := &integrityHarness{backupErr: context.Canceled, times: testIntegrityTimes()}
	source, probe := newIntegritySource(&harness.events, "payload", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := backupTarget(ctx, "run-1", source, harness.dependencies())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
	if result.Status != store.StatusFail || probe.waitCalls != 1 || probe.closeCalls != 1 {
		t.Errorf("结果 = %#v, Wait=%d Close=%d", result, probe.waitCalls, probe.closeCalls)
	}
	if harness.recordCtxErr != nil || len(harness.records) != 1 {
		t.Errorf("取消后的状态写入 context=%v, records=%d", harness.recordCtxErr, len(harness.records))
	}
}

func TestBackupTarget_HistoryAndRecordFailuresRemainVisible(t *testing.T) {
	historyErr := errors.New("history failed")
	const recordSecret = "SECRET_FROM_SQLITE_DETAIL"
	recordErr := errors.New("record failed " + recordSecret)
	tests := []struct {
		name       string
		historyErr error
		recordErr  error
		wantErr    error
	}{
		{name: "历史查询失败仍记录 fail", historyErr: historyErr, wantErr: historyErr},
		{name: "最终记录失败降级为 fail", recordErr: recordErr, wantErr: recordErr},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			harness := &integrityHarness{
				snapshot:   restic.Snapshot{ID: "snapshot-1"},
				historyErr: tc.historyErr,
				recordErr:  tc.recordErr,
				times:      testIntegrityTimes(),
			}
			source, _ := newIntegritySource(&harness.events, "payload", nil, nil)

			result, err := backupTarget(context.Background(), "run-1", source, harness.dependencies())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望保留 %v", err, tc.wantErr)
			}
			if result.Status != store.StatusFail || result.Error == "" {
				t.Errorf("TargetResult = %#v", result)
			}
			if strings.Contains(result.Error, recordSecret) {
				t.Errorf("TargetResult.Error 泄漏了持久化底层详情: %q", result.Error)
			}
			if len(harness.records) != 1 {
				t.Errorf("状态库调用数 = %d", len(harness.records))
			}
			if tc.historyErr != nil && harness.records[0].Status != store.StatusFail {
				t.Errorf("历史失败记录 = %#v", harness.records[0])
			}
		})
	}
}

func TestBackupTarget_UsesRealStoreHistoryAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ark.db")
	state, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("打开状态库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("关闭状态库失败: %v", err)
		}
	})
	startedAt := time.Date(2026, 8, 11, 4, 17, 0, 0, time.UTC)
	for _, run := range []store.Run{
		{ID: "run-previous", Status: store.StatusRunning, StartedAt: startedAt, ArkVersion: "test"},
		{ID: "run-current", Status: store.StatusRunning, StartedAt: startedAt.Add(time.Hour), ArkVersion: "test"},
	} {
		if err := state.CreateRun(ctx, run); err != nil {
			t.Fatalf("创建运行 %q 失败: %v", run.ID, err)
		}
	}
	if err := state.RecordRunTarget(ctx, store.RunTarget{
		RunID: "run-previous", Host: "web-01", TargetID: "image_digest",
		TargetType: "image_digest", Status: store.StatusOK, Bytes: 100,
		Duration: time.Second, SnapshotID: "snapshot-previous",
	}); err != nil {
		t.Fatalf("写入历史 target 失败: %v", err)
	}

	events := []string{}
	source, _ := newIntegritySource(&events, strings.Repeat("x", 40), nil, nil)
	times := testIntegrityTimes()
	dependencies := targetDependencies{
		backupStdin: func(_ context.Context, reader io.Reader, _ string, _ []string) (restic.Snapshot, error) {
			if _, err := io.Copy(io.Discard, reader); err != nil {
				return restic.Snapshot{}, err
			}
			return restic.Snapshot{ID: "snapshot-current"}, nil
		},
		forgetSnapshot:            func(context.Context, string) error { return nil },
		lastSuccessfulTargetBytes: state.LastSuccessfulTargetBytes,
		recordRunTarget:           state.RecordRunTarget,
		now: func() time.Time {
			value := times[0]
			times = times[1:]
			return value
		},
	}

	result, err := backupTarget(ctx, "run-current", source, dependencies)
	if err != nil {
		t.Fatalf("backupTarget 失败: %v", err)
	}
	if result.Status != store.StatusWarn || result.Bytes != 40 || result.SnapshotID != "snapshot-current" {
		t.Errorf("TargetResult = %#v", result)
	}
	bytes, found, err := state.LastSuccessfulTargetBytes(ctx, "web-01", "image_digest")
	if err != nil {
		t.Fatalf("查询历史失败: %v", err)
	}
	if !found || bytes != 100 {
		t.Errorf("最近成功历史 = (%d, %t)，期望 (100, true)", bytes, found)
	}
}

func TestBelowHalf_DoesNotOverflow(t *testing.T) {
	tests := []struct {
		name     string
		current  int64
		previous int64
		want     bool
	}{
		{name: "最大奇数低于一半", current: 1, previous: int64(^uint64(0) >> 1), want: true},
		{name: "最大奇数边界", current: int64(^uint64(0)>>1)/2 + 1, previous: int64(^uint64(0) >> 1), want: false},
		{name: "当前值为负不比较", current: -1, previous: 100, want: false},
		{name: "历史为零不比较", current: 0, previous: 0, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := belowHalf(tc.current, tc.previous); got != tc.want {
				t.Errorf("belowHalf(%d, %d) = %t，期望 %t", tc.current, tc.previous, got, tc.want)
			}
		})
	}
}
