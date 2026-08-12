package restic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
)

type commandCall struct {
	name string
	args []string
}

type helperPlan struct {
	mode  string
	extra []string
}

func TestResticHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}

	mode := os.Args[separator+1]
	extra := os.Args[separator+2:]
	switch mode {
	case "ok":
		os.Exit(0)
	case "exit10":
		os.Exit(10)
	case "exit12":
		os.Exit(12)
	case "backup-success":
		if len(extra) != 1 {
			os.Exit(90)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil || os.WriteFile(extra[0], data, 0o600) != nil {
			os.Exit(91)
		}
		fmt.Fprintln(os.Stdout, `{"message_type":"status","bytes_done":4}`)
		fmt.Fprintln(os.Stdout, `{"message_type":"summary","backup_start":"2026-08-12T01:02:03Z","snapshot_id":"snapshot-123","total_bytes_processed":4}`)
		os.Exit(0)
	case "backup-summary-fail":
		fmt.Fprintln(os.Stdout, `{"message_type":"summary","backup_start":"2026-08-12T01:02:03Z","snapshot_id":"snapshot-failed","total_bytes_processed":4}`)
		os.Exit(7)
	case "backup-malformed":
		fmt.Fprint(os.Stdout, `{`)
		os.Exit(0)
	case "snapshots":
		fmt.Fprint(os.Stdout, `[
  {"id":"b","time":"2026-08-12T02:00:00Z","hostname":"hub","paths":["/b"],"tags":["host:web"]},
  {"id":"c","time":"2026-08-12T01:00:00Z","hostname":"hub","paths":["/c"]},
  {"id":"a","time":"2026-08-12T02:00:00Z","hostname":"hub","paths":["/a"]}
]`)
		os.Exit(0)
	case "dump-success":
		fmt.Fprint(os.Stdout, "dump-data")
		os.Exit(0)
	case "dump-fail":
		fmt.Fprint(os.Stdout, "partial-data")
		os.Exit(7)
	case "stderr-secret":
		fmt.Fprint(os.Stderr, "SECRET_FROM_RESTIC")
		os.Exit(7)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "capture-env":
		if len(extra) != 1 {
			os.Exit(92)
		}
		_, passwordPresent := os.LookupEnv("RESTIC_PASSWORD")
		content := strings.Join([]string{
			os.Getenv("RESTIC_REPOSITORY"),
			os.Getenv("RESTIC_PASSWORD_FILE"),
			os.Getenv("OBJECT_TOKEN"),
			fmt.Sprintf("RESTIC_PASSWORD_PRESENT=%t", passwordPresent),
		}, "\n")
		if err := os.WriteFile(extra[0], []byte(content), 0o600); err != nil {
			os.Exit(93)
		}
		os.Exit(0)
	default:
		os.Exit(99)
	}
}

func TestNew_凭证只进入当前Restic子进程(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "repo.env")
	if err := os.WriteFile(envPath, []byte(strings.Join([]string{
		"OBJECT_TOKEN='secret value'",
		"RESTIC_REPOSITORY=env-wrong",
		"RESTIC_PASSWORD_FILE=env-wrong",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("写入 repo env 失败: %v", err)
	}
	t.Setenv("RESTIC_REPOSITORY", "parent-wrong")
	t.Setenv("RESTIC_PASSWORD_FILE", "parent-wrong")
	t.Setenv("RESTIC_PASSWORD", "parent-secret")

	capturePath := filepath.Join(t.TempDir(), "captured-env")
	var calls []commandCall
	repo := newTestRepo(t, config.Repo{
		Type:         config.DefaultRepoType,
		URL:          "s3:https://example.com/backup",
		PasswordFile: "/etc/ark/repo.pass",
		EnvFile:      envPath,
	}, helperCommand(t, &calls, helperPlan{mode: "capture-env", extra: []string{capturePath}}))

	if err := repo.Check(context.Background()); err != nil {
		t.Fatalf("Check 返回错误: %v", err)
	}
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("读取捕获环境失败: %v", err)
	}
	want := strings.Join([]string{
		"s3:https://example.com/backup",
		"/etc/ark/repo.pass",
		"secret value",
		"RESTIC_PASSWORD_PRESENT=false",
	}, "\n")
	if string(captured) != want {
		t.Fatalf("restic 环境 = %q，期望 %q", captured, want)
	}
	assertCall(t, calls, 0, "check", "--json")
}

func TestNew_拒绝危险Restic控制变量且不泄漏值(t *testing.T) {
	for _, key := range []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND", "RESTIC_REPOSITORY_FILE"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "repo.env")
			secret := "SHOULD_NOT_APPEAR"
			if err := os.WriteFile(path, []byte(key+"="+secret), 0o600); err != nil {
				t.Fatalf("写入 repo env 失败: %v", err)
			}
			_, err := New(&config.Repo{
				Type:         config.DefaultRepoType,
				URL:          "/repo",
				PasswordFile: "/repo.pass",
				EnvFile:      path,
			})
			if err == nil {
				t.Fatalf("%s 应被拒绝", key)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("错误泄漏了凭证值: %v", err)
			}
		})
	}
}

