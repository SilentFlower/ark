package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
)

type fakeRunner struct {
	t     *testing.T
	run   func([]string) (string, error)
	calls [][]string
}

func (f *fakeRunner) Run(ctx context.Context, argv ...string) (string, error) {
	f.t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		f.t.Error("Runner.Run 没有收到命令级超时")
	}
	call := append([]string(nil), argv...)
	f.calls = append(f.calls, call)
	return f.run(call)
}

func (f *fakeRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fakeRunner 不支持 Stream")
}

func (f *fakeRunner) Feed(context.Context, io.Reader, ...string) error {
	return errors.New("测试 fakeRunner 不支持 Feed")
}

func TestRunLocal_凭证只进入Restic子进程(t *testing.T) {
	binDir := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "restic-env")
	leakPath := filepath.Join(t.TempDir(), "leaked-env")
	script := `#!/bin/sh
name=${0##*/}
if [ "$name" = "restic" ]; then
  printf '%s\n%s\n%s\n' "$RESTIC_REPOSITORY" "$RESTIC_PASSWORD_FILE" "$OBJECT_TOKEN" > "$DOCTOR_CAPTURE"
elif [ -n "${OBJECT_TOKEN+x}" ]; then
  printf 'leaked' > "$DOCTOR_LEAK"
fi
if [ "$name" = "systemd-analyze" ] && [ "$1" = "calendar" ]; then
  printf 'Next elapse: test\n'
else
  printf 'fake version\n'
fi
`
	for _, name := range []string{"restic", "ssh", "systemd-analyze"} {
		writeTestFile(t, filepath.Join(binDir, name), []byte(script), 0o700)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("DOCTOR_CAPTURE", capturePath)
	t.Setenv("DOCTOR_LEAK", leakPath)
	t.Setenv("RESTIC_REPOSITORY", "parent-wrong")
	t.Setenv("RESTIC_PASSWORD_FILE", "parent-wrong")

	passwordFile := filepath.Join(t.TempDir(), "repo.pass")
	envFile := filepath.Join(t.TempDir(), "repo.env")
	identityFile := filepath.Join(t.TempDir(), "host.key")
	knownHostsFile := filepath.Join(t.TempDir(), "known_hosts")
	writeTestFile(t, passwordFile, []byte("password"), 0o600)
	writeTestFile(t, envFile, []byte("OBJECT_TOKEN='secret value'\nRESTIC_REPOSITORY=env-wrong\nRESTIC_PASSWORD_FILE=env-wrong\n"), 0o600)
	writeTestFile(t, identityFile, []byte("identity"), 0o600)
	writeTestFile(t, knownHostsFile, []byte("host key"), 0o600)

	cfg := &config.Config{
		Repo: config.Repo{
			URL:          "s3:https://example.com/backup",
			PasswordFile: passwordFile,
			EnvFile:      envFile,
		},
		Hosts: []config.Host{
			{Host: "hub-01", Local: true},
			{
				Host: "web-01",
				SSH: &config.SSH{
					IdentityFile:   identityFile,
					KnownHostsFile: knownHostsFile,
				},
			},
		},
	}

	report := RunLocal(context.Background(), cfg)
	if report.Failed() {
		t.Fatalf("RunLocal 出现失败检查: %#v", report.Checks)
	}
	assertCheckStatus(t, report, "repo.access", StatusOK)
	assertCheckStatus(t, report, "web-01 / ssh.identity_file", StatusOK)
	assertCheckStatus(t, report, "web-01 / schedule.on_calendar", StatusOK)

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("读取 restic 环境捕获文件失败: %v", err)
	}
	want := strings.Join([]string{cfg.Repo.URL, passwordFile, "secret value", ""}, "\n")
	if string(captured) != want {
		t.Fatalf("restic 环境 = %q，期望 %q", captured, want)
	}
	if _, err := os.Stat(leakPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("对象存储凭证泄漏给了非 restic 子进程: %v", err)
	}
}

func TestRunLocal_Restic缺失时仓库检查降级(t *testing.T) {
	binDir := t.TempDir()
	script := []byte("#!/bin/sh\nprintf 'fake version\\n'\n")
	for _, name := range []string{"ssh", "systemd-analyze"} {
		writeTestFile(t, filepath.Join(binDir, name), script, 0o700)
	}
	t.Setenv("PATH", binDir)

	passwordFile := filepath.Join(t.TempDir(), "repo.pass")
	writeTestFile(t, passwordFile, []byte("password"), 0o600)
	report := RunLocal(context.Background(), &config.Config{
		Repo: config.Repo{URL: "local:/repo", PasswordFile: passwordFile},
	})

	assertCheckStatus(t, report, "restic", StatusFail)
	assertCheckStatus(t, report, "repo.access", StatusWarn)
}

