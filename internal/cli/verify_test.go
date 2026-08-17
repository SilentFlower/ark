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
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
	"github.com/silentflower/ark/internal/verify"
)

type verifyEventCloser struct {
	events *[]string
	err    error
}

func (c *verifyEventCloser) Close() error {
	*c.events = append(*c.events, "unlock")
	return c.err
}

func testVerifyCommandInputs() (*config.Config, backup.Manifest, restic.Snapshot) {
	cfg, manifest, snapshot := testRestoreCommandInputs()
	second := manifest.Hosts[0]
	second.Host = "web-02"
	second.Targets = append([]backup.TargetResult(nil), second.Targets...)
	for index := range second.Targets {
		second.Targets[index].Host = second.Host
	}
	manifest.Hosts = append(manifest.Hosts, second)
	return cfg, manifest, snapshot
}

func verifyDoctorOK() *doctor.Report {
	return &doctor.Report{Checks: []doctor.Check{{Name: "ok", Status: doctor.StatusOK}}}
}

func TestVerifyCommand_全Host失败后继续且JSON纯净(t *testing.T) {
	cfg, manifest, snapshot := testVerifyCommandInputs()
	web01Manifest := manifest
	web01Manifest.RunID = "run-web-01"
	web01Manifest.Hosts = append([]backup.ManifestHost(nil), manifest.Hosts[:1]...)
	web01Snapshot := snapshot
	web01Snapshot.ID = "manifest-web-01"
	web02Manifest := manifest
	web02Manifest.RunID = "run-web-02"
	web02Manifest.Hosts = append([]backup.ManifestHost(nil), manifest.Hosts[1:]...)
	web02Snapshot := snapshot
	web02Snapshot.ID = "manifest-web-02"
	configPath := "/etc/ark/test.yaml"
	var events []string
	var doctorReports []store.DoctorReport
	dependencies := verifyDependencies{
		loadConfig: func(string) (*config.Config, error) {
			events = append(events, "load")
			return cfg, nil
		},
		acquireLock: func(path string) (io.Closer, error) {
			events = append(events, "lock:"+path)
			return &verifyEventCloser{events: &events}, nil
		},
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			events = append(events, "doctor:local")
			return verifyDoctorOK()
		},
		runHostDoctor: func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
			events = append(events, "doctor:"+host.Host)
			return verifyDoctorOK()
		},
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			events = append(events, "repo")
			return nil, nil
		},
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			t.Fatal("latest 全 host 不应调用单 manifest 选择")
			return backup.Manifest{}, restic.Snapshot{}, false, nil
		},
		loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
			events = append(events, "manifest:latest")
			return backup.LatestManifestSelections{
				Latest: backup.ManifestSelection{Manifest: web02Manifest, Snapshot: web02Snapshot},
				ByHost: map[string]backup.ManifestSelection{
					"web-01": {Manifest: web01Manifest, Snapshot: web01Snapshot},
					"web-02": {Manifest: web02Manifest, Snapshot: web02Snapshot},
				},
			}, true, nil
		},
		newRunner: func(host *config.Host) (sshexec.Runner, error) {
			events = append(events, "runner:"+host.Host)
			return restoreNoopRunner{}, nil
		},
		openStore: func(context.Context, string) (*store.Store, error) {
			events = append(events, "store:open")
			return &store.Store{}, nil
		},
		closeStore: func(*store.Store) error {
			events = append(events, "store:close")
			return nil
		},
		execute: func(
			_ context.Context,
			plan restore.Plan,
			_ *restic.Repo,
			_ sshexec.Runner,
			_ *store.Store,
			_ verify.Options,
		) (verify.Result, error) {
			events = append(events, "execute:"+plan.SourceHost)
			result := verify.Result{
				ID: "verify-" + plan.SourceHost, Host: plan.SourceHost, RunID: plan.RunID,
				ManifestSnapshotID: plan.ManifestSnapshotID, Status: store.StatusOK,
				Baseline: verify.BaselineEvidence{Differences: []string{}},
			}
			if plan.SourceHost == "web-01" {
				result.Status = store.StatusFail
				result.Error = "隔离恢复未完成"
				return result, errors.New("restore failed")
			}
			return result, nil
		},
		recordFailure: func(context.Context, *store.Store, verify.Failure) (verify.Result, error) {
			t.Fatal("测试不应记录前置失败")
			return verify.Result{}, nil
		},
		recordDoctor: func(_ context.Context, _ *store.Store, report store.DoctorReport) error {
			doctorReports = append(doctorReports, report)
			return nil
		},
		analyzeSchedule: func(_ context.Context, _ string, baseTime time.Time) (time.Time, error) {
			return baseTime.Add(24 * time.Hour), nil
		},
		now:       func() time.Time { return time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC) },
		statePath: "/var/lib/ark/ark.db",
	}
	cmd := newVerifyCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--json"})
	err := cmd.Execute()
	if !errors.Is(err, errVerifyFailed) {
		t.Fatalf("错误=%v", err)
	}
	var summary verifyCommandSummary
	if err := json.Unmarshal(output.Bytes(), &summary); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if summary.Status != store.StatusFail || len(summary.Results) != 2 ||
		summary.Results[0].RunID != "run-web-01" || summary.Results[0].ManifestSnapshotID != "manifest-web-01" ||
		summary.Results[1].RunID != "run-web-02" || summary.Results[1].ManifestSnapshotID != "manifest-web-02" {
		t.Fatalf("summary=%#v", summary)
	}
	wantEvents := []string{
		"load", "lock:/run/ark.lock", "repo", "manifest:latest", "store:open", "doctor:local",
		"doctor:web-01", "runner:web-01", "execute:web-01",
		"doctor:web-02", "runner:web-02", "execute:web-02", "store:close", "unlock",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events=%#v\nwant=%#v", events, wantEvents)
	}
	if len(doctorReports) != 3 || doctorReports[0].Scope != store.DoctorScopeLocal ||
		doctorReports[1].Scope != store.DoctorScopeHost || doctorReports[1].NextRunAt.IsZero() {
		t.Fatalf("doctor reports=%#v", doctorReports)
	}
	for _, secret := range []string{"secret-bucket", "restic-password", "source.invalid", "destination.invalid"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("JSON 泄漏 %q: %s", secret, output.String())
		}
	}
}