func TestEnsureInit_只对仓库不存在执行Init(t *testing.T) {
	tests := []struct {
		name      string
		plans     []helperPlan
		wantCalls [][]string
		wantErr   string
	}{
		{
			name:      "已初始化",
			plans:     []helperPlan{{mode: "ok"}},
			wantCalls: [][]string{{"cat", "config"}},
		},
		{
			name:      "退出码10后初始化",
			plans:     []helperPlan{{mode: "exit10"}, {mode: "ok"}},
			wantCalls: [][]string{{"cat", "config"}, {"init"}},
		},
		{
			name:      "密码错误不初始化",
			plans:     []helperPlan{{mode: "exit12"}},
			wantCalls: [][]string{{"cat", "config"}},
			wantErr:   "exit status 12",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []commandCall
			repo := newTestRepo(t, testRepoConfig(), helperCommand(t, &calls, tc.plans...))
			err := repo.EnsureInit(context.Background())
			if tc.wantErr == "" && err != nil {
				t.Fatalf("EnsureInit 返回错误: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("EnsureInit 错误 = %v，期望包含 %q", err, tc.wantErr)
			}
			if len(calls) != len(tc.wantCalls) {
				t.Fatalf("调用数 = %d，期望 %d: %#v", len(calls), len(tc.wantCalls), calls)
			}
			for i, want := range tc.wantCalls {
				if !reflect.DeepEqual(calls[i].args, want) {
					t.Fatalf("调用[%d] = %#v，期望 %#v", i, calls[i].args, want)
				}
			}
		})
	}
}

func TestBackupStdin_流式输入并解析Summary(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "stdin")
	var calls []commandCall
	repo := newTestRepo(t, testRepoConfig(), helperCommand(t, &calls,
		helperPlan{mode: "backup-success", extra: []string{capturePath}}))

	snapshot, err := repo.BackupStdin(context.Background(), strings.NewReader("data"),
		"web-01/postgres/app.sql", []string{"host:web-01", "target:postgres/app"})
	if err != nil {
		t.Fatalf("BackupStdin 返回错误: %v", err)
	}
	if snapshot.ID != "snapshot-123" || !snapshot.Time.Equal(time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("读取 stdin 捕获失败: %v", err)
	}
	if string(data) != "data" {
		t.Fatalf("stdin = %q，期望 data", data)
	}
	assertCall(t, calls, 0,
		"backup", "--json", "--stdin", "--stdin-filename", "web-01/postgres/app.sql",
		"--tag", "host:web-01", "--tag", "target:postgres/app")
}

func TestBackupStdin_JSON或命令错误均可见且不泄漏输出(t *testing.T) {
	t.Run("summary后非零退出仍返回快照ID", func(t *testing.T) {
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "backup-summary-fail"}))
		snapshot, err := repo.BackupStdin(context.Background(), strings.NewReader("data"), "file", nil)
		if err == nil || !strings.Contains(err.Error(), "exit status 7") {
			t.Fatalf("错误 = %v，期望命令非零退出", err)
		}
		if snapshot.ID != "snapshot-failed" {
			t.Fatalf("snapshot = %#v，期望保留已提交快照 ID", snapshot)
		}
	})

	t.Run("JSON损坏", func(t *testing.T) {
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "backup-malformed"}))
		_, err := repo.BackupStdin(context.Background(), strings.NewReader("data"), "file", nil)
		if err == nil || !strings.Contains(err.Error(), "JSON") {
			t.Fatalf("错误 = %v，期望 JSON 解析失败", err)
		}
	})

	t.Run("stderr不泄漏", func(t *testing.T) {
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "stderr-secret"}))
		_, err := repo.BackupStdin(context.Background(), strings.NewReader("data"), "file", nil)
		if err == nil {
			t.Fatal("非零退出应返回错误")
		}
		if strings.Contains(err.Error(), "SECRET_FROM_RESTIC") {
			t.Fatalf("错误泄漏了 restic 输出: %v", err)
		}
	})

	t.Run("运行中超时保留context错误链", func(t *testing.T) {
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "sleep"}))
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := repo.BackupStdin(ctx, strings.NewReader("data"), "file", nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("错误 = %v，期望保留 context deadline", err)
		}
	})
}

