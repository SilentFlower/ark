package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/dnsmgr"
	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

type restoreNoopRunner struct{}

func (restoreNoopRunner) Run(context.Context, ...string) (string, error) { return "", nil }

func (restoreNoopRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader("")), func() error { return nil }, nil
}

func (restoreNoopRunner) Feed(context.Context, io.Reader, ...string) error { return nil }

type restoreDNSSetter struct{}

// SetRecordValue 实现 CLI 测试所需的最小 DNS client 契约。
func (restoreDNSSetter) SetRecordValue(
	context.Context,
	int64,
	string,
	string,
	*string,
) (dnsmgr.ValueResult, error) {
	return dnsmgr.ValueResult{}, nil
}

type restoreEventCloser struct {
	events *[]string
}

func (c *restoreEventCloser) Close() error {
	*c.events = append(*c.events, "unlock")
	return nil
}

type restoreErrorWriter struct {
	err error
}

func (w restoreErrorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func testRestoreCommandInputs() (*config.Config, backup.Manifest, restic.Snapshot) {
	project := config.Project{
		Name:        "web",
		ComposeFile: "/srv/web/compose.yaml",
		EnvFile:     "/srv/web/.env",
		ProjectName: "web-prod",
	}
	targets := []config.Target{
		{Type: config.TargetFiles, Name: "config", Paths: []string{"/srv/web/compose.yaml", "/srv/web/.env"}},
		{Type: config.TargetImageDigest, Services: []string{"api"}},
		{Type: config.TargetPostgres, Service: "db", Database: "app", User: "postgres"},
	}
	cfg := &config.Config{
		Repo: config.Repo{
			Type:         config.DefaultRepoType,
			URL:          "s3:https://storage.invalid/secret-bucket",
			PasswordFile: "/secret/restic-password",
			EnvFile:      "/secret/object-storage.env",
		},
		Hosts: []config.Host{
			{
				Host:    "web-01",
				SSH:     &config.SSH{Address: "source.invalid:22", User: "root", IdentityFile: "/secret/source-key", KnownHostsFile: "/secret/known-hosts"},
				Project: project,
				Targets: targets,
			},
			{
				Host:    "web-02",
				SSH:     &config.SSH{Address: "destination.invalid:22", User: "root", IdentityFile: "/secret/destination-key", KnownHostsFile: "/secret/known-hosts"},
				Project: project,
				Targets: targets,
			},
		},
	}
	startedAt := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)
	manifest := backup.Manifest{
		SchemaVersion: backup.ManifestSchemaVersion,
		RunID:         "run-cli",
		ArkVersion:    "v0.3.0",
		StartedAt:     startedAt,
		FinishedAt:    startedAt.Add(time.Minute),
		Hosts: []backup.ManifestHost{{
			Host: "web-01",
			Targets: []backup.TargetResult{
				{Host: "web-01", TargetID: "files/config", TargetType: config.TargetFiles, Status: store.StatusOK, SnapshotID: "files-snapshot"},
				{Host: "web-01", TargetID: "image_digest", TargetType: config.TargetImageDigest, Status: store.StatusOK, SnapshotID: "image-snapshot", ImageDigests: map[string]string{"api": "registry.invalid/api@sha256:1111111111111111111111111111111111111111111111111111111111111111"}, ComposeMetadata: &backup.ComposeMetadata{PublishedPorts: []backup.PublishedPort{
					{Service: "api", HostIP: "127.0.0.1", Published: "8080", Target: 8080, Protocol: "tcp", AppProtocol: "http", Mode: "ingress"},
				}}},
				{Host: "web-01", TargetID: "postgres/db/app", TargetType: config.TargetPostgres, Status: store.StatusOK, SnapshotID: "postgres-snapshot"},
			},
		}},
	}
	snapshot := restic.Snapshot{ID: "manifest-snapshot"}
	return cfg, manifest, snapshot
}

func enableRestoreDNS(cfg *config.Config) {
	cfg.DNSMgr = &config.DNSMgr{
		BaseURL: "https://dns.invalid",
		EnvFile: "/etc/ark/dnsmgr.env",
	}
	cfg.Hosts[1].DNSMgr = &config.HostDNSMgr{
		Value: "203.0.113.10",
		Records: []config.DNSMgrRecord{
			{DomainID: 12, RecordID: "record-a"},
			{DomainID: 12, RecordID: "record-b"},
		},
	}
}

