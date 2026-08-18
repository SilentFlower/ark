package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/monitoring"
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
	doctorReports   []store.DoctorReport
	createdRun      store.Run
	finishedRun     store.RunResult
	manifest        backup.Manifest
	manifestID      string
	manifestErr     error
	finishErr       error
	forgetCalls     []string
	nowCalls        int
	sourceFilenames []string
	monitoringLoads int
	heartbeatFailed []bool
	heartbeatCtxErr []error
	heartbeatErr    error
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
		loadMonitoring: func(path string) (monitoring.Settings, error) {
			h.monitoringLoads++
			return monitoring.Load(path)
		},
		sendHeartbeat: func(ctx context.Context, _ monitoring.HeartbeatSettings, failed bool) error {
			h.heartbeatFailed = append(h.heartbeatFailed, failed)
			h.heartbeatCtxErr = append(h.heartbeatCtxErr, ctx.Err())
			return h.heartbeatErr
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
		recordDoctor: func(_ context.Context, _ *store.Store, report store.DoctorReport) error {
			h.doctorReports = append(h.doctorReports, report)
			return nil
		},
		analyzeSchedule: func(_ context.Context, _ string, baseTime time.Time) (time.Time, error) {
			return baseTime.Add(24 * time.Hour), nil
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
				StdinFilename: plannedStdinFilename(host, target, "/state/ark.db"),
				Reader:        io.NopCloser(strings.NewReader("data")),
				Wait:          func() error { return nil },
				ImageDigests:  map[string]string{"api": "repo/api@sha256:111"},
			}, nil
		},
		exportState: func(context.Context, *store.Store) (io.ReadCloser, error) {
			h.events = append(h.events, "state:export")
			return io.NopCloser(strings.NewReader("sqlite-data")), nil
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
			h.sourceFilenames = append(h.sourceFilenames, source.StdinFilename)
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

func writeBackupMonitoringFile(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/monitoring.env"
	contents := "ARK_HEARTBEAT_SUCCESS_URL=http://127.0.0.1/success\n" +
		"ARK_HEARTBEAT_FAILURE_URL=http://127.0.0.1/failure\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("写入监控配置失败: %v", err)
	}
	return path
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
	if len(harness.doctorReports) != 3 || harness.doctorReports[0].Scope != store.DoctorScopeLocal ||
		harness.doctorReports[1].Scope != store.DoctorScopeHost || harness.doctorReports[1].NextRunAt.IsZero() {
		t.Fatalf("doctor reports=%#v", harness.doctorReports)
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

func TestRunBackup_LocalDoctor失败先持久化报告且不打开仓库(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "失败项: local") {
		t.Fatalf("错误 = %v", err)
	}
	want := []string{"lock:" + defaultBackupLockPath, "store:open", "doctor:local", "store:close", "unlock"}
	if !reflect.DeepEqual(harness.events, want) {
		t.Fatalf("调用 = %#v，期望 %#v", harness.events, want)
	}
	if len(harness.doctorReports) != 1 || harness.doctorReports[0].Scope != store.DoctorScopeLocal ||
		harness.doctorReports[0].Status != store.StatusFail {
		t.Fatalf("doctor 报告 = %#v", harness.doctorReports)
	}
}

func TestRunBackup_调度解析失败仍持久化HostDoctor(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	hosts, err := selectBackupHosts(harness.cfg, "web-01")
	if err != nil {
		t.Fatalf("选择 host 失败: %v", err)
	}
	dependencies := harness.dependencies()
	dependencies.analyzeSchedule = func(context.Context, string, time.Time) (time.Time, error) {
		return time.Time{}, errors.New("systemd-analyze 不可用")
	}
	_, err = runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "调度解析失败") {
		t.Fatalf("调度解析错误=%v", err)
	}
	if len(harness.doctorReports) != 2 || harness.doctorReports[1].Scope != store.DoctorScopeHost ||
		harness.doctorReports[1].Host != "web-01" || !harness.doctorReports[1].NextRunAt.IsZero() {
		t.Fatalf("调度失败后的 doctor 报告=%#v", harness.doctorReports)
	}
	for _, event := range harness.events {
		if strings.HasPrefix(event, "execute:web-01:") {
			t.Fatalf("调度失败后仍执行 target: %#v", harness.events)
		}
	}
}

