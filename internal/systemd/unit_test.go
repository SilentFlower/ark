package systemd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

func testConfig() *config.Config {
	defaultSchedule := config.Schedule{OnCalendar: "*-*-* 04:17:00"}
	hostSchedule := config.Schedule{OnCalendar: "*-*-* 05:23:00"}
	return &config.Config{
		Defaults: config.Defaults{Schedule: &defaultSchedule},
		Hosts: []config.Host{
			{Host: "hub-01", Local: true},
			{Host: "web-01", SSH: &config.SSH{}, Schedule: &hostSchedule},
		},
	}
}

func TestBuildUnits_生成全量模板与每HostTimer(t *testing.T) {
	units, err := BuildUnits(testConfig(), "/usr/local/bin/ark", "/etc/ark/ark.yaml")
	if err != nil {
		t.Fatalf("BuildUnits 失败: %v", err)
	}
	wantNames := []string{
		"ark-backup.service",
		"ark-backup@.service",
		"ark-backup@hub-01.timer",
		"ark-backup@web-01.timer",
		"ark-verify.service",
		"ark-verify.timer",
	}
	var gotNames []string
	byName := make(map[string]string)
	for _, unit := range units {
		gotNames = append(gotNames, unit.Name)
		byName[unit.Name] = unit.Content
		if !strings.HasPrefix(unit.Content, ManagedMarker+"\n") {
			t.Errorf("unit %s 缺少管理标记", unit.Name)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("unit 名称 = %#v，期望 %#v", gotNames, wantNames)
	}
	if !strings.Contains(byName["ark-backup.service"], "Type=oneshot") ||
		!strings.Contains(byName["ark-backup.service"], `ExecStart="/usr/local/bin/ark" --config "/etc/ark/ark.yaml" backup`) {
		t.Errorf("全量 service 内容错误:\n%s", byName["ark-backup.service"])
	}
	if !strings.Contains(byName["ark-backup@.service"], "backup --host %i") {
		t.Errorf("实例 service 未按 host 调用:\n%s", byName["ark-backup@.service"])
	}
	if !strings.Contains(byName["ark-verify.service"], `ExecStart="/usr/local/bin/ark" --config "/etc/ark/ark.yaml" verify`) ||
		strings.Contains(byName["ark-verify.service"], "--host") {
		t.Errorf("verify service 内容错误:\n%s", byName["ark-verify.service"])
	}
	for _, name := range []string{"ark-backup.service", "ark-backup@.service", "ark-verify.service"} {
		for _, want := range []string{
			"CacheDirectory=ark",
			"CacheDirectoryMode=0700",
			"Environment=XDG_CACHE_HOME=/var/cache/ark",
		} {
			if !strings.Contains(byName[name], want) {
				t.Errorf("service %s 缺少 %q:\n%s", name, want, byName[name])
			}
		}
	}
	for _, want := range []string{
		"OnCalendar=weekly",
		"Persistent=true",
		"RandomizedDelaySec=21600",
		"Unit=ark-verify.service",
	} {
		if !strings.Contains(byName["ark-verify.timer"], want) {
			t.Errorf("verify timer 缺少 %q:\n%s", want, byName["ark-verify.timer"])
		}
	}
	for name, schedule := range map[string]string{
		"ark-backup@hub-01.timer": "*-*-* 04:17:00",
		"ark-backup@web-01.timer": "*-*-* 05:23:00",
	} {
		content := byName[name]
		for _, want := range []string{
			"OnCalendar=" + schedule,
			"Persistent=true",
			"RandomizedDelaySec=600",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("timer %s 缺少 %q:\n%s", name, want, content)
			}
		}
	}
}

