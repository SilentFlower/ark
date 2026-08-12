package hostkey

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	hostKeyHelperEnv = "ARK_HOSTKEY_HELPER"
	oldKey           = "[example.com]:2222 ssh-ed25519 AAAAOLD\n"
	newKey           = "example.com ssh-ed25519 AAAANEW\n"
	hashedKey        = "|1|c2FsdA==|aGFzaA== ssh-ed25519 AAAAOLD\n"
)

func TestHostKeyHelperProcess(t *testing.T) {
	if os.Getenv(hostKeyHelperEnv) != "1" {
		return
	}
	time.Sleep(10 * time.Second)
}

func TestRefresh_PreviewDoesNotModifyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}

	result, err := refresh(context.Background(), "example.com:2222", path, false, testDependencies(t, nil))
	if err != nil {
		t.Fatalf("预览主机密钥失败: %v", err)
	}
	if result.Applied {
		t.Fatal("预览不应标记为已应用")
	}
	if len(result.Existing) != 1 || result.Existing[0].SHA256 != "SHA256:old" {
		t.Fatalf("旧指纹 = %#v", result.Existing)
	}
	if len(result.Scanned) != 1 || result.Scanned[0].SHA256 != "SHA256:new" {
		t.Fatalf("新指纹 = %#v", result.Scanned)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 known_hosts 失败: %v", err)
	}
	if string(data) != oldKey {
		t.Errorf("预览修改了 known_hosts: %q", data)
	}
}

func TestRefresh_ApplyReplacesOnlySelectedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	other := "other.example ssh-rsa AAAAOTHER\n"
	if err := os.WriteFile(path, []byte(oldKey+other), 0o644); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}

	result, err := refresh(context.Background(), "example.com:2222", path, true, testDependencies(t, nil))
	if err != nil {
		t.Fatalf("应用主机密钥失败: %v", err)
	}
	if !result.Applied {
		t.Fatal("应用后应标记为已应用")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 known_hosts 失败: %v", err)
	}
	if strings.Contains(string(data), "AAAAOLD") || !strings.Contains(string(data), "AAAAOTHER") ||
		!strings.Contains(string(data), "[example.com]:2222 ssh-ed25519 AAAANEW") {
		t.Errorf("刷新后的 known_hosts 内容不正确: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取 known_hosts 元数据失败: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("known_hosts 权限 = %04o, 期望 0600", got)
	}
}

func TestRefresh_ApplyCreatesKnownHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	deps := testDependencies(t, nil)
	deps.command = func(_ context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		switch {
		case name == "ssh-keyscan":
			return commandOutput{stdout: []byte(newKey)}, nil
		case name == "ssh-keygen" && contains(args, "-lf"):
			return commandOutput{stdout: []byte("256 SHA256:new host (ED25519)\n")}, nil
		case name == "ssh-keygen" && contains(args, "-R"):
			return commandOutput{}, nil
		default:
			t.Fatalf("意外命令: %s %v，stdin=%q", name, args, stdin)
			return commandOutput{}, nil
		}
	}

	result, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if err != nil {
		t.Fatalf("首次创建 known_hosts 失败: %v", err)
	}
	if !result.Applied || len(result.Existing) != 0 {
		t.Fatalf("首次创建结果 = %#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取新 known_hosts 失败: %v", err)
	}
	if string(data) != "[example.com]:2222 ssh-ed25519 AAAANEW\n" {
		t.Errorf("新 known_hosts 内容 = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取新 known_hosts 元数据失败: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("新 known_hosts 权限 = %04o，期望 0600", got)
	}
}

func TestRefresh_PreviewSupportsHashedKnownHostsRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(hashedKey), 0o600); err != nil {
		t.Fatalf("写入哈希 known_hosts 失败: %v", err)
	}
	deps := testDependencies(t, nil)
	originalCommand := deps.command
	deps.command = func(ctx context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		if name == "ssh-keygen" && contains(args, "-F") {
			return commandOutput{stdout: []byte("# found\n" + hashedKey)}, nil
		}
		return originalCommand(ctx, stdin, name, args...)
	}

	result, err := refresh(context.Background(), "example.com:2222", path, false, deps)
	if err != nil {
		t.Fatalf("预览哈希主机记录失败: %v", err)
	}
	if len(result.Existing) != 1 || result.Existing[0].SHA256 != "SHA256:old" {
		t.Fatalf("哈希记录指纹 = %#v", result.Existing)
	}
}

func TestRefresh_EmptyScanDoesNotModifyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}
	deps := testDependencies(t, nil)
	originalCommand := deps.command
	deps.command = func(ctx context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		if name == "ssh-keyscan" {
			return commandOutput{stdout: []byte("# no keys\n")}, nil
		}
		return originalCommand(ctx, stdin, name, args...)
	}

	_, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if err == nil || !strings.Contains(err.Error(), "未返回任何主机密钥") {
		t.Fatalf("空扫描错误 = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("读取原 known_hosts 失败: %v", readErr)
	}
	if string(data) != oldKey {
		t.Errorf("空扫描后原文件被修改: %q", data)
	}
}

func TestRefresh_ScanFailureDoesNotModifyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}
	deps := testDependencies(t, nil)
	originalCommand := deps.command
	deps.command = func(ctx context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		if name == "ssh-keyscan" {
			return commandOutput{stderr: []byte("network failure"), exitCode: 1}, errors.New("exit status 1")
		}
		return originalCommand(ctx, stdin, name, args...)
	}

	_, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if err == nil || !strings.Contains(err.Error(), "network failure") {
		t.Fatalf("扫描失败错误 = %v", err)
	}
	assertFileContent(t, path, oldKey)
}

func TestRefresh_RemoveFailureKeepsOriginalAndCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}
	deps := testDependencies(t, nil)
	originalCommand := deps.command
	deps.command = func(ctx context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		if name == "ssh-keygen" && contains(args, "-R") {
			return commandOutput{stderr: []byte("invalid known_hosts"), exitCode: 1}, errors.New("exit status 1")
		}
		return originalCommand(ctx, stdin, name, args...)
	}

	_, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if err == nil || !strings.Contains(err.Error(), "invalid known_hosts") {
		t.Fatalf("移除旧记录错误 = %v", err)
	}
	assertFileContent(t, path, oldKey)
	assertNoTemporaryFiles(t, dir)
}

func TestApplyKeys_RealSSHKeygenRejectsMalformedKnownHosts(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实 ssh-keygen，短模式跳过")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("未安装 ssh-keygen，跳过真实退出码回归测试")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	const malformed = "not-a-valid-known-host-line\n"
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatalf("写入损坏 known_hosts 失败: %v", err)
	}

	err := applyKeys(
		context.Background(),
		"example.invalid",
		path,
		[]byte("example.invalid ssh-ed25519 AAAANEW\n"),
		true,
		dependencies{
			command:       runCommand,
			rename:        os.Rename,
			remove:        os.Remove,
			syncDirectory: syncDirectory,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "移除旧主机密钥失败") {
		t.Fatalf("损坏信任库错误 = %v", err)
	}
	assertFileContent(t, path, malformed)
	assertNoTemporaryFiles(t, dir)
}

func TestRefresh_RenameFailureKeepsOriginalAndCleansTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}
	wantErr := errors.New("rename failed")

	_, err := refresh(context.Background(), "example.com:2222", path, true, testDependencies(t,
		func(string, string) error { return wantErr }))
	if !errors.Is(err, wantErr) {
		t.Fatalf("刷新错误 = %v, 期望识别 rename 失败", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("读取原 known_hosts 失败: %v", readErr)
	}
	if string(data) != oldKey {
		t.Errorf("rename 失败后原文件被修改: %q", data)
	}
	assertNoTemporaryFiles(t, dir)
}

func TestRefresh_DirectorySyncFailureRestoresExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(oldKey), 0o640); err != nil {
		t.Fatalf("写入 known_hosts 失败: %v", err)
	}
	wantErr := errors.New("directory sync failed")
	deps := testDependencies(t, nil)
	syncCalls := 0
	deps.syncDirectory = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return wantErr
		}
		return nil
	}

	_, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("刷新错误 = %v，期望识别目录同步失败", err)
	}
	if syncCalls != 2 {
		t.Fatalf("目录同步次数 = %d，期望更新和回滚各一次", syncCalls)
	}
	assertFileContent(t, path, oldKey)
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("读取回滚文件元数据失败: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("回滚后权限 = %04o，期望 0640", got)
	}
	assertNoTemporaryFiles(t, dir)
}

func TestRefresh_DirectorySyncFailureRemovesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	wantErr := errors.New("directory sync failed")
	deps := testDependencies(t, nil)
	syncCalls := 0
	deps.syncDirectory = func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return wantErr
		}
		return nil
	}
	deps.command = func(_ context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
		switch {
		case name == "ssh-keyscan":
			return commandOutput{stdout: []byte(newKey)}, nil
		case name == "ssh-keygen" && reflect.DeepEqual(args, []string{"-lf", "-"}):
			return commandOutput{stdout: []byte("256 SHA256:new host (ED25519)\n")}, nil
		case name == "ssh-keygen" && len(args) == 4 && args[0] == "-R":
			return commandOutput{}, nil
		default:
			t.Fatalf("意外命令: %s %v，stdin=%q", name, args, stdin)
			return commandOutput{}, nil
		}
	}

	_, err := refresh(context.Background(), "example.com:2222", path, true, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("刷新错误 = %v，期望识别目录同步失败", err)
	}
	if syncCalls != 2 {
		t.Fatalf("目录同步次数 = %d，期望更新和回滚各一次", syncCalls)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("回滚后新文件仍存在或状态异常: %v", statErr)
	}
	assertNoTemporaryFiles(t, dir)
}

