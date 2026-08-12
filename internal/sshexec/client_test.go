package sshexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
)

const (
	helperEnabledEnv    = "ARK_SSHEXEC_HELPER"
	helperModeEnv       = "ARK_SSHEXEC_HELPER_MODE"
	helperStdoutEnv     = "ARK_SSHEXEC_HELPER_STDOUT"
	helperStderrEnv     = "ARK_SSHEXEC_HELPER_STDERR"
	helperExitCodeEnv   = "ARK_SSHEXEC_HELPER_EXIT_CODE"
	helperOutputFileEnv = "ARK_SSHEXEC_HELPER_OUTPUT_FILE"
)

func helperCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	helperArgs := []string{"-test.run=TestHelperProcess", "--", name}
	helperArgs = append(helperArgs, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], helperArgs...)
	cmd.Env = append(os.Environ(), helperEnabledEnv+"=1")
	return cmd
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnabledEnv) != "1" {
		return
	}

	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		os.Exit(125)
	}
	args := os.Args[separator+1:]

	switch os.Getenv(helperModeEnv) {
	case "args":
		if err := json.NewEncoder(os.Stdout).Encode(args); err != nil {
			os.Exit(125)
		}
	case "output":
		if _, err := io.WriteString(os.Stdout, os.Getenv(helperStdoutEnv)); err != nil {
			os.Exit(125)
		}
		if _, err := io.WriteString(os.Stderr, os.Getenv(helperStderrEnv)); err != nil {
			os.Exit(125)
		}
	case "feed":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(125)
		}
		if err := os.WriteFile(os.Getenv(helperOutputFileEnv), data, 0o600); err != nil {
			os.Exit(125)
		}
		if _, err := io.WriteString(os.Stderr, os.Getenv(helperStderrEnv)); err != nil {
			os.Exit(125)
		}
	case "block":
		time.Sleep(10 * time.Second)
	case "kill":
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			os.Exit(125)
		}
		time.Sleep(10 * time.Second)
	default:
		os.Exit(125)
	}

	exitCode := 0
	if raw := os.Getenv(helperExitCodeEnv); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			os.Exit(125)
		}
		exitCode = parsed
	}
	os.Exit(exitCode)
}