func TestBuildUnits_不输出凭证并拒绝非法路径(t *testing.T) {
	cfg := testConfig()
	cfg.Repo.PasswordFile = "SECRET_PASSWORD"
	cfg.Repo.EnvFile = "SECRET_ACCESS_KEY"
	cfg.Hosts[1].SSH.IdentityFile = "SECRET_PRIVATE_KEY"
	units, err := BuildUnits(cfg, "/opt/ark/bin/ark", "/etc/ark/ark.yaml")
	if err != nil {
		t.Fatalf("BuildUnits 失败: %v", err)
	}
	for _, unit := range units {
		for _, secret := range []string{"SECRET_PASSWORD", "SECRET_ACCESS_KEY", "SECRET_PRIVATE_KEY"} {
			if strings.Contains(unit.Content, secret) {
				t.Fatalf("unit %s 泄漏 %s", unit.Name, secret)
			}
		}
	}
	if _, err := BuildUnits(cfg, "relative/ark", "/etc/ark/ark.yaml"); err == nil ||
		!strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("相对 binary 错误 = %v", err)
	}
}

func TestInstall_替换Units并仅清理受管旧Timer(t *testing.T) {
	dir := t.TempDir()
	managedStale := filepath.Join(dir, "ark-backup@old.timer")
	managedVerifyStale := filepath.Join(dir, "ark-verify@old.timer")
	userTimer := filepath.Join(dir, "ark-backup@user.timer")
	writeTestFile(t, managedStale, ManagedMarker+"\nold\n")
	writeTestFile(t, managedVerifyStale, ManagedMarker+"\nold\n")
	writeTestFile(t, userTimer, "# user owned\n")

	var verified []string
	result, err := install(context.Background(), testConfig(), InstallOptions{
		UnitDir: dir, BinaryPath: "/usr/local/bin/ark", ConfigPath: "/etc/ark/ark.yaml",
	}, installDependencies{
		verify: func(_ context.Context, paths []string) error {
			verified = append([]string(nil), paths...)
			return nil
		},
		rename: os.Rename,
		remove: os.Remove,
	})
	if err != nil {
		t.Fatalf("install 失败: %v", err)
	}
	if len(verified) != 6 || len(result.Written) != 6 ||
		!reflect.DeepEqual(result.Removed, []string{"ark-backup@old.timer", "ark-verify@old.timer"}) {
		t.Fatalf("verified=%#v result=%#v", verified, result)
	}
	if _, err := os.Stat(managedStale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("受管旧 timer 未删除: %v", err)
	}
	if _, err := os.Stat(managedVerifyStale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("受管旧 verify timer 未删除: %v", err)
	}
	if data, err := os.ReadFile(userTimer); err != nil || string(data) != "# user owned\n" {
		t.Fatalf("用户 timer 被修改: data=%q err=%v", data, err)
	}
}

func TestInstall_Verify失败保持旧Unit不变(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "ark-backup.service")
	writeTestFile(t, service, ManagedMarker+"\nold service\n")
	_, err := install(context.Background(), testConfig(), InstallOptions{
		UnitDir: dir, BinaryPath: "/usr/local/bin/ark", ConfigPath: "/etc/ark/ark.yaml",
	}, installDependencies{
		verify: func(context.Context, []string) error { return errors.New("verify failed") },
		rename: os.Rename,
		remove: os.Remove,
	})
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("错误 = %v", err)
	}
	if data, readErr := os.ReadFile(service); readErr != nil || string(data) != ManagedMarker+"\nold service\n" {
		t.Fatalf("verify 失败后旧 service 被修改: data=%q err=%v", data, readErr)
	}
}

func TestInstall_拒绝覆盖非Ark管理的同名Unit(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "ark-backup.service")
	writeTestFile(t, service, "# user owned\n")
	_, err := install(context.Background(), testConfig(), InstallOptions{
		UnitDir: dir, BinaryPath: "/usr/local/bin/ark", ConfigPath: "/etc/ark/ark.yaml",
	}, installDependencies{
		verify: func(context.Context, []string) error { return nil },
		rename: os.Rename,
		remove: os.Remove,
	})
	if err == nil || !strings.Contains(err.Error(), "非 ark 管理") {
		t.Fatalf("用户 unit 覆盖错误 = %v", err)
	}
	if data, readErr := os.ReadFile(service); readErr != nil || string(data) != "# user owned\n" {
		t.Fatalf("用户 unit 被修改: data=%q err=%v", data, readErr)
	}
}

