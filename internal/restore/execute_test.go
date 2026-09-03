package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/dnsmgr"
	"github.com/silentflower/ark/internal/store"
)

type executeCall struct {
	kind  string
	argv  []string
	input string
}

type runnerFuncs struct {
	run    func(context.Context, ...string) (string, error)
	stream func(context.Context, ...string) (io.ReadCloser, func() error, error)
	feed   func(context.Context, io.Reader, ...string) error
}

func (r *runnerFuncs) Run(ctx context.Context, argv ...string) (string, error) {
	return r.run(ctx, argv...)
}

func (r *runnerFuncs) Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error) {
	if r.stream != nil {
		return r.stream(ctx, argv...)
	}
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (r *runnerFuncs) Feed(ctx context.Context, input io.Reader, argv ...string) error {
	if r.feed == nil {
		return fmt.Errorf("测试不应 Feed: %#v", argv)
	}
	return r.feed(ctx, input, argv...)
}

type resumeRunner struct {
	t           *testing.T
	plan        Plan
	executionID string
}

func (r *resumeRunner) Run(_ context.Context, argv ...string) (string, error) {
	r.t.Helper()
	switch {
	case len(argv) == 3 && argv[0] == "test" && (argv[1] == "-e" || argv[1] == "-L"):
		switch argv[2] {
		case planStatePath(r.plan), stepMarkerPath(r.plan, r.plan.Steps[0]), r.plan.Steps[0].Target.Paths[0]:
			return "", nil
		default:
			return "", commandExitError(r.t, 1)
		}
	case reflect.DeepEqual(argv, []string{"cat", "--", planStatePath(r.plan)}):
		return r.executionID + "\n", nil
	case reflect.DeepEqual(argv, []string{"cat", "--", stepMarkerPath(r.plan, r.plan.Steps[0])}):
		marker, err := completedStepMarker(context.Background(), r.plan.Steps[0],
			stepMarkerValue(r.plan, r.plan.Steps[0], nil), r)
		return marker + "\n", err
	case len(argv) >= 2 && argv[0] == "docker" && argv[1] == "ps":
		return "", nil
	case len(argv) >= 3 && argv[0] == "docker" && argv[1] == "volume" && argv[2] == "ls":
		return "", nil
	case len(argv) >= 1 && (argv[0] == "install" || argv[0] == "chmod" || argv[0] == "rm" || argv[0] == "mv"):
		return "", nil
	case len(argv) == 5 && argv[0] == "stat" && argv[1] == "-c":
		return "81a4 600 0 0 128 1700000000\n", nil
	default:
		r.t.Fatalf("未配置 resume Run 响应: %#v", argv)
		return "", nil
	}
}

func (r *resumeRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (r *resumeRunner) Feed(_ context.Context, _ io.Reader, argv ...string) error {
	if reflect.DeepEqual(argv, []string{"tar", "-xpf", "-", "-C", "/"}) {
		return nil
	}
	if len(argv) == 2 && argv[0] == "tee" {
		if argv[1] == planCompletePath(r.plan)+".tmp" ||
			(len(r.plan.Steps) > 1 && argv[1] == stepMarkerPath(r.plan, r.plan.Steps[1])+".tmp") {
			return nil
		}
	}
	return fmt.Errorf("测试不应 Feed: %#v", argv)
}

type executeFakeRunner struct {
	t             *testing.T
	calls         []executeCall
	runError      func([]string) error
	feedError     func([]string) error
	imagePulled   map[string]bool
	volumeCreated bool
	applicationUp bool
	pathExists    map[string]bool
	projectOutput string
	volumeOwner   string
}

func (f *executeFakeRunner) Run(_ context.Context, argv ...string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, executeCall{kind: "run", argv: append([]string(nil), argv...)})
	if f.runError != nil {
		if err := f.runError(argv); err != nil {
			return "", err
		}
	}
	switch {
	case len(argv) == 3 && argv[0] == "test" && (argv[1] == "-e" || argv[1] == "-L"):
		if f.pathExists[argv[2]] {
			return "", nil
		}
		return "", commandExitError(f.t, 1)
	case len(argv) >= 1 && (argv[0] == "install" || argv[0] == "chmod" || argv[0] == "mv" || argv[0] == "rm"):
		return "", nil
	case len(argv) == 5 && argv[0] == "stat" && argv[1] == "-c":
		return "81a4 600 0 0 128 1700000000\n", nil
	case len(argv) >= 2 && argv[0] == "docker" && argv[1] == "ps":
		return f.projectOutput, nil
	case len(argv) == 6 && reflect.DeepEqual(argv[:5], []string{"docker", "image", "inspect", "--format", "{{json .RepoDigests}}"}) &&
		strings.Contains(argv[5], "@sha256:"):
		if !f.imagePulled[argv[5]] {
			return "", commandExitError(f.t, 1)
		}
		return fmt.Sprintf("[%q]", argv[5]), nil
	case len(argv) == 3 && argv[0] == "docker" && argv[1] == "pull":
		if f.imagePulled == nil {
			f.imagePulled = make(map[string]bool)
		}
		f.imagePulled[argv[2]] = true
		return "", nil
	case len(argv) >= 3 && argv[0] == "docker" && argv[1] == "volume" && argv[2] == "ls":
		joined := strings.Join(argv, "\x00")
		if strings.Contains(joined, "label=com.docker.compose.project=app-prod") {
			return "", nil
		}
		if strings.Contains(joined, "uploads") && f.volumeCreated {
			return "uploads\n", nil
		}
		if strings.Contains(joined, "redis-data") {
			return "redis-data\n", nil
		}
		return "", nil
	case reflect.DeepEqual(argv, append(composeBaseArgv(fullExecutePlan().Project), "config", "--format", "json")):
		return `{"services":{"api":{},"db":{},"redis":{}},"volumes":{"uploads":{"name":"uploads"}}}`, nil
	case len(argv) >= 3 && argv[0] == "docker" && argv[1] == "volume" && argv[2] == "create":
		f.volumeCreated = true
		return "uploads\n", nil
	case reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", "uploads"}),
		reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", "redis-data"}):
		return `{"com.docker.compose.project":"app-prod","com.docker.compose.volume":"uploads"}`, nil
	case isComposeCommand(argv, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "db"):
		return "", nil
	case isComposeCommand(argv, "up", "--no-start", "--no-build", "--pull", "never", "--no-deps", "redis"):
		return "", nil
	case isComposeCommand(argv, "stop", "redis"):
		return "", nil
	case isComposeCommand(argv, "exec", "-T", "db", "pg_isready", "-U", "postgres", "-d", "app"):
		return "accepting connections\n", nil
	case isComposeCommand(argv, "exec", "-T", "redis", "redis-cli", "PING"):
		return "PONG\n", nil
	case isComposeCommand(argv, "ps", "--all", "--format", "json"):
		if !f.applicationUp {
			return `[{"ID":"cid-redis","Service":"redis","State":"created","Health":""}]`, nil
		}
		return `[{"ID":"cid-api","Service":"api","State":"running","Health":"healthy"},` +
			`{"ID":"cid-db","Service":"db","State":"running","Health":"healthy"},` +
			`{"ID":"cid-redis","Service":"redis","State":"running","Health":"healthy"}]`, nil
	case reflect.DeepEqual(argv, []string{"docker", "container", "inspect", "--format", "{{json .Mounts}}", "cid-redis"}):
		return `[{"Type":"volume","Name":"redis-data","Destination":"/data"}]`, nil
	case reflect.DeepEqual(argv, []string{
		"docker", "run", "--rm", "-v", "redis-data:/data", "alpine", "stat", "-c", "%u:%g", "/data",
	}):
		if f.volumeOwner == "" {
			f.volumeOwner = "999:999"
		}
		return f.volumeOwner + "\n", nil
	case len(argv) >= 2 && argv[0] == "docker" && argv[1] == "run":
		return "", nil
	case isComposeCommand(argv, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "redis"):
		return "", nil
	case isComposeCommand(argv, "up", "-d", "--no-build", "--pull", "never"):
		f.applicationUp = true
		return "", nil
	case isComposeCommand(argv, "config", "--format", "json"):
		return `{"services":{"api":{},"db":{},"redis":{}},"volumes":{"uploads":{"name":"uploads"}}}`, nil
	case len(argv) == 6 && reflect.DeepEqual(argv[:5], []string{"docker", "container", "inspect", "--format", "{{.Image}}"}):
		return "sha256:image-" + strings.TrimPrefix(argv[5], "cid-") + "\n", nil
	case len(argv) == 6 && reflect.DeepEqual(argv[:5], []string{"docker", "image", "inspect", "--format", "{{json .RepoDigests}}"}) &&
		strings.HasPrefix(argv[5], "sha256:image-"):
		service := strings.TrimPrefix(argv[5], "sha256:image-")
		return fmt.Sprintf("[%q]", fullImageDigests()[service]), nil
	default:
		f.t.Fatalf("未配置 Run 响应: %#v", argv)
		return "", nil
	}
}

func (f *executeFakeRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (f *executeFakeRunner) Feed(_ context.Context, input io.Reader, argv ...string) error {
	f.t.Helper()
	data, err := io.ReadAll(input)
	if err != nil {
		f.t.Fatalf("读取 Feed 输入失败: %v", err)
	}
	f.calls = append(f.calls, executeCall{
		kind: "feed", argv: append([]string(nil), argv...), input: string(data),
	})
	if f.feedError != nil {
		if err := f.feedError(argv); err != nil {
			return err
		}
	}
	return nil
}

func TestExecute_按固定顺序流式恢复全部Target(t *testing.T) {
	plan := fullExecutePlan()
	runner := &executeFakeRunner{t: t, pathExists: map[string]bool{}}
	var dumpCalls []string
	ready := false
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		OnPlanReady: func(got Plan) error {
			ready = got.ManifestSnapshotID == plan.ManifestSnapshotID
			return nil
		},
	}, executeDependencies{
		dump: func(_ context.Context, snapshotID string, snapshotPath string) (io.ReadCloser, error) {
			dumpCalls = append(dumpCalls, snapshotID+":"+snapshotPath)
			return io.NopCloser(strings.NewReader("payload:" + snapshotID)), nil
		},
		pollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !ready || result.Status != store.StatusOK || len(result.Steps) != len(plan.Steps) {
		t.Fatalf("恢复结果 = %#v, ready=%v", result, ready)
	}
	wantDumps := []string{
		"snapshot-files:source-01/files/config.tar",
		"snapshot-volume:source-01/volume/uploads.tar",
		"snapshot-postgres:source-01/postgres/db/app.sql",
		"snapshot-redis:source-01/redis/redis.rdb",
	}
	if !reflect.DeepEqual(dumpCalls, wantDumps) {
		t.Fatalf("Dump 顺序 = %#v，期望 %#v", dumpCalls, wantDumps)
	}

	override := imageOverridePath(plan)
	assertExecuteFeed(t, runner.calls, []string{"tar", "-xpf", "-", "-C", "/"}, "payload:snapshot-files")
	assertExecuteFeed(t, runner.calls, []string{"tee", override + ".tmp"},
		"{\"services\":{\"api\":{\"image\":\"registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111\"},\"db\":{\"image\":\"registry/db@sha256:3333333333333333333333333333333333333333333333333333333333333333\"},\"redis\":{\"image\":\"registry/redis@sha256:4444444444444444444444444444444444444444444444444444444444444444\"}}}\n")
	assertExecuteFeed(t, runner.calls, []string{
		"docker", "run", "--rm", "-i", "-v", "uploads:/dst", "alpine", "tar", "-xpf", "-", "-C", "/dst",
	}, "payload:snapshot-volume")
	assertExecuteFeed(t, runner.calls, append(composeArgv(plan),
		"exec", "-T", "db", "psql", "-U", "postgres", "-d", "app", "--set", "ON_ERROR_STOP=1"),
		"payload:snapshot-postgres")
	assertExecuteFeed(t, runner.calls, []string{
		"docker", "run", "--rm", "-i", "--user", "999:999", "-v", "redis-data:/data", "alpine",
		"tee", "/data/.ark-restore-dump.rdb",
	}, "payload:snapshot-redis")
	assertExecuteRunOrder(t, runner.calls,
		[]string{"docker", "pull", fullImageDigests()["api"]},
		[]string{"docker", "pull", fullImageDigests()["db"]},
		[]string{"docker", "pull", fullImageDigests()["redis"]},
		append(composeArgv(plan), "up", "-d", "--no-build", "--pull", "never", "--no-deps", "db"),
		append(composeArgv(plan), "up", "--no-start", "--no-build", "--pull", "never", "--no-deps", "redis"),
		append(composeArgv(plan), "up", "-d", "--no-build", "--pull", "never"),
		append(composeArgv(plan), "config", "--format", "json"),
	)
}

func TestExecute_原始状态库使用DB快照并原子替换(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "hub", DestinationHost: "destination",
		Project: Project{Name: "ark", ComposeFile: "/srv/ark/compose.yaml", ProjectName: "ark-prod"},
		Steps: []Step{{
			Phase: PhaseFiles, TargetID: "files/ark-state", TargetType: config.TargetFiles, SnapshotID: "snapshot-state",
			Target: &Target{Type: config.TargetFiles, Name: "ark-state", Paths: []string{"/var/lib/ark/ark.db"}},
		}},
	}
	runner := &executeFakeRunner{t: t, pathExists: map[string]bool{}}
	var dumpedPath string
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		RawFileTargets: map[string]string{"files/ark-state": "/var/lib/ark/ark.db"},
	}, executeDependencies{
		dump: func(_ context.Context, _ string, snapshotPath string) (io.ReadCloser, error) {
			dumpedPath = snapshotPath
			return io.NopCloser(strings.NewReader("sqlite-data")), nil
		},
		pollInterval: time.Millisecond,
	})
	if err != nil || result.Status != store.StatusOK {
		t.Fatalf("原始状态库恢复失败: result=%#v err=%v", result, err)
	}
	if dumpedPath != "hub/files/ark-state.db" {
		t.Fatalf("状态库 snapshot 路径 = %q", dumpedPath)
	}
	temporary := "/var/lib/ark/ark.db.ark-restore.tmp"
	assertExecuteFeed(t, runner.calls, []string{"tee", temporary}, "sqlite-data")
	assertExecuteRunOrder(t, runner.calls,
		[]string{"chmod", "0600", temporary},
		[]string{"mv", "--", temporary, "/var/lib/ark/ark.db"},
		[]string{"rm", "-f", "--", "/var/lib/ark/ark.db-wal", "/var/lib/ark/ark.db-shm"},
	)
	for _, call := range runner.calls {
		if call.kind == "feed" && len(call.argv) > 0 && call.argv[0] == "tar" {
			t.Fatalf("原始状态库不应通过 tar 恢复: %#v", call)
		}
		if reflect.DeepEqual(call.argv, []string{"rm", "-rf", "--", "/var/lib/ark/ark.db"}) {
			t.Fatalf("原始状态库不应在完整数据落盘前删除旧文件")
		}
	}
}

