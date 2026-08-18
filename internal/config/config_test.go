package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validManifest 是一份完整合法的多机清单，各用例在它的基础上做最小改动。
// 三台机器覆盖了三种形态：hub 自身（local）、普通远程机、带 per-host 覆盖的远程机。
const validManifest = `
version: 2
repo:
  type: restic
  url: "s3:https://example.r2.cloudflarestorage.com/backup"
  password_file: /etc/ark/repo.pass
  env_file: /etc/ark/repo.env
defaults:
  schedule:
    on_calendar: "*-*-* 04:17:00"
  retention:
    daily: 7
    weekly: 4
    monthly: 6
monitoring:
  env_file: /etc/ark/monitoring.env
hosts:
  - host: hub-01
    local: true
    project:
      name: ark-hub
      compose_file: /root/ark-hub/docker-compose.yml
    targets:
      - type: files
        name: ark-state
        paths:
          - /var/lib/ark/ark.db
  - host: web-01
    ssh:
      address: 10.0.0.11:22
      user: root
      identity_file: /etc/ark/keys/web-01.key
      known_hosts_file: /etc/ark/known_hosts
    project:
      name: sub2api
      compose_file: /root/sub2api/deploy/docker-compose.yml
      env_file: /root/sub2api/deploy/.env
      project_name: deploy
    targets:
      - type: postgres
        service: postgres
        database: sub2api
        user: sub2api
      - type: redis
        service: redis
      - type: volume
        name: sub2api_data
      - type: files
        name: deploy-config
        paths:
          - /root/sub2api/deploy/docker-compose.yml
          - /root/sub2api/deploy/.env
      - type: image_digest
        services: [sub2api]
  - host: db-01
    ssh:
      address: 10.0.0.12:22
      user: root
      identity_file: /etc/ark/keys/db-01.key
      known_hosts_file: /etc/ark/known_hosts
    project:
      name: pgcluster
      compose_file: /srv/pgcluster/docker-compose.yml
    schedule:
      on_calendar: "*-*-* 00,06,12,18:23:00"
    retention:
      daily: 30
      weekly: 8
      monthly: 12
    targets:
      - type: postgres
        service: postgres
        database: app
`

// writeManifest 把清单内容写入临时文件并返回路径。
func writeManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ark.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时清单失败: %v", err)
	}
	return path
}

// hostByName 从清单里取出指定机器，找不到直接失败。
func hostByName(t *testing.T, cfg *Config, name string) *Host {
	t.Helper()
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Host == name {
			return &cfg.Hosts[i]
		}
	}
	t.Fatalf("清单里没有 host %q", name)
	return nil
}

func TestLoadAndValidate_Valid(t *testing.T) {
	cfg, err := LoadAndValidate(writeManifest(t, validManifest))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}
	if len(cfg.Hosts) != 3 {
		t.Fatalf("hosts 数量 = %d, 期望 3", len(cfg.Hosts))
	}
	if cfg.Monitoring == nil || cfg.Monitoring.EnvFile != "/etc/ark/monitoring.env" {
		t.Fatalf("monitoring 配置 = %#v，期望读取受限秘密文件路径", cfg.Monitoring)
	}

	hub := hostByName(t, cfg, "hub-01")
	if !hub.Local {
		t.Error("hub-01 应该是 local")
	}
	if hub.SSH != nil {
		t.Error("local host 不应该有 ssh 配置")
	}

	web := hostByName(t, cfg, "web-01")
	if web.Local {
		t.Error("web-01 不应该是 local")
	}
	if web.SSH == nil || web.SSH.Address != "10.0.0.11:22" {
		t.Errorf("web-01 的 ssh 配置 = %+v, 期望 address 为 10.0.0.11:22", web.SSH)
	}
	if got := web.SSH.EffectiveHostKeyPolicy(); got != SSHHostKeyPolicyAcceptNew {
		t.Errorf("web-01 主机密钥策略 = %q, 期望默认值 %q", got, SSHHostKeyPolicyAcceptNew)
	}
	if len(web.Targets) != 5 {
		t.Errorf("web-01 targets 数量 = %d, 期望 5", len(web.Targets))
	}
}