func TestInstall_拒绝覆盖符号链接且不清理符号链接Timer(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "outside")
	writeTestFile(t, target, ManagedMarker+"\noutside\n")
	serviceLink := filepath.Join(dir, "ark-backup.service")
	staleLink := filepath.Join(dir, "ark-backup@old.timer")
	if err := os.Symlink(target, serviceLink); err != nil {
		t.Fatalf("创建 service symlink 失败: %v", err)
	}
	if err := os.Symlink(target, staleLink); err != nil {
		t.Fatalf("创建 timer symlink 失败: %v", err)
	}
	_, err := install(context.Background(), testConfig(), InstallOptions{
		UnitDir: dir, BinaryPath: "/usr/local/bin/ark", ConfigPath: "/etc/ark/ark.yaml",
	}, installDependencies{
		verify: func(context.Context, []string) error { return nil },
		rename: os.Rename,
		remove: os.Remove,
	})
	if err == nil || !strings.Contains(err.Error(), "不是普通文件") {
		t.Fatalf("符号链接覆盖错误 = %v", err)
	}
	for _, path := range []string{serviceLink, staleLink} {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("符号链接 %s 被修改: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestInstall_替换中途失败回滚全部文件(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "ark-backup.service")
	template := filepath.Join(dir, "ark-backup@.service")
	writeTestFile(t, service, ManagedMarker+"\nold service\n")
	writeTestFile(t, template, ManagedMarker+"\nold template\n")
	renameCalls := 0
	_, err := install(context.Background(), testConfig(), InstallOptions{
		UnitDir: dir, BinaryPath: "/usr/local/bin/ark", ConfigPath: "/etc/ark/ark.yaml",
	}, installDependencies{
		verify: func(context.Context, []string) error { return nil },
		rename: func(oldPath, newPath string) error {
			renameCalls++
			if renameCalls == 2 {
				return errors.New("rename failed")
			}
			return os.Rename(oldPath, newPath)
		},
		remove: os.Remove,
	})
	if err == nil || !strings.Contains(err.Error(), "rename failed") {
		t.Fatalf("错误 = %v", err)
	}
	for path, want := range map[string]string{
		service: ManagedMarker + "\nold service\n", template: ManagedMarker + "\nold template\n",
	} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || string(data) != want {
			t.Errorf("%s 回滚结果 data=%q err=%v", path, data, readErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ark-backup@hub-01.timer")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("回滚后新增 timer 仍存在: %v", statErr)
	}
}

func TestGeneratedUnits_SystemdAnalyzeVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过 systemd-analyze 集成校验")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("未安装 systemd-analyze")
	}
	dir := t.TempDir()
	units, err := BuildUnits(testConfig(), "/usr/bin/true", "/etc/ark/ark.yaml")
	if err != nil {
		t.Fatalf("BuildUnits 失败: %v", err)
	}
	var paths []string
	for _, unit := range units {
		path := filepath.Join(dir, unit.Name)
		writeTestFile(t, path, unit.Content)
		paths = append(paths, path)
	}
	if err := verifyUnitFiles(context.Background(), paths); err != nil {
		t.Fatalf("systemd-analyze verify 失败: %v", err)
	}
}

