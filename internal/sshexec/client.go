// Package sshexec 在 hub 本地或通过系统 OpenSSH 执行命令。
//
// 本包只负责命令构造、输入输出连接和退出状态，不理解 docker、restic
// 或备份目标。上层选择本地或 SSH Runner 后即可使用同一套接口；
// 流式调用刻意要求调用方读取 stdout 后再显式 Wait，以防 SSH 中断被误判为成功。
package sshexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/silentflower/ark/internal/config"
)

// Runner 统一本地与 SSH 命令执行方式。
type Runner interface {
	// Run 执行短命令并等待结束，返回 stdout 与 stderr 的合并输出。
	// @param ctx 控制命令的取消与超时。
	// @param argv 命令名及其参数，至少包含命令名。
	// @return string 命令的合并输出，命令失败时仍会返回已经产生的内容。
	// @return error 命令启动、取消或非零退出时的错误。
	Run(ctx context.Context, argv ...string) (string, error)
	// Stream 启动命令并把纯 stdout 交给调用方读取。
	// @param ctx 控制命令的取消与超时。
	// @param argv 命令名及其参数，至少包含命令名。
	// @return io.ReadCloser 命令 stdout；调用方必须先读完，并可在 Wait 后安全关闭。
	// @return func() error 读取结束后必须调用一次的 Wait，用于获取真实退出状态。
	// @return error 命令创建或启动失败时的错误。
	Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error)
	// Feed 启动命令，把 stdin 流式交给它并等待结束。
	// @param ctx 控制命令的取消与超时。
	// @param stdin 要传给命令标准输入的数据流。
	// @param argv 命令名及其参数，至少包含命令名。
	// @return error 命令启动、取消或非零退出时的错误。
	Feed(ctx context.Context, stdin io.Reader, argv ...string) error
}

// ReadAllStdout 执行流式命令并在内存中读取完整 stdout，同时严格回收 Reader 与子进程。
// @param ctx 控制命令的取消与超时。
// @param runner 提供本地或 SSH 命令执行能力。
// @param argv 命令名及其参数，至少包含命令名。
// @return []byte 命令成功时的完整纯 stdout。
// @return error 命令启动、读取、等待或资源释放失败时的聚合错误。
func ReadAllStdout(ctx context.Context, runner Runner, argv ...string) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("读取命令 stdout 失败: context 不能为空")
	}
	if runner == nil {
		return nil, fmt.Errorf("读取命令 stdout 失败: runner 不能为空")
	}

	reader, wait, streamErr := runner.Stream(ctx, argv...)
	if streamErr != nil || reader == nil || wait == nil {
		var lifecycleErr error
		if reader == nil || wait == nil {
			lifecycleErr = fmt.Errorf("Runner 返回了不完整的 Reader/Wait")
		}
		var closeErr error
		if reader != nil {
			closeErr = reader.Close()
		}
		var waitErr error
		if wait != nil {
			waitErr = wait()
		}
		return nil, errors.Join(streamErr, lifecycleErr, closeErr, waitErr)
	}

	payload, readErr := io.ReadAll(reader)
	if readErr != nil {
		// 读取失败时先关闭 stdout，避免子进程继续写满 pipe 后让 Wait 永久阻塞。
		closeErr := reader.Close()
		waitErr := wait()
		return nil, errors.Join(readErr, closeErr, waitErr)
	}
	waitErr := wait()
	closeErr := reader.Close()
	if err := errors.Join(waitErr, closeErr); err != nil {
		return nil, err
	}
	return payload, nil
}

type commandFunc func(context.Context, string, ...string) *exec.Cmd

type commandSpec struct {
	cmd     *exec.Cmd
	display string
}

type commandStdout struct {
	reader io.ReadCloser
	waited atomic.Bool
}

