// Package backup 把清单中的备份 target 转换为可交给 restic 的稳定数据流。
//
// 本包只负责目标机命令、流生命周期和 target 元数据，不调用 restic、状态库或 CLI。
// 调用方选择本地或 SSH Runner 后，五类 target 共用同一套执行边界。
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

// Result 是单个 target 可供 restic 消费的数据流及稳定元数据。
type Result struct {
	// Host 是清单中的 host 标识。
	Host string
	// TargetID 是 config.Target.ID 返回的稳定 target 标识。
	TargetID string
	// TargetType 是 target 的清单类型。
	TargetType config.TargetType
	// StdinFilename 是传给 restic --stdin-filename 的跨运行稳定路径。
	StdinFilename string
	// Reader 只包含备份数据；调用方消费完成后必须关闭。
	Reader io.ReadCloser
	// Wait 在 Reader 读完后返回上游命令的真实退出状态；重复调用只等待一次。
	Wait func() error
	// ImageDigests 仅对 image_digest target 非空，键是 service，值是确定的 RepoDigest。
	ImageDigests map[string]string
}

// Execute 为一个已校验 target 创建备份数据流。
// @param ctx 控制目标机命令的取消与超时。
// @param host target 所属 host 及 compose 项目配置。
// @param target 要执行的备份目标。
// @param runner 已由调用方选择的本地或 SSH Runner。
// @return *Result 可交给 restic 的数据流和稳定元数据。
// @return error 输入无效、命令启动或准备阶段失败时的错误。
func Execute(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
) (*Result, error) {
	if ctx == nil {
		return nil, fmt.Errorf("执行 target %q 失败: context 不能为空", target.ID())
	}
	if runner == nil {
		return nil, fmt.Errorf("执行 target %q 失败: runner 不能为空", target.ID())
	}
	if host.Host == "" {
		return nil, fmt.Errorf("执行 target %q 失败: host 不能为空", target.ID())
	}
	if host.Project.ComposeFile == "" {
		return nil, fmt.Errorf("执行 target %q 失败: compose_file 不能为空", target.ID())
	}

	switch target.Type {
	case config.TargetPostgres:
		return executePostgres(ctx, host, target, runner)
	case config.TargetRedis:
		return executeRedis(ctx, host, target, runner, redisPollInterval)
	case config.TargetVolume:
		return executeVolume(ctx, host, target, runner)
	case config.TargetFiles:
		return executeFiles(ctx, host, target, runner)
	case config.TargetImageDigest:
		return executeImageDigest(ctx, host, target, runner)
	default:
		return nil, fmt.Errorf("执行 target %q 失败: 不支持类型 %q", target.ID(), target.Type)
	}
}

func composeArgv(project config.Project) []string {
	argv := []string{"docker", "compose", "-f", project.ComposeFile}
	if project.ProjectName != "" {
		argv = append(argv, "-p", project.ProjectName)
	}
	if project.EnvFile != "" {
		argv = append(argv, "--env-file", project.EnvFile)
	}
	return argv
}

func streamResult(
	host config.Host,
	target config.Target,
	suffix string,
	reader io.ReadCloser,
	wait func() error,
) *Result {
	return &Result{
		Host:          host.Host,
		TargetID:      target.ID(),
		TargetType:    target.Type,
		StdinFilename: host.Host + "/" + target.ID() + suffix,
		Reader:        newOnceReadCloser(reader),
		Wait:          onceWait(target.ID(), wait),
	}
}

func memoryResult(
	host config.Host,
	target config.Target,
	suffix string,
	reader io.ReadCloser,
	imageDigests map[string]string,
) *Result {
	result := streamResult(host, target, suffix, reader, func() error { return nil })
	result.ImageDigests = imageDigests
	return result
}

func startStream(
	ctx context.Context,
	host config.Host,
	target config.Target,
	suffix string,
	runner sshexec.Runner,
	argv []string,
) (*Result, error) {
	reader, wait, err := runner.Stream(ctx, argv...)
	if err != nil {
		return nil, fmt.Errorf("启动 target %q 数据流失败: %w", target.ID(), err)
	}
	if reader == nil || wait == nil {
		var cleanupErrors []error
		if reader != nil {
			if closeErr := reader.Close(); closeErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("关闭半初始化数据流失败: %w", closeErr))
			}
		}
		if wait != nil {
			if waitErr := wait(); waitErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("回收半初始化数据流失败: %w", waitErr))
			}
		}
		return nil, errors.Join(append(
			[]error{fmt.Errorf("启动 target %q 数据流失败: Runner 返回了不完整的 Reader/Wait", target.ID())},
			cleanupErrors...,
		)...)
	}
	return streamResult(host, target, suffix, reader, wait), nil
}

func onceWait(targetID string, wait func() error) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			if wait == nil {
				result = fmt.Errorf("等待 target %q 数据流失败: Wait 为空", targetID)
				return
			}
			if err := wait(); err != nil {
				result = fmt.Errorf("等待 target %q 数据流失败: %w", targetID, err)
			}
		})
		return result
	}
}

type onceReadCloser struct {
	reader io.ReadCloser
	once   sync.Once
	err    error
}

func newOnceReadCloser(reader io.ReadCloser) io.ReadCloser {
	return &onceReadCloser{reader: reader}
}

func (r *onceReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *onceReadCloser) Close() error {
	r.once.Do(func() {
		r.err = r.reader.Close()
	})
	return r.err
}