func TestVerifyCommand_显式Host不在Manifest时记录失败(t *testing.T) {
	cfg, manifest, snapshot := testVerifyCommandInputs()
	manifest.Hosts = manifest.Hosts[:1]
	configPath := "unused"
	var failure verify.Failure
	executed := false
	dependencies := verifyDependencies{
		loadConfig:     func(string) (*config.Config, error) { return cfg, nil },
		acquireLock:    func(string) (io.Closer, error) { return &verifyEventCloser{events: &[]string{}}, nil },
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report { return verifyDoctorOK() },
		runHostDoctor:  func(context.Context, *config.Config, *config.Host) *doctor.Report { return verifyDoctorOK() },
		newRepo:        func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
			t.Fatal("显式 snapshot 不应调用 latest 选择")
			return backup.LatestManifestSelections{}, false, nil
		},
		newRunner:  func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		openStore:  func(context.Context, string) (*store.Store, error) { return &store.Store{}, nil },
		closeStore: func(*store.Store) error { return nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, *store.Store, verify.Options) (verify.Result, error) {
			executed = true
			return verify.Result{}, nil
		},
		recordFailure: func(_ context.Context, _ *store.Store, value verify.Failure) (verify.Result, error) {
			failure = value
			return verify.Result{
				ID: "verify-preflight", Host: value.Host, RunID: value.RunID,
				ManifestSnapshotID: value.ManifestSnapshotID, Status: store.StatusFail, Error: value.Error,
			}, nil
		},
		statePath: "/var/lib/ark/ark.db",
	}
	cmd := newVerifyCmdWithDependencies(&configPath, dependencies)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-02", "--snapshot", snapshot.ID})
	err := cmd.Execute()
	if !errors.Is(err, errVerifyFailed) || executed || failure.Host != "web-02" ||
		!strings.Contains(output.String(), "所选 manifest 不包含该 host") {
		t.Fatalf("err=%v executed=%v failure=%#v output=%s", err, executed, failure, output.String())
	}
}

