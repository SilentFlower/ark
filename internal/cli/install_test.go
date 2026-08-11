package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/silentflower/ark/internal/config"
	arksystemd "github.com/silentflower/ark/internal/systemd"
)

func TestInstallCommand_传递路径并输出纯JSON(t *testing.T) {
	cfg := testBackupConfig()
	configPath := "/etc/ark/ark.yaml"
	var gotOptions arksystemd.InstallOptions
	cmd := newInstallCmdWithDependencies(&configPath, installDependencies{
		loadConfig: func(path string) (*config.Config, error) {
			if path != configPath {
				t.Fatalf("config path = %q", path)
			}
			return cfg, nil
		},
		executable: func() (string, error) { return "/opt/ark/bin/ark", nil },
		install: func(
			_ context.Context,
			gotConfig *config.Config,
			options arksystemd.InstallOptions,
		) (arksystemd.InstallResult, error) {
			if gotConfig != cfg {
				t.Fatal("install 未收到加载后的 config")
			}
			gotOptions = options
			return arksystemd.InstallResult{
				Written: []string{"ark-backup.service"},
				Removed: []string{"ark-backup@old.timer"},
			}, nil
		},
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--unit-dir", "/tmp/systemd", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install 命令失败: %v", err)
	}
	wantOptions := arksystemd.InstallOptions{
		UnitDir: "/tmp/systemd", BinaryPath: "/opt/ark/bin/ark", ConfigPath: configPath,
	}
	if !reflect.DeepEqual(gotOptions, wantOptions) {
		t.Fatalf("options = %#v，期望 %#v", gotOptions, wantOptions)
	}
	var result map[string][]string
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("JSON 输出无效: %v\n%s", err, output.String())
	}
	if !reflect.DeepEqual(result["written"], []string{"ark-backup.service"}) ||
		!reflect.DeepEqual(result["removed"], []string{"ark-backup@old.timer"}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestRootCommand_注册Backup和Install(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"backup", "install"} {
		command, _, err := root.Find([]string{name})
		if err != nil || command == root || command.Name() != name {
			t.Fatalf("根命令未注册 %s: command=%v err=%v", name, command, err)
		}
	}
}