func validSSHConfig() config.SSH {
	return config.SSH{
		Address:        "127.0.0.1:22",
		User:           "backup",
		IdentityFile:   "/etc/ark/keys/backup.key",
		KnownHostsFile: "/etc/ark/known_hosts",
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "普通值", in: "abc", want: "'abc'"},
		{name: "空字符串", in: "", want: "''"},
		{name: "包含空格", in: "a b", want: "'a b'"},
		{name: "命令分隔符", in: "a; id", want: "'a; id'"},
		{name: "命令替换", in: "$(id)", want: "'$(id)'"},
		{name: "单引号", in: "a'b", want: "'a'\\''b'"},
		{name: "换行", in: "a\nb", want: "'a\nb'"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Errorf("shellQuote(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewSSH_RejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.SSH)
		wantSub string
	}{
		{name: "地址缺少端口", mutate: func(c *config.SSH) { c.Address = "127.0.0.1" }, wantSub: "host:port"},
		{name: "地址缺少主机名", mutate: func(c *config.SSH) { c.Address = ":22" }, wantSub: "缺少主机名"},
		{name: "端口不是数字", mutate: func(c *config.SSH) { c.Address = "127.0.0.1:ssh" }, wantSub: "端口"},
		{name: "用户为空", mutate: func(c *config.SSH) { c.User = "" }, wantSub: "ssh.user"},
		{name: "私钥为空", mutate: func(c *config.SSH) { c.IdentityFile = "" }, wantSub: "identity_file"},
		{name: "私钥是相对路径", mutate: func(c *config.SSH) { c.IdentityFile = "keys/id" }, wantSub: "绝对路径"},
		{name: "known_hosts 为空", mutate: func(c *config.SSH) { c.KnownHostsFile = "" }, wantSub: "known_hosts_file"},
		{name: "known_hosts 是相对路径", mutate: func(c *config.SSH) { c.KnownHostsFile = "known_hosts" }, wantSub: "绝对路径"},
		{name: "主机密钥策略非法", mutate: func(c *config.SSH) { c.HostKeyPolicy = "no" }, wantSub: "host_key_policy"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validSSHConfig()
			tc.mutate(&cfg)
			_, err := NewSSH(cfg)
			if err == nil {
				t.Fatal("期望构造失败，实际通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 中未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSSHRunner_RunBuildsArguments(t *testing.T) {
	t.Setenv(helperModeEnv, "args")
	cfg := config.SSH{
		Address:        "[2001:db8::1]:2222",
		User:           "backup",
		IdentityFile:   "/etc/ark/keys/backup key",
		KnownHostsFile: "/etc/ark/known hosts",
	}
	runner, err := newSSHRunner(cfg, helperCommand)
	if err != nil {
		t.Fatalf("构造 SSH Runner 失败: %v", err)
	}

	out, err := runner.Run(context.Background(), "printf", "%s", "a b", "x'y", "$(id);")
	if err != nil {
		t.Fatalf("执行伪 SSH 失败: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析伪 SSH 参数失败: %v，原始输出: %q", err, out)
	}

	want := []string{
		"ssh",
		"-T",
		"-o", "Compression=no",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/etc/ark/known hosts",
		"-o", "IdentitiesOnly=yes",
		"-i", "/etc/ark/keys/backup key",
		"-p", "2222",
		"-l", "backup",
		"--", "2001:db8::1",
		"'printf' '%s' 'a b' 'x'\\''y' '$(id);'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ssh argv = %#v，期望 %#v", got, want)
	}
	if containsValue(got, "StrictHostKeyChecking=no") {
		t.Errorf("SSH argv 不得关闭主机密钥校验: %#v", got)
	}
}

func TestSSHRunner_StrictHostKeyPolicy(t *testing.T) {
	t.Setenv(helperModeEnv, "args")
	cfg := validSSHConfig()
	cfg.HostKeyPolicy = config.SSHHostKeyPolicyStrict
	runner, err := newSSHRunner(cfg, helperCommand)
	if err != nil {
		t.Fatalf("构造严格模式 SSH Runner 失败: %v", err)
	}

	out, err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("执行伪 SSH 失败: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析伪 SSH 参数失败: %v", err)
	}
	if !containsAdjacent(got, "-o", "StrictHostKeyChecking=yes") {
		t.Errorf("严格模式 SSH argv = %#v，缺少 StrictHostKeyChecking=yes", got)
	}
	if containsValue(got, "StrictHostKeyChecking=no") {
		t.Errorf("严格模式 SSH argv 不得关闭主机密钥校验: %#v", got)
	}
}

func containsAdjacent(values []string, first, second string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == first && values[i+1] == second {
			return true
		}
	}
	return false
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestLocalRunner_RunPreservesArgv(t *testing.T) {
	t.Setenv(helperModeEnv, "args")
	runner := newLocalRunner(helperCommand)
	marker := filepath.Join(t.TempDir(), "不应创建")
	arguments := []string{"a b", "$(touch " + marker + ")", "x;y", "x'y", ""}

	out, err := runner.Run(context.Background(), append([]string{"literal-command"}, arguments...)...)
	if err != nil {
		t.Fatalf("执行伪本地命令失败: %v", err)
	}
	var got []string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("解析本地 argv 失败: %v", err)
	}
	want := append([]string{"literal-command"}, arguments...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("本地 argv = %#v，期望 %#v", got, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("shell 元字符被执行，意外创建了 %s", marker)
	}
}

func TestRunner_RejectsEmptyArgv(t *testing.T) {
	ssh, err := newSSHRunner(validSSHConfig(), helperCommand)
	if err != nil {
		t.Fatalf("构造 SSH Runner 失败: %v", err)
	}
	runners := []struct {
		name   string
		runner Runner
	}{
		{name: "本地", runner: newLocalRunner(helperCommand)},
		{name: "SSH", runner: ssh},
	}

	for _, tc := range runners {
		t.Run(tc.name+" Run", func(t *testing.T) {
			if _, err := tc.runner.Run(context.Background()); err == nil {
				t.Fatal("空 argv 应该报错")
			}
		})
		t.Run(tc.name+" Stream", func(t *testing.T) {
			if _, _, err := tc.runner.Stream(context.Background()); err == nil {
				t.Fatal("空 argv 应该报错")
			}
		})
		t.Run(tc.name+" Feed", func(t *testing.T) {
			if err := tc.runner.Feed(context.Background(), strings.NewReader("")); err == nil {
				t.Fatal("空 argv 应该报错")
			}
		})
	}
}

func TestRun_ReturnsOutputOnFailure(t *testing.T) {
	t.Setenv(helperModeEnv, "output")
	t.Setenv(helperStdoutEnv, "stdout\n")
	t.Setenv(helperStderrEnv, "stderr\n")
	t.Setenv(helperExitCodeEnv, "7")

	out, err := newLocalRunner(helperCommand).Run(context.Background(), "probe")
	if err == nil {
		t.Fatal("非零退出应该返回错误")
	}
	if !strings.Contains(out, "stdout") || !strings.Contains(out, "stderr") {
		t.Errorf("合并输出 = %q，期望同时包含 stdout 和 stderr", out)
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("错误信息 %q 中没有退出状态", err.Error())
	}
}

func TestRun_HostKeyConflictIncludesRefreshHint(t *testing.T) {
	t.Setenv(helperModeEnv, "output")
	t.Setenv(helperStderrEnv, "REMOTE HOST IDENTIFICATION HAS CHANGED!")
	t.Setenv(helperExitCodeEnv, "255")
	runner, err := newSSHRunner(validSSHConfig(), helperCommand)
	if err != nil {
		t.Fatalf("构造 SSH Runner 失败: %v", err)
	}

	_, err = runner.Run(context.Background(), "true")
	if err == nil || !strings.Contains(err.Error(), "ark host-key refresh --host") {
		t.Fatalf("主机密钥冲突错误 = %v，期望包含刷新指引", err)
	}
}

func TestRun_ContextCanceledIsRecognizable(t *testing.T) {
	t.Setenv(helperModeEnv, "block")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newLocalRunner(helperCommand).Run(ctx, "probe")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run 错误 = %v，期望可识别 context.Canceled", err)
	}
}

func TestStream(t *testing.T) {
	t.Run("stdout 与 stderr 分离", func(t *testing.T) {
		t.Setenv(helperModeEnv, "output")
		t.Setenv(helperStdoutEnv, "payload")
		t.Setenv(helperStderrEnv, "diagnostic")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		stdout, wait, err := runner.Stream(context.Background(), "stream")
		if err != nil {
			t.Fatalf("启动流命令失败: %v", err)
		}
		got, err := io.ReadAll(stdout)
		if err != nil {
			t.Fatalf("读取 stdout 失败: %v", err)
		}
		if err := wait(); err != nil {
			t.Fatalf("等待流命令失败: %v", err)
		}
		if string(got) != "payload" {
			t.Errorf("stdout = %q，期望只包含 payload", got)
		}
	})

	t.Run("非零退出由 Wait 返回", func(t *testing.T) {
		t.Setenv(helperModeEnv, "output")
		t.Setenv(helperStdoutEnv, "partial")
		t.Setenv(helperStderrEnv, "remote failed")
		t.Setenv(helperExitCodeEnv, "9")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		stdout, wait, err := runner.Stream(context.Background(), "stream")
		if err != nil {
			t.Fatalf("启动流命令失败: %v", err)
		}
		got, err := io.ReadAll(stdout)
		if err != nil {
			t.Fatalf("读取 stdout 失败: %v", err)
		}
		if string(got) != "partial" {
			t.Errorf("stdout = %q，期望 partial", got)
		}
		waitErr := wait()
		if waitErr == nil {
			t.Fatal("非零退出的 Wait 应该报错")
		}
		if !strings.Contains(waitErr.Error(), "remote failed") {
			t.Errorf("Wait 错误 %q 中未包含 stderr 诊断", waitErr.Error())
		}
	})

	t.Run("主机密钥冲突由 Wait 返回刷新指引", func(t *testing.T) {
		t.Setenv(helperModeEnv, "output")
		t.Setenv(helperStderrEnv, "Host key verification failed.")
		t.Setenv(helperExitCodeEnv, "255")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		stdout, wait, err := runner.Stream(context.Background(), "stream")
		if err != nil {
			t.Fatalf("启动流命令失败: %v", err)
		}
		if _, err := io.Copy(io.Discard, stdout); err != nil {
			t.Fatalf("读取冲突命令 stdout 失败: %v", err)
		}
		waitErr := wait()
		if waitErr == nil || !strings.Contains(waitErr.Error(), "ark host-key refresh --host") {
			t.Fatalf("主机密钥冲突 Wait 错误 = %v，期望包含刷新指引", waitErr)
		}
	})

	t.Run("进程被 kill 由 Wait 返回", func(t *testing.T) {
		t.Setenv(helperModeEnv, "kill")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		stdout, wait, err := runner.Stream(context.Background(), "stream")
		if err != nil {
			t.Fatalf("启动流命令失败: %v", err)
		}
		if _, err := io.Copy(io.Discard, stdout); err != nil {
			t.Fatalf("读取被 kill 进程的 stdout 失败: %v", err)
		}
		if err := wait(); err == nil {
			t.Fatal("进程被 kill 后 Wait 应该报错")
		}
	})

	t.Run("context 超时可识别", func(t *testing.T) {
		t.Setenv(helperModeEnv, "block")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		stdout, wait, err := runner.Stream(ctx, "stream")
		if err != nil {
			t.Fatalf("启动流命令失败: %v", err)
		}
		if _, err := io.Copy(io.Discard, stdout); err != nil {
			t.Fatalf("读取超时进程的 stdout 失败: %v", err)
		}
		waitErr := wait()
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			t.Errorf("Wait 错误 = %v，期望可识别 DeadlineExceeded", waitErr)
		}
	})
}

func TestFeed(t *testing.T) {
	t.Run("原样传递大输入", func(t *testing.T) {
		t.Setenv(helperModeEnv, "feed")
		outputFile := filepath.Join(t.TempDir(), "stdin.bin")
		t.Setenv(helperOutputFileEnv, outputFile)
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}
		want := bytes.Repeat([]byte("ark-stream\x00"), 16*1024)

		if err := runner.Feed(context.Background(), bytes.NewReader(want), "restore"); err != nil {
			t.Fatalf("Feed 失败: %v", err)
		}
		got, err := os.ReadFile(outputFile)
		if err != nil {
			t.Fatalf("读取 helper 输入文件失败: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Feed 输入长度 = %d，期望 %d，内容不一致", len(got), len(want))
		}
	})

	t.Run("非零退出返回 stderr", func(t *testing.T) {
		t.Setenv(helperModeEnv, "output")
		t.Setenv(helperStderrEnv, "restore failed")
		t.Setenv(helperExitCodeEnv, "5")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		err = runner.Feed(context.Background(), strings.NewReader("data"), "restore")
		if err == nil || !strings.Contains(err.Error(), "restore failed") {
			t.Errorf("Feed 错误 = %v，期望包含 stderr 诊断", err)
		}
	})

	t.Run("主机密钥冲突返回刷新指引", func(t *testing.T) {
		t.Setenv(helperModeEnv, "output")
		t.Setenv(helperStderrEnv, "REMOTE HOST IDENTIFICATION HAS CHANGED!")
		t.Setenv(helperExitCodeEnv, "255")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}

		err = runner.Feed(context.Background(), strings.NewReader("data"), "restore")
		if err == nil || !strings.Contains(err.Error(), "ark host-key refresh --host") {
			t.Fatalf("主机密钥冲突 Feed 错误 = %v，期望包含刷新指引", err)
		}
	})

	t.Run("context 超时可识别", func(t *testing.T) {
		t.Setenv(helperModeEnv, "block")
		runner, err := newSSHRunner(validSSHConfig(), helperCommand)
		if err != nil {
			t.Fatalf("构造 SSH Runner 失败: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err = runner.Feed(ctx, strings.NewReader("data"), "restore")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Feed 错误 = %v，期望可识别 DeadlineExceeded", err)
		}
	})
}