func TestRestoreCommand_DryRunJSON只调用只读依赖(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "/etc/ark/test.yaml"
	var events []string
	var selector string
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(path string) (*config.Config, error) {
			events = append(events, "load-config:"+path)
			return cfg, nil
		},
		newRepo: func(repoConfig *config.Repo) (*restic.Repo, error) {
			events = append(events, "new-repo:"+repoConfig.Type)
			return nil, nil
		},
		loadManifest: func(_ context.Context, _ *restic.Repo, value string) (backup.Manifest, restic.Snapshot, bool, error) {
			events = append(events, "load-manifest")
			selector = value
			return manifest, snapshot, true, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--snapshot", "manifest-", "--dry-run", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("restore dry-run 失败: %v", err)
	}
	wantEvents := []string{"load-config:/etc/ark/test.yaml", "new-repo:restic", "load-manifest"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("依赖调用 = %#v，期望 %#v", events, wantEvents)
	}
	if selector != "manifest-" {
		t.Fatalf("snapshot selector = %q", selector)
	}
	var plan restore.Plan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if plan.ManifestSnapshotID != "manifest-snapshot" || plan.DestinationHost != "web-02" {
		t.Fatalf("Plan 选择错误: %#v", plan)
	}
	for _, forbidden := range []string{
		"secret-bucket",
		"restic-password",
		"object-storage.env",
		"source-key",
		"destination-key",
		"known-hosts",
		"source.invalid",
		"destination.invalid",
	} {
		if strings.Contains(output.String(), forbidden) {
			t.Errorf("JSON 泄漏敏感连接字段 %q: %s", forbidden, output.String())
		}
	}
}

func TestRestoreCommand_DNSDryRun展示计划但不读凭证或调用HTTP(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		newRepo:    func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) {
			t.Fatal("dry-run 不应加载 dnsmgr 凭证或创建 client")
			return nil, nil
		},
		switchDNS: func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			t.Fatal("dry-run 不应调用 dnsmgr HTTP")
			return dnsmgr.SwitchResult{}, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--dry-run", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("DNS dry-run 失败: %v", err)
	}
	var plan restore.Plan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("DNS Plan JSON 无效: %v\n%s", err, output.String())
	}
	if plan.DNS == nil || plan.DNS.Value != "203.0.113.10" || len(plan.DNS.Records) != 2 {
		t.Fatalf("DNS Plan = %#v", plan.DNS)
	}
	if strings.Contains(output.String(), cfg.DNSMgr.EnvFile) || strings.Contains(output.String(), cfg.DNSMgr.BaseURL) {
		t.Fatalf("DNS dry-run 泄漏连接配置: %s", output.String())
	}
}

func TestRestoreCommand_DNSInspect保持只读并绑定计划(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	var events []string
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) {
			events = append(events, "config")
			return cfg, nil
		},
		acquireLock: func(string) (io.Closer, error) {
			events = append(events, "lock")
			return &restoreEventCloser{events: &events}, nil
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			events = append(events, "repo")
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			events = append(events, "manifest")
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) {
			events = append(events, "runner")
			return restoreNoopRunner{}, nil
		},
		inspect: func(_ context.Context, plan restore.Plan, _ sshexec.Runner, _ restore.InspectOptions) (restore.Preview, error) {
			events = append(events, "inspect")
			if plan.DNS == nil || plan.DNS.Value != "203.0.113.10" {
				t.Fatalf("inspect 未收到 DNS 计划: %#v", plan.DNS)
			}
			return restore.Preview{Plan: plan, Digest: "dns-preview"}, nil
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) {
			t.Fatal("inspect 不应加载 dnsmgr 凭证")
			return nil, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--dry-run", "--inspect", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("DNS inspect 失败: %v", err)
	}
	wantEvents := []string{"config", "lock", "repo", "manifest", "runner", "inspect", "unlock"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("inspect 调用 = %#v，期望 %#v", events, wantEvents)
	}
	var preview restore.Preview
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil || preview.Plan.DNS == nil {
		t.Fatalf("DNS inspect JSON = %s, err=%v", output.String(), err)
	}
}

