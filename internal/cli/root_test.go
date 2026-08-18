package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/doctor"
)

func TestRunDoctor_范围与调用顺序(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Host: "hub-01", Local: true},
		{Host: "web-01", SSH: &config.SSH{}},
	}}
	tests := []struct {
		name     string
		hostName string
		all      bool
		want     []string
	}{
		{name: "默认只检查hub", want: []string{"local"}},
		{name: "指定host", hostName: "web-01", want: []string{"host:web-01"}},
		{name: "全部按清单顺序", all: true, want: []string{"local", "host:hub-01", "host:web-01"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			runLocal := func(context.Context, *config.Config) *doctor.Report {
				calls = append(calls, "local")
				return &doctor.Report{Checks: []doctor.Check{{Name: "local", Status: doctor.StatusOK}}}
			}
			runHost := func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
				calls = append(calls, "host:"+host.Host)
				return &doctor.Report{Checks: []doctor.Check{{Name: host.Host, Status: doctor.StatusOK}}}
			}

			report, err := runDoctor(context.Background(), cfg, tc.hostName, tc.all, runLocal, runHost)
			if err != nil {
				t.Fatalf("runDoctor 返回错误: %v", err)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("调用顺序 = %v，期望 %v", calls, tc.want)
			}
			if len(report.Checks) != len(tc.want) {
				t.Fatalf("合并检查数 = %d，期望 %d", len(report.Checks), len(tc.want))
			}
		})
	}
}

func TestRunDoctor_未知Host返回工具错误(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{{Host: "hub-01", Local: true}}}
	_, err := runDoctor(
		context.Background(),
		cfg,
		"missing",
		false,
		func(context.Context, *config.Config) *doctor.Report { return &doctor.Report{} },
		func(context.Context, *config.Config, *config.Host) *doctor.Report { return &doctor.Report{} },
	)
	if err == nil || !strings.Contains(err.Error(), `host "missing"`) {
		t.Fatalf("未知 host 错误 = %v", err)
	}
}

func TestRunDoctorWithDNSMgr_只追加到本地范围(t *testing.T) {
	cfg := &config.Config{
		DNSMgr: &config.DNSMgr{BaseURL: "https://dns.example", EnvFile: "/etc/ark/dnsmgr.env"},
		Hosts:  []config.Host{{Host: "hub-01", Local: true}, {Host: "web-01", SSH: &config.SSH{}}},
	}
	tests := []struct {
		name     string
		hostName string
		all      bool
		want     []string
	}{
		{name: "默认本地范围", want: []string{"local", "dnsmgr"}},
		{name: "全部范围", all: true, want: []string{"local", "host:hub-01", "host:web-01", "dnsmgr"}},
		{name: "指定host不追加", hostName: "web-01", want: []string{"host:web-01"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			report, err := runDoctorWithDNSMgr(
				context.Background(), cfg, tc.hostName, tc.all,
				func(context.Context, *config.Config) *doctor.Report {
					calls = append(calls, "local")
					return &doctor.Report{}
				},
				func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
					calls = append(calls, "host:"+host.Host)
					return &doctor.Report{}
				},
				func(context.Context, *config.Config) *doctor.Report {
					calls = append(calls, "dnsmgr")
					return &doctor.Report{}
				},
			)
			if err != nil || !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("调用顺序 = %#v, err=%v，期望 %#v", calls, err, tc.want)
			}
			if report == nil {
				t.Fatal("报告不能为空")
			}
		})
	}
}

