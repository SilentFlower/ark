package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

type backupTestHarness struct {
	events          []string
	cfg             *config.Config
	localDoctorFail bool
	localDoctorWarn bool
	hostDoctorFail  map[string]bool
	hostDoctorWarn  map[string]bool
	executeErrors   map[string]error
	targetStatuses  map[string]store.Status
	targetErrors    map[string]error
	records         []store.RunTarget
	createdRun      store.Run
	finishedRun     store.RunResult
	manifest        backup.Manifest
	manifestID      string
	manifestErr     error
	finishErr       error
	forgetCalls     []string
	nowCalls        int
}

type backupEventCloser struct {
	harness *backupTestHarness
	event   string
	err     error
}

func (c *backupEventCloser) Close() error {
	c.harness.events = append(c.harness.events, c.event)
	return c.err
}

type backupNoopRunner struct{}

func (backupNoopRunner) Run(context.Context, ...string) (string, error) {
	return "", nil
}

func (backupNoopRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader("data")), func() error { return nil }, nil
}

func (backupNoopRunner) Feed(context.Context, io.Reader, ...string) error {
	return nil
}

func testBackupConfig() *config.Config {
	defaultRetention := config.Retention{Daily: 7, Weekly: 4, Monthly: 6}
	webRetention := config.Retention{Daily: 3, Weekly: 1}
	return &config.Config{
		Repo:     config.Repo{Type: config.DefaultRepoType, URL: "local:/repo", PasswordFile: "/repo.pass"},
		Defaults: config.Defaults{Retention: &defaultRetention},
		Hosts: []config.Host{
			{
				Host:  "hub-01",
				Local: true,
				Project: config.Project{
					Name: "hub", ComposeFile: "/srv/hub/compose.yaml",
				},
				Targets: []config.Target{
					{Type: config.TargetFiles, Name: "config", Paths: []string{"/etc/ark/ark.yaml"}},
				},
			},
			{
				Host: "web-01",
				SSH:  &config.SSH{Address: "127.0.0.1:22", User: "root", IdentityFile: "/id", KnownHostsFile: "/known"},
				Project: config.Project{
					Name: "web", ComposeFile: "/srv/web/compose.yaml",
				},
				Targets: []config.Target{
					{Type: config.TargetPostgres, Service: "db", Database: "app"},
					{Type: config.TargetImageDigest, Services: []string{"api"}},
				},
				Retention: &webRetention,
			},
		},
	}
}