func TestExecute_默认冲突在写入前失败(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	runner := &executeFakeRunner{
		t:             t,
		pathExists:    map[string]bool{},
		projectOutput: `{"ID":"cid-existing","Service":"api","State":"running"}`,
	}
	safetyCalled := false
	readyCalled := false
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		SafetyBackup: func(context.Context) error { safetyCalled = true; return nil },
		OnPlanReady:  func(Plan) error { readyCalled = true; return nil },
	}, executeDependencies{dump: func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}, pollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "默认拒绝覆盖") {
		t.Fatalf("冲突错误 = %v", err)
	}
	if result.Status != store.StatusFail || safetyCalled || readyCalled {
		t.Fatalf("冲突后出现副作用: result=%#v safety=%v ready=%v", result, safetyCalled, readyCalled)
	}
	for _, call := range runner.calls {
		if call.kind == "feed" || (len(call.argv) > 1 && call.argv[0] == "docker" && call.argv[1] == "stop") {
			t.Fatalf("默认冲突后执行了写操作: %#v", call)
		}
	}
}

func TestPreview_冲突排序稳定且资源变化改变Digest(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	left, err := newPreview(plan, true, preflight{conflicts: []Conflict{
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
		{Resource: "container-a", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成左侧 preview 失败: %v", err)
	}
	right, err := newPreview(plan, true, preflight{conflicts: []Conflict{
		{Resource: "container-a", Detail: "已存在", ForceAllowed: true},
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成右侧 preview 失败: %v", err)
	}
	changed, err := newPreview(plan, true, preflight{conflicts: []Conflict{
		{Resource: "container-c", Detail: "已存在", ForceAllowed: true},
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成变化 preview 失败: %v", err)
	}
	if left.Digest != right.Digest || left.Digest == changed.Digest || !left.Destructive {
		t.Fatalf("preview digest 不符合稳定性要求: left=%#v right=%#v changed=%#v", left, right, changed)
	}
	dnsPlan := plan
	dnsPlan.DNS = &dnsmgr.Plan{
		Value:   "203.0.113.10",
		Records: []dnsmgr.Record{{DomainID: 12, RecordID: "record-a"}},
	}
	dnsPreview, err := newPreview(dnsPlan, true, preflight{conflicts: []Conflict{
		{Resource: "container-a", Detail: "已存在", ForceAllowed: true},
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成 DNS preview 失败: %v", err)
	}
	if dnsPreview.Digest == left.Digest {
		t.Fatal("DNS 计划变化必须进入 preview digest")
	}
	maintenancePlan := plan
	maintenancePlan.Maintenance = &dnsmgr.MaintenancePlan{TaskIDs: []int64{21, 34}}
	maintenancePreview, err := newPreview(maintenancePlan, true, preflight{conflicts: []Conflict{
		{Resource: "container-a", Detail: "已存在", ForceAllowed: true},
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成维护 preview 失败: %v", err)
	}
	reorderedMaintenancePlan := plan
	reorderedMaintenancePlan.Maintenance = &dnsmgr.MaintenancePlan{TaskIDs: []int64{34, 21}}
	reorderedMaintenancePreview, err := newPreview(reorderedMaintenancePlan, true, preflight{conflicts: []Conflict{
		{Resource: "container-a", Detail: "已存在", ForceAllowed: true},
		{Resource: "volume-b", Detail: "已存在", ForceAllowed: true},
	}})
	if err != nil {
		t.Fatalf("生成重排维护 preview 失败: %v", err)
	}
	if maintenancePreview.Digest == left.Digest ||
		maintenancePreview.Digest == reorderedMaintenancePreview.Digest {
		t.Fatal("维护任务的内容或顺序变化必须进入 preview digest")
	}
}

func TestExecute_ExpectedPreview不匹配时零备份零写入(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	runner := &executeFakeRunner{
		t: t, pathExists: map[string]bool{},
		projectOutput: `{"ID":"cid-existing","Service":"api","State":"running"}`,
	}
	backupCalled := false
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		Force: true, ExpectedPreviewSHA256: strings.Repeat("b", 64),
		SafetyBackup: func(context.Context) error { backupCalled = true; return nil },
	}, executeDependencies{
		dump: func(context.Context, string, string) (io.ReadCloser, error) {
			return nil, errors.New("不应读取备份数据")
		},
		pollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "已确认预检不一致") ||
		result.Status != store.StatusFail || backupCalled {
		t.Fatalf("result=%#v err=%v backupCalled=%t", result, err, backupCalled)
	}
	assertNoRestoreWrites(t, runner.calls)
}

func TestExecute_SafetyBackup后资源变化拒绝写入(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	runner := &executeFakeRunner{
		t: t, pathExists: map[string]bool{},
		projectOutput: `{"ID":"cid-before","Service":"api","State":"running"}`,
	}
	preview, err := Inspect(context.Background(), plan, runner, InspectOptions{Force: true})
	if err != nil {
		t.Fatalf("Inspect 失败: %v", err)
	}
	runner.calls = nil
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		Force: true, ExpectedPreviewSHA256: preview.Digest,
		SafetyBackup: func(context.Context) error {
			runner.projectOutput = `{"ID":"cid-after","Service":"api","State":"running"}`
			return nil
		},
	}, executeDependencies{
		dump: func(context.Context, string, string) (io.ReadCloser, error) {
			return nil, errors.New("不应读取备份数据")
		},
		pollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "safety backup 后恢复目标已变化") ||
		result.Status != store.StatusFail {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	assertNoRestoreWrites(t, runner.calls)
}

func assertNoRestoreWrites(t *testing.T, calls []executeCall) {
	t.Helper()
	for _, call := range calls {
		if call.kind == "feed" || (len(call.argv) > 1 && call.argv[0] == "docker" && call.argv[1] == "stop") ||
			(len(call.argv) > 0 && (call.argv[0] == "mkdir" || call.argv[0] == "mv")) {
			t.Fatalf("预检失败后发生目标写入: %#v", call)
		}
	}
}

func TestExecute_Force先备份和展示Plan再停容器(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	var events []string
	runner := &forceOrderRunner{t: t, events: &events, plan: plan}
	_, err := execute(context.Background(), plan, runner, ExecuteOptions{
		Force: true,
		SafetyBackup: func(context.Context) error {
			events = append(events, "backup")
			return nil
		},
		OnPlanReady: func(Plan) error {
			events = append(events, "plan")
			return nil
		},
	}, executeDependencies{dump: func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}, pollInterval: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("错误 = %v", err)
	}
	if !reflect.DeepEqual(events, []string{"backup", "plan", "stop"}) {
		t.Fatalf("force 顺序 = %#v", events)
	}
}

func TestExecute_Force安全备份失败时零写入(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   applicationTestSteps(),
	}
	runner := &executeFakeRunner{
		t:             t,
		pathExists:    map[string]bool{},
		projectOutput: `{"ID":"cid-existing","Service":"api","State":"running"}`,
	}
	backupErr := errors.New("backup failed")
	readyCalled := false
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{
		Force:        true,
		SafetyBackup: func(context.Context) error { return backupErr },
		OnPlanReady:  func(Plan) error { readyCalled = true; return nil },
	}, executeDependencies{
		dump:         func(context.Context, string, string) (io.ReadCloser, error) { return nil, errors.New("不应调用") },
		pollInterval: time.Millisecond,
	})
	if !errors.Is(err, backupErr) || result.Status != store.StatusFail || readyCalled {
		t.Fatalf("result=%#v err=%v ready=%v", result, err, readyCalled)
	}
	for _, call := range runner.calls {
		if call.kind == "feed" || (len(call.argv) > 1 && call.argv[0] == "docker" && call.argv[1] == "stop") {
			t.Fatalf("安全备份失败后发生写入: %#v", call)
		}
	}
}

func TestExecute_同一Plan重跑跳过已验证步骤(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps: []Step{
			{
				Phase: PhaseFiles, TargetID: "files/config", TargetType: config.TargetFiles, SnapshotID: "snapshot-files",
				Target: &Target{Type: config.TargetFiles, Name: "config", Paths: []string{"/srv/app/compose.yaml"}},
			},
			{
				Phase: PhaseFiles, TargetID: "files/data", TargetType: config.TargetFiles, SnapshotID: "snapshot-data",
				Target: &Target{Type: config.TargetFiles, Name: "data", Paths: []string{"/srv/app/data"}},
			},
		},
	}
	runner := &resumeRunner{t: t, plan: plan}
	runner.executionID = executionIdentity(plan, nil)
	var dumpCalls []string
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{}, executeDependencies{
		dump: func(_ context.Context, snapshotID string, _ string) (io.ReadCloser, error) {
			dumpCalls = append(dumpCalls, snapshotID)
			return io.NopCloser(strings.NewReader("payload")), nil
		},
		pollInterval: time.Millisecond,
	})
	if err != nil || result.Status != store.StatusOK ||
		!reflect.DeepEqual(dumpCalls, []string{"snapshot-data"}) || len(result.Steps) != 2 ||
		result.Steps[0].Status != "skipped" || result.Steps[1].Status != "ok" {
		t.Fatalf("result=%#v err=%v dumps=%#v", result, err, dumpCalls)
	}
}

func TestResumeRequiresStop_存在未完成数据步骤时需要停项目(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps: []Step{
			{
				Phase: PhaseFiles, TargetID: "files/config", TargetType: config.TargetFiles, SnapshotID: "snapshot-files",
				Target: &Target{Type: config.TargetFiles, Name: "config", Paths: []string{"/srv/app/compose.yaml"}},
			},
			{
				Phase: PhaseFiles, TargetID: "files/data", TargetType: config.TargetFiles, SnapshotID: "snapshot-data",
				Target: &Target{Type: config.TargetFiles, Name: "data", Paths: []string{"/srv/app/data"}},
			},
		},
	}
	runner := &resumeRunner{t: t, plan: plan, executionID: executionIdentity(plan, nil)}
	needsStop, err := resumeRequiresStop(context.Background(), plan, ExecuteOptions{}, runner)
	if err != nil || !needsStop {
		t.Fatalf("needsStop=%v err=%v", needsStop, err)
	}
}

func TestExecute_各阶段中断不写完成Marker(t *testing.T) {
	plan := fullExecutePlan()
	failure := errors.New("injected failure")
	tests := []struct {
		name      string
		phase     Phase
		runError  func([]string) error
		feedError func([]string) error
	}{
		{
			name: "files", phase: PhaseFiles,
			feedError: func(argv []string) error {
				if reflect.DeepEqual(argv, []string{"tar", "-xpf", "-", "-C", "/"}) {
					return failure
				}
				return nil
			},
		},
		{
			name: "image digest", phase: PhaseImageDigest,
			runError: func(argv []string) error {
				if len(argv) == 3 && argv[0] == "docker" && argv[1] == "pull" {
					return failure
				}
				return nil
			},
		},
		{
			name: "volume", phase: PhaseVolume,
			runError: func(argv []string) error {
				if len(argv) >= 3 && argv[0] == "docker" && argv[1] == "volume" && argv[2] == "create" {
					return failure
				}
				return nil
			},
		},
		{
			name: "database prepare", phase: PhaseDatabasePrepare,
			runError: func(argv []string) error {
				if isComposeCommand(argv, "up", "-d", "--no-build", "--pull", "never", "--no-deps", "db") {
					return failure
				}
				return nil
			},
		},
		{
			name: "database data", phase: PhaseDatabaseData,
			feedError: func(argv []string) error {
				if len(argv) > 0 && argv[len(argv)-1] == "ON_ERROR_STOP=1" {
					return failure
				}
				return nil
			},
		},
		{
			name: "application", phase: PhaseApplication,
			runError: func(argv []string) error {
				if isComposeCommand(argv, "up", "-d", "--no-build", "--pull", "never") {
					return failure
				}
				return nil
			},
		},
		{
			name: "health", phase: PhaseHealth,
			runError: func(argv []string) error {
				if isComposeCommand(argv, "config", "--format", "json") {
					return failure
				}
				return nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &executeFakeRunner{
				t: t, pathExists: map[string]bool{}, runError: tc.runError, feedError: tc.feedError,
			}
			result, err := execute(context.Background(), plan, runner, ExecuteOptions{}, executeDependencies{
				dump: func(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("payload")), nil
				},
				pollInterval: time.Millisecond,
			})
			if err == nil || result.Status != store.StatusFail || len(result.Steps) == 0 {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("阶段错误链未保留注入错误: %v", err)
			}
			last := result.Steps[len(result.Steps)-1]
			if last.Phase != tc.phase || last.Status != "fail" {
				t.Fatalf("失败步骤 = %#v，期望 phase=%s", last, tc.phase)
			}
			for _, call := range runner.calls {
				if call.kind == "feed" && len(call.argv) == 2 && call.argv[0] == "tee" &&
					call.argv[1] == planCompletePath(plan)+".tmp" {
					t.Fatalf("阶段失败后写入完成 marker: %#v", call)
				}
			}
		})
	}
}

func TestRestoreImages_拒绝缺少Digest的活跃Service(t *testing.T) {
	plan := fullExecutePlan()
	plan.Steps[1].ImageDigests = map[string]string{"api": fullImageDigests()["api"]}
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		if reflect.DeepEqual(argv, append(composeBaseArgv(plan.Project), "config", "--format", "json")) {
			return `{"services":{"api":{},"db":{}}}`, nil
		}
		return "", fmt.Errorf("不应执行命令: %#v", argv)
	}}
	err := restoreImages(context.Background(), plan, plan.Steps[1], runner)
	if err == nil || !strings.Contains(err.Error(), "db") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestRedisPingReady_只接受独立PONG响应(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "纯响应", output: "PONG\n", want: true},
		{name: "首尾空白", output: " \n\tPONG \r\n\n", want: true},
		{name: "警告在响应前", output: "AUTH failed: default user 未配置密码\nPONG\n", want: true},
		{name: "警告在响应后", output: "PONG\nAUTH failed: default user 未配置密码\n", want: true},
		{name: "空输出", output: "", want: false},
		{name: "只有警告", output: "AUTH failed: default user 未配置密码\n", want: false},
		{name: "小写响应", output: "pong\n", want: false},
		{name: "RESP响应", output: "+PONG\n", want: false},
		{name: "附加内容", output: "PONG extra\n", want: false},
		{name: "警告包含子串", output: "warning PONG\n", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redisPingReady(tc.output); got != tc.want {
				t.Fatalf("redisPingReady(%q) = %t，期望 %t", tc.output, got, tc.want)
			}
		})
	}
}