func TestSnapshots_解析并稳定排序(t *testing.T) {
	var calls []commandCall
	repo := newTestRepo(t, testRepoConfig(), helperCommand(t, &calls, helperPlan{mode: "snapshots"}))
	snapshots, err := repo.Snapshots(context.Background(), []string{"ark-manifest", "run:1"})
	if err != nil {
		t.Fatalf("Snapshots 返回错误: %v", err)
	}
	gotIDs := []string{snapshots[0].ID, snapshots[1].ID, snapshots[2].ID}
	if !reflect.DeepEqual(gotIDs, []string{"c", "a", "b"}) {
		t.Fatalf("快照顺序 = %#v，期望按时间和 ID 排序", gotIDs)
	}
	assertCall(t, calls, 0, "snapshots", "--json", "--tag", "ark-manifest", "--tag", "run:1")
}

func TestForgetCheck_参数精确映射(t *testing.T) {
	var calls []commandCall
	repo := newTestRepo(t, testRepoConfig(), helperCommand(t, &calls,
		helperPlan{mode: "ok"}, helperPlan{mode: "ok"}, helperPlan{mode: "ok"},
		helperPlan{mode: "ok"}, helperPlan{mode: "ok"}))

	if err := repo.Forget(context.Background(), config.Retention{Daily: 7, Monthly: 6},
		[]string{"host:web-01"}); err != nil {
		t.Fatalf("Forget 返回错误: %v", err)
	}
	if err := repo.ForgetSnapshot(context.Background(), "snapshot-123"); err != nil {
		t.Fatalf("ForgetSnapshot 返回错误: %v", err)
	}
	if err := repo.ForgetPolicy(context.Background(), config.Retention{Daily: 3},
		[]string{"host:web-02"}); err != nil {
		t.Fatalf("ForgetPolicy 返回错误: %v", err)
	}
	if err := repo.Prune(context.Background()); err != nil {
		t.Fatalf("Prune 返回错误: %v", err)
	}
	if err := repo.Check(context.Background()); err != nil {
		t.Fatalf("Check 返回错误: %v", err)
	}

	assertCall(t, calls, 0,
		"forget", "--json", "--prune", "--keep-daily", "7", "--keep-monthly", "6",
		"--tag", "host:web-01")
	assertCall(t, calls, 1, "forget", "--json", "--prune", "snapshot-123")
	assertCall(t, calls, 2, "forget", "--json", "--keep-daily", "3", "--tag", "host:web-02")
	assertCall(t, calls, 3, "prune", "--json")
	assertCall(t, calls, 4, "check", "--json")
}

func TestDump_最终Read暴露退出状态(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    string
		wantErr string
	}{
		{name: "成功", mode: "dump-success", want: "dump-data"},
		{name: "非零退出", mode: "dump-fail", want: "partial-data", wantErr: "exit status 7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls []commandCall
			repo := newTestRepo(t, testRepoConfig(), helperCommand(t, &calls, helperPlan{mode: tc.mode}))
			reader, err := repo.Dump(context.Background(), "snapshot-123", "/data.sql")
			if err != nil {
				t.Fatalf("Dump 返回错误: %v", err)
			}
			data, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if string(data) != tc.want {
				t.Fatalf("dump 数据 = %q，期望 %q", data, tc.want)
			}
			combined := errors.Join(readErr, closeErr)
			if tc.wantErr == "" && combined != nil {
				t.Fatalf("读取或关闭返回错误: %v", combined)
			}
			if tc.wantErr != "" && (combined == nil || !strings.Contains(combined.Error(), tc.wantErr)) {
				t.Fatalf("错误 = %v，期望包含 %q", combined, tc.wantErr)
			}
			assertCall(t, calls, 0, "dump", "snapshot-123", "/data.sql")
		})
	}
}

