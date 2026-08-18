package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/dnsmgr"
	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

type restoreCommandOptions struct {
	configPath      string
	sourceHost      string
	destinationHost string
	snapshot        string
	dryRun          bool
	inspect         bool
	force           bool
	isolate         bool
	skipDoctor      bool
	asJSON          bool
	expectedPreview string
}

type restoreDependencies struct {
	loadConfig       func(string) (*config.Config, error)
	acquireLock      func(string) (io.Closer, error)
	runLocalDoctor   runLocalFunc
	runRestoreDoctor runHostFunc
	runDNSMgrDoctor  runDNSMgrFunc
	newRepo          func(*config.Repo) (*restic.Repo, error)
	loadManifest     func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error)
	newRunner        func(*config.Host) (sshexec.Runner, error)
	execute          func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error)
	inspect          func(context.Context, restore.Plan, sshexec.Runner, restore.InspectOptions) (restore.Preview, error)
	cleanup          func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error)
	newDNSMgrClient  func(string, string) (dnsmgr.ValueSetter, error)
	switchDNS        func(context.Context, dnsmgr.ValueSetter, dnsmgr.Plan) (dnsmgr.SwitchResult, error)
	backup           backupDependencies
}

// newRestoreCmd 构建恢复计划与真实恢复命令。
func newRestoreCmd(configPath *string) *cobra.Command {
	return newRestoreCmdWithDependencies(configPath, defaultRestoreDependencies())
}

func defaultRestoreDependencies() restoreDependencies {
	return restoreDependencies{
		loadConfig:       config.LoadAndValidate,
		acquireLock:      acquireBackupLock,
		runLocalDoctor:   doctor.RunLocal,
		runRestoreDoctor: doctor.RunRestoreHost,
		runDNSMgrDoctor:  doctor.RunDNSMgr,
		newRepo:          restic.New,
		loadManifest:     backup.LoadManifestSelection,
		newRunner:        backupRunnerForHost,
		execute:          restore.Execute,
		inspect:          restore.Inspect,
		cleanup:          restore.CleanupIsolation,
		newDNSMgrClient: func(baseURL string, envFile string) (dnsmgr.ValueSetter, error) {
			return dnsmgr.New(baseURL, envFile)
		},
		switchDNS: dnsmgr.Switch,
		backup:    defaultBackupDependencies(),
	}
}