func TestDoctorFailureNames_仅返回失败项名称(t *testing.T) {
	report := &doctor.Report{Checks: []doctor.Check{
		{Name: "repo.access", Status: doctor.StatusFail, Detail: "secret-value"},
		{Name: "repo.object_lock", Status: doctor.StatusWarn, Detail: "人工确认"},
		{Name: "restic", Status: doctor.StatusOK, Detail: "restic 0.19.1"},
	}}

	got := doctorFailureNames(report)
	if !reflect.DeepEqual(got, []string{"repo.access"}) {
		t.Fatalf("失败项名称 = %#v", got)
	}
	if strings.Contains(strings.Join(got, ","), "secret-value") {
		t.Fatal("失败项摘要不应包含检查详情")
	}
	if got := doctorFailureNames(nil); !reflect.DeepEqual(got, []string{"doctor 报告为空"}) {
		t.Fatalf("空报告失败项 = %#v", got)
	}
}

func TestBackupCommand_DryRun只加载清单并输出纯JSON(t *testing.T) {
	harness := &backupTestHarness{cfg: testBackupConfig()}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: "/not/read/monitoring.env"}
	dependencies := backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			harness.events = append(harness.events, "load")
			return harness.cfg, nil
		},
		loadMonitoring: func(string) (monitoring.Settings, error) {
			harness.events = append(harness.events, "monitoring:load")
			return monitoring.Settings{}, nil
		},
		sendHeartbeat: func(context.Context, monitoring.HeartbeatSettings, bool) error {
			harness.events = append(harness.events, "heartbeat:send")
			return nil
		},
		statePath: "/state/ark.db",
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

func TestRunBackup_Hub状态库使用在线导出而非普通Tar(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	harness.cfg.Hosts[0].Targets = []config.Target{{
		Type: config.TargetFiles, Name: "ark-state", Paths: []string{"/state/../state/ark.db"},
	}}
	hosts, err := selectBackupHosts(harness.cfg, "hub-01")
	if err != nil {
		t.Fatalf("selectBackupHosts 失败: %v", err)
	}

	summary, err := runBackup(context.Background(), harness.cfg, hosts, backupCommandOptions{}, harness.dependencies())
	if err != nil {
		t.Fatalf("runBackup 失败: %v", err)
	}
	if summary.Status != store.StatusOK {
		t.Fatalf("summary 状态 = %s，期望 ok", summary.Status)
	}
	if !containsEvent(harness.events, "state:export") {
		t.Fatalf("未调用状态库在线导出: %#v", harness.events)
	}
	if containsEvent(harness.events, "execute:hub-01:files/ark-state") {
		t.Fatalf("状态库错误走了普通 files tar: %#v", harness.events)
	}
	if !reflect.DeepEqual(harness.sourceFilenames, []string{"hub-01/files/ark-state.db"}) {
		t.Fatalf("状态库稳定文件名 = %#v", harness.sourceFilenames)
	}
}

func TestBackupCommand_Hub状态库混合路径在加锁前拒绝(t *testing.T) {
	harness := &backupTestHarness{cfg: testBackupConfig()}
	harness.cfg.Hosts[0].Targets = []config.Target{{
		Type:  config.TargetFiles,
		Name:  "mixed",
		Paths: []string{"/state/ark.db", "/etc/ark/ark.yaml"},
	}}
	dependencies := backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			harness.events = append(harness.events, "load")
			return harness.cfg, nil
		},
		statePath: "/state/ark.db",
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	cmd.SetArgs([]string{"--dry-run", "--host", "hub-01"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "必须作为独立 files target") {
		t.Fatalf("混合状态库 target 错误 = %v", err)
	}
	if !reflect.DeepEqual(harness.events, []string{"load"}) {
		t.Fatalf("拒绝混合 target 前产生副作用: %#v", harness.events)
	}
}

