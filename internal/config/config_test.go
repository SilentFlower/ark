package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validManifest 是一份完整合法的清单，各用例在它的基础上做最小改动。
const validManifest = `
version: 1
host: web-01
project:
  name: sub2api
  compose_file: /root/sub2api/deploy/docker-compose.yml
  env_file: /root/sub2api/deploy/.env
  project_name: deploy
repo:
  type: restic
  url: "s3:https://example.r2.cloudflarestorage.com/backup/web-01"
  password_file: /etc/ark/repo.pass
  env_file: /etc/ark/repo.env
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
schedule:
  on_calendar: "*-*-* 04:17:00"
retention:
  daily: 7
  weekly: 4
  monthly: 6
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

func TestLoadAndValidate_Valid(t *testing.T) {
	cfg, err := LoadAndValidate(writeManifest(t, validManifest))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}
	if cfg.Host != "web-01" {
		t.Errorf("host = %q, 期望 web-01", cfg.Host)
	}
	if len(cfg.Targets) != 5 {
		t.Errorf("targets 数量 = %d, 期望 5", len(cfg.Targets))
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	// 只保留必填字段，其余交给默认值。
	minimal := `
version: 1
host: db-01
project:
  name: demo
  compose_file: /srv/demo/docker-compose.yml
repo:
  url: "s3:https://example.com/bucket"
  password_file: /etc/ark/repo.pass
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
	if cfg.Schedule.OnCalendar != DefaultOnCalendar {
		t.Errorf("on_calendar = %q, 期望默认值 %q", cfg.Schedule.OnCalendar, DefaultOnCalendar)
	}
	if cfg.Retention.Daily != DefaultRetentionDaily {
		t.Errorf("retention.daily = %d, 期望默认值 %d", cfg.Retention.Daily, DefaultRetentionDaily)
	}
}

// TestLoad_PartialRetentionNotOverridden 确认用户显式配置的保留策略
// 不会被默认值补全——「我只想留 3 天」必须被尊重。
func TestLoad_PartialRetentionNotOverridden(t *testing.T) {
	manifest := strings.Replace(validManifest,
		"retention:\n  daily: 7\n  weekly: 4\n  monthly: 6",
		"retention:\n  daily: 3", 1)

	cfg, err := LoadAndValidate(writeManifest(t, manifest))
	if err != nil {
		t.Fatalf("期望校验通过，实际报错: %v", err)
	}
	if cfg.Retention.Daily != 3 || cfg.Retention.Weekly != 0 || cfg.Retention.Monthly != 0 {
		t.Errorf("retention = %+v, 期望只保留 daily=3", cfg.Retention)
	}
}

// TestLoad_RejectsUnknownField 是这个包最重要的一条测试：
// 清单里的拼写错误必须立刻失败，而不是被静默忽略后
// 在真正需要恢复的那天才发现某个目标从没被备份过。
func TestLoad_RejectsUnknownField(t *testing.T) {
	manifest := strings.Replace(validManifest, "  name: sub2api_data", "  nmae: sub2api_data", 1)

	if _, err := Load(writeManifest(t, manifest)); err == nil {
		t.Fatal("期望拒绝未知字段 nmae，实际通过了")
	}
}

func TestValidate_Errors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{
			name:    "host 含非法字符",
			mutate:  func(s string) string { return strings.Replace(s, "host: web-01", "host: Web_01", 1) },
			wantSub: "host",
		},
		{
			name: "compose_file 是相对路径",
			mutate: func(s string) string {
				return strings.Replace(s, "/root/sub2api/deploy/docker-compose.yml\n  env_file", "deploy/docker-compose.yml\n  env_file", 1)
			},
			wantSub: "绝对路径",
		},
		{
			name: "字段填在了错误的类型下",
			mutate: func(s string) string {
				return strings.Replace(s, "  - type: redis\n    service: redis", "  - type: redis\n    service: redis\n    database: oops", 1)
			},
			wantSub: "不适用于类型",
		},
		{
			name: "重复的备份目标",
			mutate: func(s string) string {
				return s + "  - type: volume\n    name: sub2api_data\n"
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
					"  - type: files\n    name: deploy-config\n    paths:\n      - /root/sub2api/deploy/docker-compose.yml\n      - /root/sub2api/deploy/.env",
					"  - type: files\n    name: deploy-config", 1)
			},
			wantSub: "至少需要一个路径",
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

// TestValidate_RetentionAllZero 覆盖手工构造的 Config，
// 这条路径绕过了 applyDefaults。
func TestValidate_RetentionAllZero(t *testing.T) {
	cfg := &Config{
		Version:  SchemaVersion,
		Host:     "web-01",
		Project:  Project{Name: "demo", ComposeFile: "/srv/demo/docker-compose.yml"},
		Repo:     Repo{Type: DefaultRepoType, URL: "s3:https://example.com/b", PasswordFile: "/etc/ark/repo.pass"},
		Targets:  []Target{{Type: TargetVolume, Name: "demo_data"}},
		Schedule: Schedule{OnCalendar: DefaultOnCalendar},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "删光所有快照") {
		t.Fatalf("期望拒绝全零保留策略，实际: %v", err)
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
