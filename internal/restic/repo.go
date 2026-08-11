// Package restic 封装 ark 对 restic 仓库的全部命令行交互。
//
// 本包只理解仓库、凭证环境、标签和快照。备份目标如何产流、状态如何持久化
// 以及 CLI 如何编排均由上层负责。所有凭证只进入当前 restic 子进程，机器可读
// 输出使用 JSON，数据流则保持纯 stdout 并显式保留最终退出状态。
package restic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/envfile"
)

const repositoryMissingExitCode = 10

var resticControlEnvKeys = map[string]bool{
	"RESTIC_REPOSITORY":       true,
	"RESTIC_REPOSITORY_FILE":  true,
	"RESTIC_PASSWORD":         true,
	"RESTIC_PASSWORD_FILE":    true,
	"RESTIC_PASSWORD_COMMAND": true,
}

// Snapshot 是 ark 后续流程需要的稳定 restic 快照字段。
type Snapshot struct {
	// ID 是完整快照 ID。
	ID string `json:"id"`
	// Time 是快照创建时间。
	Time time.Time `json:"time"`
	// Hostname 是 restic 记录的来源主机名。
	Hostname string `json:"hostname,omitempty"`
	// Paths 是快照包含的顶层路径。
	Paths []string `json:"paths,omitempty"`
	// Tags 是快照标签。
	Tags []string `json:"tags,omitempty"`
}

type commandFunc func(context.Context, string, ...string) *exec.Cmd

// Repo 是一个已配置好隔离凭证环境的 restic 仓库。
type Repo struct {
	cfg     config.Repo
	env     []string
	command commandFunc
}

// New 创建 restic 仓库封装并读取受限语法的凭证环境文件。
// @param cfg 已完成清单静态校验的仓库配置。
// @return *Repo 可安全复用的仓库命令边界。
// @return error 配置缺失、后端类型不支持或凭证环境文件非法时的错误。
func New(cfg *config.Repo) (*Repo, error) {
	return newRepo(cfg, exec.CommandContext)
}

func newRepo(cfg *config.Repo, command commandFunc) (*Repo, error) {
	if cfg == nil {
		return nil, fmt.Errorf("repo: 配置不能为空")
	}
	if cfg.Type != "" && cfg.Type != config.DefaultRepoType {
		return nil, fmt.Errorf("repo.type: 只支持 %q，实际 %q", config.DefaultRepoType, cfg.Type)
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("repo.url: 不能为空")
	}
	if strings.TrimSpace(cfg.PasswordFile) == "" {
		return nil, fmt.Errorf("repo.password_file: 不能为空")
	}
	if command == nil {
		return nil, fmt.Errorf("restic 命令构造器不能为空")
	}

	values := make(map[string]string)
	if cfg.EnvFile != "" {
		parsed, err := envfile.Parse(cfg.EnvFile)
		if err != nil {
			return nil, err
		}
		for key, value := range parsed {
			values[key] = value
		}
	}
	for _, key := range []string{"RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND", "RESTIC_REPOSITORY_FILE"} {
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("repo.env_file %s 不允许设置 %s", cfg.EnvFile, key)
		}
	}

	// 仓库位置和密码文件必须以清单为唯一事实源。先移除父进程中的所有
	// 冲突入口，再覆盖 env 文件同名值，避免 restic 按自身优先级选错凭证。
	values["RESTIC_REPOSITORY"] = cfg.URL
	values["RESTIC_PASSWORD_FILE"] = cfg.PasswordFile
	base := withoutEnvKeys(os.Environ(), resticControlEnvKeys)

	return &Repo{
		cfg:     *cfg,
		env:     envfile.Merge(base, values),
		command: command,
	}, nil
}

// EnsureInit 确保仓库已初始化。
// @param ctx 控制仓库探测和初始化的取消；本方法不附加固定超时。
// @return error 仓库鉴权、锁、网络、损坏或初始化失败时的错误。
func (r *Repo) EnsureInit(ctx context.Context) error {
	cmd, display := r.newCommand(ctx, "cat", "config")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if code, ok := commandExitCode(err); !ok || code != repositoryMissingExitCode {
		return wrapCommandError(ctx, display, err)
	}

	// 只有 restic 明确返回“仓库不存在”的专用退出码时才 init。
	// 退出码 1 可能是网络、损坏或旧版本的歧义错误，不能冒险覆盖仓库。
	if err := r.run(ctx, "init"); err != nil {
		return fmt.Errorf("初始化 restic 仓库失败: %w", err)
	}
	return nil
}