func TestRestoreCommand_人类输出包含完整审计信息且不泄漏凭证(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		newRepo:    func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("restore dry-run 失败: %v", err)
	}
	text := output.String()
	for _, want := range []string{
		"manifest-snapshot",
		"run-cli",
		"来源: web-01",
		"目标: web-01",
		"/srv/web/compose.yaml",
		"阶段 files",
		"阶段 image digest",
		"阶段 database prepare",
		"阶段 database data",
		"阶段 application",
		"阶段 health",
		"files-snapshot",
		"postgres-snapshot",
		"registry.invalid/api@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"默认拒绝覆盖",
		"确认 DNS 指向目标主机",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("人类输出缺少 %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"secret-bucket", "restic-password", "source-key", "source.invalid"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("人类输出泄漏敏感连接字段 %q: %s", forbidden, text)
		}
	}
}

func TestRestoreCommand_参数错误不调用依赖(t *testing.T) {
	configPath := "unused"
	called := false
	dependencies := restoreDependencies{
		loadConfig: func(string) (*config.Config, error) {
			called = true
			return nil, nil
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			called = true
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			called = true
			return backup.Manifest{}, restic.Snapshot{}, false, nil
		},
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "缺少 host", args: []string{"--dry-run"}, want: "--host"},
		{name: "dry-run 与 force 冲突", args: []string{"--host", "web-01", "--dry-run", "--force"}, want: "不能与"},
		{name: "isolate 与 force 冲突", args: []string{"--host", "web-01", "--isolate", "--force"}, want: "不能与"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
			if called {
				t.Fatal("参数错误后不应调用任何依赖")
			}
		})
	}
}

func TestRestoreCommand_隔离DryRun保持只读并输出稳定身份(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	var events []string
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) {
			events = append(events, "config")
			return cfg, nil
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			events = append(events, "repo")
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			events = append(events, "manifest")
			return manifest, snapshot, true, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--isolate", "--dry-run", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("隔离 dry-run 失败: %v", err)
	}
	if !reflect.DeepEqual(events, []string{"config", "repo", "manifest"}) {
		t.Fatalf("隔离 dry-run 产生额外调用: %#v", events)
	}
	var plan restore.Plan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("JSON 无效: %v\n%s", err, output.String())
	}
	if plan.Isolation == nil || plan.Isolation.PortAllocation != "runtime_auto" ||
		!strings.Contains(plan.Project.ProjectName, "-restore-") || len(plan.Isolation.Ports) != 1 ||
		plan.Isolation.Ports[0].AllocatedPort != "auto" {
		t.Fatalf("隔离 Plan 不完整: %#v", plan)
	}
}

func TestRestoreCommand_隔离DryRun人类输出完整端口映射(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		newRepo:    func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--isolate", "--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("隔离 dry-run 失败: %v", err)
	}
	for _, want := range []string{
		"api", "127.0.0.1:8080", "auto", "8080/tcp",
		"/srv/web/compose.yaml -> /var/lib/ark/restore/isolations/",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("输出缺少 %q:\n%s", want, output.String())
		}
	}
}

func TestRestoreCommand_真实恢复按锁和Doctor顺序执行且JSON仅输出结果(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "/etc/ark/test.yaml"
	var events []string
	dependencies := restoreDependencies{
		loadConfig: func(path string) (*config.Config, error) {
			events = append(events, "load:"+path)
			return cfg, nil
		},
		acquireLock: func(path string) (io.Closer, error) {
			events = append(events, "lock:"+path)
			return &restoreEventCloser{events: &events}, nil
		},
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			events = append(events, "doctor:local")
			return &doctor.Report{}
		},
		runRestoreDoctor: func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
			events = append(events, "doctor:"+host.Host)
			return &doctor.Report{}
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			events = append(events, "repo")
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			events = append(events, "manifest")
			return manifest, snapshot, true, nil
		},
		newRunner: func(host *config.Host) (sshexec.Runner, error) {
			events = append(events, "runner:"+host.Host)
			return restoreNoopRunner{}, nil
		},
		execute: func(
			_ context.Context,
			plan restore.Plan,
			_ *restic.Repo,
			_ sshexec.Runner,
			options restore.ExecuteOptions,
		) (restore.Result, error) {
			events = append(events, "execute")
			if options.Force || options.OnPlanReady != nil || options.SafetyBackup == nil {
				t.Fatalf("JSON 执行选项错误: %#v", options)
			}
			return restore.Result{
				ManifestSnapshotID: plan.ManifestSnapshotID,
				RunID:              plan.RunID,
				SourceHost:         plan.SourceHost,
				DestinationHost:    plan.DestinationHost,
				Status:             store.StatusOK,
			}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("真实 restore 失败: %v", err)
	}
	wantEvents := []string{
		"load:/etc/ark/test.yaml", "lock:/run/ark.lock", "repo", "manifest",
		"doctor:local", "doctor:web-02", "runner:web-02", "execute", "unlock",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", events, wantEvents)
	}
	var result restore.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("JSON 结果无效: %v\n%s", err, output.String())
	}
	if result.Status != store.StatusOK || strings.Contains(output.String(), "恢复计划") {
		t.Fatalf("JSON 输出 = %s", output.String())
	}
}

