// Package doctor 校验 ark 的运行环境。
//
// 它和 config.Validate 的分工是：config 只看清单本身写得对不对，
// doctor 则验证 hub 与目标机是否真的具备执行条件。hub 本地检查由
// RunLocal 负责，单台 local 或 SSH host 的项目环境由 RunHost 负责。
//
// 备份工具最常见的失败模式不是「代码写错了」，而是「三个月前某个
// 前提条件悄悄变了，而没人发现」。doctor 把这些前提条件变成可以
// 定期自动检查的断言，并明确区分失败、告警与通过。
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/envfile"
)

// commandTimeout 是单条探测命令的超时时间。
// docker 或网络依赖异常时可能长时间无响应，不设超时会让整轮 doctor 卡死。
const commandTimeout = 15 * time.Second

// Status 是单项检查的结果。
type Status string

const (
	// StatusOK 表示检查通过。
	StatusOK Status = "ok"
	// StatusWarn 表示当前无法确认或存在不阻断执行的风险。
	StatusWarn Status = "warn"
	// StatusFail 表示备份或恢复会失败，必须修复。
	StatusFail Status = "fail"
)

// Check 是一项环境检查的结果。
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report 汇总一次或多次 doctor 检查的结果。
type Report struct {
	Checks []Check `json:"checks"`
}

// add 追加一条检查结果。
func (r *Report) add(name string, status Status, format string, args ...any) {
	r.Checks = append(r.Checks, Check{
		Name:   name,
		Status: status,
		Detail: fmt.Sprintf(format, args...),
	})
}

// Failed 报告是否存在必须修复的问题。
// @return bool 存在 StatusFail 时返回 true。
func (r *Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Counts 返回各状态的数量，用于输出摘要。
// @return ok 通过项数量。
// @return warn 告警项数量。
// @return fail 失败项数量。
func (r *Report) Counts() (ok, warn, fail int) {
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			ok++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	return ok, warn, fail
}

// RunLocal 检查 hub 自身及清单在 hub 上依赖的文件、计划和仓库访问能力。
// @param ctx 控制每条探测命令的取消和超时。
// @param cfg 已完成静态校验的备份清单。
// @return *Report hub 本地检查报告；无效输入会记录为失败而不会 panic。
func RunLocal(ctx context.Context, cfg *config.Config) *Report {
	r := &Report{}
	if cfg == nil {
		r.add("config", StatusFail, "清单为空")
		return r
	}

	resticOK := checkBinary(ctx, r, "restic", "restic", "version")
	checkBinary(ctx, r, "ssh", "ssh", "-V")
	systemdOK := checkBinary(ctx, r, "systemd-analyze", "systemd-analyze", "--version")

	passwordOK := checkLocalPath(r, "repo.password_file", cfg.Repo.PasswordFile, true, 0o077)
	envValues, envOK := checkRepoEnvFile(r, cfg.Repo.EnvFile)

	for i := range cfg.Hosts {
		h := &cfg.Hosts[i]
		name := func(item string) string { return h.Host + " / " + item }
		if h.SSH != nil {
			checkLocalPath(r, name("ssh.identity_file"), h.SSH.IdentityFile, true, 0o077)
			checkLocalPath(r, name("ssh.known_hosts_file"), h.SSH.KnownHostsFile, true, 0)
		}

		if systemdOK {
			checkOnCalendar(ctx, r, name("schedule.on_calendar"), cfg.ScheduleFor(h).OnCalendar)
		} else {
			r.add(name("schedule.on_calendar"), StatusWarn,
				"systemd-analyze 不可用，跳过语法校验")
		}
	}

	checkRepoAccess(ctx, r, cfg, envValues, resticOK && passwordOK && envOK)
	return r
}

// checkBinary 检查外部命令是否存在且可执行，返回是否可用。
func checkBinary(ctx context.Context, r *Report, name string, argv ...string) bool {
	if _, err := exec.LookPath(argv[0]); err != nil {
		r.add(name, StatusFail, "未找到可执行文件 %s", argv[0])
		return false
	}
	out, err := runCommand(ctx, argv...)
	if err != nil {
		r.add(name, StatusFail, "执行失败: %v", err)
		return false
	}
	r.add(name, StatusOK, "%s", firstLine(out))
	return true
}

// checkOnCalendar 用 systemd 自己来校验 OnCalendar 表达式。
func checkOnCalendar(ctx context.Context, r *Report, name, expr string) {
	out, err := runCommand(ctx, "systemd-analyze", "calendar", expr)
	if err != nil {
		r.add(name, StatusFail, "%q 不是合法的 OnCalendar 表达式", expr)
		return
	}
	// systemd 的下一次触发时间比简单复述表达式更有排障价值。
	detail := expr
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Next elapse:") {
			detail = fmt.Sprintf("%s（%s）", expr, strings.TrimSpace(line))
			break
		}
	}
	r.add(name, StatusOK, "%s", detail)
}