// BackupStdin 把输入流直接保存为一个稳定文件名的 restic 快照。
// @param ctx 控制备份取消；本方法不附加固定超时。
// @param stdin 要备份的数据流，不会被整读或写入临时文件。
// @param filename 快照内稳定的 stdin 文件名，不能为空。
// @param tags 逐项传给 restic 的快照标签。
// @return Snapshot 新快照的稳定字段。
// @return error 输入、命令、JSON、pipe 或退出状态错误。
func (r *Repo) BackupStdin(ctx context.Context, stdin io.Reader, filename string, tags []string) (Snapshot, error) {
	if stdin == nil {
		return Snapshot{}, fmt.Errorf("restic backup stdin 不能为空")
	}
	if strings.TrimSpace(filename) == "" {
		return Snapshot{}, fmt.Errorf("restic stdin filename 不能为空")
	}
	args, err := appendTags([]string{"backup", "--json", "--stdin", "--stdin-filename", filename}, tags)
	if err != nil {
		return Snapshot{}, err
	}

	cmd, display := r.newCommand(ctx, args...)
	cmd.Stdin = stdin
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Snapshot{}, fmt.Errorf("连接命令 %s 的 stdout 失败: %w", display, err)
	}
	if err := cmd.Start(); err != nil {
		closeErr := stdout.Close()
		return Snapshot{}, errors.Join(wrapCommandError(ctx, display, err), wrapCloseError(display, closeErr))
	}

	snapshot, decodeErr := decodeBackupSummary(stdout)
	closeErr := stdout.Close()
	waitErr := cmd.Wait()
	err = errors.Join(
		wrapJSONError(display, decodeErr),
		wrapCloseError(display, closeErr),
		wrapCommandError(ctx, display, waitErr),
	)
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// Snapshots 按标签列出快照，并按时间、ID 升序返回确定顺序。
// @param ctx 控制查询取消；本方法不附加固定超时。
// @param tags 逐项传给 restic 的过滤标签；为空时列出全部快照。
// @return []Snapshot 只包含 ark 后续流程需要的稳定字段。
// @return error 标签、命令或 JSON 解析错误。
func (r *Repo) Snapshots(ctx context.Context, tags []string) ([]Snapshot, error) {
	args, err := appendTags([]string{"snapshots", "--json"}, tags)
	if err != nil {
		return nil, err
	}

	var snapshots []Snapshot
	if err := r.runJSON(ctx, &snapshots, args...); err != nil {
		return nil, err
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Time.Equal(snapshots[j].Time) {
			return snapshots[i].ID < snapshots[j].ID
		}
		return snapshots[i].Time.Before(snapshots[j].Time)
	})
	return snapshots, nil
}

// Forget 按保留策略筛选快照并统一执行 prune。
// @param ctx 控制 forget 和 prune 的取消；本方法不附加固定超时。
// @param policy 已生效的日、周、月保留份数。
// @param tags 逐项传给 restic 的快照过滤标签。
// @return error 策略、标签或 restic 命令失败时的错误。
func (r *Repo) Forget(ctx context.Context, policy config.Retention, tags []string) error {
	if policy.Daily < 0 || policy.Weekly < 0 || policy.Monthly < 0 {
		return fmt.Errorf("restic 保留份数不能为负: daily=%d weekly=%d monthly=%d",
			policy.Daily, policy.Weekly, policy.Monthly)
	}
	if policy.Daily == 0 && policy.Weekly == 0 && policy.Monthly == 0 {
		return fmt.Errorf("restic 保留策略三项不能同时为 0")
	}

	args := []string{"forget", "--json", "--prune"}
	if policy.Daily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(policy.Daily))
	}
	if policy.Weekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(policy.Weekly))
	}
	if policy.Monthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(policy.Monthly))
	}
	args, err := appendTags(args, tags)
	if err != nil {
		return err
	}
	return r.run(ctx, args...)
}

// ForgetSnapshot 精确删除一个坏快照并立即 prune。
// @param ctx 控制删除和 prune 的取消；本方法不附加固定超时。
// @param id 要撤销的完整快照 ID。
// @return error ID 为空或 restic 命令失败时的错误。
func (r *Repo) ForgetSnapshot(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("restic snapshot ID 不能为空")
	}
	return r.run(ctx, "forget", "--json", "--prune", id)
}

// Dump 流式读取快照中的文件，并让最终 Read 或 Close 暴露 restic 退出状态。
// @param ctx 控制读取取消；本方法不附加固定超时。
// @param snapshotID 要读取的快照 ID。
// @param path 快照内文件路径。
// @return io.ReadCloser 纯文件内容流；调用方必须读取至 EOF 或显式关闭。
// @return error 参数、命令创建或启动失败时的错误。
func (r *Repo) Dump(ctx context.Context, snapshotID, path string) (io.ReadCloser, error) {
	if strings.TrimSpace(snapshotID) == "" {
		return nil, fmt.Errorf("restic snapshot ID 不能为空")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("restic dump path 不能为空")
	}

	cmd, display := r.newCommand(ctx, "dump", snapshotID, path)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("连接命令 %s 的 stdout 失败: %w", display, err)
	}
	if err := cmd.Start(); err != nil {
		closeErr := stdout.Close()
		return nil, errors.Join(wrapCommandError(ctx, display, err), wrapCloseError(display, closeErr))
	}

	return &processReadCloser{
		reader: stdout,
		waitFn: func() error {
			return wrapCommandError(ctx, display, cmd.Wait())
		},
	}, nil
}