func TestBackupCommand_Hub状态库被其它Target覆盖时在加锁前拒绝(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "父目录", path: "/state"},
		{name: "WAL sidecar", path: "/state/ark.db-wal"},
		{name: "SHM sidecar", path: "/state/ark.db-shm"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			harness := &backupTestHarness{cfg: testBackupConfig()}
			harness.cfg.Hosts[0].Targets = []config.Target{
				{Type: config.TargetFiles, Name: "ark-state", Paths: []string{"/state/ark.db"}},
				{Type: config.TargetFiles, Name: "overlap", Paths: []string{tc.path}},
			}
			dependencies := backupDependencies{
				loadConfig: func(string) (*config.Config, error) {
					harness.events = append(harness.events, "load")
					return harness.cfg, nil
				},
				statePath: "/state/ark.db",
			}
			configPath := "/etc/ark/ark.yaml"
			cmd := newBackupCmdWithDependencies(&configPath, dependencies)
			cmd.SetArgs([]string{"--dry-run", "--host", "hub-01"})

			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "必须作为独立 files target") {
				t.Fatalf("重叠状态库 target 错误 = %v", err)
			}
			if !reflect.DeepEqual(harness.events, []string{"load"}) {
				t.Fatalf("拒绝重叠 target 前产生副作用: %#v", harness.events)
			}
		})
	}
}

func TestBackupCommand_Hub状态库重复Target在加锁前拒绝(t *testing.T) {
	harness := &backupTestHarness{cfg: testBackupConfig()}
	harness.cfg.Hosts[0].Targets = []config.Target{
		{Type: config.TargetFiles, Name: "ark-state-primary", Paths: []string{"/state/ark.db"}},
		{Type: config.TargetFiles, Name: "ark-state-copy", Paths: []string{"/state/ark.db"}},
	}
	dependencies := backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			harness.events = append(harness.events, "load")
			return harness.cfg, nil
		},
		statePath: "/state/ark.db",
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	cmd.SetArgs([]string{"--dry-run", "--host", "hub-01"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "只能声明一个") {
		t.Fatalf("重复状态库 target 错误 = %v", err)
	}
	if !reflect.DeepEqual(harness.events, []string{"load"}) {
		t.Fatalf("拒绝重复 target 前产生副作用: %#v", harness.events)
	}
}

func TestStateDatabaseTarget_仅保护本地精确路径(t *testing.T) {
	target := config.Target{Type: config.TargetFiles, Name: "ark-state", Paths: []string{"/var/lib/ark/../ark/ark.db"}}
	if !isStateDatabaseTarget(config.Host{Host: "hub", Local: true}, target, store.DefaultPath) {
		t.Fatal("清理后的本地状态库路径应被识别")
	}
	if isStateDatabaseTarget(config.Host{Host: "remote"}, target, store.DefaultPath) {
		t.Fatal("远程同名路径不应被当作 hub 状态库")
	}
	other := target
	other.Paths = []string{"/var/lib/ark/other.db"}
	if isStateDatabaseTarget(config.Host{Host: "hub", Local: true}, other, store.DefaultPath) {
		t.Fatal("其它本地数据库不应被误识别")
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
	if summary.HeartbeatStatus != monitoring.HeartbeatDisabled {
		t.Fatalf("heartbeat_status = %q", summary.HeartbeatStatus)
	}
	if len(harness.heartbeatFailed) != 0 {
		t.Fatalf("未配置心跳时仍产生请求: %#v", harness.heartbeatFailed)
	}
}

func TestBackupCommand_按备份终态发送心跳并输出状态(t *testing.T) {
	tests := []struct {
		name           string
		executeErrors  map[string]error
		targetStatuses map[string]store.Status
		wantFailed     bool
		wantErr        bool
	}{
		{name: "成功使用成功端点", executeErrors: map[string]error{}, targetStatuses: map[string]store.Status{}, wantFailed: false},
		{name: "告警使用成功端点", executeErrors: map[string]error{}, targetStatuses: map[string]store.Status{"web-01:image_digest": store.StatusWarn}, wantFailed: false},
		{name: "失败使用失败端点", executeErrors: map[string]error{"web-01:postgres/db/app": errors.New("stream failed")}, targetStatuses: map[string]store.Status{}, wantFailed: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := &backupTestHarness{
				cfg:            testBackupConfig(),
				hostDoctorFail: map[string]bool{},
				hostDoctorWarn: map[string]bool{},
				executeErrors:  test.executeErrors,
				targetStatuses: test.targetStatuses,
				targetErrors:   map[string]error{},
			}
			harness.cfg.Monitoring = &config.Monitoring{EnvFile: writeBackupMonitoringFile(t)}
			configPath := "/etc/ark/ark.yaml"
			cmd := newBackupCmdWithDependencies(&configPath, harness.dependencies())
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{"--host", "web-01", "--json"})
			err := cmd.Execute()
			if (err != nil) != test.wantErr {
				t.Fatalf("命令错误 = %v", err)
			}
			if !reflect.DeepEqual(harness.heartbeatFailed, []bool{test.wantFailed}) {
				t.Fatalf("心跳端点选择 = %#v", harness.heartbeatFailed)
			}
			var summary backupRunSummary
			if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
				t.Fatalf("解析 backup JSON 失败: %v\n%s", err, output.String())
			}
			if summary.HeartbeatStatus != monitoring.HeartbeatSent {
				t.Fatalf("heartbeat_status = %q", summary.HeartbeatStatus)
			}
		})
	}
}

