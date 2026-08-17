// Package schedule 解析 systemd OnCalendar 表达式的下一次触发窗口。
package schedule

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/envfile"
)

const (
	maximumOutputBytes = 64 * 1024
	commandTimeout     = 15 * time.Second
)

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

	commandContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	output := &boundedBuffer{limit: maximumOutputBytes}
	command := exec.CommandContext(
		commandContext,
		"systemd-analyze",
		"calendar",
		"--iterations=2",
		"--base-time="+baseTime.UTC().Format("2006-01-02 15:04:05 UTC"),
		expression,
	)
	command.Env = envfile.Merge(os.Environ(), map[string]string{"LC_ALL": "C"})
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: %w", expression, ctxErr)
		}
		if commandErr := commandContext.Err(); commandErr != nil {
			return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: %w", expression, commandErr)
		}
		return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: systemd-analyze 执行失败: %w", expression, err)
	}
	if output.overflow {
		return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: systemd-analyze 输出超过 %d 字节", expression, maximumOutputBytes)
	}

	return parseOutput(expression, output.String())
}

func parseOutput(expression string, output string) (Window, error) {
	times := make([]time.Time, 0, 2)
	for _, line := range strings.Split(output, "\n") {
		marker := strings.Index(line, "(in UTC):")
		if marker < 0 {
			continue
		}
		value := strings.TrimSpace(line[marker+len("(in UTC):"):])
		parsed, err := parseUTCTime(value)
		if err != nil {
			return Window{}, fmt.Errorf("解析 OnCalendar %q 失败: UTC 时间 %q 无效: %w", expression, value, err)
		}
		times = append(times, parsed.UTC())
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
