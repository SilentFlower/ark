package restore

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/store"
)

func testRestoreInputs() (*config.Config, backup.Manifest) {
	targets := []config.Target{
		{Type: config.TargetPostgres, Service: "db", Database: "app", User: "postgres"},
		{Type: config.TargetFiles, Name: "config", Paths: []string{"/srv/app/compose.yaml", "/srv/app/.env"}},
		{Type: config.TargetRedis, Service: "redis"},
		{Type: config.TargetImageDigest, Services: []string{"api", "worker"}},
		{Type: config.TargetVolume, Name: "uploads"},
	}
	project := config.Project{
		Name:        "app",
		ComposeFile: "/srv/app/compose.yaml",
		EnvFile:     "/srv/app/.env",
		ProjectName: "app-prod",
	}
	cfg := &config.Config{Hosts: []config.Host{
		{
			Host:    "source-01",
			SSH:     &config.SSH{Address: "source.example:22", User: "root"},
			Project: project,
			Targets: cloneConfigTargets(targets),
		},
		{
			Host:    "destination-01",
			SSH:     &config.SSH{Address: "destination.example:22", User: "root"},
			Project: project,
			Targets: cloneConfigTargets(targets),
		},
	}}
	startedAt := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)
	manifest := backup.Manifest{
		SchemaVersion: backup.ManifestSchemaVersion,
		RunID:         "run-p3",
		ArkVersion:    "v0.3.0",
		StartedAt:     startedAt,
		FinishedAt:    startedAt.Add(time.Minute),
		Hosts: []backup.ManifestHost{{
			Host: "source-01",
			Targets: []backup.TargetResult{
				{Host: "source-01", TargetID: "volume/uploads", TargetType: config.TargetVolume, Status: store.StatusOK, SnapshotID: "snapshot-volume"},
				{Host: "source-01", TargetID: "postgres/db/app", TargetType: config.TargetPostgres, Status: store.StatusOK, SnapshotID: "snapshot-postgres"},
				{Host: "source-01", TargetID: "image_digest", TargetType: config.TargetImageDigest, Status: store.StatusOK, SnapshotID: "snapshot-image", ImageDigests: map[string]string{
					"worker": "registry/worker@sha256:222",
					"api":    "registry/api@sha256:111",
				}},
				{Host: "source-01", TargetID: "files/config", TargetType: config.TargetFiles, Status: store.StatusOK, SnapshotID: "snapshot-files"},
				{Host: "source-01", TargetID: "redis/redis", TargetType: config.TargetRedis, Status: store.StatusWarn, SnapshotID: "snapshot-redis", Error: "需要复核"},
			},
		}},
	}
	return cfg, manifest
}

func cloneConfigTargets(targets []config.Target) []config.Target {
	cloned := make([]config.Target, len(targets))
	for index, target := range targets {
		cloned[index] = target
		cloned[index].Paths = append([]string(nil), target.Paths...)
		cloned[index].Services = append([]string(nil), target.Services...)
	}
	return cloned
}

