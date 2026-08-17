// Package systemd 生成、校验并原子安装 ark 的备份、恢复演练与 hub systemd units。
//
// unit 只保存二进制路径、清单路径、host 与 hub 非秘密启动参数，不嵌入密码、会话、
// 对象存储凭证或 SSH 私钥内容。
// 所有文件先在目标目录内完成写入和 systemd-analyze 校验，再通过 rename 提交；失败时
// 恢复旧文件，避免 systemd 读到半份配置。
package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
)

const (
	// DefaultUnitDir 是 ark 在 Linux 上安装 systemd system unit 的默认目录。
	DefaultUnitDir = "/etc/systemd/system"
	// DefaultVerifyOnCalendar 是 all-host 恢复演练的固定首版频率。
	DefaultVerifyOnCalendar = "weekly"
	// ManagedMarker 标识由 ark 管理、允许后续 install 更新或回收的 unit。
	ManagedMarker = "# Managed by ark; DO NOT EDIT."

	unitMode      = 0o644
	verifyTimeout = 15 * time.Second
)

var unitHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Unit 是一份待安装的 systemd unit 文件。
type Unit struct {
	// Name 是不含目录的 unit 文件名。
	Name string
	// Content 是完整 unit 文本。
	Content string
}

// InstallOptions 描述 unit 安装位置与 service 调用参数。
type InstallOptions struct {
	// UnitDir 是 systemd system unit 目录。
	UnitDir string
	// BinaryPath 是 ExecStart 使用的 ark 绝对路径。
	BinaryPath string
	// ConfigPath 是 ExecStart 传给 --config 的清单绝对路径。
	ConfigPath string
}

// InstallResult 汇总本次写入和安全回收的 unit 文件名。
type InstallResult struct {
	// Written 是已生成并替换的 unit 文件名。
	Written []string `json:"written"`
	// Removed 是已不在当前清单且带 ark 管理标记的 timer 文件名。
	Removed []string `json:"removed"`
}

// HubInstallOptions 描述 ark-hub service 的安装位置与启动参数。
type HubInstallOptions struct {
	// UnitDir 是 systemd system unit 目录。
	UnitDir string
	// BinaryPath 是 ExecStart 使用的 ark-hub 绝对路径。
	BinaryPath string
	// ListenAddress 是 ark-hub serve 的监听地址。
	ListenAddress string
	// StateDBPath 是 ark-hub 打开的状态库绝对路径。
	StateDBPath string
	// AuthFile 是 ark-hub 管理员凭证文件绝对路径。
	AuthFile string
	// ConfigPath 是 ark-hub 读取的 ark v2 清单绝对路径。
	ConfigPath string
	// ArkBinaryPath 是 ark-hub 启动手工任务时使用的 ark 绝对路径。
	ArkBinaryPath string
	// SecureCookie 控制是否向 ark-hub serve 传递 --secure-cookie。
	SecureCookie bool
}

type installDependencies struct {
	verify func(context.Context, []string) error
	rename func(string, string) error
	remove func(string) error
}

type fileBackup struct {
	exists bool
	mode   os.FileMode
	data   []byte
}