func (h *backupTestHarness) dependencies() backupDependencies {
	return backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			h.events = append(h.events, "load")
			return h.cfg, nil
		},
		acquireLock: func(path string) (io.Closer, error) {
			h.events = append(h.events, "lock:"+path)
			return &backupEventCloser{harness: h, event: "unlock"}, nil
		},
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			h.events = append(h.events, "doctor:local")
			status := doctor.StatusOK
			if h.localDoctorFail {
				status = doctor.StatusFail
			} else if h.localDoctorWarn {
				status = doctor.StatusWarn
			}
			return &doctor.Report{Checks: []doctor.Check{{Name: "local", Status: status}}}
		},
		runHostDoctor: func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
			h.events = append(h.events, "doctor:"+host.Host)
			status := doctor.StatusOK
			if h.hostDoctorFail[host.Host] {
				status = doctor.StatusFail
			} else if h.hostDoctorWarn[host.Host] {
				status = doctor.StatusWarn
			}
			return &doctor.Report{Checks: []doctor.Check{{Name: host.Host, Status: status}}}
		},
		openStore: func(context.Context, string) (*store.Store, error) {
			h.events = append(h.events, "store:open")
			return nil, nil
		},
		closeStore: func(*store.Store) error {
			h.events = append(h.events, "store:close")
			return nil
		},
		createRun: func(_ context.Context, _ *store.Store, run store.Run) error {
			h.events = append(h.events, "run:create")
			h.createdRun = run
			return nil
		},
		finishRun: func(_ context.Context, _ *store.Store, id string, result store.RunResult) error {
			h.events = append(h.events, "run:finish:"+id)
			h.finishedRun = result
			return h.finishErr
		},
		recordRunTarget: func(_ context.Context, _ *store.Store, target store.RunTarget) error {
			h.events = append(h.events, "target:record:"+target.Host+":"+target.TargetID)
			h.records = append(h.records, target)
			return nil
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			h.events = append(h.events, "repo:new")
			return nil, nil
		},
		ensureRepo: func(context.Context, *restic.Repo) error {
			h.events = append(h.events, "repo:ensure")
			return nil
		},
		newRunner: func(host *config.Host) (sshexec.Runner, error) {
			h.events = append(h.events, "runner:"+host.Host)
			return backupNoopRunner{}, nil
		},
		executeTarget: func(
			_ context.Context,
			host config.Host,
			target config.Target,
			_ sshexec.Runner,
		) (*backup.Result, error) {
			key := host.Host + ":" + target.ID()
			h.events = append(h.events, "execute:"+key)
			if err := h.executeErrors[key]; err != nil {
				return nil, err
			}
			return &backup.Result{
				Host:          host.Host,
				TargetID:      target.ID(),
				TargetType:    target.Type,
				StdinFilename: plannedStdinFilename(host, target),
				Reader:        io.NopCloser(strings.NewReader("data")),
				Wait:          func() error { return nil },
				ImageDigests:  map[string]string{"api": "repo/api@sha256:111"},
			}, nil
		},
		backupTarget: func(
			_ context.Context,
			runID string,
			source *backup.Result,
			_ *restic.Repo,
			_ *store.Store,
		) (backup.TargetResult, error) {
			key := source.Host + ":" + source.TargetID
			h.events = append(h.events, "backup:"+key)
			_ = source.Reader.Close()
			_ = source.Wait()
			status := h.targetStatuses[key]
			if status == "" {
				status = store.StatusOK
			}
			result := backup.TargetResult{
				Host:         source.Host,
				TargetID:     source.TargetID,
				TargetType:   source.TargetType,
				Status:       status,
				Bytes:        4,
				Duration:     time.Second,
				SnapshotID:   "snapshot-" + strings.ReplaceAll(source.TargetID, "/", "-"),
				ImageDigests: source.ImageDigests,
			}
			if status == store.StatusWarn {
				result.Error = "体积下降"
			}
			if status == store.StatusFail {
				result.Error = "target 备份失败"
			}
			h.records = append(h.records, store.RunTarget{
				RunID: runID, Host: result.Host, TargetID: result.TargetID,
				TargetType: string(result.TargetType), Status: result.Status,
				Bytes: result.Bytes, Duration: result.Duration,
				SnapshotID: result.SnapshotID, Error: result.Error,
			})
			return result, h.targetErrors[key]
		},
		saveManifest: func(_ context.Context, _ *restic.Repo, manifest backup.Manifest) (restic.Snapshot, error) {
			h.events = append(h.events, "manifest:save")
			h.manifest = manifest
			if h.manifestErr != nil {
				return restic.Snapshot{}, h.manifestErr
			}
			if err := manifest.Validate(); err != nil {
				return restic.Snapshot{}, err
			}
			id := h.manifestID
			if id == "" {
				id = "manifest-snapshot"
			}
			return restic.Snapshot{ID: id}, nil
		},
		forgetPolicy: func(_ context.Context, _ *restic.Repo, _ config.Retention, tags []string) error {
			value := strings.Join(tags, ",")
			h.events = append(h.events, "forget:"+value)
			h.forgetCalls = append(h.forgetCalls, value)
			return nil
		},
		prune: func(context.Context, *restic.Repo) error {
			h.events = append(h.events, "prune")
			return nil
		},
		now: func() time.Time {
			base := time.Date(2026, 8, 12, 4, 17, 0, 0, time.UTC)
			value := base.Add(time.Duration(h.nowCalls) * time.Second)
			h.nowCalls++
			return value
		},
		newRunID:  func(time.Time) (string, error) { return "run-1", nil },
		statePath: "/state/ark.db",
	}
}