func newRestoreCmdWithDependencies(
	configPath *string,
	dependencies restoreDependencies,
) *cobra.Command {
	options := restoreCommandOptions{snapshot: backup.LatestManifestSelector}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "生成恢复计划或执行真实恢复",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(options.sourceHost) == "" {
				return fmt.Errorf("--host 不能为空")
			}
			if options.inspect && !options.dryRun {
				return fmt.Errorf("--inspect 必须与 --dry-run 同时使用")
			}
			if options.dryRun && options.skipDoctor {
				return fmt.Errorf("--dry-run 不能与 --skip-doctor 同时使用")
			}
			if options.dryRun && options.force && !options.inspect {
				return fmt.Errorf("--dry-run 不能与 --force 单独使用，目标预检必须同时指定 --inspect")
			}
			if options.isolate && options.force {
				return fmt.Errorf("--isolate 不能与 --force 同时使用")
			}
			if options.dryRun && strings.TrimSpace(options.expectedPreview) != "" {
				return fmt.Errorf("--expected-preview-sha256 只能用于真实恢复")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.configPath = *configPath
			if options.dryRun {
				if options.inspect {
					preview, err := buildRestoreInspection(cmd.Context(), options, dependencies)
					if err != nil {
						return err
					}
					if options.asJSON {
						return encodeRestoreJSON(cmd, preview)
					}
					return printRestorePreview(cmd, preview)
				}
				plan, err := buildRestoreDryRun(cmd.Context(), options, dependencies)
				if err != nil {
					return err
				}
				if options.asJSON {
					return encodeRestoreJSON(cmd, plan)
				}
				return printRestorePlan(cmd, plan)
			}

			result, runErr := runRestore(cmd, options, dependencies)
			if result.Status == "" {
				return runErr
			}
			var printErr error
			if options.asJSON {
				printErr = encodeRestoreJSON(cmd, result)
			} else {
				printErr = printRestoreResult(cmd, result)
			}
			if runErr != nil {
				return errors.Join(errRestoreFailed, runErr, printErr)
			}
			return printErr
		},
	}
	cmd.Flags().StringVar(&options.sourceHost, "host", "", "manifest 中的备份来源 host")
	cmd.Flags().StringVar(&options.destinationHost, "to", "", "当前清单中的恢复目标 host，默认与来源相同")
	cmd.Flags().StringVar(&options.snapshot, "snapshot", backup.LatestManifestSelector, "manifest snapshot ID 或 latest")
	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "只读取 manifest 并输出恢复计划")
	cmd.Flags().BoolVar(&options.inspect, "inspect", false, "只读检查恢复目标并输出冲突与预检摘要")
	cmd.Flags().BoolVar(&options.force, "force", false, "在成功备份目标机后覆盖当前 Plan 的冲突资源")
	cmd.Flags().BoolVar(&options.isolate, "isolate", false, "自动派生独立资源和空闲端口执行隔离恢复")
	cmd.Flags().BoolVar(&options.skipDoctor, "skip-doctor", false, "应急跳过本地和恢复目标环境检查")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出计划或最终结果")
	cmd.Flags().StringVar(&options.expectedPreview, "expected-preview-sha256", "", "要求真实恢复匹配指定预检摘要")
	cmd.AddCommand(newRestoreCleanupCmd(configPath, dependencies))
	return cmd
}

func buildRestoreInspection(
	ctx context.Context,
	options restoreCommandOptions,
	dependencies restoreDependencies,
) (preview restore.Preview, runErr error) {
	if dependencies.loadConfig == nil || dependencies.acquireLock == nil || dependencies.newRepo == nil ||
		dependencies.loadManifest == nil || dependencies.newRunner == nil || dependencies.inspect == nil {
		return preview, fmt.Errorf("生成恢复预检失败: 依赖不完整")
	}
	cfg, err := dependencies.loadConfig(options.configPath)
	if err != nil {
		return preview, err
	}
	source := findRestoreHost(cfg, options.sourceHost)
	if source == nil {
		return preview, fmt.Errorf("清单中不存在恢复来源 host %q", options.sourceHost)
	}
	destinationName := options.destinationHost
	if strings.TrimSpace(destinationName) == "" {
		destinationName = options.sourceHost
	}
	destination := findRestoreHost(cfg, destinationName)
	if destination == nil {
		return preview, fmt.Errorf("清单中不存在恢复目标 host %q", destinationName)
	}

	lock, err := dependencies.acquireLock(defaultBackupLockPath)
	if err != nil {
		return preview, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("释放 ark 全局锁失败: %w", closeErr))
		}
	}()

	repo, err := dependencies.newRepo(&cfg.Repo)
	if err != nil {
		return preview, fmt.Errorf("打开 restic 仓库失败: %w", err)
	}
	manifest, snapshot, found, err := dependencies.loadManifest(ctx, repo, options.snapshot)
	if err != nil {
		return preview, err
	}
	if !found {
		return preview, fmt.Errorf("restic 仓库中不存在 manifest 快照")
	}
	plan, err := restore.BuildPlan(cfg, manifest, snapshot.ID, options.sourceHost, destinationName)
	if err != nil {
		return preview, err
	}
	if options.isolate {
		plan, err = restore.WithIsolation(plan)
		if err != nil {
			return preview, err
		}
	}
	runner, err := dependencies.newRunner(destination)
	if err != nil {
		return preview, err
	}
	rawFileTargets := restoreRawFileTargets(*source, *destination, dependencies.backup.statePath)
	if plan.Isolation != nil {
		for targetID, targetPath := range rawFileTargets {
			mapped, mapErr := restore.IsolationPath(plan, targetPath)
			if mapErr != nil {
				return preview, mapErr
			}
			rawFileTargets[targetID] = mapped
		}
	}
	return dependencies.inspect(ctx, plan, runner, restore.InspectOptions{
		Force: options.force, RawFileTargets: rawFileTargets,
	})
}