func TestDump_Close与Context取消均回收子进程(t *testing.T) {
	t.Run("提前Close暴露退出错误", func(t *testing.T) {
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "dump-fail"}))
		reader, err := repo.Dump(context.Background(), "snapshot-123", "/data.sql")
		if err != nil {
			t.Fatalf("Dump 返回错误: %v", err)
		}
		if err := reader.Close(); err == nil {
			t.Fatal("提前 Close 必须暴露子进程非零退出")
		}
	})

	t.Run("运行中取消保留context错误链", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		repo := newTestRepo(t, testRepoConfig(), helperCommand(t, nil, helperPlan{mode: "sleep"}))
		reader, err := repo.Dump(ctx, "snapshot-123", "/data.sql")
		if err != nil {
			t.Fatalf("Dump 返回错误: %v", err)
		}
		_, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if !errors.Is(errors.Join(readErr, closeErr), context.DeadlineExceeded) {
			t.Fatalf("错误 = %v，期望保留 context deadline", errors.Join(readErr, closeErr))
		}
	})
}

func TestRepoIntegration_LocalLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 restic，短模式跳过")
	}
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("未安装 restic，跳过本地仓库集成测试")
	}

	passwordFile := filepath.Join(t.TempDir(), "repo.pass")
	if err := os.WriteFile(passwordFile, []byte("integration-password"), 0o600); err != nil {
		t.Fatalf("写入密码文件失败: %v", err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	repo, err := New(&config.Repo{
		Type:         config.DefaultRepoType,
		URL:          repoDir,
		PasswordFile: passwordFile,
	})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	ctx := context.Background()
	if err := repo.EnsureInit(ctx); err != nil {
		t.Fatalf("首次 EnsureInit 返回错误: %v", err)
	}
	if err := repo.EnsureInit(ctx); err != nil {
		t.Fatalf("重复 EnsureInit 返回错误: %v", err)
	}
	snapshot, err := repo.BackupStdin(ctx, strings.NewReader("integration-data"),
		"integration/data.txt", []string{"integration"})
	if err != nil {
		t.Fatalf("BackupStdin 返回错误: %v", err)
	}
	snapshots, err := repo.Snapshots(ctx, []string{"integration"})
	if err != nil {
		t.Fatalf("Snapshots 返回错误: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != snapshot.ID {
		t.Fatalf("Snapshots = %+v，期望包含 %s", snapshots, snapshot.ID)
	}
	if err := repo.Forget(ctx, config.Retention{Daily: 1}, []string{"integration"}); err != nil {
		t.Fatalf("Forget 返回错误: %v", err)
	}
	if err := repo.ForgetSnapshot(ctx, snapshot.ID); err != nil {
		t.Fatalf("ForgetSnapshot 返回错误: %v", err)
	}
	if err := repo.Check(ctx); err != nil {
		t.Fatalf("Check 返回错误: %v", err)
	}
}

func testRepoConfig() config.Repo {
	return config.Repo{
		Type:         config.DefaultRepoType,
		URL:          "/repo",
		PasswordFile: "/repo.pass",
	}
}

func newTestRepo(t *testing.T, cfg config.Repo, command commandFunc) *Repo {
	t.Helper()
	repo, err := newRepo(&cfg, command)
	if err != nil {
		t.Fatalf("newRepo 返回错误: %v", err)
	}
	return repo
}

func helperCommand(t *testing.T, calls *[]commandCall, plans ...helperPlan) commandFunc {
	t.Helper()
	index := 0
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if index >= len(plans) {
			t.Fatalf("未配置第 %d 次 restic 调用", index+1)
		}
		plan := plans[index]
		index++
		if calls != nil {
			*calls = append(*calls, commandCall{name: name, args: append([]string(nil), args...)})
		}
		helperArgs := []string{"-test.run=TestResticHelperProcess", "--", plan.mode}
		helperArgs = append(helperArgs, plan.extra...)
		return exec.CommandContext(ctx, os.Args[0], helperArgs...)
	}
}

func assertCall(t *testing.T, calls []commandCall, index int, wantArgs ...string) {
	t.Helper()
	if index >= len(calls) {
		t.Fatalf("缺少第 %d 次调用: %#v", index+1, calls)
	}
	if calls[index].name != "restic" {
		t.Fatalf("命令名 = %q，期望 restic", calls[index].name)
	}
	if !reflect.DeepEqual(calls[index].args, wantArgs) {
		t.Fatalf("调用[%d] argv = %#v，期望 %#v", index, calls[index].args, wantArgs)
	}
}
