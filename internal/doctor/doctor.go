// Package doctor 校验 ark 的运行环境。
//
// 它和 config.Validate 的分工是：config 只看清单本身写得对不对，
// doctor 看是不是真的能执行这份清单——外部命令在不在、
// 密钥文件权限安不安全、compose 里是不是真有这个 service、
// volume 是不是真的存在。
//
// 检查在 hub 上发起。目前只有 local: true 的机器能被完整体检，
// 远程机器需要 SSH 执行层（roadmap P1-2 / P1-3）才能查到对端，
// 在那之前对端的检查项一律记为 warn——「没检查」不能报成「没问题」。
//
// 备份工具最常见的失败模式不是「代码写错了」，而是「三个月前某个
// 前提条件悄悄变了，而没人发现」。doctor 就是用来把这些前提条件
// 变成可以定期自动检查的断言。
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
)

// commandTimeout 是单条外部命令的超时时间。
// docker 在守护进程异常时可能长时间无响应，不设超时会让 doctor 整个卡死。
const commandTimeout = 15 * time.Second

// Status 是单项检查的结果。
type Status string

const (
	// StatusOK 表示检查通过。
	StatusOK Status = "ok"
	// StatusWarn 表示不影响本次备份，但需要关注。
	StatusWarn Status = "warn"
	// StatusFail 表示备份或恢复会失败，必须修复。
	StatusFail Status = "fail"
)

// Check 是一项检查的结果。
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report 汇总一次 doctor 的全部检查结果。
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
func (r *Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Counts 返回各状态的数量，用于输出摘要。
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

// Run 执行全部检查。
//
// 检查之间存在依赖：docker 不可用时，compose service 和 volume 的检查
// 无法给出有意义的结论，此时降级为 warn 而不是伪造成 fail——
// 区分「确定有问题」和「无法判断」对运维决策很重要。
func Run(ctx context.Context, cfg *config.Config) *Report {
	r := &Report{}

	// 这几项是 hub 自身的能力，与有多少台机器无关，只查一次。
	dockerOK := checkBinary(ctx, r, "docker", "docker", "--version")
	checkBinary(ctx, r, "restic", "restic", "version")
	systemdOK := checkBinary(ctx, r, "systemd-analyze", "systemd-analyze", "--version")

	composeOK := false
	if dockerOK {
		composeOK = checkDockerCompose(ctx, r)
	} else {
		r.add("docker compose", StatusWarn, "docker 不可用，跳过检查")
	}

	// 仓库全局唯一，凭证也只有一份。
	checkFile(r, "repo.password_file", cfg.Repo.PasswordFile, 0o077)
	if cfg.Repo.EnvFile != "" {
		checkFile(r, "repo.env_file", cfg.Repo.EnvFile, 0o077)
	}

	for i := range cfg.Hosts {
		checkHost(ctx, r, cfg, &cfg.Hosts[i], systemdOK, composeOK)
	}

	return r
}

// checkHost 检查一台机器。
//
// 目前只有 local: true 的 host 能被真正体检：远程检查需要 SSH 执行层
// （roadmap P1-2 / P1-3），在它就位之前，远程机器上的 compose 文件、
// service 和 volume 是否存在都无从判断。这些项记为 warn 而不是 ok——
// 报成 ok 会让人以为已经验证过了，那正是备份工具最危险的谎言。
func checkHost(ctx context.Context, r *Report, cfg *config.Config, h *config.Host, systemdOK, composeOK bool) {
	// 一份报告里有多台机器，每项检查都要能看出属于谁。
	name := func(item string) string { return h.Host + " / " + item }

	if h.Local {
		checkFile(r, name("project.compose_file"), h.Project.ComposeFile, 0)
		if h.Project.EnvFile != "" {
			// .env 里通常有数据库密码和加密密钥，不应该对同组或其他用户可读。
			checkFile(r, name("project.env_file"), h.Project.EnvFile, 0o077)
		}
	} else if h.SSH != nil {
		// SSH 私钥和 known_hosts 都在 hub 上，不需要连过去就能查。
		checkFile(r, name("ssh.identity_file"), h.SSH.IdentityFile, 0o077)
		checkFile(r, name("ssh.known_hosts_file"), h.SSH.KnownHostsFile, 0)
	}

	// 备份时机的语法由 systemd 自己裁决，与目标机在不在线无关。
	onCalendar := cfg.ScheduleFor(h).OnCalendar
	if systemdOK {
		checkOnCalendar(ctx, r, name("schedule.on_calendar"), onCalendar)
	} else {
		r.add(name("schedule.on_calendar"), StatusWarn, "systemd-analyze 不可用，跳过语法校验")
	}

	switch {
	case !h.Local:
		r.add(name("targets"), StatusWarn, "远程检查待 SSH 执行层就位（roadmap P1-2 / P1-3），本轮跳过")
	case composeOK:
		checkTargets(ctx, r, h, name)
	default:
		r.add(name("targets"), StatusWarn, "docker compose 不可用，跳过目标存在性检查")
	}
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

// checkDockerCompose 单独检查 compose v2 插件，返回是否可用。
// docker 存在不代表 compose 插件装了，这是两件事。
func checkDockerCompose(ctx context.Context, r *Report) bool {
	out, err := runCommand(ctx, "docker", "compose", "version")
	if err != nil {
		r.add("docker compose", StatusFail, "compose v2 插件不可用: %v", err)
		return false
	}
	r.add("docker compose", StatusOK, "%s", firstLine(out))
	return true
}

// checkFile 检查文件存在性，并在 forbiddenPerm 非零时检查权限是否过宽。
// forbiddenPerm 是「不允许出现的权限位」，例如 0o077 表示禁止同组和其他用户访问。
func checkFile(r *Report, name, path string, forbiddenPerm os.FileMode) {
	if path == "" {
		r.add(name, StatusFail, "路径为空")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		r.add(name, StatusFail, "无法访问 %s: %v", path, err)
		return
	}
	if info.IsDir() {
		r.add(name, StatusFail, "%s 是目录，期望文件", path)
		return
	}
	if forbiddenPerm != 0 {
		if perm := info.Mode().Perm(); perm&forbiddenPerm != 0 {
			r.add(name, StatusFail,
				"%s 权限过宽（当前 %04o，应为 0600）", path, perm)
			return
		}
	}
	r.add(name, StatusOK, "%s", path)
}

// checkOnCalendar 用 systemd 自己来校验 OnCalendar 表达式。
func checkOnCalendar(ctx context.Context, r *Report, name, expr string) {
	out, err := runCommand(ctx, "systemd-analyze", "calendar", expr)
	if err != nil {
		r.add(name, StatusFail, "%q 不是合法的 OnCalendar 表达式", expr)
		return
	}
	// systemd-analyze 会输出 "Next elapse: ..." 这一行，直接展示给用户
	// 比复述表达式更有价值——它告诉你下一次实际会在什么时候跑。
	detail := expr
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Next elapse:") {
			detail = fmt.Sprintf("%s（%s）", expr, strings.TrimSpace(line))
			break
		}
	}
	r.add(name, StatusOK, "%s", detail)
}