type pathKind uint8

const (
	pathKindOther pathKind = iota
	pathKindRegular
	pathKindDirectory
)

type pathMetadata struct {
	kind pathKind
	perm os.FileMode
}

// localPathMetadata 将 os.Stat 结果归一化为本地和远程共用的文件元数据。
func localPathMetadata(path string) (pathMetadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		return pathMetadata{}, err
	}

	kind := pathKindOther
	switch {
	case info.Mode().IsRegular():
		kind = pathKindRegular
	case info.IsDir():
		kind = pathKindDirectory
	}
	return pathMetadata{kind: kind, perm: info.Mode().Perm()}, nil
}

// validatePathMetadata 是本地 os.Stat 与远程 stat 的唯一判定入口。
func validatePathMetadata(path string, metadata pathMetadata, requireRegular bool, forbiddenPerm os.FileMode) error {
	if requireRegular && metadata.kind != pathKindRegular {
		if metadata.kind == pathKindDirectory {
			return fmt.Errorf("%s 是目录，期望普通文件", path)
		}
		return fmt.Errorf("%s 不是普通文件", path)
	}
	if forbiddenPerm != 0 && metadata.perm&forbiddenPerm != 0 {
		return fmt.Errorf("%s 权限过宽（当前 %04o，应为 0600）", path, metadata.perm)
	}
	return nil
}

// checkLocalPath 检查 hub 上路径的存在性、类型和权限。
func checkLocalPath(r *Report, name, path string, requireRegular bool, forbiddenPerm os.FileMode) bool {
	if path == "" {
		r.add(name, StatusFail, "路径为空")
		return false
	}
	metadata, err := localPathMetadata(path)
	if err != nil {
		r.add(name, StatusFail, "无法访问 %s: %v", path, err)
		return false
	}
	if err := validatePathMetadata(path, metadata, requireRegular, forbiddenPerm); err != nil {
		r.add(name, StatusFail, "%v", err)
		return false
	}
	r.add(name, StatusOK, "%s", path)
	return true
}

// checkRepoEnvFile 同时检查凭证文件元数据并按受限语法解析内容。
func checkRepoEnvFile(r *Report, path string) (map[string]string, bool) {
	if path == "" {
		return nil, true
	}
	metadata, err := localPathMetadata(path)
	if err != nil {
		r.add("repo.env_file", StatusFail, "无法访问 %s: %v", path, err)
		return nil, false
	}
	if err := validatePathMetadata(path, metadata, true, 0o077); err != nil {
		r.add("repo.env_file", StatusFail, "%v", err)
		return nil, false
	}
	values, err := parseEnvFile(path)
	if err != nil {
		r.add("repo.env_file", StatusFail, "%v", err)
		return nil, false
	}
	r.add("repo.env_file", StatusOK, "%s", path)
	return values, true
}

// parseEnvFile 读取不执行 shell 展开、命令替换或变量插值的凭证文件。
func parseEnvFile(path string) (map[string]string, error) {
	return envfile.Parse(path)
}

// checkRepoAccess 在所有本地前置条件通过后验证仓库可达且能够解锁。
func checkRepoAccess(ctx context.Context, r *Report, cfg *config.Config, envValues map[string]string, prerequisitesOK bool) {
	if !prerequisitesOK {
		r.add("repo.access", StatusWarn, "前置检查未通过，跳过仓库解锁")
		return
	}

	overrides := make(map[string]string, len(envValues)+2)
	for key, value := range envValues {
		overrides[key] = value
	}
	// 仓库位置和密码文件必须以清单为准，不能被父进程或 env 文件悄悄覆盖。
	overrides["RESTIC_REPOSITORY"] = cfg.Repo.URL
	overrides["RESTIC_PASSWORD_FILE"] = cfg.Repo.PasswordFile

	if err := runResticCatConfig(ctx, mergeEnv(os.Environ(), overrides)); err != nil {
		r.add("repo.access", StatusFail, "%v", err)
		return
	}
	r.add("repo.access", StatusOK, "仓库可达且可以解锁")
}

// mergeEnv 合并环境变量并保证每个 key 只出现一次，覆盖值优先。
func mergeEnv(base []string, overrides map[string]string) []string {
	return envfile.Merge(base, overrides)
}

// runResticCatConfig 隔离凭证环境，并刻意不回显可能包含敏感信息的输出。
func runResticCatConfig(ctx context.Context, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "restic", "cat", "config")
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("执行 restic cat config 失败: %w", ctxErr)
		}
		return fmt.Errorf("执行 restic cat config 失败: %w", err)
	}
	return nil
}

// runCommand 执行不含凭证的本地探测命令并返回合并输出。
func runCommand(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%s: %w", strings.Join(argv, " "), ctxErr)
		}
		return "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err,
			strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// firstLine 取输出的第一行，用于展示版本号这类单行信息。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