func TestVerifyCommand_锁冲突时不访问仓库(t *testing.T) {
	cfg, _, _ := testVerifyCommandInputs()
	configPath := "unused"
	repoCalled := false
	lockErr := errors.New("未等待锁")
	cmd := newVerifyCmdWithDependencies(&configPath, verifyDependencies{
		loadConfig:  func(string) (*config.Config, error) { return cfg, nil },
		acquireLock: func(string) (io.Closer, error) { return nil, lockErr },
		newRepo: func(*config.Repo) (*restic.Repo, error) {
			repoCalled = true
			return nil, nil
		},
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report { return nil },
		runHostDoctor:  func(context.Context, *config.Config, *config.Host) *doctor.Report { return nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return backup.Manifest{}, restic.Snapshot{}, false, nil
		},
		loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
			return backup.LatestManifestSelections{}, false, nil
		},
		newRunner:  func(*config.Host) (sshexec.Runner, error) { return nil, nil },
		openStore:  func(context.Context, string) (*store.Store, error) { return nil, nil },
		closeStore: func(*store.Store) error { return nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, *store.Store, verify.Options) (verify.Result, error) {
			return verify.Result{}, nil
		},
		recordFailure: func(context.Context, *store.Store, verify.Failure) (verify.Result, error) {
			return verify.Result{}, nil
		},
		statePath: "/var/lib/ark/ark.db",
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); !errors.Is(err, lockErr) || repoCalled {
		t.Fatalf("err=%v repoCalled=%v", err, repoCalled)
	}
}

func TestVerifyCommand_前置失败保留TargetEvidence(t *testing.T) {
	cfg, manifest, snapshot := testVerifyCommandInputs()
	manifest.Hosts = manifest.Hosts[:1]
	configPath := "unused"
	var failure verify.Failure
	dependencies := verifyDependencies{
		loadConfig:  func(string) (*config.Config, error) { return cfg, nil },
		acquireLock: func(string) (io.Closer, error) { return &verifyEventCloser{events: &[]string{}}, nil },
		runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
			return &doctor.Report{Checks: []doctor.Check{{Name: "docker", Status: doctor.StatusFail}}}
		},
		runHostDoctor: func(context.Context, *config.Config, *config.Host) *doctor.Report { return verifyDoctorOK() },
		newRepo:       func(*config.Repo) (*restic.Repo, error) { return nil, nil },
		loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
			return manifest, snapshot, true, nil
		},
		loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
			return backup.LatestManifestSelections{}, false, nil
		},
		newRunner:  func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
		openStore:  func(context.Context, string) (*store.Store, error) { return &store.Store{}, nil },
		closeStore: func(*store.Store) error { return nil },
		execute: func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, *store.Store, verify.Options) (verify.Result, error) {
			t.Fatal("前置失败不应执行恢复")
			return verify.Result{}, nil
		},
		recordFailure: func(_ context.Context, _ *store.Store, value verify.Failure) (verify.Result, error) {
			failure = value
			return verify.Result{ID: "verify-preflight", Host: value.Host, RunID: value.RunID,
				ManifestSnapshotID: value.ManifestSnapshotID, Targets: value.Targets,
				Status: store.StatusFail, Error: value.Error}, nil
		},
		statePath: "/var/lib/ark/ark.db",
	}
	cmd := newVerifyCmdWithDependencies(&configPath, dependencies)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--snapshot", snapshot.ID})
	if err := cmd.Execute(); !errors.Is(err, errVerifyFailed) {
		t.Fatalf("err=%v", err)
	}
	if len(failure.Targets) == 0 || failure.Targets[0].SnapshotID == "" {
		t.Fatalf("failure=%#v", failure)
	}
}

