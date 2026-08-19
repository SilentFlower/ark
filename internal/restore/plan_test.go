package restore

import (
	"encoding/json"
	"reflect"
	"slices"
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
					"worker": "registry/worker@sha256:2222222222222222222222222222222222222222222222222222222222222222",
					"api":    "registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111",
				}, ComposeMetadata: &backup.ComposeMetadata{PublishedPorts: []backup.PublishedPort{
					{Service: "api", Published: "8080", Target: 8080, Protocol: "tcp", AppProtocol: "http", Mode: "ingress"},
					{Service: "api", HostIP: "127.0.0.1", Published: "5353", Target: 5353, Protocol: "udp"},
				}}},
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
	if plan.Steps[1].ImageDigests["api"] != "registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" {
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
	for _, forbidden := range []string{"ComposeFile", "IdentityFile", "KnownHostsFile", "PasswordFile", "compose_metadata"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("JSON 不应包含字段 %q: %s", forbidden, text)
		}
	}

	// Plan 必须与后续配置或 manifest 变更隔离，避免 dry-run 审核后内容被引用修改。
	cfg.Hosts[0].Targets[1].Paths[0] = "/changed"
	manifest.Hosts[0].Targets[2].ImageDigests["api"] = "changed"
	manifest.Hosts[0].Targets[2].ComposeMetadata.PublishedPorts[0].Published = "changed"
	if plan.Steps[0].Target.Paths[0] != "/srv/app/compose.yaml" {
		t.Fatalf("Plan target 未深拷贝: %#v", plan.Steps[0].Target.Paths)
	}
	if plan.Steps[1].ImageDigests["api"] != "registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("Plan digest 未深拷贝: %#v", plan.Steps[1].ImageDigests)
	}
	if plan.Steps[1].composeMetadata.PublishedPorts[0].Published != "8080" {
		t.Fatalf("Plan Compose 元数据未深拷贝: %#v", plan.Steps[1].composeMetadata)
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

func TestBuildPlan_DNS计划只用于跨机原位恢复且深拷贝(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	cfg.Hosts[1].DNSMgr = &config.HostDNSMgr{
		Value: "203.0.113.10",
		Records: []config.DNSMgrRecord{
			{DomainID: 12, RecordID: "record-a"},
			{DomainID: 12, RecordID: "record-b"},
		},
	}
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.DNS == nil || plan.DNS.Value != "203.0.113.10" || len(plan.DNS.Records) != 2 {
		t.Fatalf("DNS 计划 = %#v", plan.DNS)
	}
	if len(plan.ManualChecks) != 4 || slices.Contains(plan.ManualChecks, manualCheckDNS) ||
		!slices.Contains(plan.ManualChecks, manualCheckDMonitor) {
		t.Fatalf("自动 DNS 计划的人工项 = %#v", plan.ManualChecks)
	}
	cfg.Hosts[1].DNSMgr.Value = "198.51.100.20"
	cfg.Hosts[1].DNSMgr.Records[0].RecordID = "changed"
	if plan.DNS.Value != "203.0.113.10" || plan.DNS.Records[0].RecordID != "record-a" {
		t.Fatalf("DNS 计划未深拷贝: %#v", plan.DNS)
	}

	sameHost, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "source-01")
	if err != nil {
		t.Fatalf("同机 BuildPlan 失败: %v", err)
	}
	if sameHost.DNS != nil {
		t.Fatalf("同机恢复不应包含 DNS 计划: %#v", sameHost.DNS)
	}
}

func TestBuildPlan_维护计划用于同机和跨机原位恢复且深拷贝(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	cfg.Hosts[0].DNSMgr = &config.HostDNSMgr{TaskIDs: []int64{7}}
	cfg.Hosts[1].DNSMgr = &config.HostDNSMgr{TaskIDs: []int64{21, 34}}

	crossHost, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("跨机 BuildPlan 失败: %v", err)
	}
	if crossHost.Maintenance == nil || !slices.Equal(crossHost.Maintenance.TaskIDs, []int64{21, 34}) {
		t.Fatalf("跨机维护计划 = %#v", crossHost.Maintenance)
	}
	if slices.Contains(crossHost.ManualChecks, manualCheckDMonitor) {
		t.Fatalf("自动维护计划不应保留人工暂停项: %#v", crossHost.ManualChecks)
	}

	sameHost, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "source-01")
	if err != nil {
		t.Fatalf("同机 BuildPlan 失败: %v", err)
	}
	if sameHost.Maintenance == nil || !slices.Equal(sameHost.Maintenance.TaskIDs, []int64{7}) {
		t.Fatalf("同机维护计划 = %#v", sameHost.Maintenance)
	}

	cfg.Hosts[1].DNSMgr.TaskIDs[0] = 99
	if crossHost.Maintenance.TaskIDs[0] != 21 {
		t.Fatalf("维护计划未深拷贝: %#v", crossHost.Maintenance)
	}
}

