// Package schedule 解析 systemd OnCalendar 表达式的下一次触发窗口。
package schedule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/envfile"
)

const (
	maximumOutputBytes      = 64 * 1024
	commandTimeout          = 15 * time.Second
	legacyBaseTimeTolerance = time.Minute
)

var errAdvancedCalendarOptionsUnsupported = errors.New("systemd-analyze 不支持高级日历参数")

// Window 描述 OnCalendar 在给定基准时间后的下一次触发和当前有效周期。
type Window struct {
	NextRunAt time.Time
	Interval  time.Duration
}

// Analyze 使用 systemd 自身解析 OnCalendar 表达式。
// @param ctx 控制 systemd-analyze 子进程的取消与超时。
// @param expression 待解析的 OnCalendar 表达式。
// @param baseTime 计算下一次触发时间的基准时间。
// @return Window 下一次触发时间和相邻两次触发的间隔。
// @return error 参数、命令执行或输出解析失败时的错误。
func Analyze(ctx context.Context, expression string, baseTime time.Time) (Window, error) {
	if strings.TrimSpace(expression) == "" {
		return Window{}, fmt.Errorf("解析 OnCalendar 失败: 表达式不能为空")
	}
	if baseTime.IsZero() {
		return Window{}, fmt.Errorf("解析 OnCalendar 失败: base_time 不能为空")
	}

	output, stderr, err := runCalendarCommand(
		ctx,
		"--iterations=2",
		"--base-time="+baseTime.UTC().Format("2006-01-02 15:04:05 UTC"),
		expression,
	)
	if err != nil {
		if advancedCalendarOptionsUnsupported(stderr) {
			return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: %w: %v",
				expression, errAdvancedCalendarOptionsUnsupported, err)
		}
		return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: systemd-analyze 执行失败: %w", expression, err)
	}

	return parseOutput(expression, output)
}

// Next 使用 systemd 解析给定计划的下一次触发时间。
//
// systemd 241 还不支持 --iterations 与 --base-time。对靠近当前时刻的调用，
// 回退到旧版单次 calendar 输出；远离当前时刻时拒绝降级，避免忽略 baseTime 后
// 返回一个看似精确但实际错误的时间。
// @param ctx 控制 systemd-analyze 子进程的取消与超时。
// @param expression 待解析的 OnCalendar 表达式。
// @param baseTime 计算下一次触发时间的基准时间。
// @return time.Time 下一次触发时间。
// @return error 参数、命令执行或输出解析失败时的错误。
func Next(ctx context.Context, expression string, baseTime time.Time) (time.Time, error) {
	window, err := Analyze(ctx, expression, baseTime)
	if err == nil {
		return window.NextRunAt, nil
	}
	if !errors.Is(err, errAdvancedCalendarOptionsUnsupported) {
		return time.Time{}, err
	}

	now := time.Now().UTC()
	difference := now.Sub(baseTime.UTC())
	if difference < 0 {
		difference = -difference
	}
	if difference > legacyBaseTimeTolerance {
		return time.Time{}, fmt.Errorf(
			"解析 OnCalendar %q 失败: 旧版 systemd-analyze 无法按指定 base_time 计算: %w",
			expression, err,
		)
	}

	output, _, commandErr := runCalendarCommand(ctx, expression)
	if commandErr != nil {
		return time.Time{}, fmt.Errorf(
			"解析 OnCalendar %q 失败: 旧版 systemd-analyze 执行失败: %w",
			expression, commandErr,
		)
	}
	return parseNextOutput(expression, output)
}

// runCalendarCommand 为新旧 systemd 共用同一套超时、语言和有界输出约束。
func runCalendarCommand(ctx context.Context, arguments ...string) (string, string, error) {
	commandContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	stdout := &boundedBuffer{limit: maximumOutputBytes}
	stderr := &boundedBuffer{limit: maximumOutputBytes}
	command := exec.CommandContext(
		commandContext,
		"systemd-analyze",
		append([]string{"calendar"}, arguments...)...,
	)
	command.Env = envfile.Merge(os.Environ(), map[string]string{"LC_ALL": "C"})
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), stderr.String(), ctxErr
	}
	if commandErr := commandContext.Err(); commandErr != nil {
		return stdout.String(), stderr.String(), commandErr
	}
	if stdout.overflow || stderr.overflow {
		return stdout.String(), stderr.String(), fmt.Errorf(
			"systemd-analyze 输出超过 %d 字节", maximumOutputBytes,
		)
	}
	return stdout.String(), stderr.String(), err
}

func advancedCalendarOptionsUnsupported(stderr string) bool {
	return strings.Contains(stderr, "unrecognized option '--iterations") ||
		strings.Contains(stderr, "unrecognized option '--base-time")
}

func parseOutput(expression string, output string) (Window, error) {
	times, err := parseOutputTimes(expression, output)
	if err != nil {
		return Window{}, err
	}
	if len(times) != 2 {
		return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: 期望 2 条 UTC 触发时间，实际 %d 条", expression, len(times))
	}
	interval := times[1].Sub(times[0])
	if interval <= 0 {
		return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: 相邻触发时间未递增", expression)
	}
	return Window{NextRunAt: times[0], Interval: interval}, nil
}

func parseNextOutput(expression string, output string) (time.Time, error) {
	times, err := parseOutputTimes(expression, output)
	if err != nil {
		return time.Time{}, err
	}
	if len(times) != 1 {
		return time.Time{}, fmt.Errorf(
			"解析 OnCalendar %q 失败: 期望 1 条 UTC 触发时间，实际 %d 条",
			expression, len(times),
		)
	}
	return times[0], nil
}

func parseOutputTimes(expression string, output string) ([]time.Time, error) {
	times := make([]time.Time, 0, 2)
	for _, line := range strings.Split(output, "\n") {
		value := utcTimeValue(line)
		if value == "" {
			continue
		}
		parsed, err := parseUTCTime(value)
		if err != nil {
			return nil, fmt.Errorf("解析 OnCalendar %q 失败: UTC 时间 %q 无效: %w", expression, value, err)
		}
		times = append(times, parsed.UTC())
	}
	return times, nil
}

func utcTimeValue(line string) string {
	if marker := strings.Index(line, "(in UTC):"); marker >= 0 {
		return strings.TrimSpace(line[marker+len("(in UTC):"):])
	}
	trimmed := strings.TrimSpace(line)
	for _, prefix := range []string{"Next elapse:", "Iteration #"} {
		if !strings.HasPrefix(trimmed, prefix) || !strings.HasSuffix(trimmed, " UTC") {
			continue
		}
		marker := strings.Index(trimmed, ":")
		if marker >= 0 {
			return strings.TrimSpace(trimmed[marker+1:])
		}
	}
	return ""
}

func parseUTCTime(value string) (time.Time, error) {
	layouts := []string{
		"Mon 2006-01-02 15:04:05 MST",
		"Mon 2006-01-02 15:04:05.999999 MST",
	}
	var parseErrors []string
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed, nil
		}
		parseErrors = append(parseErrors, err.Error())
	}
	return time.Time{}, fmt.Errorf("不匹配支持的 systemd UTC 格式: %s", strings.Join(parseErrors, "; "))
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(payload []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		writeLength := len(payload)
		if writeLength > remaining {
			writeLength = remaining
		}
		_, _ = b.buffer.Write(payload[:writeLength])
	}
	if len(payload) > remaining {
		b.overflow = true
	}
	return len(payload), nil
}

func (b *boundedBuffer) String() string {
	return b.buffer.String()
}