func TestParseEnvFile_严格解析且不执行Shell语法(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.env")
	writeTestFile(t, path, []byte("# comment\nexport TOKEN='literal $(touch /tmp/not-created)'\nDUP=first\nDUP=second\nEMPTY=\n"), 0o600)

	values, err := parseEnvFile(path)
	if err != nil {
		t.Fatalf("parseEnvFile 返回错误: %v", err)
	}
	if values["TOKEN"] != "literal $(touch /tmp/not-created)" {
		t.Fatalf("TOKEN = %q，shell 语法不应被展开", values["TOKEN"])
	}
	if values["DUP"] != "second" {
		t.Fatalf("DUP = %q，期望后值覆盖前值", values["DUP"])
	}
	if values["EMPTY"] != "" {
		t.Fatalf("EMPTY = %q，期望空字符串", values["EMPTY"])
	}
}

func TestParseEnvFile_错误不回显行内容(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.env")
	secret := "SHOULD_NOT_APPEAR"
	writeTestFile(t, path, []byte(secret), 0o600)

	_, err := parseEnvFile(path)
	if err == nil {
		t.Fatal("无等号的 env 行应返回错误")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄漏了 env 内容: %v", err)
	}
}

func TestRunLocal_Restic错误输出不泄漏(t *testing.T) {
	binDir := t.TempDir()
	secret := "SECRET_FROM_RESTIC_OUTPUT"
	resticScript := "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then\n  printf 'restic test\\n'\n  exit 0\nfi\nprintf '" + secret + "\\n' >&2\nexit 1\n"
	writeTestFile(t, filepath.Join(binDir, "restic"), []byte(resticScript), 0o700)
	writeTestFile(t, filepath.Join(binDir, "ssh"), []byte("#!/bin/sh\nprintf 'ssh\\n'\n"), 0o700)
	writeTestFile(t, filepath.Join(binDir, "systemd-analyze"), []byte("#!/bin/sh\nprintf 'systemd\\n'\n"), 0o700)
	t.Setenv("PATH", binDir)

	passwordFile := filepath.Join(t.TempDir(), "repo.pass")
	writeTestFile(t, passwordFile, []byte("password"), 0o600)
	report := RunLocal(context.Background(), &config.Config{
		Repo: config.Repo{URL: "local:/repo", PasswordFile: passwordFile},
	})

	check := findCheck(t, report, "repo.access")
	if check.Status != StatusFail {
		t.Fatalf("repo.access 状态 = %s，期望 %s", check.Status, StatusFail)
	}
	if strings.Contains(check.Detail, secret) {
		t.Fatalf("restic 错误输出泄漏到报告: %s", check.Detail)
	}
}

func TestValidatePathMetadata_拒绝目录和过宽权限(t *testing.T) {
	tests := []struct {
		name             string
		metadata         pathMetadata
		requireRegular   bool
		forbiddenPerm    os.FileMode
		wantErrorContain string
	}{
		{
			name:             "目录不能冒充文件",
			metadata:         pathMetadata{kind: pathKindDirectory, perm: 0o700},
			requireRegular:   true,
			wantErrorContain: "期望普通文件",
		},
		{
			name:             "密钥权限不能对同组开放",
			metadata:         pathMetadata{kind: pathKindRegular, perm: 0o640},
			requireRegular:   true,
			forbiddenPerm:    0o077,
			wantErrorContain: "权限过宽",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePathMetadata("/test/path", tc.metadata, tc.requireRegular, tc.forbiddenPerm)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrorContain) {
				t.Fatalf("文件判定错误 = %v，期望包含 %q", err, tc.wantErrorContain)
			}
		})
	}
}