func TestWithIsolation_清除DNS计划且不修改原计划(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	cfg.Hosts[1].DNSMgr = &config.HostDNSMgr{
		TaskIDs: []int64{21},
		Value:   "203.0.113.10",
		Records: []config.DNSMgrRecord{{DomainID: 12, RecordID: "record-a"}},
	}
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	isolated, err := WithIsolation(plan)
	if err != nil {
		t.Fatalf("WithIsolation 失败: %v", err)
	}
	if isolated.DNS != nil || isolated.Maintenance != nil || plan.DNS == nil || plan.Maintenance == nil {
		t.Fatalf("隔离 dnsmgr 边界错误: isolated=%#v/%#v original=%#v/%#v",
			isolated.DNS, isolated.Maintenance, plan.DNS, plan.Maintenance)
	}
	if slices.Contains(isolated.ManualChecks, manualCheckDMonitor) || slices.Contains(isolated.ManualChecks, manualCheckDNS) {
		t.Fatalf("隔离计划不应保留 dmonitor/DNS 人工项: %#v", isolated.ManualChecks)
	}
}

func TestBuildPlan_一次性报告清单与Manifest漂移(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	cfg.Hosts[1].Project.ComposeFile = "/srv/other/compose.yaml"
	cfg.Hosts[1].Targets[1].Paths[0] = "/srv/other/compose.yaml"
	cfg.Hosts[1].Targets[3].Services = []string{"api"}
	manifest.Hosts[0].Targets[1].Status = store.StatusFail
	manifest.Hosts[0].Targets[2].ImageDigests = map[string]string{"api": "registry/api@sha256:1111111111111111111111111111111111111111111111111111111111111111"}
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

func TestBuildPlan_拒绝可变Tag和畸形Digest(t *testing.T) {
	tests := []struct {
		name   string
		digest string
	}{
		{name: "可变 tag", digest: "registry/api:latest"},
		{name: "过短 sha256", digest: "registry/api@sha256:1234"},
		{name: "非十六进制 sha256", digest: "registry/api@sha256:" + strings.Repeat("z", 64)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, manifest := testRestoreInputs()
			manifest.Hosts[0].Targets[2].ImageDigests["api"] = tc.digest
			_, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
			if err == nil || !strings.Contains(err.Error(), "64位十六进制") {
				t.Fatalf("错误 = %v", err)
			}
		})
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

func TestWithIsolation_稳定派生项目路径和资源(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	isolated, err := WithIsolation(plan)
	if err != nil {
		t.Fatalf("WithIsolation 失败: %v", err)
	}
	again, err := WithIsolation(plan)
	if err != nil {
		t.Fatalf("再次 WithIsolation 失败: %v", err)
	}
	if isolated.Isolation == nil || isolated.Isolation.ID != again.Isolation.ID || len(isolated.Isolation.ID) != 64 {
		t.Fatalf("isolation ID 不稳定: %#v %#v", isolated.Isolation, again.Isolation)
	}
	if isolated.Project.ProjectName != "app-prod-restore-"+isolated.Isolation.ShortID {
		t.Fatalf("project name = %q", isolated.Project.ProjectName)
	}
	if isolated.Project.ComposeFile != isolated.Isolation.GeneratedComposeFile || isolated.Project.EnvFile != "" {
		t.Fatalf("隔离 project = %#v", isolated.Project)
	}
	if len(isolated.Isolation.Ports) != 2 || isolated.Isolation.Ports[0].AllocatedPort != "auto" ||
		isolated.Isolation.Ports[1].Protocol != "udp" {
		t.Fatalf("隔离端口映射 = %#v", isolated.Isolation.Ports)
	}
	if isolated.Steps[0].Target.Paths[0] != isolated.Isolation.FilesRoot+"/srv/app/compose.yaml" {
		t.Fatalf("files 路径 = %#v", isolated.Steps[0].Target.Paths)
	}
	if isolated.Steps[2].Target.Name != "uploads-restore-"+isolated.Isolation.ShortID {
		t.Fatalf("volume 名 = %q", isolated.Steps[2].Target.Name)
	}
	if plan.Steps[0].Target.Paths[0] != "/srv/app/compose.yaml" || plan.Steps[2].Target.Name != "uploads" {
		t.Fatalf("原 Plan 被修改: %#v", plan)
	}
}

func TestWithIsolation_旧备份缺少Compose元数据时明确拒绝(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	manifest.Hosts[0].Targets[2].ComposeMetadata = nil
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	_, err = WithIsolation(plan)
	if err == nil || !strings.Contains(err.Error(), "重新执行 backup") {
		t.Fatalf("错误 = %v", err)
	}
}

func TestWithIsolation_拒绝暂不支持或重复的备份端口声明(t *testing.T) {
	tests := []struct {
		name  string
		ports []backup.PublishedPort
	}{
		{
			name: "SCTP",
			ports: []backup.PublishedPort{
				{Service: "api", Published: "8080", Target: 8080, Protocol: "sctp"},
			},
		},
		{
			name: "重复",
			ports: []backup.PublishedPort{
				{Service: "api", Published: "8080", Target: 8080, Protocol: "tcp"},
				{Service: "api", Published: "18080", Target: 8080, Protocol: "tcp"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, manifest := testRestoreInputs()
			manifest.Hosts[0].Targets[2].ComposeMetadata.PublishedPorts = tc.ports
			plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			if _, err := WithIsolation(plan); err == nil {
				t.Fatal("WithIsolation 应拒绝无法稳定映射的端口声明")
			}
		})
	}
}

func TestWithIsolation_要求Compose和Env进入Files快照(t *testing.T) {
	cfg, manifest := testRestoreInputs()
	plan, err := BuildPlan(cfg, manifest, "manifest-snapshot", "source-01", "destination-01")
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   string
	}{
		{name: "compose 缺失", mutate: func(p *Plan) { p.Steps[0].Target.Paths = []string{"/srv/app/.env"} }, want: "compose_file"},
		{name: "env 缺失", mutate: func(p *Plan) { p.Steps[0].Target.Paths = []string{"/srv/app/compose.yaml"} }, want: "env_file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := copyPlan(plan)
			tc.mutate(&candidate)
			_, err := WithIsolation(candidate)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestIsolationNames_限制长度并保持合法前缀(t *testing.T) {
	projectName := isolationProjectName(strings.Repeat("项目-ABC_", 20), IsolationPurposeRestore, "123456789abc")
	if len(projectName) > isolationProjectMaximumLength ||
		!isIsolationProjectStart(projectName[0]) || isolationProjectCharacter.MatchString(projectName) {
		t.Fatalf("project name = %q，长度=%d", projectName, len(projectName))
	}
	resourceName := isolationResourceName(strings.Repeat("v", 300), IsolationPurposeRestore, "123456789abc")
	if len(resourceName) > isolationResourceMaximumLength || !strings.HasSuffix(resourceName, "-restore-123456789abc") {
		t.Fatalf("resource name 长度=%d，值=%q", len(resourceName), resourceName)
	}
}

func TestPlanPathCovered_父目录快照覆盖子路径但反向不成立(t *testing.T) {
	plan := Plan{Steps: []Step{{
		Phase:  PhaseFiles,
		Target: &Target{Paths: []string{"/srv/app"}},
	}}}
	if !planPathCovered(plan, "/srv/app/compose.yaml") {
		t.Fatal("父目录 files target 应覆盖 compose 子路径")
	}
	plan.Steps[0].Target.Paths = []string{"/srv/app/compose.yaml"}
	if planPathCovered(plan, "/srv/app") {
		t.Fatal("单文件快照不应覆盖其父目录")
	}
}

func TestSelectIsolationPortBinding_双栈同端口可归一化(t *testing.T) {
	binding, err := selectIsolationPortBinding("", []isolationPortBinding{
		{HostIP: "0.0.0.0", HostPort: "32768"},
		{HostIP: "::", HostPort: "32768"},
	})
	if err != nil || binding.HostPort != "32768" {
		t.Fatalf("binding=%#v err=%v", binding, err)
	}
	_, err = selectIsolationPortBinding("", []isolationPortBinding{
		{HostIP: "0.0.0.0", HostPort: "32768"},
		{HostIP: "::", HostPort: "32769"},
	})
	if err == nil || !strings.Contains(err.Error(), "多个不同") {
		t.Fatalf("错误=%v", err)
	}
}