func TestDoctorCommand_AllJSON合并并保持失败语义(t *testing.T) {
	configPath := writeValidManifest(t)
	var calls []string
	cmd := newDoctorCmdWithRunners(
		&configPath,
		func(context.Context, *config.Config) *doctor.Report {
			calls = append(calls, "local")
			return &doctor.Report{Checks: []doctor.Check{{
				Name: "repo.access", Status: doctor.StatusOK, Detail: "ok",
			}}}
		},
		func(_ context.Context, _ *config.Config, host *config.Host) *doctor.Report {
			calls = append(calls, host.Host)
			status := doctor.StatusOK
			if host.Host == "hub-01" {
				status = doctor.StatusFail
			}
			return &doctor.Report{Checks: []doctor.Check{{
				Name: host.Host + " / connection", Status: status, Detail: "result",
			}}}
		},
	)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--all", "--json"})

	err := cmd.Execute()
	if !errors.Is(err, errChecksFailed) {
		t.Fatalf("命令错误 = %v，期望 errChecksFailed", err)
	}
	wantCalls := []string{"local", "hub-01", "web-01"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("调用顺序 = %v，期望 %v", calls, wantCalls)
	}
	var report doctor.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if len(report.Checks) != 3 || report.Checks[1].Status != doctor.StatusFail || report.Checks[2].Status != doctor.StatusOK {
		t.Fatalf("合并报告不符合预期: %#v", report.Checks)
	}
}

func TestDoctorCommand_ObjectLockWarnJSON不阻断(t *testing.T) {
	configPath := writeValidManifest(t)
	cmd := newDoctorCmdWithRunners(
		&configPath,
		func(context.Context, *config.Config) *doctor.Report {
			return &doctor.Report{Checks: []doctor.Check{{
				Name: "repo.object_lock", Status: doctor.StatusWarn, Detail: "请在控制台人工确认",
			}}}
		},
		func(context.Context, *config.Config, *config.Host) *doctor.Report {
			t.Fatal("默认 doctor 不应执行 host 检查")
			return nil
		},
	)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("对象锁 warn 不应阻断 doctor: %v", err)
	}
	var report doctor.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("对象锁 JSON 输出无效: %v\n%s", err, output.String())
	}
	if len(report.Checks) != 1 || report.Checks[0].Status != doctor.StatusWarn {
		t.Fatalf("对象锁 JSON 报告 = %#v", report.Checks)
	}
}

func TestDoctorCommand_Host与All互斥(t *testing.T) {
	configPath := writeValidManifest(t)
	called := false
	cmd := newDoctorCmdWithRunners(
		&configPath,
		func(context.Context, *config.Config) *doctor.Report {
			called = true
			return &doctor.Report{}
		},
		func(context.Context, *config.Config, *config.Host) *doctor.Report {
			called = true
			return &doctor.Report{}
		},
	)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "web-01", "--all"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("--host 与 --all 同时使用应返回错误")
	} else if !strings.Contains(err.Error(), "不能同时使用") {
		t.Fatalf("互斥标志错误不是中文可读消息: %v", err)
	}
	if called {
		t.Fatal("互斥标志错误后不应执行 doctor")
	}
}

func TestDoctorCommand_指定未知Host不执行检查(t *testing.T) {
	configPath := writeValidManifest(t)
	called := false
	cmd := newDoctorCmdWithRunners(
		&configPath,
		func(context.Context, *config.Config) *doctor.Report {
			called = true
			return &doctor.Report{}
		},
		func(context.Context, *config.Config, *config.Host) *doctor.Report {
			called = true
			return &doctor.Report{}
		},
	)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--host", "missing"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `host "missing"`) {
		t.Fatalf("未知 host 错误 = %v", err)
	}
	if called {
		t.Fatal("未知 host 不应执行任何检查")
	}
}

func writeValidManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ark.yaml")
	data := []byte(`version: 2
repo:
  type: restic
  url: local:/tmp/repo
  password_file: /tmp/repo.pass
hosts:
  - host: hub-01
    local: true
    project:
      name: hub
      compose_file: /tmp/hub-compose.yaml
    targets:
      - type: files
        name: config
        paths:
          - /tmp/config
  - host: web-01
    ssh:
      address: 127.0.0.1:22
      user: root
      identity_file: /tmp/web.key
      known_hosts_file: /tmp/known_hosts
    project:
      name: web
      compose_file: /tmp/web-compose.yaml
    targets:
      - type: redis
        service: redis
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入测试清单失败: %v", err)
	}
	return path
}