func TestBuildHubUnit_生成独立常驻Service(t *testing.T) {
	unit, err := BuildHubUnit(HubInstallOptions{
		BinaryPath:    "/usr/local/bin/ark-hub",
		ListenAddress: "127.0.0.1:8080",
		StateDBPath:   "/var/lib/ark/ark.db",
		AuthFile:      "/var/lib/ark-hub/auth.json",
		SecureCookie:  true,
	})
	if err != nil {
		t.Fatalf("BuildHubUnit 失败: %v", err)
	}
	if unit.Name != "ark-hub.service" || strings.Contains(unit.Content, ".timer") {
		t.Fatalf("hub unit 名称或 timer 边界错误: %#v", unit)
	}
	for _, want := range []string{
		"Type=simple",
		"UMask=0077",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"Restart=on-failure",
		"RestartSec=5",
		"WantedBy=multi-user.target",
		`ExecStart="/usr/local/bin/ark-hub" serve --listen "127.0.0.1:8080" --state-db "/var/lib/ark/ark.db" --auth-file "/var/lib/ark-hub/auth.json" --secure-cookie`,
	} {
		if !strings.Contains(unit.Content, want) {
			t.Errorf("hub service 缺少 %q:\n%s", want, unit.Content)
		}
	}
	for _, forbidden := range []string{"password_hash", "session", "Cookie", "TOTP"} {
		if strings.Contains(unit.Content, forbidden) {
			t.Errorf("hub service 包含禁止内容 %q", forbidden)
		}
	}
}

func TestBuildHubUnit_拒绝非法路径与监听地址(t *testing.T) {
	valid := HubInstallOptions{
		BinaryPath:    "/usr/local/bin/ark-hub",
		ListenAddress: "127.0.0.1:8080",
		StateDBPath:   "/var/lib/ark/ark.db",
		AuthFile:      "/var/lib/ark-hub/auth.json",
	}
	tests := []struct {
		name   string
		mutate func(*HubInstallOptions)
	}{
		{name: "二进制相对路径", mutate: func(options *HubInstallOptions) { options.BinaryPath = "bin/ark-hub" }},
		{name: "状态库相对路径", mutate: func(options *HubInstallOptions) { options.StateDBPath = "ark.db" }},
		{name: "凭证文件相对路径", mutate: func(options *HubInstallOptions) { options.AuthFile = "auth.json" }},
		{name: "监听地址包含换行", mutate: func(options *HubInstallOptions) { options.ListenAddress = "127.0.0.1:8080\nExecStart=/bin/false" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := BuildHubUnit(options); err == nil {
				t.Fatal("非法参数应被拒绝")
			}
		})
	}
}