func TestWaitDatabaseReady_Redis多行输出立即成功(t *testing.T) {
	plan := fullExecutePlan()
	step := plan.Steps[4]
	calls := 0
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		calls++
		if !isComposeCommand(argv, "exec", "-T", "redis", "redis-cli", "PING") {
			return "", fmt.Errorf("不应执行命令: %#v", argv)
		}
		return "AUTH failed: default user 未配置密码\nPONG\n", nil
	}}

	if err := waitDatabaseReady(context.Background(), plan, step, runner, time.Hour); err != nil {
		t.Fatalf("等待 Redis 就绪失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Redis readiness 调用次数 = %d，期望 1", calls)
	}
}

func TestWaitDatabaseReady_Redis命令错误不能由响应覆盖(t *testing.T) {
	plan := fullExecutePlan()
	step := plan.Steps[4]
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		calls++
		if !isComposeCommand(argv, "exec", "-T", "redis", "redis-cli", "PING") {
			return "", fmt.Errorf("不应执行命令: %#v", argv)
		}
		cancel()
		return "PONG\n", errors.New("redis-cli failed")
	}}

	err := waitDatabaseReady(ctx, plan, step, runner, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误链 = %v，期望 context canceled", err)
	}
	if calls != 1 {
		t.Fatalf("Redis readiness 调用次数 = %d，期望 1", calls)
	}
}