func TestExampleManifest_Hub自备份明确排除密钥(t *testing.T) {
	examplePath := filepath.Join("..", "..", "examples", "ark.yaml")
	cfg, err := LoadAndValidate(examplePath)
	if err != nil {
		t.Fatalf("示例清单校验失败: %v", err)
	}
	hub := hostByName(t, cfg, "hub-01")

	paths := make(map[string]bool)
	var stateTarget *Target
	for i := range hub.Targets {
		target := &hub.Targets[i]
		for _, path := range target.Paths {
			paths[path] = true
		}
		if target.Type == TargetFiles && target.Name == "ark-state" {
			stateTarget = target
		}
	}
	for _, required := range []string{
		"/etc/ark/ark.yaml",
		"/etc/ark/ssh/known_hosts",
		"/root/ark-hub/docker-compose.yml",
		"/var/lib/ark/ark.db",
	} {
		if !paths[required] {
			t.Errorf("示例 hub targets 缺少 %q", required)
		}
	}
	if stateTarget == nil || len(stateTarget.Paths) != 1 || stateTarget.Paths[0] != "/var/lib/ark/ark.db" {
		t.Fatalf("状态库必须是独立 target，实际 %#v", stateTarget)
	}

	forbidden := []string{cfg.Repo.PasswordFile, cfg.Repo.EnvFile, "/etc/ark"}
	for i := range cfg.Hosts {
		if cfg.Hosts[i].SSH != nil {
			forbidden = append(forbidden, cfg.Hosts[i].SSH.IdentityFile)
		}
	}
	for _, path := range forbidden {
		if paths[path] {
			t.Errorf("示例 hub targets 不得包含敏感或宽泛路径 %q", path)
		}
	}
}

// TestLoad_AppliesDefaults 确认只写必填字段时全局默认值会被补上。
func TestLoad_AppliesDefaults(t *testing.T) {
	minimal := `
version: 2
repo:
  url: "s3:https://example.com/bucket"
  password_file: /etc/ark/repo.pass
hosts:
  - host: db-01
    local: true
    project:
      name: demo
      compose_file: /srv/demo/docker-compose.yml
    targets:
      - type: volume
        name: demo_data
`
	cfg, err := LoadAndValidate(writeManifest(t, minimal))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}
	if cfg.Repo.Type != DefaultRepoType {
		t.Errorf("repo.type = %q, 期望默认值 %q", cfg.Repo.Type, DefaultRepoType)
	}

	h := hostByName(t, cfg, "db-01")
	if got := cfg.ScheduleFor(h).OnCalendar; got != DefaultOnCalendar {
		t.Errorf("on_calendar = %q, 期望默认值 %q", got, DefaultOnCalendar)
	}
	if got := cfg.RetentionFor(h); got.Daily != DefaultRetentionDaily ||
		got.Weekly != DefaultRetentionWeekly || got.Monthly != DefaultRetentionMonthly {
		t.Errorf("retention = %+v, 期望全部为默认值", got)
	}
}

// TestScheduleAndRetentionFor_HostOverride 确认 per-host 覆盖优先于 defaults，
// 未覆盖的机器仍然拿到 defaults。
func TestScheduleAndRetentionFor_HostOverride(t *testing.T) {
	cfg, err := LoadAndValidate(writeManifest(t, validManifest))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}

	db := hostByName(t, cfg, "db-01")
	if got := cfg.ScheduleFor(db).OnCalendar; got != "*-*-* 00,06,12,18:23:00" {
		t.Errorf("db-01 on_calendar = %q, 期望使用 host 自身的覆盖值", got)
	}
	if got := cfg.RetentionFor(db).Daily; got != 30 {
		t.Errorf("db-01 retention.daily = %d, 期望 30", got)
	}

	web := hostByName(t, cfg, "web-01")
	if got := cfg.ScheduleFor(web).OnCalendar; got != DefaultOnCalendar {
		t.Errorf("web-01 on_calendar = %q, 期望套用 defaults", got)
	}
	if got := cfg.RetentionFor(web).Weekly; got != 4 {
		t.Errorf("web-01 retention.weekly = %d, 期望套用 defaults 的 4", got)
	}
}