func (r *commandStdout) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *commandStdout) Close() error {
	err := r.reader.Close()
	// exec.Cmd.Wait 会主动关闭 StdoutPipe。上层遵守 ADR-011 在 Wait 后释放 Reader 时，
	// 这个已关闭状态表示资源已经回收，不应把完整快照误报为失败。
	if r.waited.Load() && errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

type localRunner struct {
	command commandFunc
}

// NewLocal 创建直接在 hub 本地执行命令的 Runner。
// @return Runner 不经过 shell、按 argv 原样执行命令的本地 Runner。
func NewLocal() Runner {
	return newLocalRunner(exec.CommandContext)
}

func newLocalRunner(command commandFunc) *localRunner {
	return &localRunner{command: command}
}

func (r *localRunner) Run(ctx context.Context, argv ...string) (string, error) {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return "", err
	}
	return run(ctx, spec)
}

func (r *localRunner) Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error) {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return nil, nil, err
	}
	return stream(ctx, spec)
}

func (r *localRunner) Feed(ctx context.Context, stdin io.Reader, argv ...string) error {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return err
	}
	return feed(ctx, spec, stdin)
}

func (r *localRunner) build(ctx context.Context, argv []string) (commandSpec, error) {
	if err := validateArgv(argv); err != nil {
		return commandSpec{}, err
	}
	return commandSpec{
		cmd:     r.command(ctx, argv[0], argv[1:]...),
		display: formatCommand(argv),
	}, nil
}

type sshRunner struct {
	host           string
	port           string
	user           string
	identityFile   string
	knownHostsFile string
	hostKeyPolicy  string
	command        commandFunc
}

// NewSSH 创建通过系统 OpenSSH 连接目标机的 Runner。
// @param cfg 已通过清单校验的 SSH 连接配置。
// @return Runner 按配置校验主机密钥并逐参数转义远程命令的 Runner。
// @return error 配置缺失、地址非法或密钥路径不是绝对路径时的错误。
func NewSSH(cfg config.SSH) (Runner, error) {
	return newSSHRunner(cfg, exec.CommandContext)
}

func newSSHRunner(cfg config.SSH, command commandFunc) (*sshRunner, error) {
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("ssh.address: %q 不是合法的 host:port: %w", cfg.Address, err)
	}
	if host == "" {
		return nil, fmt.Errorf("ssh.address: %q 缺少主机名或 IP", cfg.Address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("ssh.address: %q 的端口必须是 1-65535 之间的数字", cfg.Address)
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("ssh.user: 不能为空")
	}
	if cfg.IdentityFile == "" {
		return nil, fmt.Errorf("ssh.identity_file: 不能为空")
	}
	if !filepath.IsAbs(cfg.IdentityFile) {
		return nil, fmt.Errorf("ssh.identity_file: 必须是绝对路径，实际 %q", cfg.IdentityFile)
	}
	if cfg.KnownHostsFile == "" {
		return nil, fmt.Errorf("ssh.known_hosts_file: 不能为空")
	}
	if !filepath.IsAbs(cfg.KnownHostsFile) {
		return nil, fmt.Errorf("ssh.known_hosts_file: 必须是绝对路径，实际 %q", cfg.KnownHostsFile)
	}
	hostKeyPolicy, err := openSSHHostKeyPolicy(cfg.EffectiveHostKeyPolicy())
	if err != nil {
		return nil, err
	}

	return &sshRunner{
		host:           host,
		port:           port,
		user:           cfg.User,
		identityFile:   cfg.IdentityFile,
		knownHostsFile: cfg.KnownHostsFile,
		hostKeyPolicy:  hostKeyPolicy,
		command:        command,
	}, nil
}

func openSSHHostKeyPolicy(policy config.SSHHostKeyPolicy) (string, error) {
	switch policy {
	case config.SSHHostKeyPolicyAcceptNew:
		return "accept-new", nil
	case config.SSHHostKeyPolicyStrict:
		return "yes", nil
	default:
		return "", fmt.Errorf("ssh.host_key_policy: %q 非法，只允许 %q 或 %q", policy,
			config.SSHHostKeyPolicyAcceptNew, config.SSHHostKeyPolicyStrict)
	}
}

func (r *sshRunner) Run(ctx context.Context, argv ...string) (string, error) {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return "", err
	}
	return run(ctx, spec)
}