func TestWaitDatabaseReadyOnce_Redis多行输出与命令错误(t *testing.T) {
	plan := fullExecutePlan()
	step := plan.Steps[6]
	failure := errors.New("redis-cli failed")
	tests := []struct {
		name    string
		output  string
		runErr  error
		wantErr error
	}{
		{
			name:   "警告加响应成功",
			output: "AUTH failed: default user 未配置密码\nPONG\n",
		},
		{
			name:    "命令错误不能由响应覆盖",
			output:  "PONG\n",
			runErr:  failure,
			wantErr: failure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
				if !isComposeCommand(argv, "exec", "-T", "redis", "redis-cli", "PING") {
					return "", fmt.Errorf("不应执行命令: %#v", argv)
				}
				return tc.output, tc.runErr
			}}
			err := waitDatabaseReadyOnce(context.Background(), plan, step, runner)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("一次性 Redis readiness 失败: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望保留 %v", err, tc.wantErr)
			}
		})
	}
}

func TestWaitDatabaseReady_Context取消可识别(t *testing.T) {
	plan := fullExecutePlan()
	step := plan.Steps[3]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &runnerFuncs{run: func(context.Context, ...string) (string, error) {
		return "", errors.New("not ready")
	}}
	err := waitDatabaseReady(ctx, plan, step, runner, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误链 = %v", err)
	}
}