func TestBackupCommand_心跳失败不改变备份退出结果(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
		heartbeatErr:   errors.New("外部心跳投递失败: HTTP 状态 503"),
	}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: writeBackupMonitoringFile(t)}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, harness.dependencies())
	var output bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("心跳失败不应改变成功退出结果: %v", err)
	}
	var summary backupRunSummary
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatalf("解析 backup JSON 失败: %v", err)
	}
	if summary.HeartbeatStatus != monitoring.HeartbeatFailed {
		t.Fatalf("heartbeat_status = %q", summary.HeartbeatStatus)
	}
	if !strings.Contains(stderr.String(), "警告: 外部心跳投递失败") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestBackupCommand_失败心跳投递失败仍保留备份失败退出结果(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{"web-01:postgres/db/app": errors.New("stream failed")},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
		heartbeatErr:   errors.New("外部心跳投递失败: HTTP 状态 503"),
	}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: writeBackupMonitoringFile(t)}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, harness.dependencies())
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	err := cmd.Execute()
	if !errors.Is(err, errBackupFailed) {
		t.Fatalf("命令错误 = %v", err)
	}
	var summary backupRunSummary
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatalf("解析 backup JSON 失败: %v", err)
	}
	if summary.Status != store.StatusFail || summary.HeartbeatStatus != monitoring.HeartbeatFailed {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestBackupCommand_监控配置失败不改变备份退出结果(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: t.TempDir() + "/missing.env"}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, harness.dependencies())
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("监控配置失败不应改变成功退出结果: %v", err)
	}
	var summary backupRunSummary
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatalf("解析 backup JSON 失败: %v", err)
	}
	if summary.HeartbeatStatus != monitoring.HeartbeatFailed || len(harness.heartbeatFailed) != 0 {
		t.Fatalf("summary=%#v requests=%#v", summary, harness.heartbeatFailed)
	}
}

func TestBackupCommand_清单加载失败不读取监控配置(t *testing.T) {
	monitoringLoads := 0
	dependencies := backupDependencies{
		loadConfig: func(string) (*config.Config, error) {
			return nil, errors.New("manifest invalid")
		},
		loadMonitoring: func(string) (monitoring.Settings, error) {
			monitoringLoads++
			return monitoring.Settings{}, nil
		},
		statePath: "/state/ark.db",
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "manifest invalid") {
		t.Fatalf("命令错误 = %v", err)
	}
	if monitoringLoads != 0 {
		t.Fatalf("清单加载失败后读取了监控配置 %d 次", monitoringLoads)
	}
}

func TestBackupCommand_前置失败仍发送失败心跳(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: writeBackupMonitoringFile(t)}
	dependencies := harness.dependencies()
	dependencies.acquireLock = func(string) (io.Closer, error) {
		return nil, errors.New("lock busy")
	}
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "lock busy") {
		t.Fatalf("命令错误 = %v", err)
	}
	if !reflect.DeepEqual(harness.heartbeatFailed, []bool{true}) {
		t.Fatalf("心跳端点选择 = %#v", harness.heartbeatFailed)
	}
}

func TestBackupCommand_取消后使用独立上下文发送失败心跳(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		hostDoctorWarn: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	harness.cfg.Monitoring = &config.Monitoring{EnvFile: writeBackupMonitoringFile(t)}
	dependencies := harness.dependencies()
	dependencies.openStore = func(ctx context.Context, _ string) (*store.Store, error) {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	configPath := "/etc/ark/ark.yaml"
	cmd := newBackupCmdWithDependencies(&configPath, dependencies)
	cmd.SetContext(ctx)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("已取消命令应失败")
	}
	if !reflect.DeepEqual(harness.heartbeatFailed, []bool{true}) ||
		!reflect.DeepEqual(harness.heartbeatCtxErr, []error{nil}) {
		t.Fatalf("failed=%#v ctxErr=%#v", harness.heartbeatFailed, harness.heartbeatCtxErr)
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