func TestRunBackup_成功按Host串行并统一Prune(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{"web-01:image_digest": store.StatusWarn},
		targetErrors:   map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err != nil {
		t.Fatalf("runBackup 失败: %v", err)
	}
	if summary.Status != store.StatusWarn || harness.finishedRun.Status != store.StatusWarn {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
	if summary.Manifest == nil || summary.Manifest.RunID != "run-1" || summary.ManifestSnapshotID != "manifest-snapshot" {
		t.Fatalf("summary manifest = %#v", summary)
	}
	wantTail := []string{
		"manifest:save",
		"forget:host:hub-01",
		"forget:host:web-01",
		"forget:ark-manifest",
		"prune",
		"run:finish:run-1",
		"store:close",
		"unlock",
	}
	if len(harness.events) < len(wantTail) ||
		!reflect.DeepEqual(harness.events[len(harness.events)-len(wantTail):], wantTail) {
		t.Fatalf("调用尾部 = %#v，期望 %#v", harness.events, wantTail)
	}
	if len(harness.records) != 3 || len(harness.manifest.Hosts) != 2 {
		t.Fatalf("records=%#v manifest=%#v", harness.records, harness.manifest)
	}
	if !harness.createdRun.StartedAt.Equal(harness.manifest.StartedAt) ||
		harness.createdRun.ID != harness.manifest.RunID {
		t.Fatalf("run 与 manifest 不一致: run=%#v manifest=%#v", harness.createdRun, harness.manifest)
	}
}

func TestRunBackup_Partial仍记录Manifest并继续后续Target(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{"hub-01": true},
		executeErrors:  map[string]error{"web-01:postgres/db/app": errors.New("stream failed")},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
		manifestID:     "partial-manifest",
	}
	hosts, err := selectBackupHosts(harness.cfg, "")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "stream failed") {
		t.Fatalf("partial 错误 = %v", err)
	}
	if summary.Status != store.StatusFail || harness.finishedRun.Status != store.StatusFail ||
		summary.ManifestSnapshotID != "partial-manifest" {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
	if !containsEvent(harness.events, "backup:web-01:image_digest") ||
		!containsEvent(harness.events, "manifest:save") {
		t.Fatalf("失败后未继续 target 或 manifest: %#v", harness.events)
	}
	if !reflect.DeepEqual(harness.forgetCalls, []string{backup.ManifestTag}) {
		t.Fatalf("失败 host 不应执行 target retention: %#v", harness.forgetCalls)
	}
	if len(harness.records) != 3 {
		t.Fatalf("所有 target 都必须有状态记录: %#v", harness.records)
	}
	for _, record := range harness.records[:2] {
		if record.Status != store.StatusFail {
			t.Fatalf("跳过/启动失败 target 状态 = %#v", record)
		}
	}
	if !containsEvent(harness.events, "prune") {
		t.Fatal("manifest forget 后应统一 prune")
	}
}

func TestRunBackup_LocalDoctor失败不打开状态库或仓库(t *testing.T) {
	harness := &backupTestHarness{
		cfg:             testBackupConfig(),
		localDoctorFail: true,
		hostDoctorFail:  map[string]bool{},
		executeErrors:   map[string]error{},
		targetStatuses:  map[string]store.Status{},
		targetErrors:    map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	_, err = runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "doctor") {
		t.Fatalf("错误 = %v", err)
	}
	want := []string{"lock:" + defaultBackupLockPath, "doctor:local", "unlock"}
	if !reflect.DeepEqual(harness.events, want) {
		t.Fatalf("调用 = %#v，期望 %#v", harness.events, want)
	}
}

func TestBackupCommand_DryRun只加载清单并输出纯JSON(t *testing.T) {
	harness := &backupTestHarness{cfg: testBackupConfig()}
	dependencies := backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			harness.events = append(harness.events, "load")
			return harness.cfg, nil
		},
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--dry-run", "--host", "web-01", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if !reflect.DeepEqual(harness.events, []string{"load"}) {
		t.Fatalf("dry-run 产生运行副作用: %#v", harness.events)
	}
	var plan backupPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if len(plan.Hosts) != 1 || plan.Hosts[0].Host != "web-01" || len(plan.Hosts[0].Targets) != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Hosts[0].Targets[0].Filename != "web-01/postgres/db/app.sql" ||
		plan.Manifest.Filename != backup.ManifestFilename {
		t.Fatalf("稳定文件名错误: %#v", plan)
	}
	if strings.Contains(output.String(), `"Daily"`) || !strings.Contains(output.String(), `"daily": 3`) {
		t.Fatalf("retention JSON 字段不稳定: %s", output.String())
	}
}

func TestRunBackup_SkipDoctor且只执行指定Host(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "web-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{
		hostName: "web-01", skipDoctor: true,
	}, harness.dependencies())
	if err != nil {
		t.Fatalf("runBackup 失败: %v", err)
	}
	for _, event := range harness.events {
		if strings.HasPrefix(event, "doctor:") || strings.Contains(event, "hub-01") {
			t.Fatalf("skip/host 选择失效: %#v", harness.events)
		}
	}
	if harness.createdRun.RequestedHost != "web-01" || len(harness.manifest.Hosts) != 1 ||
		harness.manifest.Hosts[0].Host != "web-01" {
		t.Fatalf("run/manifest host 选择错误: run=%#v manifest=%#v", harness.createdRun, harness.manifest)
	}
	if summary.Status != store.StatusOK || harness.finishedRun.Status != store.StatusOK {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
}

