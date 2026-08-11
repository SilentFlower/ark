package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

func TestRun_ChecksSSHBinary(t *testing.T) {
	binDir := t.TempDir()
	tool := []byte("#!/bin/sh\ncase \"$*\" in\n  *\"config --services\"*) echo app ;;\n  *\"calendar\"*) echo 'Next elapse: test' ;;\n  *) echo fake ;;\nesac\n")
	for _, name := range []string{"docker", "restic", "ssh", "systemd-analyze"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, tool, 0o700); err != nil {
			t.Fatalf("创建伪命令 %s 失败: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir)

	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	passwordFile := filepath.Join(t.TempDir(), "repo.pass")
	for _, path := range []string{composeFile, passwordFile} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatalf("创建测试文件 %s 失败: %v", path, err)
		}
	}
	cfg := &config.Config{
		Version: config.SchemaVersion,
		Repo: config.Repo{
			Type:         config.DefaultRepoType,
			URL:          "s3:https://example.com/backup",
			PasswordFile: passwordFile,
		},
		Hosts: []config.Host{{
			Host:  "hub-01",
			Local: true,
			Project: config.Project{
				Name:        "ark",
				ComposeFile: composeFile,
			},
		}},
	}

	report := Run(context.Background(), cfg)
	for _, check := range report.Checks {
		if check.Name == "ssh" {
			if check.Status != StatusOK {
				t.Errorf("ssh 检查状态 = %s，期望 %s，详情: %s", check.Status, StatusOK, check.Detail)
			}
			return
		}
	}
	t.Fatal("doctor 报告中缺少 ssh 二进制检查")
}