func buildRestoreDryRun(
	ctx context.Context,
	options restoreCommandOptions,
	dependencies restoreDependencies,
) (restore.Plan, error) {
	if dependencies.loadConfig == nil || dependencies.newRepo == nil || dependencies.loadManifest == nil {
		return restore.Plan{}, fmt.Errorf("生成恢复计划失败: 依赖不完整")
	}
	cfg, err := dependencies.loadConfig(options.configPath)
	if err != nil {
		return restore.Plan{}, err
	}
	repo, err := dependencies.newRepo(&cfg.Repo)
	if err != nil {
		return restore.Plan{}, fmt.Errorf("打开 restic 仓库失败: %w", err)
	}
	manifest, snapshot, found, err := dependencies.loadManifest(ctx, repo, options.snapshot)
	if err != nil {
		return restore.Plan{}, err
	}
	if !found {
		return restore.Plan{}, fmt.Errorf("restic 仓库中不存在 manifest 快照")
	}
	destinationHost := options.destinationHost
	if strings.TrimSpace(destinationHost) == "" {
		destinationHost = options.sourceHost
	}
	plan, err := restore.BuildPlan(
		cfg,
		manifest,
		snapshot.ID,
		options.sourceHost,
		destinationHost,
	)
	if err != nil || !options.isolate {
		return plan, err
	}
	return restore.WithIsolation(plan)
}

func runRestore(
	cmd *cobra.Command,
	options restoreCommandOptions,
	dependencies restoreDependencies,
) (result restore.Result, runErr error) {
	if err := validateRestoreDependencies(dependencies); err != nil {
		return result, err
	}
	cfg, err := dependencies.loadConfig(options.configPath)
	if err != nil {
		return result, err
	}
	source := findRestoreHost(cfg, options.sourceHost)
	if source == nil {
		return result, fmt.Errorf("清单中不存在恢复来源 host %q", options.sourceHost)
	}
	destinationName := options.destinationHost
	if strings.TrimSpace(destinationName) == "" {
		destinationName = options.sourceHost
	}
	destination := findRestoreHost(cfg, destinationName)
	if destination == nil {
		return result, fmt.Errorf("清单中不存在恢复目标 host %q", destinationName)
	}

	lock, err := dependencies.acquireLock(defaultBackupLockPath)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			if result.Status != "" {
				result.Status = store.StatusFail
				result.Error = "恢复未完成"
			}
			runErr = errors.Join(runErr, fmt.Errorf("释放 ark 全局锁失败: %w", closeErr))
		}
	}()

	repo, err := dependencies.newRepo(&cfg.Repo)
	if err != nil {
		return result, fmt.Errorf("打开 restic 仓库失败: %w", err)
	}
	manifest, snapshot, found, err := dependencies.loadManifest(cmd.Context(), repo, options.snapshot)
	if err != nil {
		return result, err
	}
	if !found {
		return result, fmt.Errorf("restic 仓库中不存在 manifest 快照")
	}
	plan, err := restore.BuildPlan(cfg, manifest, snapshot.ID, options.sourceHost, destinationName)
	if err != nil {
		return result, err
	}
	if options.isolate {
		plan, err = restore.WithIsolation(plan)
		if err != nil {
			return result, err
		}
	}
	if err := validateDNSRestoreDependencies(plan, options.skipDoctor, dependencies); err != nil {
		return result, err
	}

	if !options.skipDoctor {
		if err := requireDoctor("本地", dependencies.runLocalDoctor(cmd.Context(), cfg)); err != nil {
			return result, err
		}
		if err := requireDoctor("恢复目标", dependencies.runRestoreDoctor(cmd.Context(), cfg, destination)); err != nil {
			return result, err
		}
		if plan.DNS != nil {
			if err := requireDoctor("dnsmgr", dependencies.runDNSMgrDoctor(cmd.Context(), cfg)); err != nil {
				return result, err
			}
		}
	}
	runner, err := dependencies.newRunner(destination)
	if err != nil {
		return result, err
	}

	executeOptions := restore.ExecuteOptions{
		Force:                 options.force,
		RawFileTargets:        restoreRawFileTargets(*source, *destination, dependencies.backup.statePath),
		ExpectedPreviewSHA256: options.expectedPreview,
	}
	if plan.Isolation != nil {
		for targetID, targetPath := range executeOptions.RawFileTargets {
			mapped, mapErr := restore.IsolationPath(plan, targetPath)
			if mapErr != nil {
				return result, mapErr
			}
			executeOptions.RawFileTargets[targetID] = mapped
		}
	}
	if !options.asJSON {
		executeOptions.OnPlanReady = func(ready restore.Plan) error {
			return printRestorePlan(cmd, ready)
		}
	}
	if plan.Isolation == nil {
		executeOptions.SafetyBackup = func(ctx context.Context) error {
			return runRestoreSafetyBackup(ctx, cfg, destination, options, dependencies.backup)
		}
	}
	result, runErr = dependencies.execute(cmd.Context(), plan, repo, runner, executeOptions)
	if runErr != nil || plan.DNS == nil || (result.Status != store.StatusOK && result.Status != store.StatusWarn) {
		return result, runErr
	}
	return runRestoreDNS(cmd.Context(), cfg, plan, result, dependencies)
}

