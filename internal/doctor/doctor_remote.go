package doctor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

// maxClockSkew 是 hub 与目标机允许的最大绝对时钟偏移。
// 时间差过大会让 Redis LASTSAVE 轮询和快照时间戳失去可信度。
const maxClockSkew = 60 * time.Second

// RunHost 检查一台 local 或 SSH host 的项目运行环境。
// @param ctx 控制每条目标机探测命令的取消和超时。
// @param cfg 已完成静态校验的完整备份清单。
// @param host 要检查的 host；local host 在 hub 本地执行，其他 host 通过 SSH 执行。
// @return *Report 单台 host 的检查报告；无效输入会记录为失败而不会 panic。
func RunHost(ctx context.Context, cfg *config.Config, host *config.Host) *Report {
	r := &Report{}
	if cfg == nil {
		r.add("config", StatusFail, "清单为空")
		return r
	}
	if host == nil {
		r.add("host", StatusFail, "host 为空")
		return r
	}

	runner, err := runnerForHost(host)
	if err != nil {
		name := hostCheckName(host, "connection")
		r.add(name, StatusFail, "创建执行器失败: %v", err)
		addConnectionDependentWarnings(r, host)
		return r
	}
	return runHost(ctx, cfg, host, runner, time.Now)
}

// runnerForHost 只负责 local / SSH 执行器选择，业务检查不关心连接形态。
func runnerForHost(host *config.Host) (sshexec.Runner, error) {
	if host.Local {
		return sshexec.NewLocal(), nil
	}
	if host.SSH == nil {
		return nil, fmt.Errorf("远程 host 缺少 ssh 配置")
	}
	return sshexec.NewSSH(*host.SSH)
}

// runHost 接收已构造的 Runner，使依赖降级和 argv 边界可以独立测试。
func runHost(
	ctx context.Context,
	_ *config.Config,
	host *config.Host,
	runner sshexec.Runner,
	now func() time.Time,
) *Report {
	r := &Report{}
	name := func(item string) string { return hostCheckName(host, item) }

	if host.Local {
		r.add(name("connection"), StatusOK, "本机执行")
	} else {
		if _, err := runRunner(ctx, runner, "true"); err != nil {
			r.add(name("connection"), StatusFail, "SSH 登录失败: %v", err)
			addConnectionDependentWarnings(r, host)
			return r
		}
		r.add(name("connection"), StatusOK, "SSH 登录成功")
	}

	checkClock(ctx, r, name("clock"), runner, now)

	dockerOK := checkRunnerVersion(ctx, r, name("docker"), runner, "docker", "--version")
	composeOK := false
	if dockerOK {
		composeOK = checkRunnerVersion(ctx, r, name("docker compose"), runner,
			"docker", "compose", "version")
	} else {
		r.add(name("docker compose"), StatusWarn, "docker 不可用，跳过检查")
	}

	composeFileOK := checkHostPath(ctx, r, name("project.compose_file"), host, runner,
		host.Project.ComposeFile, true, 0)
	envFileOK := true
	if host.Project.EnvFile != "" {
		envFileOK = checkHostPath(ctx, r, name("project.env_file"), host, runner,
			host.Project.EnvFile, true, 0o077)
	}

	services, servicesOK := checkComposeServices(ctx, r, name("compose.services"), host, runner,
		composeOK, composeFileOK, envFileOK)
	checkHostTargets(ctx, r, host, runner, services, dockerOK, servicesOK)
	return r
}

func hostCheckName(host *config.Host, item string) string {
	hostName := host.Host
	if hostName == "" {
		hostName = "host"
	}
	return hostName + " / " + item
}