func TestRestoreCommand_恢复完成后切换DNS并保留Warn状态(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	var events []string
	setter := restoreDNSSetter{}
	dependencies := restoreDependencies{
		loadConfig: func(string) (*config.Config, error) {
			events = append(events, "config")
			return cfg, nil
		},
		acquireLock: func(string) (io.Closer, error) {
			events = append(events, "lock")
			return &restoreEventCloser{events: &events}, nil
		},
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			events = append(events, "doctor:local")
			return &doctor.Report{}
		},
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report {
			events = append(events, "doctor:target")
			return &doctor.Report{}
		},
		runDNSMgrDoctor: func(context.Context, *config.Config) *doctor.Report {
			events = append(events, "doctor:dnsmgr")
			return &doctor.Report{}
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			events = append(events, "repo")
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			events = append(events, "manifest")
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) {
			events = append(events, "runner")
			return restoreNoopRunner{}, nil
		},
		execute: func(
			context.Context,
			restore.Plan,
			*restic.Repo,
			sshexec.Runner,
			restore.ExecuteOptions,
		) (restore.Result, error) {
			events = append(events, "execute:completion-marker")
			return restore.Result{Status: store.StatusWarn}, nil
		},
		newDNSMgrClient: func(baseURL string, envFile string) (dnsmgr.ValueSetter, error) {
			events = append(events, "dnsmgr:client")
			if baseURL != cfg.DNSMgr.BaseURL || envFile != cfg.DNSMgr.EnvFile {
				t.Fatalf("dnsmgr client 参数 = %q %q", baseURL, envFile)
			}
			return setter, nil
		},
		switchDNS: func(_ context.Context, gotSetter dnsmgr.ValueSetter, plan dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			events = append(events, "dnsmgr:switch")
			if gotSetter != setter || plan.Value != "203.0.113.10" || len(plan.Records) != 2 {
				t.Fatalf("DNS 切换参数 = %#v setter=%#v", plan, gotSetter)
			}
			return dnsmgr.SwitchResult{Status: "ok", Records: []dnsmgr.RecordResult{
				{DomainID: 12, RecordID: "record-a", Status: "updated"},
				{DomainID: 12, RecordID: "record-b", Status: "unchanged"},
			}}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("带 DNS 的真实恢复失败: %v", err)
	}
	wantEvents := []string{
		"config", "lock", "repo", "manifest", "doctor:local", "doctor:target", "doctor:dnsmgr",
		"runner", "execute:completion-marker", "dnsmgr:client", "dnsmgr:switch", "unlock",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", events, wantEvents)
	}
	var result restore.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("DNS 结果 JSON 无效: %v\n%s", err, output.String())
	}
	if result.Status != store.StatusWarn || result.DNS == nil || result.DNS.Status != "ok" {
		t.Fatalf("DNS 恢复结果 = %#v", result)
	}
}

func TestRestoreCommand_DNS失败后重跑只重试DNS(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	completionMarker := false
	executeCalls := 0
	restoreWrites := 0
	dnsCalls := 0
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		runDNSMgrDoctor:  func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			executeCalls++
			if completionMarker {
				return restore.Result{Status: store.StatusOK, Steps: []restore.StepResult{{Status: "skipped"}}}, nil
			}
			restoreWrites++
			completionMarker = true
			return restore.Result{Status: store.StatusOK, Steps: []restore.StepResult{{Status: "ok"}}}, nil
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) { return restoreDNSSetter{}, nil },
		switchDNS: func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			dnsCalls++
			if dnsCalls == 1 {
				return dnsmgr.SwitchResult{Status: "rolled_back", Error: "DNS 切换失败，已恢复本轮变更"}, errors.New("dns failed")
			}
			return dnsmgr.SwitchResult{Status: "ok"}, nil
		},
	}
	run := func() error {
		cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})
		return cmd.Execute()
	}

	if err := run(); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("首次 DNS 失败错误 = %v", err)
	}
	if !completionMarker {
		t.Fatal("DNS 失败后不得清除恢复 completion marker")
	}
	if err := run(); err != nil {
		t.Fatalf("重跑 DNS 恢复失败: %v", err)
	}
	if executeCalls != 2 || restoreWrites != 1 || dnsCalls != 2 {
		t.Fatalf("重跑计数: execute=%d restoreWrites=%d dnsCalls=%d", executeCalls, restoreWrites, dnsCalls)
	}
}