type restoreCleanupOptions struct {
	host        string
	isolationID string
	asJSON      bool
}

func newRestoreCleanupCmd(configPath *string, dependencies restoreDependencies) *cobra.Command {
	options := restoreCleanupOptions{}
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "校验归属并清理隔离恢复资源",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(options.host) == "" {
				return fmt.Errorf("--host 不能为空")
			}
			if !restore.ValidIsolationID(options.isolationID) {
				return fmt.Errorf("--isolation 必须是完整的 64 位小写十六进制 ID")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, runErr := runRestoreCleanup(cmd.Context(), *configPath, options, dependencies)
			if result.Status == "" {
				return runErr
			}
			var printErr error
			if options.asJSON {
				printErr = encodeRestoreJSON(cmd, result)
			} else {
				printErr = printRestoreCleanupResult(cmd, result)
			}
			if runErr != nil {
				return errors.Join(errRestoreFailed, runErr, printErr)
			}
			return printErr
		},
	}
	cmd.Flags().StringVar(&options.host, "host", "", "当前清单中的隔离恢复目标 host")
	cmd.Flags().StringVar(&options.isolationID, "isolation", "", "完整的隔离恢复 ID")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出清理结果")
	return cmd
}

func runRestoreCleanup(
	ctx context.Context,
	configPath string,
	options restoreCleanupOptions,
	dependencies restoreDependencies,
) (result restore.CleanupResult, runErr error) {
	if dependencies.loadConfig == nil || dependencies.acquireLock == nil ||
		dependencies.newRunner == nil || dependencies.cleanup == nil {
		return result, fmt.Errorf("执行 restore cleanup 失败: 内部依赖不完整")
	}
	cfg, err := dependencies.loadConfig(configPath)
	if err != nil {
		return result, err
	}
	destination := findRestoreHost(cfg, options.host)
	if destination == nil {
		return result, fmt.Errorf("清单中不存在恢复目标 host %q", options.host)
	}
	lock, err := dependencies.acquireLock(defaultBackupLockPath)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			if result.Status != "" {
				result.Status = store.StatusFail
				result.Error = "隔离资源清理未完成"
			}
			runErr = errors.Join(runErr, fmt.Errorf("释放 ark 全局锁失败: %w", closeErr))
		}
	}()
	runner, err := dependencies.newRunner(destination)
	if err != nil {
		return result, err
	}
	return dependencies.cleanup(ctx, runner, destination.Host, options.isolationID)
}