// addConnectionDependentWarnings 保留全部检查项的可见性，不能把未检查伪装成通过或失败。
func addConnectionDependentWarnings(r *Report, host *config.Host) {
	name := func(item string) string { return hostCheckName(host, item) }
	reason := "连接不可用，跳过检查"
	for _, item := range []string{"clock", "docker", "docker compose", "project.compose_file"} {
		r.add(name(item), StatusWarn, "%s", reason)
	}
	if host.Project.EnvFile != "" {
		r.add(name("project.env_file"), StatusWarn, "%s", reason)
	}
	r.add(name("compose.services"), StatusWarn, "%s", reason)
	for _, target := range host.Targets {
		r.add(name("target "+target.ID()), StatusWarn, "%s", reason)
	}
}

// runRunner 为每条短命令创建独立超时，避免一台 host 卡住整轮 --all。
func runRunner(ctx context.Context, runner sshexec.Runner, argv ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return runner.Run(commandCtx, argv...)
}

func checkRunnerVersion(
	ctx context.Context,
	r *Report,
	name string,
	runner sshexec.Runner,
	argv ...string,
) bool {
	out, err := runRunner(ctx, runner, argv...)
	if err != nil {
		r.add(name, StatusFail, "执行失败: %v", err)
		return false
	}
	r.add(name, StatusOK, "%s", firstLine(out))
	return true
}

func checkClock(
	ctx context.Context,
	r *Report,
	name string,
	runner sshexec.Runner,
	now func() time.Time,
) {
	startedAt := now()
	out, err := runRunner(ctx, runner, "date", "+%s")
	finishedAt := now()
	if err != nil {
		r.add(name, StatusWarn, "读取目标机时间失败: %v", err)
		return
	}

	remoteTime, err := parseRemoteTime(out)
	if err != nil {
		r.add(name, StatusWarn, "%v", err)
		return
	}
	midpoint := startedAt.Add(finishedAt.Sub(startedAt) / 2)
	offset := remoteTime.Sub(midpoint)
	if offset < 0 {
		offset = -offset
	}
	if offset > maxClockSkew {
		r.add(name, StatusWarn, "与 hub 的时钟偏移为 %s，超过 60 秒", offset.Round(time.Second))
		return
	}
	r.add(name, StatusOK, "与 hub 的时钟偏移为 %s", offset.Round(time.Second))
}

func parseRemoteTime(output string) (time.Time, error) {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		seconds, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("目标机时间输出无效")
		}
		return time.Unix(seconds, 0), nil
	}
	return time.Time{}, fmt.Errorf("目标机时间输出为空")
}

func checkHostPath(
	ctx context.Context,
	r *Report,
	name string,
	host *config.Host,
	runner sshexec.Runner,
	path string,
	requireRegular bool,
	forbiddenPerm os.FileMode,
) bool {
	if path == "" {
		r.add(name, StatusFail, "路径为空")
		return false
	}
	metadata, err := hostPathMetadata(ctx, host, runner, path)
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

func hostPathMetadata(
	ctx context.Context,
	host *config.Host,
	runner sshexec.Runner,
	path string,
) (pathMetadata, error) {
	if host.Local {
		return localPathMetadata(path)
	}
	// os.Stat 会跟随符号链接；远程 stat 必须加 -L，才能让两条路径的
	// 文件类型和权限判定保持一致，避免同一清单因 host 远近得到不同结果。
	out, err := runRunner(ctx, runner, "stat", "-L", "-c", "%f %a", "--", path)
	if err != nil {
		return pathMetadata{}, err
	}
	return parseStatMetadata(out)
}

func parseStatMetadata(output string) (pathMetadata, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return pathMetadata{}, fmt.Errorf("stat 输出格式无效")
	}
	rawMode, err := strconv.ParseUint(fields[0], 16, 32)
	if err != nil {
		return pathMetadata{}, fmt.Errorf("stat 文件类型无效")
	}
	perm, err := strconv.ParseUint(fields[1], 8, 32)
	if err != nil {
		return pathMetadata{}, fmt.Errorf("stat 权限位无效")
	}

	kind := pathKindOther
	switch rawMode & 0o170000 {
	case 0o100000:
		kind = pathKindRegular
	case 0o040000:
		kind = pathKindDirectory
	}
	return pathMetadata{kind: kind, perm: os.FileMode(perm)}, nil
}

