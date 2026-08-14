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

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), unitMode); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}