func TestRunHost_完整检查并保持参数边界(t *testing.T) {
	composeFile := "/srv/app/compose;$(touch /tmp/x).yaml"
	envFile := "/srv/app/env file"
	projectName := "project 'quoted'"
	service := "db;$(id) service"
	volume := "data;$(id)"
	filesPath := "/srv/data;$(id)"
	host := &config.Host{
		Host: "web-01",
		Project: config.Project{
			ComposeFile: composeFile,
			EnvFile:     envFile,
			ProjectName: projectName,
		},
		Targets: []config.Target{
			{Type: config.TargetPostgres, Service: service, Database: "app"},
			{Type: config.TargetVolume, Name: volume},
			{Type: config.TargetFiles, Name: "config", Paths: []string{filesPath}},
			{Type: config.TargetImageDigest, Services: []string{service}},
		},
	}

	runner := &fakeRunner{t: t}
	runner.run = func(argv []string) (string, error) {
		switch {
		case reflect.DeepEqual(argv, []string{"true"}):
			return "", nil
		case reflect.DeepEqual(argv, []string{"date", "+%s"}):
			return "1000\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "--version"}):
			return "Docker 28\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "compose", "version"}):
			return "Docker Compose v2\n", nil
		case len(argv) == 6 && argv[0] == "stat" && argv[1] == "-L":
			return "81a4 600\n", nil
		case reflect.DeepEqual(argv, []string{
			"docker", "compose", "-f", composeFile, "-p", projectName,
			"--env-file", envFile, "config", "--services",
		}):
			return service + "\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", volume}):
			return "{}", nil
		default:
			return "", fmt.Errorf("未配置 fake 响应: %q", argv)
		}
	}
	now := fixedTimes(t, time.Unix(1000, 0), time.Unix(1000, 0))
	report := runHost(context.Background(), &config.Config{}, host, runner, now)

	if report.Failed() {
		t.Fatalf("RunHost 出现失败检查: %#v", report.Checks)
	}
	assertCheckStatus(t, report, "web-01 / connection", StatusOK)
	assertCheckStatus(t, report, "web-01 / clock", StatusOK)
	assertCheckStatus(t, report, "web-01 / target postgres/"+service+"/app", StatusOK)
	assertCheckStatus(t, report, "web-01 / target volume/"+volume, StatusOK)
	assertCheckStatus(t, report, "web-01 / target files/config", StatusOK)

	assertRunnerCall(t, runner.calls, []string{
		"docker", "compose", "-f", composeFile, "-p", projectName,
		"--env-file", envFile, "config", "--services",
	})
	assertRunnerCall(t, runner.calls, []string{"docker", "volume", "inspect", volume})
	assertRunnerCall(t, runner.calls, []string{"stat", "-L", "-c", "%f %a", "--", filesPath})
}

func TestRunHost_连接失败时后续检查全部降级(t *testing.T) {
	host := &config.Host{
		Host: "web-01",
		Project: config.Project{
			ComposeFile: "/srv/compose.yaml",
			EnvFile:     "/srv/.env",
		},
		Targets: []config.Target{
			{Type: config.TargetRedis, Service: "redis"},
			{Type: config.TargetVolume, Name: "data"},
			{Type: config.TargetFiles, Name: "files", Paths: []string{"/srv/data"}},
		},
	}
	runner := &fakeRunner{t: t, run: func(argv []string) (string, error) {
		if !reflect.DeepEqual(argv, []string{"true"}) {
			t.Fatalf("连接失败后仍执行了命令: %q", argv)
		}
		return "", errors.New("拒绝连接")
	}}

	report := runHost(context.Background(), &config.Config{}, host, runner, time.Now)
	assertCheckStatus(t, report, "web-01 / connection", StatusFail)
	for _, name := range []string{
		"web-01 / clock",
		"web-01 / docker",
		"web-01 / docker compose",
		"web-01 / project.compose_file",
		"web-01 / project.env_file",
		"web-01 / compose.services",
		"web-01 / target redis/redis",
		"web-01 / target volume/data",
		"web-01 / target files/files",
	} {
		assertCheckStatus(t, report, name, StatusWarn)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("连接失败后调用数 = %d，期望 1", len(runner.calls))
	}
}