func TestVerifyHealth_退出服务与Digest不一致均失败(t *testing.T) {
	plan := fullExecutePlan()
	tests := []struct {
		name string
		run  func(context.Context, ...string) (string, error)
		want string
	}{
		{
			name: "服务已退出",
			run: func(_ context.Context, argv ...string) (string, error) {
				switch {
				case reflect.DeepEqual(argv, append(composeArgv(plan), "config", "--format", "json")):
					return `{"services":{"api":{}}}`, nil
				case reflect.DeepEqual(argv, append(composeArgv(plan), "ps", "--all", "--format", "json")):
					return `[{"ID":"cid-api","Service":"api","State":"exited"}]`, nil
				default:
					return "", fmt.Errorf("不应执行命令: %#v", argv)
				}
			},
			want: "state=\"exited\"",
		},
		{
			name: "运行镜像摘要不一致",
			run: func(_ context.Context, argv ...string) (string, error) {
				switch {
				case len(argv) == 6 && argv[0] == "docker" && argv[1] == "container":
					return "sha256:image-api", nil
				case len(argv) == 6 && argv[0] == "docker" && argv[1] == "image":
					return `["registry/api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`, nil
				default:
					return "", fmt.Errorf("不应执行命令: %#v", argv)
				}
			},
			want: "实际 image digest 与 Plan 不一致",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &runnerFuncs{run: tc.run}
			var err error
			if tc.name == "服务已退出" {
				_, err = verifyHealth(context.Background(), plan, runner, time.Millisecond)
			} else {
				err = verifyImageDigests(context.Background(), plan, runner, map[string][]composeState{
					"api": {{ID: "cid-api", Service: "api", State: "running"}},
				})
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v", err)
			}
		})
	}
}