func TestRestoreCommand_DNS回滚不完整返回失败和人工记录(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	switchErr := errors.New("dns switch failed with secret body")
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		runDNSMgrDoctor:  func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusOK}, nil
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) { return restoreDNSSetter{}, nil },
		switchDNS: func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			return dnsmgr.SwitchResult{
				Status: "rollback_failed",
				Records: []dnsmgr.RecordResult{
					{DomainID: 12, RecordID: "record-a", Status: "updated", RollbackStatus: "failed"},
					{DomainID: 12, RecordID: "record-b", Status: "failed"},
				},
				ManualRecords: []dnsmgr.Record{{DomainID: 12, RecordID: "record-a"}},
				Error:         "DNS 切换失败且回滚不完整",
			}, switchErr
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})

	err := cmd.Execute()
	if !errors.Is(err, errRestoreFailed) || !errors.Is(err, switchErr) {
		t.Fatalf("DNS 失败错误链 = %v", err)
	}
	var result restore.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("DNS 失败 JSON 无效: %v\n%s", err, output.String())
	}
	if result.Status != store.StatusFail || result.DNS == nil || result.DNS.Status != "rollback_failed" ||
		!slices.Contains(result.ManualChecks, "人工核对 dnsmgr 记录 12/record-a 当前指向") {
		t.Fatalf("DNS 失败结果 = %#v", result)
	}
	if strings.Contains(output.String(), "secret body") {
		t.Fatalf("DNS 失败输出泄漏底层错误: %s", output.String())
	}
}

func TestRestoreCommand_数据恢复失败时不创建DNSClient(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		runDNSMgrDoctor:  func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusFail, Error: "恢复未完成"}, errors.New("restore failed")
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) {
			t.Fatal("数据恢复失败后不应创建 dnsmgr client")
			return nil, nil
		},
		switchDNS: func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			t.Fatal("数据恢复失败后不应切换 DNS")
			return dnsmgr.SwitchResult{}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})
	if err := cmd.Execute(); !errors.Is(err, errRestoreFailed) {
		t.Fatalf("错误 = %v", err)
	}
}

func TestRestoreCommand_SkipDoctor仍执行受校验的DNS请求(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	enableRestoreDNS(cfg)
	configPath := "unused"
	switchCalled := false
	dependencies := restoreDependencies{
		loadConfig:  func(string) (*config.Config, error) { return cfg, nil },
		acquireLock: func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			t.Fatal("--skip-doctor 不应运行本地 doctor")
			return nil
		},
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report {
			t.Fatal("--skip-doctor 不应运行目标 doctor")
			return nil
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusOK}, nil
		},
		newDNSMgrClient: func(string, string) (dnsmgr.ValueSetter, error) { return restoreDNSSetter{}, nil },
		switchDNS: func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error) {
			switchCalled = true
			return dnsmgr.SwitchResult{Status: "ok"}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--skip-doctor", "--json"})

	if err := cmd.Execute(); err != nil || !switchCalled {
		t.Fatalf("skip-doctor DNS 结果: called=%v err=%v", switchCalled, err)
	}
}

func TestRestoreCommand_执行失败输出脱敏结果并返回哨兵(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	wantErr := errors.New("底层包含敏感 stderr")
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusFail, Error: "恢复未完成"}, wantErr
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})

	err := cmd.Execute()
	if !errors.Is(err, errRestoreFailed) || !errors.Is(err, wantErr) {
		t.Fatalf("错误链 = %v", err)
	}
	if strings.Contains(output.String(), "敏感 stderr") || !strings.Contains(output.String(), `"error": "恢复未完成"`) {
		t.Fatalf("失败输出未脱敏: %s", output.String())
	}
}