func TestRunHost_Docker失败不阻断独立文件检查(t *testing.T) {
	composeFile := "/srv/compose.yaml"
	filesPath := "/srv/data"
	host := &config.Host{
		Host:    "web-01",
		Project: config.Project{ComposeFile: composeFile},
		Targets: []config.Target{
			{Type: config.TargetRedis, Service: "redis"},
			{Type: config.TargetVolume, Name: "data"},
			{Type: config.TargetFiles, Name: "files", Paths: []string{filesPath}},
		},
	}
	runner := &fakeRunner{t: t}
	runner.run = func(argv []string) (string, error) {
		switch {
		case reflect.DeepEqual(argv, []string{"true"}):
			return "", nil
		case reflect.DeepEqual(argv, []string{"date", "+%s"}):
			return "1000\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "--version"}):
			return "", errors.New("docker 不可用")
		case len(argv) == 6 && argv[0] == "stat" && argv[1] == "-L":
			return "81a4 600\n", nil
		default:
			return "", fmt.Errorf("不应执行命令: %q", argv)
		}
	}

	report := runHost(context.Background(), &config.Config{}, host, runner,
		fixedTimes(t, time.Unix(1000, 0), time.Unix(1000, 0)))
	assertCheckStatus(t, report, "web-01 / docker", StatusFail)
	assertCheckStatus(t, report, "web-01 / docker compose", StatusWarn)
	assertCheckStatus(t, report, "web-01 / compose.services", StatusWarn)
	assertCheckStatus(t, report, "web-01 / target redis/redis", StatusWarn)
	assertCheckStatus(t, report, "web-01 / target volume/data", StatusWarn)
	assertCheckStatus(t, report, "web-01 / target files/files", StatusOK)
}

func TestRunHost_Compose服务失败不阻断Volume和Files(t *testing.T) {
	host := &config.Host{
		Host:    "web-01",
		Project: config.Project{ComposeFile: "/srv/compose.yaml"},
		Targets: []config.Target{
			{Type: config.TargetRedis, Service: "redis"},
			{Type: config.TargetVolume, Name: "data"},
			{Type: config.TargetFiles, Name: "files", Paths: []string{"/srv/data"}},
		},
	}
	runner := &fakeRunner{t: t}
	runner.run = func(argv []string) (string, error) {
		switch {
		case reflect.DeepEqual(argv, []string{"true"}),
			reflect.DeepEqual(argv, []string{"docker", "volume", "inspect", "data"}):
			return "", nil
		case reflect.DeepEqual(argv, []string{"date", "+%s"}):
			return "1000\n", nil
		case reflect.DeepEqual(argv, []string{"docker", "--version"}),
			reflect.DeepEqual(argv, []string{"docker", "compose", "version"}):
			return "version\n", nil
		case len(argv) == 6 && argv[0] == "stat" && argv[1] == "-L":
			return "81a4 600\n", nil
		case reflect.DeepEqual(argv, []string{
			"docker", "compose", "-f", "/srv/compose.yaml", "config", "--services",
		}):
			return "sensitive output", errors.New("compose 失败")
		default:
			return "", fmt.Errorf("未配置 fake 响应: %q", argv)
		}
	}

	report := runHost(context.Background(), &config.Config{}, host, runner,
		fixedTimes(t, time.Unix(1000, 0), time.Unix(1000, 0)))
	assertCheckStatus(t, report, "web-01 / compose.services", StatusFail)
	assertCheckStatus(t, report, "web-01 / target redis/redis", StatusWarn)
	assertCheckStatus(t, report, "web-01 / target volume/data", StatusOK)
	assertCheckStatus(t, report, "web-01 / target files/files", StatusOK)
	if strings.Contains(findCheck(t, report, "web-01 / compose.services").Detail, "sensitive output") {
		t.Fatal("compose 错误输出不应进入报告")
	}
}

func TestCheckServiceTarget_服务不存在时失败(t *testing.T) {
	report := &Report{}
	checkServiceTarget(report, "web-01 / target redis/redis", "redis", map[string]bool{"api": true}, true)
	assertCheckStatus(t, report, "web-01 / target redis/redis", StatusFail)
}

func TestCheckClock_六十秒边界(t *testing.T) {
	tests := []struct {
		name       string
		remoteUnix int64
		want       Status
	}{
		{name: "恰好六十秒通过", remoteUnix: 1060, want: StatusOK},
		{name: "超过六十秒告警", remoteUnix: 1061, want: StatusWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{t: t, run: func(argv []string) (string, error) {
				return fmt.Sprintf("%d\n", tc.remoteUnix), nil
			}}
			report := &Report{}
			checkClock(context.Background(), report, "clock", runner,
				fixedTimes(t, time.Unix(1000, 0), time.Unix(1000, 0)))
			assertCheckStatus(t, report, "clock", tc.want)
		})
	}
}