func TestVerifyCommand_Host级前置失败记录后继续下一Host(t *testing.T) {
	tests := []struct {
		name          string
		wantError     string
		mutateWeb01   func(*backup.Manifest)
		hostDoctor    func(string) *doctor.Report
		runnerForHost func(string) (sshexec.Runner, error)
	}{
		{
			name:      "BuildPlan 漂移",
			wantError: "manifest 与当前清单无法构建完整恢复计划",
			mutateWeb01: func(manifest *backup.Manifest) {
				manifest.Hosts[0].Targets[0].TargetID = "files/missing"
			},
		},
		{
			name:      "host doctor 失败",
			wantError: "host 环境检查未通过",
			hostDoctor: func(host string) *doctor.Report {
				if host == "web-01" {
					return &doctor.Report{Checks: []doctor.Check{{Name: "docker", Status: doctor.StatusFail}}}
				}
				return verifyDoctorOK()
			},
		},
		{
			name:      "Runner 创建失败",
			wantError: "创建 host 执行器失败",
			runnerForHost: func(host string) (sshexec.Runner, error) {
				if host == "web-01" {
					return nil, errors.New("runner failed")
				}
				return restoreNoopRunner{}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, manifest, snapshot := testVerifyCommandInputs()
			web01Manifest := manifest
			web01Manifest.RunID = "run-web-01"
			web01Manifest.Hosts = append([]backup.ManifestHost(nil), manifest.Hosts[:1]...)
			web01Manifest.Hosts[0].Targets = append([]backup.TargetResult(nil), web01Manifest.Hosts[0].Targets...)
			if test.mutateWeb01 != nil {
				test.mutateWeb01(&web01Manifest)
			}
			web02Manifest := manifest
			web02Manifest.RunID = "run-web-02"
			web02Manifest.Hosts = append([]backup.ManifestHost(nil), manifest.Hosts[1:]...)
			web01Snapshot := snapshot
			web01Snapshot.ID = "manifest-web-01"
			web02Snapshot := snapshot
			web02Snapshot.ID = "manifest-web-02"

			var failures []verify.Failure
			var executed []string
			configPath := "unused"
			dependencies := verifyDependencies{
				loadConfig:  func(string) (*config.Config, error) { return cfg, nil },
				acquireLock: func(string) (io.Closer, error) { return &verifyEventCloser{events: &[]string{}}, nil },
				runLocalDoctor: func(context.Context, *config.Config) *doctor.Report {
					return verifyDoctorOK()
				},
				runHostDoctor: func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
					if test.hostDoctor != nil {
						return test.hostDoctor(host.Host)
					}
					return verifyDoctorOK()
				},
				newRepo: func(*config.Repo) (*restic.Repo, error) { return nil, nil },
				loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
					t.Fatal("latest 全 host 不应调用单 manifest 选择")
					return backup.Manifest{}, restic.Snapshot{}, false, nil
				},
				loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
					return backup.LatestManifestSelections{
						Latest: backup.ManifestSelection{Manifest: web02Manifest, Snapshot: web02Snapshot},
						ByHost: map[string]backup.ManifestSelection{
							"web-01": {Manifest: web01Manifest, Snapshot: web01Snapshot},
							"web-02": {Manifest: web02Manifest, Snapshot: web02Snapshot},
						},
					}, true, nil
				},
				newRunner: func(host *config.Host) (sshexec.Runner, error) {
					if test.runnerForHost != nil {
						return test.runnerForHost(host.Host)
					}
					return restoreNoopRunner{}, nil
				},
				openStore:  func(context.Context, string) (*store.Store, error) { return &store.Store{}, nil },
				closeStore: func(*store.Store) error { return nil },
				execute: func(_ context.Context, plan restore.Plan, _ *restic.Repo, _ sshexec.Runner, _ *store.Store, _ verify.Options) (verify.Result, error) {
					executed = append(executed, plan.SourceHost)
					return verify.Result{
						ID: "verify-" + plan.SourceHost, Host: plan.SourceHost, RunID: plan.RunID,
						ManifestSnapshotID: plan.ManifestSnapshotID, Status: store.StatusOK,
						Baseline: verify.BaselineEvidence{Differences: []string{}},
					}, nil
				},
				recordFailure: func(_ context.Context, _ *store.Store, failure verify.Failure) (verify.Result, error) {
					failures = append(failures, failure)
					return verify.Result{
						ID: "verify-preflight", Host: failure.Host, RunID: failure.RunID,
						ManifestSnapshotID: failure.ManifestSnapshotID, Targets: failure.Targets,
						Status: store.StatusFail, Error: failure.Error,
					}, nil
				},
				statePath: "/var/lib/ark/ark.db",
			}
			cmd := newVerifyCmdWithDependencies(&configPath, dependencies)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{"--json"})

			if err := cmd.Execute(); !errors.Is(err, errVerifyFailed) {
				t.Fatalf("err=%v output=%s", err, output.String())
			}
			if len(failures) != 1 || failures[0].Host != "web-01" || failures[0].Error != test.wantError ||
				len(failures[0].Targets) == 0 || failures[0].Targets[0].SnapshotID == "" ||
				!reflect.DeepEqual(executed, []string{"web-02"}) {
				t.Fatalf("failures=%#v executed=%#v", failures, executed)
			}
		})
	}
}

