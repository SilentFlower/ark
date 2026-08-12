package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/hostkey"
)

func TestHostKeyRefreshCommand_Preview(t *testing.T) {
	configPath := "/tmp/ark.yaml"
	var gotApply bool
	cmd := newHostKeyCmdWithDependencies(&configPath, hostKeyDependencies{
		loadConfig: func(string) (*config.Config, error) {
			return testHostKeyConfig(), nil
		},
		refresh: func(_ context.Context, address, path string, apply bool) (hostkey.Result, error) {
			if address != "example.com:2222" || path != "/etc/ark/known_hosts" {
				t.Fatalf("refresh 参数 = %q %q", address, path)
			}
			gotApply = apply
			return testHostKeyResult(false), nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"refresh", "--host", "web-01"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("预览命令失败: %v", err)
	}
	if gotApply {
		t.Fatal("默认预览不应传入 apply=true")
	}
	if !strings.Contains(output.String(), "尚未修改 known_hosts") ||
		!strings.Contains(output.String(), "SHA256:new") {
		t.Errorf("预览输出不完整:\n%s", output.String())
	}
	if strings.Contains(output.String(), "AAAA") {
		t.Errorf("预览输出泄露了原始主机公钥:\n%s", output.String())
	}
}

func TestHostKeyRefreshCommand_ApplyJSON(t *testing.T) {
	configPath := "/tmp/ark.yaml"
	cmd := newHostKeyCmdWithDependencies(&configPath, hostKeyDependencies{
		loadConfig: func(string) (*config.Config, error) {
			return testHostKeyConfig(), nil
		},
		refresh: func(_ context.Context, _, _ string, apply bool) (hostkey.Result, error) {
			if !apply {
				t.Fatal("--apply 未传入 refresh")
			}
			return testHostKeyResult(true), nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"refresh", "--host", "web-01", "--apply", "--json"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("应用命令失败: %v", err)
	}
	var result hostKeyJSONResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if result.Host != "web-01" || !result.Applied || len(result.Scanned) != 1 {
		t.Fatalf("JSON 结果 = %#v", result)
	}
	if strings.Contains(output.String(), "AAAA") {
		t.Errorf("JSON 输出泄露了原始主机公钥:\n%s", output.String())
	}
}

func TestHostKeyRefreshCommand_RejectsMissingUnknownAndLocalHost(t *testing.T) {
	configPath := "/tmp/ark.yaml"
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{name: "缺少 host", args: []string{"refresh"}, wantSub: "--host"},
		{name: "未知 host", args: []string{"refresh", "--host", "missing"}, wantSub: "不存在"},
		{name: "本机 host", args: []string{"refresh", "--host", "hub-01"}, wantSub: "本机"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			cmd := newHostKeyCmdWithDependencies(&configPath, hostKeyDependencies{
				loadConfig: func(string) (*config.Config, error) { return testHostKeyConfig(), nil },
				refresh: func(context.Context, string, string, bool) (hostkey.Result, error) {
					called = true
					return hostkey.Result{}, nil
				},
			})
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, tc.wantSub)
			}
			if called {
				t.Fatal("参数或 host 校验失败后不应调用 refresh")
			}
		})
	}
}

func testHostKeyConfig() *config.Config {
	return &config.Config{Hosts: []config.Host{
		{Host: "hub-01", Local: true},
		{Host: "web-01", SSH: &config.SSH{
			Address:        "example.com:2222",
			KnownHostsFile: "/etc/ark/known_hosts",
		}},
	}}
}

func testHostKeyResult(applied bool) hostkey.Result {
	return hostkey.Result{
		Address:        "example.com:2222",
		KnownHostsFile: "/etc/ark/known_hosts",
		Existing: []hostkey.Fingerprint{{
			Algorithm: "ED25519",
			SHA256:    "SHA256:old",
		}},
		Scanned: []hostkey.Fingerprint{{
			Algorithm: "ED25519",
			SHA256:    "SHA256:new",
		}},
		Applied: applied,
	}
}