// BuildUnits 按清单顺序生成备份 units 与单个 all-host 恢复演练 service/timer。
// @param cfg 已完成静态校验的备份清单。
// @param binaryPath ExecStart 使用的 ark 绝对路径。
// @param configPath ExecStart 传给 --config 的清单绝对路径。
// @return []Unit 按文件名稳定排序的 unit 集合。
// @return error 输入路径、host 或 schedule 无效时的错误。
func BuildUnits(cfg *config.Config, binaryPath, configPath string) ([]Unit, error) {
	if cfg == nil {
		return nil, fmt.Errorf("生成 systemd unit 失败: config 不能为空")
	}
	if err := validateUnitPath("binary path", binaryPath); err != nil {
		return nil, err
	}
	if err := validateUnitPath("config path", configPath); err != nil {
		return nil, err
	}

	binaryArg := quoteExecArgument(binaryPath)
	configArg := quoteExecArgument(configPath)
	units := []Unit{
		{
			Name: "ark-backup.service",
			Content: serviceUnit(
				"ark 全量备份",
				fmt.Sprintf("%s --config %s backup", binaryArg, configArg),
			),
		},
		{
			Name: "ark-backup@.service",
			Content: serviceUnit(
				"ark host %i 备份",
				fmt.Sprintf("%s --config %s backup --host %%i", binaryArg, configArg),
			),
		},
		{
			Name: "ark-verify.service",
			Content: serviceUnit(
				"ark 全 host 隔离恢复演练",
				fmt.Sprintf("%s --config %s verify", binaryArg, configArg),
			),
		},
		{
			Name:    "ark-verify.timer",
			Content: scheduledTimerUnit("ark 每周全 host 隔离恢复演练", DefaultVerifyOnCalendar, 21600, "ark-verify.service"),
		},
	}
	for i := range cfg.Hosts {
		host := &cfg.Hosts[i]
		if !unitHostPattern.MatchString(host.Host) {
			return nil, fmt.Errorf("生成 systemd unit 失败: hosts[%d].host %q 非法", i, host.Host)
		}
		schedule := cfg.ScheduleFor(host).OnCalendar
		if strings.TrimSpace(schedule) == "" || strings.ContainsAny(schedule, "\n\r\x00") {
			return nil, fmt.Errorf("生成 systemd unit 失败: host %q 的 OnCalendar 非法", host.Host)
		}
		units = append(units, Unit{
			Name:    "ark-backup@" + host.Host + ".timer",
			Content: backupTimerUnit(host.Host, schedule),
		})
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return units, nil
}

// BuildHubUnit 生成独立的 ark-hub 常驻 service。
// @param options ark-hub 二进制、监听、状态库、凭证文件与 Cookie 参数。
// @return Unit 名为 ark-hub.service 的完整 unit。
// @return error 路径或监听参数非法时的错误。
func BuildHubUnit(options HubInstallOptions) (Unit, error) {
	if options.ConfigPath == "" {
		options.ConfigPath = "/etc/ark/ark.yaml"
	}
	if options.ArkBinaryPath == "" {
		options.ArkBinaryPath = "/usr/local/bin/ark"
	}
	if err := validateUnitPath("binary path", options.BinaryPath); err != nil {
		return Unit{}, err
	}
	if err := validateUnitPath("state db path", options.StateDBPath); err != nil {
		return Unit{}, err
	}
	if err := validateUnitPath("auth file", options.AuthFile); err != nil {
		return Unit{}, err
	}
	if err := validateUnitPath("config path", options.ConfigPath); err != nil {
		return Unit{}, err
	}
	if err := validateUnitPath("ark binary path", options.ArkBinaryPath); err != nil {
		return Unit{}, err
	}
	if strings.TrimSpace(options.ListenAddress) == "" || strings.ContainsAny(options.ListenAddress, "\n\r\x00") {
		return Unit{}, fmt.Errorf("生成 systemd unit 失败: listen address %q 非法", options.ListenAddress)
	}
	execStart := strings.Join([]string{
		quoteExecArgument(options.BinaryPath),
		"serve",
		"--listen", quoteExecArgument(options.ListenAddress),
		"--state-db", quoteExecArgument(options.StateDBPath),
		"--auth-file", quoteExecArgument(options.AuthFile),
		"--config", quoteExecArgument(options.ConfigPath),
		"--ark-binary", quoteExecArgument(options.ArkBinaryPath),
	}, " ")
	if options.SecureCookie {
		execStart += " --secure-cookie"
	}
	return Unit{
		Name:    "ark-hub.service",
		Content: hubServiceUnit(execStart),
	}, nil
}

// Install 生成、预校验并原子安装 ark systemd unit。
// @param ctx 控制 systemd-analyze verify 和文件安装取消。
// @param cfg 已完成静态校验的备份清单。
// @param options 目标目录、二进制路径和清单路径。
// @return InstallResult 实际写入和回收的文件名。
// @return error 生成、暂存、verify、替换、回收或回滚失败时的错误。
func Install(ctx context.Context, cfg *config.Config, options InstallOptions) (InstallResult, error) {
	return install(ctx, cfg, options, installDependencies{
		verify: verifyUnitFiles,
		rename: os.Rename,
		remove: os.Remove,
	})
}

// InstallHub 生成、预校验并原子安装独立的 ark-hub service。
// @param ctx 控制 systemd-analyze verify 和文件安装取消。
// @param options 目标目录与 ark-hub 启动参数。
// @return InstallResult 只包含 ark-hub.service，不扫描或清理 timer。
// @return error 生成、暂存、verify、替换或回滚失败时的错误。
func InstallHub(ctx context.Context, options HubInstallOptions) (InstallResult, error) {
	return installHub(ctx, options, installDependencies{
		verify: verifyUnitFiles,
		rename: os.Rename,
		remove: os.Remove,
	})
}

func install(
	ctx context.Context,
	cfg *config.Config,
	options InstallOptions,
	dependencies installDependencies,
) (InstallResult, error) {
	units, err := BuildUnits(cfg, options.BinaryPath, options.ConfigPath)
	if err != nil {
		return InstallResult{}, err
	}
	return installUnitSet(ctx, options.UnitDir, units, true, dependencies)
}

func installHub(
	ctx context.Context,
	options HubInstallOptions,
	dependencies installDependencies,
) (InstallResult, error) {
	unit, err := BuildHubUnit(options)
	if err != nil {
		return InstallResult{}, err
	}
	return installUnitSet(ctx, options.UnitDir, []Unit{unit}, false, dependencies)
}

func installUnitSet(
	ctx context.Context,
	unitDir string,
	units []Unit,
	cleanupTimers bool,
	dependencies installDependencies,
) (InstallResult, error) {
	if ctx == nil {
		return InstallResult{}, fmt.Errorf("安装 systemd unit 失败: context 不能为空")
	}
	if strings.TrimSpace(unitDir) == "" {
		return InstallResult{}, fmt.Errorf("安装 systemd unit 失败: unit 目录不能为空")
	}
	if !filepath.IsAbs(unitDir) {
		return InstallResult{}, fmt.Errorf("安装 systemd unit 失败: unit 目录 %q 必须是绝对路径", unitDir)
	}
	if len(units) == 0 {
		return InstallResult{}, fmt.Errorf("安装 systemd unit 失败: unit 集合不能为空")
	}
	if dependencies.verify == nil || dependencies.rename == nil || dependencies.remove == nil {
		return InstallResult{}, fmt.Errorf("安装 systemd unit 失败: 内部依赖不完整")
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("创建 systemd unit 目录失败: %w", err)
	}

	stageDir, err := os.MkdirTemp(unitDir, ".ark-units-")
	if err != nil {
		return InstallResult{}, fmt.Errorf("创建 systemd unit 暂存目录失败: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	stagePaths := make([]string, 0, len(units))
	for _, unit := range units {
		path := filepath.Join(stageDir, unit.Name)
		if err := writeSyncedFile(path, []byte(unit.Content), unitMode); err != nil {
			return InstallResult{}, fmt.Errorf("暂存 systemd unit %q 失败: %w", unit.Name, err)
		}
		stagePaths = append(stagePaths, path)
	}
	if err := dependencies.verify(ctx, stagePaths); err != nil {
		return InstallResult{}, fmt.Errorf("校验 systemd unit 失败: %w", err)
	}

	var staleTimers []string
	if cleanupTimers {
		desiredTimers := make(map[string]bool)
		for _, unit := range units {
			if strings.HasSuffix(unit.Name, ".timer") {
				desiredTimers[unit.Name] = true
			}
		}
		staleTimers, err = managedStaleTimers(unitDir, desiredTimers)
		if err != nil {
			return InstallResult{}, err
		}
	}

	backups := make(map[string]fileBackup, len(units)+len(staleTimers))
	for _, unit := range units {
		path := filepath.Join(unitDir, unit.Name)
		backup, err := backupFile(path)
		if err != nil {
			return InstallResult{}, err
		}
		if backup.exists && !strings.HasPrefix(string(backup.data), ManagedMarker+"\n") {
			return InstallResult{}, fmt.Errorf("拒绝覆盖非 ark 管理的 unit %q", unit.Name)
		}
		backups[path] = backup
	}
	for _, name := range staleTimers {
		path := filepath.Join(unitDir, name)
		backup, err := backupFile(path)
		if err != nil {
			return InstallResult{}, err
		}
		backups[path] = backup
	}

	result := InstallResult{
		Written: make([]string, 0, len(units)),
		Removed: append([]string(nil), staleTimers...),
	}
	var applyErr error
	for _, unit := range units {
		from := filepath.Join(stageDir, unit.Name)
		to := filepath.Join(unitDir, unit.Name)
		if err := dependencies.rename(from, to); err != nil {
			applyErr = fmt.Errorf("替换 systemd unit %q 失败: %w", unit.Name, err)
			break
		}
		result.Written = append(result.Written, unit.Name)
	}
	if applyErr == nil {
		for _, name := range staleTimers {
			if err := dependencies.remove(filepath.Join(unitDir, name)); err != nil {
				applyErr = fmt.Errorf("删除陈旧 ark timer %q 失败: %w", name, err)
				break
			}
		}
	}
	if applyErr != nil {
		rollbackErr := restoreFiles(backups)
		return InstallResult{}, errors.Join(applyErr, rollbackErr)
	}
	if err := syncDirectory(unitDir); err != nil {
		rollbackErr := restoreFiles(backups)
		return InstallResult{}, errors.Join(err, rollbackErr)
	}
	return result, nil
}

func serviceUnit(description, execStart string) string {
	return strings.Join([]string{
		ManagedMarker,
		"[Unit]",
		"Description=" + description,
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=oneshot",
		// restic 需要 HOME 或 XDG_CACHE_HOME 才能启动；system service 不保证继承 HOME，
		// 由 systemd 创建受限缓存目录可以避免依赖调用者环境，也不把缓存放进 root home。
		"CacheDirectory=ark",
		"CacheDirectoryMode=0700",
		"Environment=XDG_CACHE_HOME=/var/cache/ark",
		"ExecStart=" + execStart,
		"",
	}, "\n")
}

func hubServiceUnit(execStart string) string {
	return strings.Join([]string{
		ManagedMarker,
		"[Unit]",
		"Description=ark hub 管理服务",
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"UMask=0077",
		"NoNewPrivileges=true",
		"PrivateTmp=true",
		"ExecStart=" + execStart,
		"Restart=on-failure",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

func backupTimerUnit(host, schedule string) string {
	return scheduledTimerUnit("ark host "+host+" 定时备份", schedule, 600, "ark-backup@"+host+".service")
}

func scheduledTimerUnit(description, schedule string, randomizedDelaySeconds int, service string) string {
	return strings.Join([]string{
		ManagedMarker,
		"[Unit]",
		"Description=" + description,
		"",
		"[Timer]",
		"OnCalendar=" + schedule,
		"Persistent=true",
		"RandomizedDelaySec=" + strconv.Itoa(randomizedDelaySeconds),
		"Unit=" + service,
		"",
		"[Install]",
		"WantedBy=timers.target",
		"",
	}, "\n")
}

func validateUnitPath(field, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("生成 systemd unit 失败: %s 不能为空", field)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("生成 systemd unit 失败: %s %q 必须是绝对路径", field, path)
	}
	if strings.ContainsAny(path, "\n\r\x00") {
		return fmt.Errorf("生成 systemd unit 失败: %s %q 包含非法字符", field, path)
	}
	return nil
}

func quoteExecArgument(value string) string {
	// systemd 会展开百分号 specifier；路径中的普通百分号必须翻倍，避免被误解释。
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func verifyUnitFiles(ctx context.Context, paths []string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	args := append([]string{"verify"}, paths...)
	output, err := exec.CommandContext(verifyCtx, "systemd-analyze", args...).CombinedOutput()
	if err != nil {
		if verifyCtx.Err() != nil {
			return errors.Join(fmt.Errorf("systemd-analyze verify 超时或取消"), verifyCtx.Err())
		}
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("systemd-analyze verify 失败: %w", err)
		}
		return fmt.Errorf("systemd-analyze verify 失败: %w: %s", err, detail)
	}
	return nil
}

func writeSyncedFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		closeErr := file.Close()
		return errors.Join(err, closeErr)
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		return errors.Join(err, closeErr)
	}
	return file.Close()
}

func managedStaleTimers(unitDir string, desired map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(unitDir)
	if err != nil {
		return nil, fmt.Errorf("扫描 systemd unit 目录失败: %w", err)
	}
	var stale []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || desired[name] || !managedTimerCandidate(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(unitDir, name))
		if err != nil {
			return nil, fmt.Errorf("读取候选陈旧 timer %q 失败: %w", name, err)
		}
		if strings.HasPrefix(string(data), ManagedMarker+"\n") {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func managedTimerCandidate(name string) bool {
	return strings.HasSuffix(name, ".timer") &&
		(strings.HasPrefix(name, "ark-backup@") || strings.HasPrefix(name, "ark-verify"))
}

func backupFile(path string) (fileBackup, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileBackup{}, nil
	}
	if err != nil {
		return fileBackup{}, fmt.Errorf("读取待替换 unit %q 元数据失败: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return fileBackup{}, fmt.Errorf("待替换 unit %q 不是普通文件", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileBackup{}, fmt.Errorf("读取待替换 unit %q 失败: %w", filepath.Base(path), err)
	}
	return fileBackup{exists: true, mode: info.Mode().Perm(), data: data}, nil
}

func restoreFiles(backups map[string]fileBackup) error {
	paths := make([]string, 0, len(backups))
	for path := range backups {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var errs []error
	for _, path := range paths {
		backup := backups[path]
		if !backup.exists {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("回滚新增 unit %q 失败: %w", filepath.Base(path), err))
			}
			continue
		}
		if err := atomicReplace(path, backup.data, backup.mode); err != nil {
			errs = append(errs, fmt.Errorf("恢复旧 unit %q 失败: %w", filepath.Base(path), err))
		}
	}
	if err := syncDirectory(filepath.Dir(paths[0])); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("回滚 systemd unit 失败: %w", errors.Join(errs...))
}

func atomicReplace(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".ark-rollback-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(mode); err != nil {
		closeErr := temporary.Close()
		return errors.Join(err, closeErr)
	}
	if _, err := temporary.Write(data); err != nil {
		closeErr := temporary.Close()
		return errors.Join(err, closeErr)
	}
	if err := temporary.Sync(); err != nil {
		closeErr := temporary.Close()
		return errors.Join(err, closeErr)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 systemd unit 目录用于同步失败: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("同步 systemd unit 目录失败: %w", err)
	}
	return nil
}
