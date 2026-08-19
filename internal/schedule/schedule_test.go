package schedule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnalyze_调用Systemd并解析两次UTC触发(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(t.TempDir(), "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$SCHEDULE_CAPTURE"
printf '  Iter. #1: Mon 2026-08-17 20:17:00 UTC\n'
printf '       (in UTC): Mon 2026-08-17 20:17:00 UTC\n'
printf '  Iter. #2: Tue 2026-08-18 20:17:00 UTC\n'
printf '       (in UTC): Tue 2026-08-18 20:17:00 UTC\n'
`
	path := filepath.Join(directory, "systemd-analyze")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("写入 systemd-analyze 测试脚本失败: %v", err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("SCHEDULE_CAPTURE", capture)

	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	window, err := Analyze(context.Background(), "*-*-* 04:17:00", base)
	if err != nil {
		t.Fatalf("Analyze 失败: %v", err)
	}
	if want := time.Date(2026, 8, 17, 20, 17, 0, 0, time.UTC); !window.NextRunAt.Equal(want) {
		t.Errorf("NextRunAt = %s，期望 %s", window.NextRunAt, want)
	}
	if window.Interval != 24*time.Hour {
		t.Errorf("Interval = %s，期望 24h", window.Interval)
	}
	arguments, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("读取参数捕获失败: %v", err)
	}
	wantArguments := strings.Join([]string{
		"calendar",
		"--iterations=2",
		"--base-time=2026-08-17 12:00:00 UTC",
		"*-*-* 04:17:00",
		"",
	}, "\n")
	if string(arguments) != wantArguments {
		t.Errorf("参数 = %q，期望 %q", arguments, wantArguments)
	}
}

func TestNext_旧Systemd回退单次日历输出(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(t.TempDir(), "args")
	script := `#!/bin/sh
if [ "$2" = "--iterations=2" ]; then
  printf "systemd-analyze: unrecognized option '--iterations=2'\n" >&2
  exit 1
fi
printf '%s\n' "$@" > "$SCHEDULE_CAPTURE"
printf '    Next elapse: Thu 2026-08-20 04:17:00 CST\n'
printf '       (in UTC): Wed 2026-08-19 20:17:00 UTC\n'
`
	path := filepath.Join(directory, "systemd-analyze")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("写入 systemd-analyze 测试脚本失败: %v", err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("SCHEDULE_CAPTURE", capture)

	nextRunAt, err := Next(context.Background(), "*-*-* 04:17:00", time.Now().UTC())
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if want := time.Date(2026, 8, 19, 20, 17, 0, 0, time.UTC); !nextRunAt.Equal(want) {
		t.Errorf("NextRunAt = %s，期望 %s", nextRunAt, want)
	}
	arguments, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("读取参数捕获文件失败: %v", err)
	}
	if want := "calendar\n*-*-* 04:17:00\n"; string(arguments) != want {
		t.Errorf("旧版回退参数 = %q，期望 %q", arguments, want)
	}
}

func TestNext_旧Systemd拒绝忽略远端基准时间(t *testing.T) {
	directory := t.TempDir()
	script := `#!/bin/sh
printf "systemd-analyze: unrecognized option '--iterations=2'\n" >&2
exit 1
`
	path := filepath.Join(directory, "systemd-analyze")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("写入 systemd-analyze 测试脚本失败: %v", err)
	}
	t.Setenv("PATH", directory)

	_, err := Next(context.Background(), "daily", time.Now().UTC().Add(-2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "无法按指定 base_time") {
		t.Fatalf("错误 = %v，期望拒绝忽略远端 base_time", err)
	}
}

func TestNext_现代Systemd非法表达式不回退(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
printf 'call\n' >> "$SCHEDULE_CAPTURE"
printf 'Failed to parse calendar specification\n' >&2
exit 1
`
	path := filepath.Join(directory, "systemd-analyze")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("写入 systemd-analyze 测试脚本失败: %v", err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("SCHEDULE_CAPTURE", capture)

	_, err := Next(context.Background(), "not-a-calendar", time.Now().UTC())
	if err == nil {
		t.Fatal("Next 对非法表达式未返回错误")
	}
	calls, readErr := os.ReadFile(capture)
	if readErr != nil {
		t.Fatalf("读取调用次数失败: %v", readErr)
	}
	if string(calls) != "call\n" {
		t.Fatalf("调用记录 = %q，非法表达式不应回退", calls)
	}
}

func TestParseOutput_解析DailyWeeklyMonthly周期(t *testing.T) {
	tests := []struct {
		name     string
		first    string
		second   string
		interval time.Duration
	}{
		{name: "daily", first: "Mon 2026-08-17 20:17:00 UTC", second: "Tue 2026-08-18 20:17:00 UTC", interval: 24 * time.Hour},
		{name: "weekly", first: "Mon 2026-08-17 20:17:00 UTC", second: "Mon 2026-08-24 20:17:00 UTC", interval: 7 * 24 * time.Hour},
		{name: "monthly", first: "Sat 2026-08-01 20:17:00 UTC", second: "Tue 2026-09-01 20:17:00 UTC", interval: 31 * 24 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window, err := parseOutput(test.name, strings.Join([]string{
				"(in UTC): " + test.first,
				"(in UTC): " + test.second,
			}, "\n"))
			if err != nil || window.Interval != test.interval {
				t.Fatalf("周期=%s err=%v，期望 %s", window.Interval, err, test.interval)
			}
		})
	}
}

func TestParseOutput_UTC主机直接解析触发行(t *testing.T) {
	window, err := parseOutput("daily", strings.Join([]string{
		"Next elapse: Wed 2026-08-19 04:17:00 UTC",
		"Iteration #2: Thu 2026-08-20 04:17:00 UTC",
	}, "\n"))
	if err != nil {
		t.Fatalf("parseOutput 失败: %v", err)
	}
	if want := 24 * time.Hour; window.Interval != want {
		t.Fatalf("周期 = %s，期望 %s", window.Interval, want)
	}
}

func TestAnalyze_Context取消会终止SystemdAnalyze(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "systemd-analyze")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n/bin/sleep 10\n"), 0o700); err != nil {
		t.Fatalf("写入 systemd-analyze 阻塞脚本失败: %v", err)
	}
	t.Setenv("PATH", directory)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Analyze(ctx, "daily", time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误=%v，期望 context.Canceled", err)
	}
}

func TestBoundedBuffer_输出超限(t *testing.T) {
	buffer := &boundedBuffer{limit: 4}
	if written, err := buffer.Write([]byte("12345")); err != nil || written != 5 {
		t.Fatalf("写入结果 written=%d err=%v", written, err)
	}
	if !buffer.overflow || buffer.String() != "1234" {
		t.Fatalf("超限 buffer=%q overflow=%t", buffer.String(), buffer.overflow)
	}
}

func TestParseOutput_拒绝缺失和倒序时间(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{name: "缺少第二次触发", output: "(in UTC): Mon 2026-08-17 20:17:00 UTC\n", want: "期望 2 条"},
		{
			name: "时间倒序",
			output: strings.Join([]string{
				"(in UTC): Tue 2026-08-18 20:17:00 UTC",
				"(in UTC): Mon 2026-08-17 20:17:00 UTC",
			}, "\n"),
			want: "未递增",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOutput("daily", test.output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("错误 = %v，期望包含 %q", err, test.want)
			}
		})
	}
}