func TestRemovedHostFailures_审计全部已选Manifest且去重(t *testing.T) {
	cfg, manifest, snapshot := testVerifyCommandInputs()
	cfg.Hosts = cfg.Hosts[:2]
	removed := manifest.Hosts[0]
	removed.Host = "old-host"
	for index := range removed.Targets {
		removed.Targets[index].Host = removed.Host
	}
	latestManifest := manifest
	latestManifest.RunID = "run-latest"
	latestManifest.Hosts = []backup.ManifestHost{manifest.Hosts[1]}
	latestSnapshot := snapshot
	latestSnapshot.ID = "manifest-latest"
	latestSnapshot.Time = snapshot.Time.Add(time.Hour)
	olderManifest := manifest
	olderManifest.RunID = "run-older"
	olderManifest.Hosts = []backup.ManifestHost{manifest.Hosts[0], removed}
	olderSnapshot := snapshot
	olderSnapshot.ID = "manifest-older"
	failures := removedHostFailures(
		cfg,
		backup.ManifestSelection{Manifest: latestManifest, Snapshot: latestSnapshot},
		[]verifyHostSelection{
			{host: &cfg.Hosts[0], manifest: olderManifest, snapshot: olderSnapshot},
			{host: &cfg.Hosts[1], manifest: latestManifest, snapshot: latestSnapshot},
		},
	)
	if len(failures) != 1 || failures[0].Host != "old-host" || failures[0].RunID != "run-older" ||
		failures[0].ManifestSnapshotID != "manifest-older" || len(failures[0].Targets) == 0 {
		t.Fatalf("failures=%#v", failures)
	}
}

func TestVerifyCommand_Writer错误保留失败哨兵并聚合收尾错误(t *testing.T) {
	closeErr := errors.New("close store failed")
	unlockErr := errors.New("unlock failed")
	writerErr := errors.New("writer failed")
	tests := []struct {
		name      string
		closeErr  error
		unlockErr error
		want      []error
	}{
		{name: "仅 writer 失败", want: []error{errVerifyFailed, writerErr}},
		{name: "writer 与收尾同时失败", closeErr: closeErr, unlockErr: unlockErr, want: []error{errVerifyFailed, closeErr, unlockErr, writerErr}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, manifest, snapshot := testVerifyCommandInputs()
			manifest.Hosts = manifest.Hosts[:1]
			configPath := "unused"
			dependencies := verifyDependencies{
				loadConfig: func(string) (*config.Config, error) { return cfg, nil },
				acquireLock: func(string) (io.Closer, error) {
					return &verifyEventCloser{events: &[]string{}, err: test.unlockErr}, nil
				},
				runLocalDoctor: func(context.Context, *config.Config) *doctor.Report { return verifyDoctorOK() },
				runHostDoctor:  func(context.Context, *config.Config, *config.Host) *doctor.Report { return verifyDoctorOK() },
				newRepo:        func(*config.Repo) (*restic.Repo, error) { return nil, nil },
				loadManifest: func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error) {
					return manifest, snapshot, true, nil
				},
				loadLatest: func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error) {
					return backup.LatestManifestSelections{}, false, nil
				},
				newRunner:  func(*config.Host) (sshexec.Runner, error) { return restoreNoopRunner{}, nil },
				openStore:  func(context.Context, string) (*store.Store, error) { return &store.Store{}, nil },
				closeStore: func(*store.Store) error { return test.closeErr },
				execute: func(_ context.Context, plan restore.Plan, _ *restic.Repo, _ sshexec.Runner, _ *store.Store, _ verify.Options) (verify.Result, error) {
					return verify.Result{
						ID: "verify-ok", Host: plan.SourceHost, RunID: plan.RunID,
						ManifestSnapshotID: plan.ManifestSnapshotID, Status: store.StatusOK,
						Baseline: verify.BaselineEvidence{Differences: []string{}},
					}, nil
				},
				recordFailure: func(context.Context, *store.Store, verify.Failure) (verify.Result, error) {
					t.Fatal("测试不应记录前置失败")
					return verify.Result{}, nil
				},
				statePath: "/var/lib/ark/ark.db",
			}
			cmd := newVerifyCmdWithDependencies(&configPath, dependencies)
			cmd.SetOut(restoreErrorWriter{err: writerErr})
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{"--host", "web-01", "--snapshot", snapshot.ID, "--json"})
			err := cmd.Execute()
			for _, expected := range test.want {
				if !errors.Is(err, expected) {
					t.Fatalf("错误链 %v 未包含 %v", err, expected)
				}
			}
		})
	}
}