func TestBuildPlan_生成完整稳定的跨主机计划(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.ManifestSnapshotID != "manifest-snapshot" || plan.RunID != "run-p3" {
		t.Fatalf("manifest 标识 = (%q, %q)", plan.ManifestSnapshotID, plan.RunID)
	}
	if plan.SourceHost != "source-01" || plan.DestinationHost != "destination-01" {
		t.Fatalf("host 选择 = (%q, %q)", plan.SourceHost, plan.DestinationHost)
	}
	wantPhases := []Phase{
		PhaseFiles,
		PhaseImageDigest,
		PhaseVolume,
		PhaseDatabasePrepare,
		PhaseDatabasePrepare,
		PhaseDatabaseData,
		PhaseDatabaseData,
		PhaseApplication,
		PhaseHealth,
	}
	gotPhases := make([]Phase, len(plan.Steps))
	for index, step := range plan.Steps {
		gotPhases[index] = step.Phase
	}
	if !reflect.DeepEqual(gotPhases, wantPhases) {
		t.Fatalf("阶段 = %#v，期望 %#v", gotPhases, wantPhases)
	}
	if plan.Steps[0].SnapshotID != "snapshot-files" || plan.Steps[1].SnapshotID != "snapshot-image" {
		t.Fatalf("前两步快照 = %#v", plan.Steps[:2])
	}
	if plan.Steps[3].SnapshotID != "" || plan.Steps[5].SnapshotID != "snapshot-postgres" {
		t.Fatalf("数据库 prepare/data 快照语义错误: %#v %#v", plan.Steps[3], plan.Steps[5])
	}
	if plan.Steps[1].ImageDigests["api"] != "registry/api@sha256:111" {
		t.Fatalf("image digest = %#v", plan.Steps[1].ImageDigests)
	}
	if len(plan.ManualChecks) != 5 || plan.ConflictPolicy != defaultConflictPolicy {
		t.Fatalf("人工项或冲突策略错误: %#v %q", plan.ManualChecks, plan.ConflictPolicy)
	}

	firstJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Plan JSON 编码失败: %v", err)
	}
	secondJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Plan 再次 JSON 编码失败: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("同一 Plan JSON 不稳定:\n%s\n%s", firstJSON, secondJSON)
	}
	text := string(firstJSON)
	for _, field := range []string{"manifest_snapshot_id", "destination_host", "compose_file", "image_digests"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Errorf("JSON 缺少 snake_case 字段 %q: %s", field, text)
		}
	}
	for _, forbidden := range []string{"ComposeFile", "IdentityFile", "KnownHostsFile", "PasswordFile"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("JSON 不应包含字段 %q: %s", forbidden, text)
		}
	}

	// Plan 必须与后续配置或 manifest 变更隔离，避免 dry-run 审核后内容被引用修改。
	cfg.Hosts[0].Targets[1].Paths[0] = "/changed"
	manifest.Hosts[0].Targets[2].ImageDigests["api"] = "changed"
	if plan.Steps[0].Target.Paths[0] != "/srv/app/compose.yaml" {
		t.Fatalf("Plan target 未深拷贝: %#v", plan.Steps[0].Target.Paths)
	}
	if plan.Steps[1].ImageDigests["api"] != "registry/api@sha256:111" {
		t.Fatalf("Plan digest 未深拷贝: %#v", plan.Steps[1].ImageDigests)
	}
}

func TestBuildPlan_同名恢复默认目标为来源(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.DestinationHost != "source-01" {
		t.Fatalf("destination = %q", plan.DestinationHost)
	}
}

func TestBuildPlan_一次性报告清单与Manifest漂移(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	cfg.Hosts[1].Project.ComposeFile = "/srv/other/compose.yaml"
	cfg.Hosts[1].Targets[1].Paths[0] = "/srv/other/compose.yaml"
	cfg.Hosts[1].Targets[3].Services = []string{"api"}
	manifest.Hosts[0].Targets[1].Status = store.StatusFail
	manifest.Hosts[0].Targets[2].ImageDigests = map[string]string{"api": "registry/api@sha256:111"}
	manifest.Hosts[0].Targets = manifest.Hosts[0].Targets[:4]

	_, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err == nil {
		t.Fatal("存在多处漂移时应拒绝构建计划")
	}
	for _, want := range []string{
		"project.compose_file",
		"targets[files/config].paths",
		"targets[image_digest].services",
		"manifest targets[postgres/db/app].status",
		"image_digests[worker]",
		`缺少 target "redis/redis"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("聚合错误缺少 %q:\n%v", want, err)
		}
	}
}

func TestBuildPlan_拒绝未知Host与无效参数(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	tests := []struct {
		name        string
		cfg         *config.Config
		snapshotID  string
		source      string
		destination string
		want        string
	}{
		{name: "nil config", cfg: nil, snapshotID: "s1", source: "source-01", destination: "source-01", want: "config"},
		{name: "空 manifest snapshot", cfg: cfg, source: "source-01", destination: "source-01", want: "manifest_snapshot_id"},
		{name: "未知 source", cfg: cfg, snapshotID: "s1", source: "missing", destination: "destination-01", want: "source_host"},
		{name: "未知 destination", cfg: cfg, snapshotID: "s1", source: "source-01", destination: "missing", want: "destination_host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(tc.cfg, manifest, tc.snapshotID, tc.source, tc.destination)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}