// TestRetentionFor_PartialNotFilled 确认用户显式配置的保留策略
// 不会被默认值补全——「我只想留 3 天」必须被尊重。
//
// 这条在 v2 里由指针表达：写了 retention 段就是显式选择，
// 不再靠「三项是否同时为 0」去猜。
func TestRetentionFor_PartialNotFilled(t *testing.T) {
	manifest := strings.Replace(validManifest,
		"    retention:\n      daily: 30\n      weekly: 8\n      monthly: 12",
		"    retention:\n      daily: 3", 1)

	cfg, err := LoadAndValidate(writeManifest(t, manifest))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}
	got := cfg.RetentionFor(hostByName(t, cfg, "db-01"))
	if got.Daily != 3 || got.Weekly != 0 || got.Monthly != 0 {
		t.Errorf("retention = %+v, 期望只保留 daily=3", got)
	}
}

// TestLoad_RejectsUnknownField 是这个包最重要的一条测试：
// 清单里的拼写错误必须立刻失败，而不是被静默忽略后
// 在真正需要恢复的那天才发现某个目标从没被备份过。
func TestLoad_RejectsUnknownField(t *testing.T) {
	manifest := strings.Replace(validManifest, "        name: sub2api_data", "        nmae: sub2api_data", 1)

	if _, err := Load(writeManifest(t, manifest)); err == nil {
		t.Fatal("期望拒绝未知字段 nmae，实际通过了")
	}
}