func validateRestoreDependencies(dependencies restoreDependencies) error {
	if dependencies.loadConfig == nil || dependencies.acquireLock == nil ||
		dependencies.runLocalDoctor == nil || dependencies.runRestoreDoctor == nil ||
		dependencies.newRepo == nil || dependencies.loadManifest == nil ||
		dependencies.newRunner == nil || dependencies.execute == nil {
		return fmt.Errorf("执行 restore 失败: 内部依赖不完整")
	}
	return nil
}

func validateDNSRestoreDependencies(plan restore.Plan, skipDoctor bool, dependencies restoreDependencies) error {
	if plan.DNS == nil {
		return nil
	}
	if (!skipDoctor && dependencies.runDNSMgrDoctor == nil) ||
		dependencies.newDNSMgrClient == nil || dependencies.switchDNS == nil {
		return fmt.Errorf("执行 restore DNS 切换失败: 内部依赖不完整")
	}
	return nil
}

func runRestoreDNS(
	ctx context.Context,
	cfg *config.Config,
	plan restore.Plan,
	result restore.Result,
	dependencies restoreDependencies,
) (restore.Result, error) {
	if cfg == nil || cfg.DNSMgr == nil || plan.DNS == nil {
		return result, fmt.Errorf("执行 restore DNS 切换失败: 配置或计划不完整")
	}
	client, err := dependencies.newDNSMgrClient(cfg.DNSMgr.BaseURL, cfg.DNSMgr.EnvFile)
	if err != nil {
		records := make([]dnsmgr.RecordResult, len(plan.DNS.Records))
		for index, record := range plan.DNS.Records {
			records[index] = dnsmgr.RecordResult{
				DomainID: record.DomainID, RecordID: record.RecordID, Status: "not_attempted",
			}
		}
		dnsResult := dnsmgr.SwitchResult{
			Status:        "rollback_failed",
			Records:       records,
			ManualRecords: append([]dnsmgr.Record(nil), plan.DNS.Records...),
			Error:         "DNS 切换未执行",
		}
		result.DNS = &dnsResult
		return failDNSRestore(result, fmt.Errorf("创建 dnsmgr client 失败: %w", err))
	}
	dnsResult, err := dependencies.switchDNS(ctx, client, *plan.DNS)
	result.DNS = &dnsResult
	if err != nil {
		return failDNSRestore(result, err)
	}
	return result, nil
}

func failDNSRestore(result restore.Result, err error) (restore.Result, error) {
	result.Status = store.StatusFail
	result.Error = "数据恢复已完成，但 DNS 切换失败"
	if result.DNS != nil {
		for _, record := range result.DNS.ManualRecords {
			result.ManualChecks = append(result.ManualChecks,
				fmt.Sprintf("人工核对 dnsmgr 记录 %d/%s 当前指向", record.DomainID, record.RecordID))
		}
	}
	return result, fmt.Errorf("数据恢复完成后切换 DNS 失败: %w", err)
}

func requireDoctor(scope string, report *doctor.Report) error {
	if failures := doctorFailureNames(report); len(failures) > 0 {
		return fmt.Errorf("%s doctor 未通过（失败项: %s），恢复已在目标写入前中止",
			scope, strings.Join(failures, ", "))
	}
	return nil
}