func TestSSHIntegration_Localhost(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实 localhost sshd，短模式跳过")
	}

	cfg := config.SSH{
		Address:        os.Getenv("ARK_SSH_TEST_ADDRESS"),
		User:           os.Getenv("ARK_SSH_TEST_USER"),
		IdentityFile:   os.Getenv("ARK_SSH_TEST_IDENTITY_FILE"),
		KnownHostsFile: os.Getenv("ARK_SSH_TEST_KNOWN_HOSTS_FILE"),
	}
	if cfg.Address == "" || cfg.User == "" || cfg.IdentityFile == "" || cfg.KnownHostsFile == "" {
		t.Skip("未配置 ARK_SSH_TEST_*，跳过 localhost SSH 集成测试")
	}
	runner, err := NewSSH(cfg)
	if err != nil {
		t.Fatalf("构造 localhost SSH Runner 失败: %v", err)
	}

	runStream := func(t *testing.T, argv ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stdout, wait, err := runner.Stream(ctx, argv...)
		if err != nil {
			t.Fatalf("启动 localhost SSH 命令失败: %v", err)
		}
		data, err := io.ReadAll(stdout)
		if err != nil {
			t.Fatalf("读取 localhost SSH stdout 失败: %v", err)
		}
		return string(data), wait()
	}

	t.Run("正常执行", func(t *testing.T) {
		out, err := runStream(t, "printf", "%s", "ok")
		if err != nil {
			t.Fatalf("Wait 失败: %v", err)
		}
		if out != "ok" {
			t.Errorf("stdout = %q，期望 ok", out)
		}
	})

	t.Run("远程参数不能注入额外命令", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "不应创建")
		payload := "safe; $(touch " + marker + "); x'y with spaces"
		out, err := runStream(t, "printf", "%s", payload)
		if err != nil {
			t.Fatalf("Wait 失败: %v", err)
		}
		if out != payload {
			t.Errorf("stdout = %q，期望原样返回 %q", out, payload)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("远程 shell 元字符被执行，意外创建了 %s", marker)
		}
	})

	t.Run("远程命令非零退出", func(t *testing.T) {
		_, err := runStream(t, "sh", "-c", "exit 7")
		if err == nil {
			t.Fatal("远程命令非零退出时 Wait 应该报错")
		}
	})

	t.Run("远程进程被 kill", func(t *testing.T) {
		_, err := runStream(t, "sh", "-c", "kill -KILL $$")
		if err == nil {
			t.Fatal("远程进程被 kill 时 Wait 应该报错")
		}
	})
}