func TestRestoreCommand_执行与结果输出同时失败仍保留哨兵(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	runErr := errors.New("底层包含敏感 stderr")
	writeErr := errors.New("writer failed")
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error) {
			return restore.Result{Status: store.StatusFail, Error: "恢复未完成"}, runErr
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(restoreErrorWriter{err: writeErr})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--json"})

	err := cmd.Execute()
	if !errors.Is(err, errRestoreFailed) || !errors.Is(err, runErr) || !errors.Is(err, writeErr) {
		t.Fatalf("错误链 = %v", err)
	}
}

func TestRestoreCommand_源端本地状态库映射为原始文件恢复(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	statePath := "/var/lib/ark/ark.db"
	stateTarget := config.Target{Type: config.TargetFiles, Name: "ark-state", Paths: []string{statePath}}
	cfg.Hosts[0].Local = true
	cfg.Hosts[0].SSH = nil
	cfg.Hosts[0].Targets = append(cfg.Hosts[0].Targets, stateTarget)
	cfg.Hosts[1].Targets = append(cfg.Hosts[1].Targets, stateTarget)
	manifest.Hosts[0].Targets = append(manifest.Hosts[0].Targets, backup.TargetResult{
		Host: "web-01", TargetID: stateTarget.ID(), TargetType: config.TargetFiles,
		Status: store.StatusOK, SnapshotID: "state-snapshot",
	})
	configPath := "unused"
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(
			_ context.Context,
			_ restore.Plan,
			_ *restic.Repo,
			_ sshexec.Runner,
			options restore.ExecuteOptions,
		) (restore.Result, error) {
			if !reflect.DeepEqual(options.RawFileTargets, map[string]string{stateTarget.ID(): statePath}) {
				t.Fatalf("原始文件映射 = %#v", options.RawFileTargets)
			}
			return restore.Result{Status: store.StatusOK}, nil
		},
		backup: backupDependencies{statePath: statePath},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("真实 restore 失败: %v", err)
	}
}