func TestFeedDump_组合Feed与Close错误(t *testing.T) {
	feedErr := errors.New("feed failed")
	closeErr := errors.New("close failed")
	runner := &feedErrorRunner{err: feedErr}
	err := feedDump(context.Background(), func(context.Context, string, string) (io.ReadCloser, error) {
		return &closeErrorReader{Reader: strings.NewReader("data"), err: closeErr}, nil
	}, runner, "snapshot", "path", []string{"sink"})
	if !errors.Is(err, feedErr) || !errors.Is(err, closeErr) {
		t.Fatalf("错误链 = %v", err)
	}
}

type forceOrderRunner struct {
	t      *testing.T
	events *[]string
	plan   Plan
}

func (f *forceOrderRunner) Run(_ context.Context, argv ...string) (string, error) {
	switch {
	case len(argv) == 3 && argv[0] == "test" &&
		(argv[1] == "-e" || argv[1] == "-L") && argv[2] == planStatePath(f.plan):
		return "", commandExitError(f.t, 1)
	case len(argv) >= 2 && argv[0] == "docker" && argv[1] == "ps":
		return `{"ID":"cid-existing","Service":"api","State":"running"}`, nil
	case len(argv) >= 3 && argv[0] == "docker" && argv[1] == "volume" && argv[2] == "ls":
		return "", nil
	case len(argv) >= 1 && (argv[0] == "install" || argv[0] == "chmod" || argv[0] == "rm" || argv[0] == "mv"):
		return "", nil
	case reflect.DeepEqual(argv, []string{"docker", "stop", "cid-existing"}):
		*f.events = append(*f.events, "stop")
		return "", errors.New("stop failed")
	default:
		f.t.Fatalf("未配置 force Run 响应: %#v", argv)
		return "", nil
	}
}