func TestInstallHub_只替换Service且不扫描清理Timer(t *testing.T) {
	dir := t.TempDir()
	managedTimer := filepath.Join(dir, "ark-backup@old.timer")
	writeTestFile(t, managedTimer, ManagedMarker+"\nold timer\n")
	result, err := installHub(context.Background(), HubInstallOptions{
		UnitDir:       dir,
		BinaryPath:    "/usr/local/bin/ark-hub",
		ListenAddress: "127.0.0.1:8080",
		StateDBPath:   "/var/lib/ark/ark.db",
		AuthFile:      "/var/lib/ark-hub/auth.json",
	}, installDependencies{
		verify: func(_ context.Context, paths []string) error {
			if len(paths) != 1 || filepath.Base(paths[0]) != "ark-hub.service" {
				t.Fatalf("verify paths=%#v", paths)
			}
			return nil
		},
		rename: os.Rename,
		remove: func(path string) error {
			t.Fatalf("InstallHub 不应删除文件: %s", path)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("installHub 失败: %v", err)
	}
	if !reflect.DeepEqual(result.Written, []string{"ark-hub.service"}) || len(result.Removed) != 0 {
		t.Fatalf("hub install result=%#v", result)
	}
	if data, err := os.ReadFile(managedTimer); err != nil || string(data) != ManagedMarker+"\nold timer\n" {
		t.Fatalf("既有 timer 被修改: data=%q err=%v", data, err)
	}
}

func TestInstallHub_失败保持既有Service(t *testing.T) {
	options := HubInstallOptions{
		BinaryPath:    "/usr/local/bin/ark-hub",
		ListenAddress: "127.0.0.1:8080",
		StateDBPath:   "/var/lib/ark/ark.db",
		AuthFile:      "/var/lib/ark-hub/auth.json",
	}
	t.Run("verify 失败", func(t *testing.T) {
		dir := t.TempDir()
		options.UnitDir = dir
		service := filepath.Join(dir, "ark-hub.service")
		oldContent := ManagedMarker + "\nold hub service\n"
		writeTestFile(t, service, oldContent)
		verifyFailure := errors.New("verify failure")
		_, err := installHub(context.Background(), options, installDependencies{
			verify: func(context.Context, []string) error { return verifyFailure },
			rename: os.Rename,
			remove: os.Remove,
		})
		assertFileUnchanged(t, service, oldContent, err, verifyFailure)
	})

	t.Run("拒绝非受管文件", func(t *testing.T) {
		dir := t.TempDir()
		options.UnitDir = dir
		service := filepath.Join(dir, "ark-hub.service")
		oldContent := "# user owned\n"
		writeTestFile(t, service, oldContent)
		_, err := installHub(context.Background(), options, installDependencies{
			verify: func(context.Context, []string) error { return nil },
			rename: os.Rename,
			remove: os.Remove,
		})
		if err == nil || !strings.Contains(err.Error(), "非 ark 管理") {
			t.Fatalf("非受管文件错误 = %v", err)
		}
		assertFileContent(t, service, oldContent)
	})

	t.Run("拒绝符号链接", func(t *testing.T) {
		dir := t.TempDir()
		options.UnitDir = dir
		target := filepath.Join(dir, "outside")
		writeTestFile(t, target, ManagedMarker+"\noutside\n")
		service := filepath.Join(dir, "ark-hub.service")
		if err := os.Symlink(target, service); err != nil {
			t.Fatalf("创建符号链接失败: %v", err)
		}
		_, err := installHub(context.Background(), options, installDependencies{
			verify: func(context.Context, []string) error { return nil },
			rename: os.Rename,
			remove: os.Remove,
		})
		if err == nil || !strings.Contains(err.Error(), "不是普通文件") {
			t.Fatalf("符号链接错误 = %v", err)
		}
		info, statErr := os.Lstat(service)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("符号链接被修改: info=%v err=%v", info, statErr)
		}
	})

	t.Run("rename 失败回滚", func(t *testing.T) {
		dir := t.TempDir()
		options.UnitDir = dir
		service := filepath.Join(dir, "ark-hub.service")
		oldContent := ManagedMarker + "\nold hub service\n"
		writeTestFile(t, service, oldContent)
		renameFailure := errors.New("rename failure")
		_, err := installHub(context.Background(), options, installDependencies{
			verify: func(context.Context, []string) error { return nil },
			rename: func(string, string) error { return renameFailure },
			remove: os.Remove,
		})
		assertFileUnchanged(t, service, oldContent, err, renameFailure)
	})
}

func TestGeneratedHubUnit_SystemdAnalyzeVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过 systemd-analyze 集成校验")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("未安装 systemd-analyze")
	}
	unit, err := BuildHubUnit(HubInstallOptions{
		BinaryPath:    "/usr/bin/true",
		ListenAddress: "127.0.0.1:8080",
		StateDBPath:   "/var/lib/ark/ark.db",
		AuthFile:      "/var/lib/ark-hub/auth.json",
	})
	if err != nil {
		t.Fatalf("BuildHubUnit 失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), unit.Name)
	writeTestFile(t, path, unit.Content)
	if err := verifyUnitFiles(context.Background(), []string{path}); err != nil {
		t.Fatalf("systemd-analyze verify hub 失败: %v", err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), unitMode); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

func assertFileUnchanged(t *testing.T, path, want string, err, expected error) {
	t.Helper()
	if !errors.Is(err, expected) {
		t.Fatalf("错误 %v 不包含 %v", err, expected)
	}
	assertFileContent(t, path, want)
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("文件 %s 内容=%q err=%v，期望 %q", path, data, err, want)
	}
}