func runRestoreSafetyBackup(
	ctx context.Context,
	cfg *config.Config,
	destination *config.Host,
	options restoreCommandOptions,
	dependencies backupDependencies,
) error {
	// 恢复源快照仍要继续读取，因此 safety backup 不在恢复中途执行 forget/prune。
	dependencies.forgetPolicy = func(context.Context, *restic.Repo, config.Retention, []string) error { return nil }
	dependencies.prune = func(context.Context, *restic.Repo) error { return nil }
	summary, err := runBackupLocked(ctx, cfg, []*config.Host{destination}, backupCommandOptions{
		configPath: options.configPath,
		hostName:   destination.Host,
		skipDoctor: options.skipDoctor,
	}, dependencies)
	if err != nil {
		return err
	}
	if (summary.Status != store.StatusOK && summary.Status != store.StatusWarn) ||
		summary.Manifest == nil || strings.TrimSpace(summary.ManifestSnapshotID) == "" {
		return fmt.Errorf("destination safety backup 未生成完整 manifest")
	}
	return nil
}

func findRestoreHost(cfg *config.Config, name string) *config.Host {
	if cfg == nil {
		return nil
	}
	for index := range cfg.Hosts {
		if cfg.Hosts[index].Host == name {
			return &cfg.Hosts[index]
		}
	}
	return nil
}

func restoreRawFileTargets(source config.Host, destination config.Host, statePath string) map[string]string {
	if strings.TrimSpace(statePath) == "" {
		return nil
	}
	destinationTargets := make(map[string]config.Target, len(destination.Targets))
	for _, target := range destination.Targets {
		destinationTargets[target.ID()] = target
	}
	for _, sourceTarget := range source.Targets {
		if !isStateDatabaseTarget(source, sourceTarget, statePath) {
			continue
		}
		destinationTarget, ok := destinationTargets[sourceTarget.ID()]
		if !ok || len(destinationTarget.Paths) != 1 {
			return nil
		}
		return map[string]string{sourceTarget.ID(): destinationTarget.Paths[0]}
	}
	return nil
}

func encodeRestoreJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printRestoreResult(cmd *cobra.Command, result restore.Result) error {
	var output strings.Builder
	fmt.Fprintf(&output, "\n恢复结果: %s\n", result.Status)
	if result.Isolation != nil {
		fmt.Fprintf(&output, "  isolation ID: %s\n", result.Isolation.ID)
		fmt.Fprintf(&output, "  compose project: %s\n", result.Isolation.ProjectName)
		fmt.Fprintf(&output, "  generated compose: %s\n", result.Isolation.GeneratedComposeFile)
		for _, container := range result.Isolation.Containers {
			fmt.Fprintf(&output, "  container: %s (%s)\n", container.Service, container.ID)
		}
		for _, volume := range result.Isolation.Volumes {
			fmt.Fprintf(&output, "  volume: %s\n", volume)
		}
		for _, network := range result.Isolation.Networks {
			fmt.Fprintf(&output, "  network: %s\n", network)
		}
		for _, port := range result.Isolation.Ports {
			fmt.Fprintf(&output, "  port: %s %s -> %d/%s\n",
				port.Service, restoreIsolationAddress(port), port.Target, port.Protocol)
		}
		fmt.Fprintf(&output, "  清理命令: %s\n", result.Isolation.CleanupCommand)
	}
	if result.DNS != nil {
		fmt.Fprintf(&output, "  DNS 切换: %s\n", result.DNS.Status)
		for _, record := range result.DNS.Records {
			fmt.Fprintf(&output, "    - %d/%s: %s", record.DomainID, record.RecordID, record.Status)
			if record.RollbackStatus != "" {
				fmt.Fprintf(&output, "，回滚 %s", record.RollbackStatus)
			}
			fmt.Fprintln(&output)
		}
	}
	for _, step := range result.Steps {
		target := "项目级"
		if step.TargetID != "" {
			target = step.TargetID
		}
		fmt.Fprintf(&output, "  %-18s %-24s %s", restorePhaseLabel(step.Phase), target, step.Status)
		if step.Detail != "" {
			fmt.Fprintf(&output, " - %s", step.Detail)
		}
		fmt.Fprintln(&output)
	}
	if result.Error != "" {
		fmt.Fprintf(&output, "  失败摘要: %s\n", result.Error)
	}
	fmt.Fprintln(&output, "  人工确认:")
	for _, item := range result.ManualChecks {
		fmt.Fprintf(&output, "    - %s\n", item)
	}
	_, err := io.WriteString(cmd.OutOrStdout(), output.String())
	return err
}