// checkTargets 检查每个备份目标引用的资源是否真实存在。
// name 用于给检查项加上所属机器的前缀。
func checkTargets(ctx context.Context, r *Report, h *config.Host, name func(string) string) {
	services, err := composeServices(ctx, h)
	if err != nil {
		r.add(name("targets"), StatusWarn, "无法读取 compose 服务列表: %v", err)
		return
	}
	known := make(map[string]bool, len(services))
	for _, s := range services {
		known[s] = true
	}

	for _, t := range h.Targets {
		item := name("target " + t.ID())
		switch t.Type {
		case config.TargetPostgres, config.TargetRedis:
			if !known[t.Service] {
				r.add(item, StatusFail, "compose 中不存在服务 %q", t.Service)
				continue
			}
			r.add(item, StatusOK, "服务 %q 已定义", t.Service)

		case config.TargetVolume:
			if _, err := runCommand(ctx, "docker", "volume", "inspect", t.Name); err != nil {
				r.add(item, StatusFail, "volume %q 不存在", t.Name)
				continue
			}
			r.add(item, StatusOK, "volume %q 存在", t.Name)

		case config.TargetFiles:
			missing := make([]string, 0, len(t.Paths))
			for _, p := range t.Paths {
				if _, err := os.Stat(p); err != nil {
					missing = append(missing, p)
				}
			}
			if len(missing) > 0 {
				r.add(item, StatusFail, "以下路径不存在: %s", strings.Join(missing, ", "))
				continue
			}
			r.add(item, StatusOK, "%d 个路径均存在", len(t.Paths))

		case config.TargetImageDigest:
			var unknown []string
			for _, s := range t.Services {
				if !known[s] {
					unknown = append(unknown, s)
				}
			}
			if len(unknown) > 0 {
				r.add(item, StatusFail, "compose 中不存在服务: %s", strings.Join(unknown, ", "))
				continue
			}
			r.add(item, StatusOK, "%d 个服务均已定义", len(t.Services))
		}
	}
}

// composeServices 返回 compose 文件中定义的服务名列表。
func composeServices(ctx context.Context, h *config.Host) ([]string, error) {
	argv := []string{"docker", "compose", "-f", h.Project.ComposeFile}
	if h.Project.ProjectName != "" {
		argv = append(argv, "-p", h.Project.ProjectName)
	}
	if h.Project.EnvFile != "" {
		argv = append(argv, "--env-file", h.Project.EnvFile)
	}
	argv = append(argv, "config", "--services")

	out, err := runCommand(ctx, argv...)
	if err != nil {
		return nil, err
	}

	var services []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			services = append(services, s)
		}
	}
	return services, nil
}

// runCommand 执行外部命令并返回合并后的输出。
func runCommand(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
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