func checkComposeServices(
	ctx context.Context,
	r *Report,
	name string,
	host *config.Host,
	runner sshexec.Runner,
	composeOK bool,
	composeFileOK bool,
	envFileOK bool,
) (map[string]bool, bool) {
	switch {
	case !composeOK:
		r.add(name, StatusWarn, "docker compose 不可用，跳过服务检查")
		return nil, false
	case !composeFileOK || !envFileOK:
		r.add(name, StatusWarn, "项目文件检查未通过，跳过服务检查")
		return nil, false
	}

	argv := composeArgv(host)
	argv = append(argv, "config", "--services")
	out, err := runRunner(ctx, runner, argv...)
	if err != nil {
		// compose 会读取 env 文件，错误输出不进入报告，避免工具回显敏感值。
		r.add(name, StatusFail, "读取 compose 服务列表失败")
		return nil, false
	}

	services := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if service := strings.TrimSpace(line); service != "" {
			services[service] = true
		}
	}
	r.add(name, StatusOK, "发现 %d 个服务", len(services))
	return services, true
}

func composeArgv(host *config.Host) []string {
	argv := []string{"docker", "compose", "-f", host.Project.ComposeFile}
	if host.Project.ProjectName != "" {
		argv = append(argv, "-p", host.Project.ProjectName)
	}
	if host.Project.EnvFile != "" {
		argv = append(argv, "--env-file", host.Project.EnvFile)
	}
	return argv
}

// checkHostTargets 按每种 target 的真实依赖独立判断，避免一个失败掩盖其余问题。
func checkHostTargets(
	ctx context.Context,
	r *Report,
	host *config.Host,
	runner sshexec.Runner,
	services map[string]bool,
	dockerOK bool,
	servicesOK bool,
) {
	name := func(item string) string { return hostCheckName(host, item) }
	for _, target := range host.Targets {
		item := name("target " + target.ID())
		switch target.Type {
		case config.TargetPostgres, config.TargetRedis:
			checkServiceTarget(r, item, target.Service, services, servicesOK)
		case config.TargetVolume:
			if !dockerOK {
				r.add(item, StatusWarn, "docker 不可用，跳过 volume 检查")
				continue
			}
			if _, err := runRunner(ctx, runner, "docker", "volume", "inspect", target.Name); err != nil {
				r.add(item, StatusFail, "volume %q 不存在或不可访问", target.Name)
				continue
			}
			r.add(item, StatusOK, "volume %q 存在", target.Name)
		case config.TargetFiles:
			var missing []string
			for _, path := range target.Paths {
				if _, err := hostPathMetadata(ctx, host, runner, path); err != nil {
					missing = append(missing, path)
				}
			}
			if len(missing) > 0 {
				r.add(item, StatusFail, "以下路径无法访问: %s", strings.Join(missing, ", "))
				continue
			}
			r.add(item, StatusOK, "%d 个路径均存在", len(target.Paths))
		case config.TargetImageDigest:
			if !servicesOK {
				r.add(item, StatusWarn, "compose 服务列表不可用，跳过服务检查")
				continue
			}
			var unknown []string
			for _, service := range target.Services {
				if !services[service] {
					unknown = append(unknown, service)
				}
			}
			if len(unknown) > 0 {
				r.add(item, StatusFail, "compose 中不存在服务: %s", strings.Join(unknown, ", "))
				continue
			}
			r.add(item, StatusOK, "%d 个服务均已定义", len(target.Services))
		}
	}
}

func checkServiceTarget(r *Report, item, service string, services map[string]bool, servicesOK bool) {
	if !servicesOK {
		r.add(item, StatusWarn, "compose 服务列表不可用，跳过服务检查")
		return
	}
	if !services[service] {
		r.add(item, StatusFail, "compose 中不存在服务 %q", service)
		return
	}
	r.add(item, StatusOK, "服务 %q 已定义", service)
}