func TestRestoreCommand_人类Plan输出失败时不执行目标写入(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	wantErr := errors.New("writer failed")
	executeCalled := false
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(
			_ context.Context,
			plan restore.Plan,
			_ *restic.Repo,
			_ sshexec.Runner,
			options restore.ExecuteOptions,
		) (restore.Result, error) {
			executeCalled = true
			if err := options.OnPlanReady(plan); !errors.Is(err, wantErr) {
				t.Fatalf("Plan 输出错误 = %v", err)
			}
			return restore.Result{}, wantErr
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(restoreErrorWriter{err: wantErr})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01"})

	err := cmd.Execute()
	if !errors.Is(err, wantErr) || !executeCalled {
		t.Fatalf("错误=%v execute=%v", err, executeCalled)
	}
}

func TestRunRestoreSafetyBackup_继承SkipDoctor并禁用Retention(t *testing.T) {
	harness := &backupTestHarness{
		cfg:            testBackupConfig(),
		hostDoctorFail: map[string]bool{},
		executeErrors:  map[string]error{},
		targetStatuses: map[string]store.Status{},
		targetErrors:   map[string]error{},
	}
	destination := &harness.cfg.Hosts[1]
	dependencies := harness.dependencies()

	err := runRestoreSafetyBackup(context.Background(), harness.cfg, destination, restoreCommandOptions{
		configPath: "/etc/ark/test.yaml",
		skipDoctor: true,
	}, dependencies)
	if err != nil {
		t.Fatalf("safety backup 失败: %v", err)
	}
	for _, event := range harness.events {
		if strings.HasPrefix(event, "doctor:") || strings.HasPrefix(event, "forget:") || event == "prune" {
			t.Fatalf("skip-doctor safety backup 执行了禁用阶段: %#v", harness.events)
		}
	}
	if !containsEvent(harness.events, "manifest:save") {
		t.Fatalf("safety backup 未保存 manifest: %#v", harness.events)
	}
}

func TestRestoreCommand_仓库无Manifest时失败(t *testing.T) {
	cfg, _, _ := testRestoreCommandInputs()
	configPath := "unused"
	cmd := newRestoreCmdWithDependencies(&configPath, restoreDependencies{
		loadConfig: func(string) (*config.Config, error) { return cfg, nil },
		newRepo:    func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return backup.Manifest{}, restic.Snapshot{}, false, nil
		},
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--dry-run"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "不存在 manifest") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestRestoreCommand_隔离执行不注入SafetyBackup(t *testing.T) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	configPath := "unused"
	dependencies := restoreDependencies{
		loadConfig:       func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:      func(string) (io.Closer, error) { return io.NopCloser(strings.NewReader("")), nil },
		runLocalDoctor:   func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		runRestoreDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
		newRepo:          func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		newRunner: func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		execute: func(
			_ context.Context,
			plan restore.Plan,
			_ *restic.Repo,
			_ sshexec.Runner,
			options restore.ExecuteOptions,
		) (restore.Result, error) {
			if plan.Isolation == nil || options.SafetyBackup != nil || options.Force {
				t.Fatalf("隔离执行选项错误: plan=%#v options=%#v", plan.Isolation, options)
			}
			return restore.Result{Status: store.StatusOK, Isolation: &restore.IsolationResult{
				ID:             plan.Isolation.ID,
				ProjectName:    plan.Isolation.ProjectName,
				CleanupCommand: "ark restore cleanup --host web-02 --isolation " + plan.Isolation.ID,
			}}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SetOut(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--to", "web-02", "--isolate", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("隔离 restore 失败: %v", err)
	}
}

func TestRestoreCleanupCommand_只加载清单锁和目标Runner(t *testing.T) {
	cfg, _, _ := testRestoreCommandInputs()
	configPath := "/etc/ark/test.yaml"
	isolationID := strings.Repeat("a", 64)
	var events []string
	dependencies := restoreDependencies{
		loadConfig: func(path string) (*config.Config, error) {
			events = append(events, "load:"+path)
			return cfg, nil
		},
		acquireLock: func(path string) (io.Closer, error) {
			events = append(events, "lock:"+path)
			return &restoreEventCloser{events: &events}, nil
		},
		newRunner: func(host *config.Host) (sshexec.Runner, error) {
			events = append(events, "runner:"+host.Host)
			return restoreNoopRunner{}, nil
		},
		cleanup: func(_ context.Context, _ sshexec.Runner, host string, gotID string) (restore.CleanupResult, error) {
			events = append(events, "cleanup:"+host)
			if gotID != isolationID {
				t.Fatalf("isolation ID = %q", gotID)
			}
			return restore.CleanupResult{
				IsolationID:     gotID,
				DestinationHost: host,
				Status:          store.StatusOK,
				Removed:         []string{"containers", "root:/var/lib/ark/restore/isolations/" + gotID},
			}, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"cleanup", "--host", "web-02", "--isolation", isolationID, "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("restore cleanup 失败: %v", err)
	}
	wantEvents := []string{"load:/etc/ark/test.yaml", "lock:/run/ark.lock", "runner:web-02", "cleanup:web-02", "unlock"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", events, wantEvents)
	}
	var result restore.CleanupResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Status != store.StatusOK {
		t.Fatalf("cleanup JSON = %s err=%v", output.String(), err)
	}
}

func TestRestoreCleanupCommand_无效参数零依赖调用(t *testing.T) {
	configPath := "unused"
	called := false
	dependencies := restoreDependencies{
		loadConfig: func(string) (*config.Config, error) {
			called = true
			return nil, nil
		},
	}
	cmd := newRestoreCmdWithDependencies(&configPath, dependencies)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"cleanup", "--host", "web-02", "--isolation", "short"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "64 位") || called {
		t.Fatalf("错误=%v called=%v", err, called)
	}
}

func TestPrintRestoreResult_输出隔离端口和清理命令(t *testing.T) {
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	result := restore.Result{Status: store.StatusOK, Isolation: &restore.IsolationResult{
		ID:          strings.Repeat("d", 64),
		ProjectName: "web-restore-dddddddddddd",
		Ports: []restore.IsolationPort{{
			Service: "api", HostIP: "127.0.0.1", AllocatedPort: "32768", Target: 8080, Protocol: "tcp",
		}},
		CleanupCommand: "ark restore cleanup --host web-02 --isolation " + strings.Repeat("d", 64),
	}}
	if err := printRestoreResult(cmd, result); err != nil {
		t.Fatalf("输出恢复结果失败: %v", err)
	}
	for _, want := range []string{"web-restore-dddddddddddd", "127.0.0.1:32768 -> 8080/tcp", "ark restore cleanup"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("输出缺少 %q:\n%s", want, output.String())
		}
	}
}
