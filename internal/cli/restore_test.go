package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/store"
)

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
				{Host: "web-01", TargetID: "image_digest", TargetType: config.TargetImageDigest, Status: store.StatusOK, SnapshotID: "image-snapshot", ImageDigests: map[string]string{"api": "registry.invalid/api@sha256:111"}},
				{Host: "web-01", TargetID: "postgres/db/app", TargetType: config.TargetPostgres, Status: store.StatusOK, SnapshotID: "postgres-snapshot"},
			},
		}},
	}
	snapshot := restic.Snapshot{ID: "manifest-snapshot"}
	return cfg, manifest, snapshot
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
		"registry.invalid/api@sha256:111",
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
		{name: "缺少 dry-run", args: []string{"--host", "web-01"}, want: "仅支持 --dry-run"},
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