func printRestorePreview(cmd *cobra.Command, preview restore.Preview) error {
	if err := printRestorePlan(cmd, preview.Plan); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\n恢复预检摘要: %s\n", preview.Digest)
	fmt.Fprintf(out, "  force: %t\n", preview.Force)
	fmt.Fprintf(out, "  resume: %t\n", preview.Resume)
	if len(preview.Conflicts) == 0 {
		fmt.Fprintln(out, "  冲突: 无")
		return nil
	}
	fmt.Fprintln(out, "  冲突:")
	for _, conflict := range preview.Conflicts {
		fmt.Fprintf(out, "    - %s: %s（force 授权: %t）\n", conflict.Resource, conflict.Detail, conflict.ForceAllowed)
	}
	return nil
}

func restoreIsolationAddress(port restore.IsolationPort) string {
	hostIP := port.HostIP
	if hostIP == "" {
		hostIP = "0.0.0.0"
	}
	if strings.Contains(hostIP, ":") {
		return "[" + hostIP + "]:" + port.AllocatedPort
	}
	return hostIP + ":" + port.AllocatedPort
}

func printRestoreCleanupResult(cmd *cobra.Command, result restore.CleanupResult) error {
	var output strings.Builder
	fmt.Fprintf(&output, "隔离资源清理: %s\n", result.Status)
	fmt.Fprintf(&output, "  host: %s\n", result.DestinationHost)
	fmt.Fprintf(&output, "  isolation ID: %s\n", result.IsolationID)
	for _, resource := range result.Removed {
		fmt.Fprintf(&output, "  已删除: %s\n", resource)
	}
	if result.Error != "" {
		fmt.Fprintf(&output, "  失败摘要: %s\n", result.Error)
	}
	_, err := io.WriteString(cmd.OutOrStdout(), output.String())
	return err
}

func printRestorePlan(cmd *cobra.Command, plan restore.Plan) error {
	out := cmd.OutOrStdout()
	var output strings.Builder
	fmt.Fprintln(&output, "恢复计划")
	fmt.Fprintf(&output, "  manifest snapshot: %s\n", plan.ManifestSnapshotID)
	fmt.Fprintf(&output, "  backup run: %s\n", plan.RunID)
	fmt.Fprintf(&output, "  来源: %s\n", plan.SourceHost)
	fmt.Fprintf(&output, "  目标: %s\n", plan.DestinationHost)
	fmt.Fprintf(&output, "  项目: %s\n", plan.Project.Name)
	fmt.Fprintf(&output, "  compose: %s\n", plan.Project.ComposeFile)
	if plan.Project.EnvFile != "" {
		fmt.Fprintf(&output, "  env: %s\n", plan.Project.EnvFile)
	}
	if plan.Project.ProjectName != "" {
		fmt.Fprintf(&output, "  compose project: %s\n", plan.Project.ProjectName)
	}
	if plan.Isolation != nil {
		fmt.Fprintf(&output, "  isolation ID: %s\n", plan.Isolation.ID)
		fmt.Fprintf(&output, "  isolation root: %s\n", plan.Isolation.Root)
		for _, mapping := range plan.Isolation.PathMappings {
			fmt.Fprintf(&output, "  文件: %s -> %s\n", mapping.Source, mapping.Destination)
		}
		for _, mapping := range plan.Isolation.VolumeMappings {
			fmt.Fprintf(&output, "  volume: %s -> %s\n", mapping.Source, mapping.Destination)
		}
		for _, port := range plan.Isolation.Ports {
			fmt.Fprintf(
				&output,
				"  端口: %s %s -> auto -> %d/%s\n",
				port.Service,
				restoreIsolationOriginalAddress(port),
				port.Target,
				port.Protocol,
			)
		}
		fmt.Fprintln(&output, "  隔离策略: 不覆盖原项目资源；--force 禁用")
	} else {
		fmt.Fprintf(&output, "  冲突策略: %s（默认拒绝覆盖；真实恢复需显式 --force）\n", plan.ConflictPolicy)
		if plan.DNS != nil {
			fmt.Fprintf(&output, "  DNS 目标: %s\n", plan.DNS.Value)
			for _, record := range plan.DNS.Records {
				fmt.Fprintf(&output, "  DNS 记录: %d/%s\n", record.DomainID, record.RecordID)
			}
		}
	}

	var current restore.Phase
	for _, step := range plan.Steps {
		if step.Phase != current {
			current = step.Phase
			fmt.Fprintf(&output, "\n阶段 %s\n", restorePhaseLabel(current))
		}
		printRestoreStep(&output, step)
	}

	fmt.Fprintln(&output, "\n人工确认")
	for _, item := range plan.ManualChecks {
		fmt.Fprintf(&output, "  - %s\n", item)
	}
	_, err := io.WriteString(out, output.String())
	return err
}