func TestPathMetadata_本地与远程判定一致(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	writeTestFile(t, path, []byte("secret"), 0o600)
	local, err := localPathMetadata(path)
	if err != nil {
		t.Fatalf("读取本地元数据失败: %v", err)
	}
	remote, err := parseStatMetadata("8180 600\n")
	if err != nil {
		t.Fatalf("解析远程元数据失败: %v", err)
	}
	if local != remote {
		t.Fatalf("本地元数据 %#v 与远程元数据 %#v 不一致", local, remote)
	}
	if err := validatePathMetadata(path, local, true, 0o077); err != nil {
		t.Fatalf("本地文件判定失败: %v", err)
	}
	if err := validatePathMetadata(path, remote, true, 0o077); err != nil {
		t.Fatalf("远程文件判定失败: %v", err)
	}
}

func TestHostPathMetadata_远程Stat跟随符号链接(t *testing.T) {
	host := &config.Host{Host: "web-01"}
	runner := &fakeRunner{t: t, run: func(argv []string) (string, error) {
		want := []string{"stat", "-L", "-c", "%f %a", "--", "/srv/current/compose.yaml"}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("远程 stat argv = %q，期望 %q", argv, want)
		}
		return "81a4 644\n", nil
	}}

	metadata, err := hostPathMetadata(context.Background(), host, runner, "/srv/current/compose.yaml")
	if err != nil {
		t.Fatalf("读取远程符号链接目标元数据失败: %v", err)
	}
	if metadata.kind != pathKindRegular || metadata.perm != 0o644 {
		t.Fatalf("远程元数据 = %#v，期望普通文件 0644", metadata)
	}
}

func TestRunnerForHost_选择Local或SSH(t *testing.T) {
	local, err := runnerForHost(&config.Host{Local: true})
	if err != nil || local == nil {
		t.Fatalf("创建 local Runner 失败: %v", err)
	}
	remote, err := runnerForHost(&config.Host{SSH: &config.SSH{
		Address:        "127.0.0.1:22",
		User:           "root",
		IdentityFile:   "/tmp/test.key",
		KnownHostsFile: "/tmp/known_hosts",
	}})
	if err != nil || remote == nil {
		t.Fatalf("创建 SSH Runner 失败: %v", err)
	}
	if reflect.TypeOf(local) == reflect.TypeOf(remote) {
		t.Fatalf("local 与 SSH 应选择不同 Runner，实际都是 %T", local)
	}
	if _, err := runnerForHost(&config.Host{}); err == nil {
		t.Fatal("远程 host 缺少 SSH 配置时应返回错误")
	}
}

func TestRunHostIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过真实 host 集成测试")
	}
	configPath := os.Getenv("ARK_DOCTOR_TEST_CONFIG")
	hostName := os.Getenv("ARK_DOCTOR_TEST_HOST")
	if configPath == "" || hostName == "" {
		t.Skip("未配置 ARK_DOCTOR_TEST_CONFIG 和 ARK_DOCTOR_TEST_HOST")
	}

	cfg, err := config.LoadAndValidate(configPath)
	if err != nil {
		t.Fatalf("加载集成测试清单失败: %v", err)
	}
	var host *config.Host
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Host == hostName {
			host = &cfg.Hosts[i]
			break
		}
	}
	if host == nil {
		t.Fatalf("清单中不存在集成测试 host %q", hostName)
	}

	report := RunHost(context.Background(), cfg, host)
	for _, item := range []string{"connection", "clock", "docker", "docker compose", "project.compose_file", "compose.services"} {
		findCheck(t, report, hostName+" / "+item)
	}
	if report.Failed() {
		t.Fatalf("真实 host 检查未通过: %#v", report.Checks)
	}
}

func fixedTimes(t *testing.T, times ...time.Time) func() time.Time {
	t.Helper()
	index := 0
	return func() time.Time {
		if index >= len(times) {
			t.Fatalf("now 调用次数超过预期 %d", len(times))
		}
		value := times[index]
		index++
		return value
	}
}

func writeTestFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("写入测试文件 %s 失败: %v", path, err)
	}
}

func findCheck(t *testing.T, report *Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("报告中缺少检查项 %q: %#v", name, report.Checks)
	return Check{}
}

func assertCheckStatus(t *testing.T, report *Report, name string, want Status) {
	t.Helper()
	check := findCheck(t, report, name)
	if check.Status != want {
		t.Fatalf("检查项 %q 状态 = %s，期望 %s，详情: %s", name, check.Status, want, check.Detail)
	}
}

func assertRunnerCall(t *testing.T, calls [][]string, want []string) {
	t.Helper()
	for _, call := range calls {
		if reflect.DeepEqual(call, want) {
			return
		}
	}
	t.Fatalf("Runner 调用中没有 %q，实际: %q", want, calls)
}