func (f *forceOrderRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (f *forceOrderRunner) Feed(_ context.Context, _ io.Reader, argv ...string) error {
	if len(argv) == 2 && argv[0] == "tee" && argv[1] == planStatePath(f.plan)+".tmp" {
		return nil
	}
	return fmt.Errorf("测试不应 Feed: %#v", argv)
}

type feedErrorRunner struct {
	err error
}

func (f *feedErrorRunner) Run(context.Context, ...string) (string, error) { return "", nil }

func (f *feedErrorRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (f *feedErrorRunner) Feed(context.Context, io.Reader, ...string) error { return f.err }

type closeErrorReader struct {
	io.Reader
	err error
}

func (r *closeErrorReader) Close() error { return r.err }

func TestComposeArgv_使用DigestOverride且不共享底层切片(t *testing.T) {
	plan := fullExecutePlan()
	first := composeArgv(plan)
	first = append(first, "ps")
	second := composeArgv(plan)
	want := []string{
		"docker", "compose", "-f", "/srv/app/compose.yaml",
		"-f", imageOverridePath(plan), "-p", "app-prod", "--env-file", "/srv/app/.env",
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("Compose argv = %#v，期望 %#v；首次追加后=%#v", second, want, first)
	}
}

func TestValidateExecutePlan_拒绝覆盖Ark运行时路径(t *testing.T) {
	plan := fullExecutePlan()
	plan.Steps[0].Target.Paths = []string{"/run/ark.lock"}
	result, err := execute(context.Background(), plan, &executeFakeRunner{t: t, pathExists: map[string]bool{}},
		ExecuteOptions{}, executeDependencies{
			dump: func(context.Context, string, string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("")), nil
			},
			pollInterval: time.Millisecond,
		})
	if err == nil || !strings.Contains(err.Error(), "ark 运行时路径") || result.Status != store.StatusFail {
		t.Fatalf("错误=%v result=%#v", err, result)
	}
}

func TestValidateExecutePlan_缺少ImageDigest步骤时在目标命令前拒绝(t *testing.T) {
	plan := Plan{
		ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "source-01", DestinationHost: "destination-01",
		Project: Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", ProjectName: "app-prod"},
		Steps:   []Step{{Phase: PhaseApplication}, {Phase: PhaseHealth}},
	}
	commandCalled := false
	runner := &runnerFuncs{run: func(context.Context, ...string) (string, error) {
		commandCalled = true
		return "", errors.New("不应调用")
	}}
	result, err := execute(context.Background(), plan, runner, ExecuteOptions{}, executeDependencies{
		dump:         func(context.Context, string, string) (io.ReadCloser, error) { return nil, errors.New("不应调用") },
		pollInterval: time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "image digest") || result.Status != store.StatusFail || commandCalled {
		t.Fatalf("result=%#v err=%v commandCalled=%v", result, err, commandCalled)
	}
}

func TestStepCompleted_Files元数据漂移时不跳过(t *testing.T) {
	plan := fullExecutePlan()
	step := plan.Steps[0]
	markerValue := stepMarkerValue(plan, step, nil)
	storedMetadata := "81a4 600 0 0 128 1700000000\n"
	currentMetadata := "81a4 600 0 0 256 1700000001\n"
	markerRunner := &runnerFuncs{run: func(_ context.Context, argv ...string) (string, error) {
		switch {
		case len(argv) == 3 && argv[0] == "test" && argv[2] == stepMarkerPath(plan, step):
			return "", nil
		case reflect.DeepEqual(argv, []string{"cat", "--", stepMarkerPath(plan, step)}):
			storedRunner := &runnerFuncs{run: func(context.Context, ...string) (string, error) {
				return storedMetadata, nil
			}}
			storedMarker, err := completedStepMarker(context.Background(), step, markerValue, storedRunner)
			return storedMarker + "\n", err
		case len(argv) == 5 && argv[0] == "stat" && argv[1] == "-c":
			return currentMetadata, nil
		default:
			return "", fmt.Errorf("未配置命令: %#v", argv)
		}
	}}
	completed, _, err := stepCompleted(context.Background(), plan, step, markerValue, markerRunner)
	if err != nil || completed {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
}

func TestValidateExecutePlan_拒绝原始文件与其它Files路径重叠(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "父目录", path: "/var/lib/ark"},
		{name: "WAL sidecar", path: "/var/lib/ark/ark.db-wal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := Plan{
				ManifestSnapshotID: "manifest-1", RunID: "run-1", SourceHost: "hub", DestinationHost: "destination",
				Project: Project{Name: "ark", ComposeFile: "/srv/ark/compose.yaml", ProjectName: "ark-prod"},
				Steps: []Step{
					{
						Phase: PhaseFiles, TargetID: "files/ark-state", TargetType: config.TargetFiles, SnapshotID: "state-snapshot",
						Target: &Target{Type: config.TargetFiles, Name: "ark-state", Paths: []string{"/var/lib/ark/ark.db"}},
					},
					{
						Phase: PhaseFiles, TargetID: "files/overlap", TargetType: config.TargetFiles, SnapshotID: "overlap-snapshot",
						Target: &Target{Type: config.TargetFiles, Name: "overlap", Paths: []string{tc.path}},
					},
				},
			}
			runner := &runnerFuncs{run: func(context.Context, ...string) (string, error) {
				return "", errors.New("验证应在目标命令前失败")
			}}
			result, err := execute(context.Background(), plan, runner, ExecuteOptions{
				RawFileTargets: map[string]string{"files/ark-state": "/var/lib/ark/ark.db"},
			}, executeDependencies{
				dump:         func(context.Context, string, string) (io.ReadCloser, error) { return nil, errors.New("不应调用") },
				pollInterval: time.Millisecond,
			})
			if err == nil || !strings.Contains(err.Error(), "重叠") || result.Status != store.StatusFail {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestFailResult_只传播受控安全摘要(t *testing.T) {
	sensitiveErr := errors.New("底层包含敏感 stderr")
	safeSummary := "隔离 Compose external volume 无法隔离"
	result, err := failResult(Result{}, fmt.Errorf(
		"准备隔离恢复失败: %w",
		withResultSummary(sensitiveErr, safeSummary),
	))
	if !errors.Is(err, sensitiveErr) || result.Status != store.StatusFail || result.Error != safeSummary {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if strings.Contains(result.Error, "敏感 stderr") {
		t.Fatalf("安全摘要泄漏底层错误: %q", result.Error)
	}

	plainResult, plainErr := failResult(Result{}, sensitiveErr)
	if !errors.Is(plainErr, sensitiveErr) || plainResult.Error != "恢复未完成" {
		t.Fatalf("普通错误不应进入结果: result=%#v err=%v", plainResult, plainErr)
	}
}

func TestParseComposeStates_支持数组与NDJSON(t *testing.T) {
	want := []composeState{{ID: "one", Service: "api", State: "running"}, {ID: "two", Service: "api", State: "exited"}}
	array, err := parseComposeStates(`[{"ID":"one","Service":"api","State":"running"},{"ID":"two","Service":"api","State":"exited"}]`)
	if err != nil || !reflect.DeepEqual(array, want) {
		t.Fatalf("数组解析=%#v err=%v", array, err)
	}
	ndjson, err := parseComposeStates("{\"ID\":\"one\",\"Service\":\"api\",\"State\":\"running\"}\n" +
		"{\"ID\":\"two\",\"Service\":\"api\",\"State\":\"exited\"}\n")
	if err != nil || !reflect.DeepEqual(ndjson, want) {
		t.Fatalf("NDJSON 解析=%#v err=%v", ndjson, err)
	}
}

func fullExecutePlan() Plan {
	project := Project{Name: "app", ComposeFile: "/srv/app/compose.yaml", EnvFile: "/srv/app/.env", ProjectName: "app-prod"}
	return Plan{
		ManifestSnapshotID: "manifest-1",
		RunID:              "run-1",
		SourceHost:         "source-01",
		DestinationHost:    "destination-01",
		Project:            project,
		Steps: []Step{
			{Phase: PhaseFiles, TargetID: "files/config", TargetType: config.TargetFiles, SnapshotID: "snapshot-files", Target: &Target{Type: config.TargetFiles, Name: "config", Paths: []string{"/srv/app/compose.yaml"}}},
			{Phase: PhaseImageDigest, TargetID: "image_digest", TargetType: config.TargetImageDigest, SnapshotID: "snapshot-image", Target: &Target{Type: config.TargetImageDigest, Services: []string{"api", "db", "redis"}}, ImageDigests: fullImageDigests()},
			{Phase: PhaseVolume, TargetID: "volume/uploads", TargetType: config.TargetVolume, SnapshotID: "snapshot-volume", Target: &Target{Type: config.TargetVolume, Name: "uploads"}},
			{Phase: PhaseDatabasePrepare, TargetID: "postgres/db/app", TargetType: config.TargetPostgres, Target: &Target{Type: config.TargetPostgres, Service: "db", Database: "app", User: "postgres"}},
			{Phase: PhaseDatabasePrepare, TargetID: "redis/redis", TargetType: config.TargetRedis, Target: &Target{Type: config.TargetRedis, Service: "redis"}},
			{Phase: PhaseDatabaseData, TargetID: "postgres/db/app", TargetType: config.TargetPostgres, SnapshotID: "snapshot-postgres", Target: &Target{Type: config.TargetPostgres, Service: "db", Database: "app", User: "postgres"}},
			{Phase: PhaseDatabaseData, TargetID: "redis/redis", TargetType: config.TargetRedis, SnapshotID: "snapshot-redis", Target: &Target{Type: config.TargetRedis, Service: "redis"}},
			{Phase: PhaseApplication},
			{Phase: PhaseHealth},
		},
	}
}

func fullImageDigests() map[string]string {
	return map[string]string{
		"api":   "registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"db":    "registry/db@sha256:3333333333333333333333333333333333333333333333333333333333333333",
		"redis": "registry/redis@sha256:4444444444444444444444444444444444444444444444444444444444444444",
	}
}

func applicationTestSteps() []Step {
	return []Step{
		{
			Phase: PhaseImageDigest, TargetID: "image_digest", TargetType: config.TargetImageDigest,
			SnapshotID: "snapshot-image", Target: &Target{Type: config.TargetImageDigest, Services: []string{"api"}},
			ImageDigests: map[string]string{"api": fullImageDigests()["api"]},
		},
		{Phase: PhaseApplication},
	}
}

func isComposeCommand(argv []string, suffix ...string) bool {
	plan := fullExecutePlan()
	prefix := composeArgv(plan)
	return len(argv) == len(prefix)+len(suffix) &&
		reflect.DeepEqual(argv[:len(prefix)], prefix) && reflect.DeepEqual(argv[len(prefix):], suffix)
}

func commandExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+string(rune('0'+code))).Run()
	if err == nil {
		t.Fatalf("未生成退出码 %d", code)
	}
	return err
}

func assertExecuteFeed(t *testing.T, calls []executeCall, argv []string, input string) {
	t.Helper()
	for _, call := range calls {
		if call.kind == "feed" && reflect.DeepEqual(call.argv, argv) && call.input == input {
			return
		}
	}
	t.Fatalf("未找到 Feed argv=%#v input=%q\n全部调用=%#v", argv, input, calls)
}

func assertExecuteRunOrder(t *testing.T, calls []executeCall, wanted ...[]string) {
	t.Helper()
	next := 0
	for _, call := range calls {
		if call.kind == "run" && next < len(wanted) && reflect.DeepEqual(call.argv, wanted[next]) {
			next++
		}
	}
	if next != len(wanted) {
		t.Fatalf("Run 顺序仅匹配 %d/%d，全部调用=%#v", next, len(wanted), calls)
	}
}