func restoreIsolationOriginalAddress(port restore.IsolationPort) string {
	host := port.HostIP
	if host == "" {
		host = "0.0.0.0"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	published := port.OriginalPublished
	if published == "" {
		published = "auto"
	}
	return host + ":" + published
}

func printRestoreStep(out io.Writer, step restore.Step) {
	if step.TargetID == "" {
		fmt.Fprintln(out, "  - 项目级步骤")
		return
	}
	fmt.Fprintf(out, "  - target: %s（%s）", step.TargetID, step.TargetType)
	if step.SnapshotID != "" {
		fmt.Fprintf(out, " snapshot=%s", step.SnapshotID)
	}
	fmt.Fprintln(out)
	if step.Target != nil {
		printRestoreTargetConfig(out, *step.Target)
	}
	if len(step.ImageDigests) > 0 {
		services := make([]string, 0, len(step.ImageDigests))
		for service := range step.ImageDigests {
			services = append(services, service)
		}
		sort.Strings(services)
		for _, service := range services {
			fmt.Fprintf(out, "      digest[%s]=%s\n", service, step.ImageDigests[service])
		}
	}
}

func printRestoreTargetConfig(out io.Writer, target restore.Target) {
	switch target.Type {
	case config.TargetPostgres:
		fmt.Fprintf(out, "      service=%s database=%s user=%s\n", target.Service, target.Database, target.User)
	case config.TargetRedis:
		fmt.Fprintf(out, "      service=%s\n", target.Service)
	case config.TargetVolume:
		fmt.Fprintf(out, "      volume=%s\n", target.Name)
	case config.TargetFiles:
		fmt.Fprintf(out, "      files=%s paths=%s\n", target.Name, strings.Join(target.Paths, ","))
	case config.TargetImageDigest:
		fmt.Fprintf(out, "      services=%s\n", strings.Join(target.Services, ","))
	}
}

func restorePhaseLabel(phase restore.Phase) string {
	switch phase {
	case restore.PhaseFiles:
		return "files"
	case restore.PhaseImageDigest:
		return "image digest"
	case restore.PhaseVolume:
		return "volume"
	case restore.PhaseDatabasePrepare:
		return "database prepare"
	case restore.PhaseDatabaseData:
		return "database data"
	case restore.PhaseApplication:
		return "application"
	case restore.PhaseHealth:
		return "health"
	default:
		return string(phase)
	}
}