func (r *sshRunner) Stream(ctx context.Context, argv ...string) (io.ReadCloser, func() error, error) {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return nil, nil, err
	}
	return stream(ctx, spec)
}

func (r *sshRunner) Feed(ctx context.Context, stdin io.Reader, argv ...string) error {
	spec, err := r.build(ctx, argv)
	if err != nil {
		return err
	}
	return feed(ctx, spec, stdin)
}

func (r *sshRunner) build(ctx context.Context, argv []string) (commandSpec, error) {
	if err := validateArgv(argv); err != nil {
		return commandSpec{}, err
	}

	// sshd 最终会把命令串交给登录 shell。每个值必须先独立转义，
	// 否则清单里的路径、服务名或数据库名会变成远程命令注入点。
	remoteCommand := make([]string, 0, len(argv))
	for _, arg := range argv {
		remoteCommand = append(remoteCommand, shellQuote(arg))
	}

	sshArgv := []string{
		"-T",
		"-o", "Compression=no",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=" + r.hostKeyPolicy,
		"-o", "UserKnownHostsFile=" + r.knownHostsFile,
		"-o", "IdentitiesOnly=yes",
		"-i", r.identityFile,
		"-p", r.port,
		"-l", r.user,
		"--", r.host,
		strings.Join(remoteCommand, " "),
	}
	displayArgv := append([]string{"ssh"}, sshArgv...)

	return commandSpec{
		cmd:     r.command(ctx, "ssh", sshArgv...),
		display: formatCommand(displayArgv),
	}, nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("执行命令失败: argv 必须包含非空命令名")
	}
	return nil
}

func run(ctx context.Context, spec commandSpec) (string, error) {
	out, err := spec.cmd.CombinedOutput()
	if err != nil {
		wrapped := wrapCommandError(ctx, spec.display, err, "")
		if isHostKeyConflict(string(out)) {
			wrapped = fmt.Errorf("%w；检测到 SSH 主机密钥冲突，请运行 ark host-key refresh --host <清单中的 host 名> 预览并确认新指纹", wrapped)
		}
		return string(out), wrapped
	}
	return string(out), nil
}

func stream(ctx context.Context, spec commandSpec) (io.ReadCloser, func() error, error) {
	pipe, err := spec.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("连接命令 %s 的 stdout 失败: %w", spec.display, err)
	}
	stdout := &commandStdout{reader: pipe}

	var stderr bytes.Buffer
	spec.cmd.Stderr = &stderr
	if err := spec.cmd.Start(); err != nil {
		// Start 失败后 Wait 不会接管 pipe，必须在这里主动释放描述符。
		_ = stdout.Close()
		return nil, nil, wrapCommandError(ctx, spec.display, err, stderr.String())
	}

	wait := func() error {
		err := spec.cmd.Wait()
		stdout.waited.Store(true)
		if err != nil {
			return wrapCommandError(ctx, spec.display, err, stderr.String())
		}
		return nil
	}
	return stdout, wait, nil
}

func feed(ctx context.Context, spec commandSpec, stdin io.Reader) error {
	var stderr bytes.Buffer
	spec.cmd.Stdin = stdin
	spec.cmd.Stdout = io.Discard
	spec.cmd.Stderr = &stderr
	if err := spec.cmd.Run(); err != nil {
		return wrapCommandError(ctx, spec.display, err, stderr.String())
	}
	return nil
}

func wrapCommandError(ctx context.Context, display string, err error, stderr string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("执行命令 %s 失败: %w", display, ctxErr)
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		if isHostKeyConflict(detail) {
			detail += "；请运行 ark host-key refresh --host <清单中的 host 名> 预览并确认新指纹"
		}
		return fmt.Errorf("执行命令 %s 失败: %w: %s", display, err, detail)
	}
	return fmt.Errorf("执行命令 %s 失败: %w", display, err)
}

func isHostKeyConflict(detail string) bool {
	return strings.Contains(detail, "REMOTE HOST IDENTIFICATION HAS CHANGED") ||
		strings.Contains(detail, "Host key verification failed")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func formatCommand(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}