func TestRefresh_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(target, []byte(oldKey), 0o600); err != nil {
		t.Fatalf("写入真实文件失败: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}

	_, err := refresh(context.Background(), "example.com:2222", path, false, testDependencies(t, nil))
	if err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("符号链接错误 = %v", err)
	}
}

func TestRefresh_RejectsNonRegularKnownHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("创建 known_hosts 目录失败: %v", err)
	}

	_, err := refresh(context.Background(), "example.com:2222", path, false, testDependencies(t, nil))
	if err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("非普通文件错误 = %v", err)
	}
}

func TestScanKeys_HostUsesOptionSeparator(t *testing.T) {
	want := []string{"-T", "10", "-p", "22", "--", "-example.invalid"}
	command := func(_ context.Context, _ []byte, name string, args ...string) (commandOutput, error) {
		if name != "ssh-keyscan" || !reflect.DeepEqual(args, want) {
			t.Fatalf("扫描命令 = %s %v，期望 ssh-keyscan %v", name, args, want)
		}
		return commandOutput{stdout: []byte("-example.invalid ssh-ed25519 AAAANEW\n")}, nil
	}

	keys, err := scanKeys(context.Background(), "-example.invalid", "22", "-example.invalid", command)
	if err != nil {
		t.Fatalf("扫描带短横线主机失败: %v", err)
	}
	if string(keys) != "-example.invalid ssh-ed25519 AAAANEW\n" {
		t.Errorf("归一化扫描结果 = %q", keys)
	}
}

func TestRunCommand_ContextTimeoutIsRecognizable(t *testing.T) {
	t.Setenv(hostKeyHelperEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := runCommand(ctx, nil, os.Args[0], "-test.run=TestHostKeyHelperProcess")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时错误 = %v，期望可识别 context.DeadlineExceeded", err)
	}
}

func testDependencies(t *testing.T, rename func(string, string) error) dependencies {
	t.Helper()
	if rename == nil {
		rename = os.Rename
	}
	return dependencies{
		rename:        rename,
		remove:        os.Remove,
		syncDirectory: syncDirectory,
		command: func(_ context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
			switch {
			case name == "ssh-keygen" && contains(args, "-F"):
				want := []string{"-F", "[example.com]:2222", "-f", args[len(args)-1]}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("ssh-keygen -F 参数 = %v，期望 %v", args, want)
				}
				return commandOutput{stdout: []byte("# found\n" + oldKey)}, nil
			case name == "ssh-keyscan":
				want := []string{"-T", "10", "-p", "2222", "--", "example.com"}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("ssh-keyscan 参数 = %v，期望 %v", args, want)
				}
				return commandOutput{stdout: []byte(newKey)}, nil
			case name == "ssh-keygen" && contains(args, "-lf"):
				want := []string{"-lf", "-"}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("ssh-keygen -lf 参数 = %v，期望 %v", args, want)
				}
				if strings.Contains(string(stdin), "AAAAOLD") {
					return commandOutput{stdout: []byte("256 SHA256:old host (ED25519)\n")}, nil
				}
				return commandOutput{stdout: []byte("256 SHA256:new host (ED25519)\n")}, nil
			case name == "ssh-keygen" && contains(args, "-R"):
				want := []string{"-R", "[example.com]:2222", "-f", args[len(args)-1]}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("ssh-keygen -R 参数 = %v，期望 %v", args, want)
				}
				path := args[len(args)-1]
				data, err := os.ReadFile(path)
				if err != nil {
					return commandOutput{}, err
				}
				var kept []string
				for _, line := range strings.Split(string(data), "\n") {
					if line != "" && !strings.Contains(line, "AAAAOLD") {
						kept = append(kept, line)
					}
				}
				content := ""
				if len(kept) > 0 {
					content = strings.Join(kept, "\n") + "\n"
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					return commandOutput{}, err
				}
				if err := os.WriteFile(path+".old", data, 0o600); err != nil {
					return commandOutput{}, err
				}
				return commandOutput{}, nil
			default:
				t.Fatalf("意外命令: %s %v", name, args)
				return commandOutput{}, nil
			}
		},
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s 内容 = %q，期望 %q", path, data, want)
	}
}

func assertNoTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".ark-known-hosts-*"))
	if err != nil {
		t.Fatalf("查找临时文件失败: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("残留 known_hosts 临时文件: %v", matches)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
