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
	force           bool
	skipDoctor      bool
	asJSON          bool
}

type restoreDependencies struct {
	loadConfig       func(string) (*config.Config, error)
	acquireLock      func(string) (io.Closer, error)
	runLocalDoctor   runLocalFunc
	runRestoreDoctor runHostFunc
	newRepo          func(*config.Repo) (*restic.Repo, error)
	loadManifest     func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error)
	newRunner        func(*config.Host) (sshexec.Runner, error)
	execute          func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error)
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
		newRepo:          restic.New,
		loadManifest:     backup.LoadManifestSelection,
		newRunner:        backupRunnerForHost,
		execute:          restore.Execute,
		backup:           defaultBackupDependencies(),
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
			if options.dryRun && (options.force || options.skipDoctor) {
				return fmt.Errorf("--dry-run 不能与 --force 或 --skip-doctor 同时使用")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.configPath = *configPath
			if options.dryRun {
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
	cmd.Flags().BoolVar(&options.force, "force", false, "在成功备份目标机后覆盖当前 Plan 的冲突资源")
	cmd.Flags().BoolVar(&options.skipDoctor, "skip-doctor", false, "应急跳过本地和恢复目标环境检查")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出计划或最终结果")
	return cmd
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
	return restore.BuildPlan(
		cfg,
		manifest,
		snapshot.ID,
		options.sourceHost,
		destinationHost,
	)
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

	if !options.skipDoctor {
		if err := requireDoctor("本地", dependencies.runLocalDoctor(cmd.Context(), cfg)); err != nil {
			return result, err
		}
		if err := requireDoctor("恢复目标", dependencies.runRestoreDoctor(cmd.Context(), cfg, destination)); err != nil {
			return result, err
		}
	}
	runner, err := dependencies.newRunner(destination)
	if err != nil {
		return result, err
	}

	executeOptions := restore.ExecuteOptions{
		Force:          options.force,
		RawFileTargets: restoreRawFileTargets(*source, *destination, dependencies.backup.statePath),
	}
	if !options.asJSON {
		executeOptions.OnPlanReady = func(ready restore.Plan) error {
			return printRestorePlan(cmd, ready)
		}
	}
	executeOptions.SafetyBackup = func(ctx context.Context) error {
		return runRestoreSafetyBackup(ctx, cfg, destination, options, dependencies.backup)
	}
	return dependencies.execute(cmd.Context(), plan, repo, runner, executeOptions)
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
	fmt.Fprintf(&output, "  冲突策略: %s（默认拒绝覆盖；真实恢复需显式 --force）\n", plan.ConflictPolicy)

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