// Check 执行 restic 仓库完整性检查。
// @param ctx 控制检查取消；本方法不附加固定超时。
// @return error restic 检查发现问题或命令执行失败时的错误。
func (r *Repo) Check(ctx context.Context) error {
	return r.run(ctx, "check", "--json")
}

func (r *Repo) run(ctx context.Context, args ...string) error {
	cmd, display := r.newCommand(ctx, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return wrapCommandError(ctx, display, cmd.Run())
}

func (r *Repo) runJSON(ctx context.Context, target any, args ...string) error {
	cmd, display := r.newCommand(ctx, args...)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("连接命令 %s 的 stdout 失败: %w", display, err)
	}
	if err := cmd.Start(); err != nil {
		closeErr := stdout.Close()
		return errors.Join(wrapCommandError(ctx, display, err), wrapCloseError(display, closeErr))
	}

	decodeErr := decodeSingleJSON(stdout, target)
	closeErr := stdout.Close()
	waitErr := cmd.Wait()
	return errors.Join(
		wrapJSONError(display, decodeErr),
		wrapCloseError(display, closeErr),
		wrapCommandError(ctx, display, waitErr),
	)
}

func (r *Repo) newCommand(ctx context.Context, args ...string) (*exec.Cmd, string) {
	cmd := r.command(ctx, "restic", args...)
	cmd.Env = append([]string(nil), r.env...)
	return cmd, formatCommand(append([]string{"restic"}, args...))
}

type backupMessage struct {
	MessageType string    `json:"message_type"`
	SnapshotID  string    `json:"snapshot_id"`
	BackupStart time.Time `json:"backup_start"`
}

func decodeBackupSummary(reader io.Reader) (Snapshot, error) {
	decoder := json.NewDecoder(reader)
	var summary *backupMessage
	for {
		var message backupMessage
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Snapshot{}, err
		}
		if message.MessageType != "summary" {
			continue
		}
		if summary != nil {
			return Snapshot{}, fmt.Errorf("restic backup JSON 包含多个 summary")
		}
		copy := message
		summary = &copy
	}
	if summary == nil {
		return Snapshot{}, fmt.Errorf("restic backup JSON 缺少 summary")
	}
	if summary.SnapshotID == "" {
		return Snapshot{}, fmt.Errorf("restic backup summary 缺少 snapshot_id")
	}
	return Snapshot{ID: summary.SnapshotID, Time: summary.BackupStart}, nil
}

func decodeSingleJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON 输出包含多个顶层值")
		}
		return err
	}
	return nil
}

func appendTags(args, tags []string) ([]string, error) {
	for i, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return nil, fmt.Errorf("restic tag[%d] 不能为空", i)
		}
		args = append(args, "--tag", tag)
	}
	return args, nil
}

func withoutEnvKeys(base []string, excluded map[string]bool) []string {
	filtered := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || excluded[key] {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func commandExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

func wrapCommandError(ctx context.Context, display string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("执行命令 %s 失败: %w", display, ctxErr)
	}
	return fmt.Errorf("执行命令 %s 失败: %w", display, err)
}

func wrapJSONError(display string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("解析命令 %s 的 JSON 输出失败: %w", display, err)
}

func wrapCloseError(display string, err error) error {
	if err == nil || errors.Is(err, os.ErrClosed) {
		return nil
	}
	return fmt.Errorf("关闭命令 %s 的 stdout 失败: %w", display, err)
}

func formatCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

type processReadCloser struct {
	reader   io.ReadCloser
	waitFn   func() error
	waitOnce sync.Once
	waitErr  error
	finished atomic.Bool
}

func (r *processReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == nil {
		return n, nil
	}
	r.finished.Store(true)
	waitErr := r.wait()
	if errors.Is(err, io.EOF) {
		if waitErr != nil {
			return n, waitErr
		}
		return n, io.EOF
	}
	return n, errors.Join(err, waitErr)
}

func (r *processReadCloser) Close() error {
	var closeErr error
	if !r.finished.Swap(true) {
		closeErr = r.reader.Close()
	}
	return errors.Join(closeErr, r.wait())
}

func (r *processReadCloser) wait() error {
	r.waitOnce.Do(func() {
		r.waitErr = r.waitFn()
	})
	return r.waitErr
}