// TestLoad_VersionErrors 覆盖版本判定。
//
// 这些错误在严格解析之前给出，否则 v1 清单会退化成一串
// 「未知字段 host」，而用户真正需要知道的是「该迁移了」。
func TestLoad_VersionErrors(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		wantSub  string
	}{
		{
			name: "v1 单机清单",
			manifest: `
version: 1
host: web-01
project:
  name: sub2api
  compose_file: /root/sub2api/deploy/docker-compose.yml
repo:
  url: "s3:https://example.com/bucket/web-01"
  password_file: /etc/ark/repo.pass
targets:
  - type: volume
    name: sub2api_data
`,
			wantSub: "迁移到 v2",
		},
		{
			name:     "未声明 version",
			manifest: strings.Replace(validManifest, "version: 2\n", "", 1),
			wantSub:  "未声明",
		},
		{
			name:     "不支持的版本号",
			manifest: strings.Replace(validManifest, "version: 2", "version: 99", 1),
			wantSub:  "不支持的版本 99",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, tc.manifest))
			if err == nil {
				t.Fatal("期望报错，实际通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 中未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{
			name: "monitoring env_file 为空",
			mutate: func(s string) string {
				return strings.Replace(s, "  env_file: /etc/ark/monitoring.env", "  env_file: ''", 1)
			},
			wantSub: "monitoring.env_file",
		},
		{
			name: "monitoring env_file 是相对路径",
			mutate: func(s string) string {
				return strings.Replace(s, "/etc/ark/monitoring.env", "monitoring.env", 1)
			},
			wantSub: "绝对路径",
		},
		{
			name:    "host 含非法字符",
			mutate:  func(s string) string { return strings.Replace(s, "host: web-01", "host: Web_01", 1) },
			wantSub: "只允许小写字母",
		},
		{
			name:    "host 重名",
			mutate:  func(s string) string { return strings.Replace(s, "host: db-01", "host: web-01", 1) },
			wantSub: "必须全局唯一",
		},
		{
			name: "local 与 ssh 并存",
			mutate: func(s string) string {
				return strings.Replace(s,
					"  - host: hub-01\n    local: true\n",
					"  - host: hub-01\n    local: true\n    ssh:\n      address: 10.0.0.10:22\n"+
						"      user: root\n      identity_file: /etc/ark/keys/hub.key\n"+
						"      known_hosts_file: /etc/ark/known_hosts\n", 1)
			},
			wantSub: "不能同时配置 ssh",
		},
		{
			name: "既非 local 也没有 ssh",
			mutate: func(s string) string {
				return strings.Replace(s, "  - host: hub-01\n    local: true\n", "  - host: hub-01\n", 1)
			},
			wantSub: "远程机器必填",
		},
		{
			name: "缺少 known_hosts_file",
			mutate: func(s string) string {
				return strings.Replace(s, "      known_hosts_file: /etc/ark/known_hosts\n", "", 1)
			},
			wantSub: "known_hosts_file",
		},
		{
			name: "主机密钥策略不允许关闭校验",
			mutate: func(s string) string {
				return strings.Replace(s, "      known_hosts_file: /etc/ark/known_hosts\n",
					"      known_hosts_file: /etc/ark/known_hosts\n      host_key_policy: no\n", 1)
			},
			wantSub: "host_key_policy",
		},
		{
			name: "identity_file 是相对路径",
			mutate: func(s string) string {
				return strings.Replace(s, "identity_file: /etc/ark/keys/web-01.key", "identity_file: keys/web-01.key", 1)
			},
			wantSub: "绝对路径",
		},
		{
			name: "ssh 地址缺少端口",
			mutate: func(s string) string {
				return strings.Replace(s, "address: 10.0.0.11:22", "address: 10.0.0.11", 1)
			},
			wantSub: "host:port",
		},
		{
			name: "ssh 地址缺少主机名",
			mutate: func(s string) string {
				return strings.Replace(s, "address: 10.0.0.11:22", `address: ":22"`, 1)
			},
			wantSub: "缺少主机名",
		},
		{
			name: "ssh 端口用了服务名",
			mutate: func(s string) string {
				return strings.Replace(s, "address: 10.0.0.11:22", "address: 10.0.0.11:ssh", 1)
			},
			wantSub: "1-65535",
		},
		{
			name: "ssh 端口越界",
			mutate: func(s string) string {
				return strings.Replace(s, "address: 10.0.0.11:22", "address: 10.0.0.11:70000", 1)
			},
			wantSub: "1-65535",
		},
		{
			name:    "hosts 为空",
			mutate:  func(s string) string { return s[:strings.Index(s, "hosts:")] + "hosts: []\n" },
			wantSub: "至少需要一台机器",
		},
		{
			name: "compose_file 是相对路径",
			mutate: func(s string) string {
				return strings.Replace(s, "compose_file: /root/sub2api/deploy/docker-compose.yml\n      env_file",
					"compose_file: deploy/docker-compose.yml\n      env_file", 1)
			},
			wantSub: "绝对路径",
		},
		{
			name: "字段填在了错误的类型下",
			mutate: func(s string) string {
				return strings.Replace(s, "      - type: redis\n        service: redis",
					"      - type: redis\n        service: redis\n        database: oops", 1)
			},
			wantSub: "不适用于类型",
		},
		{
			name: "同一台机器上重复的备份目标",
			mutate: func(s string) string {
				return strings.Replace(s, "      - type: volume\n        name: sub2api_data",
					"      - type: volume\n        name: sub2api_data\n      - type: volume\n        name: sub2api_data", 1)
			},
			wantSub: "重复",
		},
		{
			name:    "未知的 target 类型",
			mutate:  func(s string) string { return strings.Replace(s, "type: redis", "type: mongodb", 1) },
			wantSub: "未知类型",
		},
		{
			name: "files 缺少 paths",
			mutate: func(s string) string {
				return strings.Replace(s,
					"      - type: files\n        name: ark-state\n        paths:\n          - /var/lib/ark/ark.db",
					"      - type: files\n        name: ark-state", 1)
			},
			wantSub: "至少需要一个路径",
		},
		{
			name: "机器没有任何备份目标",
			mutate: func(s string) string {
				return strings.Replace(s,
					"    targets:\n      - type: postgres\n        service: postgres\n        database: app\n",
					"    targets: []\n", 1)
			},
			wantSub: "至少需要一个备份目标",
		},
		{
			name: "host 的保留策略三项全为 0",
			mutate: func(s string) string {
				return strings.Replace(s, "      daily: 30\n      weekly: 8\n      monthly: 12",
					"      daily: 0\n      weekly: 0\n      monthly: 0", 1)
			},
			wantSub: "删光所有快照",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadAndValidate(writeManifest(t, tc.mutate(validManifest)))
			if err == nil {
				t.Fatal("期望校验失败，实际通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 中未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSSH_EffectiveHostKeyPolicy(t *testing.T) {
	tests := []struct {
		name string
		ssh  SSH
		want SSHHostKeyPolicy
	}{
		{name: "缺省接受首次连接", want: SSHHostKeyPolicyAcceptNew},
		{name: "显式严格模式", ssh: SSH{HostKeyPolicy: SSHHostKeyPolicyStrict}, want: SSHHostKeyPolicyStrict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ssh.EffectiveHostKeyPolicy(); got != tc.want {
				t.Errorf("EffectiveHostKeyPolicy() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// TestValidate_ErrorMentionsHost 确认多机清单里的错误能定位到具体机器。
// 一份十几台机器的清单如果只报 "targets[2] 有问题"，用户还得自己数。
func TestValidate_ErrorMentionsHost(t *testing.T) {
	manifest := strings.Replace(validManifest, "        service: redis", "", 1)

	_, err := LoadAndValidate(writeManifest(t, manifest))
	if err == nil {
		t.Fatal("期望校验失败，实际通过了")
	}
	if !strings.Contains(err.Error(), "web-01") {
		t.Errorf("错误信息 %q 中未指出出错的机器 web-01", err.Error())
	}
}

// TestValidate_DefaultsRetentionAllZero 覆盖手工构造的 Config，
// 这条路径绕过了 applyDefaults。
func TestValidate_DefaultsRetentionAllZero(t *testing.T) {
	cfg := &Config{
		Version: SchemaVersion,
		Repo:    Repo{Type: DefaultRepoType, URL: "s3:https://example.com/b", PasswordFile: "/etc/ark/repo.pass"},
		Defaults: Defaults{
			Schedule:  &Schedule{OnCalendar: DefaultOnCalendar},
			Retention: &Retention{},
		},
		Hosts: []Host{{
			Host:    "web-01",
			Local:   true,
			Project: Project{Name: "demo", ComposeFile: "/srv/demo/docker-compose.yml"},
			Targets: []Target{{Type: TargetVolume, Name: "demo_data"}},
		}},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "删光所有快照") {
		t.Fatalf("期望拒绝全零保留策略，实际: %v", err)
	}
}

// TestScheduleFor_NoDefaults 确认手工构造、完全没有 defaults 的 Config
// 也能拿到内置兜底值，而不是空字符串。
func TestScheduleFor_NoDefaults(t *testing.T) {
	cfg := &Config{Version: SchemaVersion}
	h := &Host{Host: "web-01"}

	if got := cfg.ScheduleFor(h).OnCalendar; got != DefaultOnCalendar {
		t.Errorf("on_calendar = %q, 期望兜底到 %q", got, DefaultOnCalendar)
	}
	if got := cfg.RetentionFor(h).Daily; got != DefaultRetentionDaily {
		t.Errorf("retention.daily = %d, 期望兜底到 %d", got, DefaultRetentionDaily)
	}
}

func TestTargetID(t *testing.T) {
	tests := []struct {
		target Target
		want   string
	}{
		{Target{Type: TargetPostgres, Service: "postgres", Database: "sub2api"}, "postgres/postgres/sub2api"},
		{Target{Type: TargetRedis, Service: "redis"}, "redis/redis"},
		{Target{Type: TargetVolume, Name: "sub2api_data"}, "volume/sub2api_data"},
		{Target{Type: TargetFiles, Name: "deploy-config"}, "files/deploy-config"},
		{Target{Type: TargetImageDigest, Services: []string{"app"}}, "image_digest"},
	}
	for _, tc := range tests {
		if got := tc.target.ID(); got != tc.want {
			t.Errorf("ID() = %q, 期望 %q", got, tc.want)
		}
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	_, err := Load(writeManifest(t, ""))
	if err == nil || !strings.Contains(err.Error(), "空文件") {
		t.Fatalf("期望提示空文件，实际: %v", err)
	}
}
