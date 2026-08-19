package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

const redisPollInterval = 250 * time.Millisecond

func executeRedis(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
	pollInterval time.Duration,
) (*Result, error) {
	lastSaveArgv := append(composeArgv(host.Project),
		"exec", "-T", target.Service, "redis-cli", "LASTSAVE")
	baseline, err := redisLastSave(ctx, target.ID(), runner, lastSaveArgv)
	if err != nil {
		return nil, err
	}

	bgsaveArgv := append(composeArgv(host.Project),
		"exec", "-T", target.Service, "redis-cli", "BGSAVE")
	if _, err := runner.Run(ctx, bgsaveArgv...); err != nil {
		return nil, fmt.Errorf("触发 target %q Redis BGSAVE 失败: %w", target.ID(), err)
	}

	if err := waitRedisSave(ctx, target.ID(), runner, lastSaveArgv, baseline, pollInterval); err != nil {
		return nil, err
	}

	streamArgv := append(composeArgv(host.Project),
		"exec", "-T", target.Service, "cat", "/data/dump.rdb")
	return startStream(ctx, host, target, ".rdb", runner, streamArgv)
}

func waitRedisSave(
	ctx context.Context,
	targetID string,
	runner sshexec.Runner,
	argv []string,
	baseline int64,
	pollInterval time.Duration,
) error {
	for {
		current, err := redisLastSave(ctx, targetID, runner, argv)
		if err != nil {
			return err
		}
		if current != baseline {
			return nil
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("等待 target %q Redis BGSAVE 完成失败: %w", targetID, ctx.Err())
		case <-timer.C:
		}
	}
}

func redisLastSave(
	ctx context.Context,
	targetID string,
	runner sshexec.Runner,
	argv []string,
) (int64, error) {
	// redis-cli 可能在命令成功时向 stderr 写入非致命认证警告；LASTSAVE 是结构化值，
	// 必须只解析 stdout，避免警告文本把有效时间戳误判为损坏输出。
	out, err := sshexec.ReadAllStdout(ctx, runner, argv...)
	if err != nil {
		return 0, fmt.Errorf("读取 target %q Redis LASTSAVE 失败: %w", targetID, err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("读取 target %q Redis LASTSAVE 失败: 输出不是非负时间戳", targetID)
	}
	return value, nil
}