func TestRunBackup_Manifest失败保留Target且跳过Retention(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
		manifestErr:    errors.New("manifest store failed"),
	}
	hosts, err := selectBackupHosts(harness.cfg, "web-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "manifest store failed") {
		t.Fatalf("错误 = %v", err)
	}
	if summary.Status != store.StatusFail || harness.finishedRun.Status != store.StatusFail ||
		!strings.Contains(summary.Error, "保存 manifest 失败") {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
	if len(harness.records) != 2 || len(harness.forgetCalls) != 0 || containsEvent(harness.events, "prune") {
		t.Fatalf("manifest 失败后的状态/保留行为错误: records=%#v events=%#v", harness.records, harness.events)
	}
}

func TestRunBackup_FinishRun失败不会伪造成成功(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
		finishErr:      errors.New("finish failed"),
	}
	hosts, err := selectBackupHosts(harness.cfg, "hub-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "finish failed") {
		t.Fatalf("错误 = %v", err)
	}
	if summary.Status != store.StatusFail || !strings.Contains(summary.Error, "完成 run 状态写入失败") {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestRunBackup_Target仅返回Fail状态也使整体失败(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{"web-01:image_digest": store.StatusFail},
		targetErrors:   map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "web-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err == nil || !strings.Contains(err.Error(), "返回失败状态") {
		t.Fatalf("错误 = %v", err)
	}
	if summary.Status != store.StatusFail || harness.finishedRun.Status != store.StatusFail {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
}

func TestRunBackup_DoctorWarn进入整体Warn状态(t *testing.T) {
	harness := &backupTestHarness{
		cfg:             testBackupConfig(),
		localDoctorWarn: true,
		hostDoctorFail:  map[string]bool{},
		hostDoctorWarn:  map[string]bool{"web-01": true},
		executeErrors:   map[string]error{},
		targetStatuses:  map[string]store.Status{},
		targetErrors:    map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "web-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}
	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err != nil {
		t.Fatalf("doctor warn 不应使 backup 失败: %v", err)
	}
	if summary.Status != store.StatusWarn || harness.finishedRun.Status != store.StatusWarn {
		t.Fatalf("summary=%#v finished=%#v", summary, harness.finishedRun)
	}
}

func TestBackupCommand_Partial输出后返回可识别哨兵(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{"web-01:postgres/db/app": errors.New("stream failed")},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, harness.dependencies())
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	err := cmd.Execute()
	if !errors.Is(err, errBackupFailed) || !strings.Contains(err.Error(), "stream failed") {
		t.Fatalf("命令错误 = %v", err)
	}
	var summary backupRunSummary
	if decodeErr := json.Unmarshal(output.Bytes(), &summary); decodeErr != nil {
		t.Fatalf("partial JSON 无效: %v\n%s", decodeErr, output.String())
	}
	if summary.Status != store.StatusFail || summary.Error == "" {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestRecordSyntheticTarget_Context取消后仍使用收尾Context(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var gotContextErr error
	err := recordSyntheticTarget(ctx, "run-1", nil, backup.TargetResult{
		Host: "web-01", TargetID: "files/config", TargetType: config.TargetFiles,
		Status: store.StatusFail, Error: "未执行",
	}, backupDependencies{
		recordRunTarget: func(ctx context.Context, _ *store.Store, _ store.RunTarget) error {
			gotContextErr = ctx.Err()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("recordSyntheticTarget 失败: %v", err)
	}
	if gotContextErr != nil {
		t.Fatalf("收尾 context 已取消: %v", gotContextErr)
	}
}

func TestAcquireBackupLock_冲突立即失败且释放后可重取(t *testing.T) {
	path := t.TempDir() + "/ark.lock"
	first, err := acquireBackupLock(path)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	if _, err := acquireBackupLock(path); err == nil || !strings.Contains(err.Error(), "未等待锁") {
		t.Fatalf("锁冲突错误 = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("释放锁失败: %v", err)
	}
	third, err := acquireBackupLock(path)
	if err != nil {
		t.Fatalf("释放后重新加锁失败: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("最终释放锁失败: %v", err)
	}
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}
