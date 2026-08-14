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
	"github.com/silentflower/ark/internal/verify"
)

type verifyCommandOptions struct {
	configPath    string
	hostName      string
	snapshot      string
	keepOnFailure bool
	asJSON        bool
}

type verifyDependencies struct {
	loadConfig     func(string) (*config.Config, error)
	acquireLock    func(string) (io.Closer, error)
	runLocalDoctor runLocalFunc
	runHostDoctor  runHostFunc
	newRepo        func(*config.Repo) (*restic.Repo, error)
	loadManifest   func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error)
	loadLatest     func(context.Context, *restic.Repo, []string) (backup.LatestManifestSelections, bool, error)
	newRunner      func(*config.Host) (sshexec.Runner, error)
	openStore      func(context.Context, string) (*store.Store, error)
	closeStore     func(*store.Store) error
	execute        func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, *store.Store, verify.Options) (verify.Result, error)
	recordFailure  func(context.Context, *store.Store, verify.Failure) (verify.Result, error)
	statePath      string
}

type verifyCommandSummary struct {
	ManifestSnapshotID string          `json:"manifest_snapshot_id"`
	Status             store.Status    `json:"status"`
	Results            []verify.Result `json:"results"`
	Error              string          `json:"error,omitempty"`
}

type verifyHostSelection struct {
	host     *config.Host
	manifest backup.Manifest
	snapshot restic.Snapshot
}

type verifySelectionResult struct {
	latestSnapshotID string
	hosts            []verifyHostSelection
	failures         []verify.Failure
}

// newVerifyCmd 构建原 host 隔离恢复演练命令。
func newVerifyCmd(configPath *string) *cobra.Command {
	return newVerifyCmdWithDependencies(configPath, defaultVerifyDependencies())
}

func defaultVerifyDependencies() verifyDependencies {
	return verifyDependencies{
		loadConfig:     config.LoadAndValidate,
		acquireLock:    acquireBackupLock,
		runLocalDoctor: doctor.RunLocal,
		runHostDoctor:  doctor.RunHost,
		newRepo:        restic.New,
		loadManifest:   backup.LoadManifestSelection,
		loadLatest:     backup.LoadLatestManifestSelections,
		newRunner:      backupRunnerForHost,
		openStore:      store.Open,
		closeStore: func(state *store.Store) error {
			return state.Close()
		},
		execute:       verify.Execute,
		recordFailure: verify.RecordFailure,
		statePath:     store.DefaultPath,
	}
}

func newVerifyCmdWithDependencies(configPath *string, dependencies verifyDependencies) *cobra.Command {
	options := verifyCommandOptions{snapshot: backup.LatestManifestSelector}
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "在原 host 执行隔离恢复演练",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.configPath = *configPath
			summary, runErr := runVerify(cmd.Context(), options, dependencies)
			if summary.Status == "" {
				return runErr
			}
			printErr := printVerifySummary(cmd, options.asJSON, summary)
			if runErr != nil || summary.Status == store.StatusFail || printErr != nil {
				return errors.Join(errVerifyFailed, runErr, printErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&options.hostName, "host", "", "只验证指定的 manifest host")
	cmd.Flags().StringVar(&options.snapshot, "snapshot", backup.LatestManifestSelector, "manifest snapshot ID 或 latest")
	cmd.Flags().BoolVar(&options.keepOnFailure, "keep-on-failure", false, "失败时仅保留已证明归属的演练资源")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出最终结果")
	return cmd
}

func runVerify(
	ctx context.Context,
	options verifyCommandOptions,
	dependencies verifyDependencies,
) (summary verifyCommandSummary, runErr error) {
	if err := validateVerifyDependencies(dependencies); err != nil {
		return summary, err
	}
	cfg, err := dependencies.loadConfig(options.configPath)
	if err != nil {
		return summary, err
	}
	if options.hostName != "" && findRestoreHost(cfg, options.hostName) == nil {
		return summary, fmt.Errorf("清单中不存在验证 host %q", options.hostName)
	}

	lock, err := dependencies.acquireLock(defaultBackupLockPath)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			summary.Status = store.StatusFail
			summary.Error = appendVerifyError(summary.Error, "释放 ark 全局锁失败")
			runErr = errors.Join(runErr, fmt.Errorf("释放 ark 全局锁失败: %w", closeErr))
		}
	}()

	repo, err := dependencies.newRepo(&cfg.Repo)
	if err != nil {
		return summary, fmt.Errorf("打开 restic 仓库失败: %w", err)
	}
	selection, err := resolveVerifySelections(ctx, cfg, repo, options, dependencies)
	if err != nil {
		return summary, err
	}
	summary.ManifestSnapshotID = selection.latestSnapshotID

	state, err := dependencies.openStore(ctx, dependencies.statePath)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := dependencies.closeStore(state); closeErr != nil {
			summary.Status = store.StatusFail
			summary.Error = appendVerifyError(summary.Error, "关闭状态库失败")
			runErr = errors.Join(runErr, closeErr)
		}
	}()

	var causes []error
	for _, failure := range selection.failures {
		result, recordErr := dependencies.recordFailure(ctx, state, failure)
		summary.Results = append(summary.Results, result)
		causes = append(causes, fmt.Errorf("host %q 恢复演练前置失败: %s", failure.Host, failure.Error), recordErr)
	}
	if len(selection.hosts) == 0 {
		summary.Status, summary.Error = summarizeVerifyResults(summary.Results, len(causes) > 0)
		if len(causes) == 0 {
			return summary, fmt.Errorf("没有可执行的恢复演练 host")
		}
		return summary, errors.Join(causes...)
	}

	localDoctorErr := requireDoctor("本地", dependencies.runLocalDoctor(ctx, cfg))
	for _, selected := range selection.hosts {
		host := selected.host
		manifest := selected.manifest
		snapshot := selected.snapshot
		targets := verifyTargetEvidence(manifest, host.Host)
		if ctx.Err() != nil {
			causes = append(causes, ctx.Err())
			break
		}
		if localDoctorErr != nil {
			result, recordErr := dependencies.recordFailure(ctx, state, verify.Failure{
				Host: host.Host, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
				Targets: targets, Error: "本地环境检查未通过",
			})
			summary.Results = append(summary.Results, result)
			causes = append(causes, localDoctorErr, recordErr)
			continue
		}

		plan, planErr := restore.BuildPlan(cfg, manifest, snapshot.ID, host.Host, host.Host)
		if planErr != nil {
			result, recordErr := dependencies.recordFailure(ctx, state, verify.Failure{
				Host: host.Host, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
				Targets: targets, Error: "manifest 与当前清单无法构建完整恢复计划",
			})
			summary.Results = append(summary.Results, result)
			causes = append(causes, planErr, recordErr)
			continue
		}
		if doctorErr := requireDoctor("验证 host", dependencies.runHostDoctor(ctx, cfg, host)); doctorErr != nil {
			result, recordErr := dependencies.recordFailure(ctx, state, verify.Failure{
				Host: host.Host, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
				Targets: targets, Error: "host 环境检查未通过",
			})
			summary.Results = append(summary.Results, result)
			causes = append(causes, doctorErr, recordErr)
			continue
		}
		runner, runnerErr := dependencies.newRunner(host)
		if runnerErr != nil {
			result, recordErr := dependencies.recordFailure(ctx, state, verify.Failure{
				Host: host.Host, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
				Targets: targets, Error: "创建 host 执行器失败",
			})
			summary.Results = append(summary.Results, result)
			causes = append(causes, runnerErr, recordErr)
			continue
		}
		result, executeErr := dependencies.execute(ctx, plan, repo, runner, state, verify.Options{
			KeepOnFailure:  options.keepOnFailure,
			RawFileTargets: restoreRawFileTargets(*host, *host, dependencies.statePath),
		})
		summary.Results = append(summary.Results, result)
		if executeErr != nil {
			causes = append(causes, executeErr)
		} else if result.Status == store.StatusFail {
			causes = append(causes, fmt.Errorf("host %q 恢复演练返回失败状态", host.Host))
		}
	}

	summary.Status, summary.Error = summarizeVerifyResults(summary.Results, len(causes) > 0)
	return summary, errors.Join(causes...)
}

func validateVerifyDependencies(dependencies verifyDependencies) error {
	if dependencies.loadConfig == nil || dependencies.acquireLock == nil || dependencies.runLocalDoctor == nil ||
		dependencies.runHostDoctor == nil || dependencies.newRepo == nil || dependencies.loadManifest == nil ||
		dependencies.loadLatest == nil ||
		dependencies.newRunner == nil || dependencies.openStore == nil || dependencies.closeStore == nil ||
		dependencies.execute == nil || dependencies.recordFailure == nil || strings.TrimSpace(dependencies.statePath) == "" {
		return fmt.Errorf("执行 verify 失败: 内部依赖不完整")
	}
	return nil
}

func resolveVerifySelections(
	ctx context.Context,
	cfg *config.Config,
	repo *restic.Repo,
	options verifyCommandOptions,
	dependencies verifyDependencies,
) (verifySelectionResult, error) {
	if strings.TrimSpace(options.snapshot) != backup.LatestManifestSelector {
		return resolveExplicitVerifySelection(ctx, cfg, repo, options, dependencies)
	}
	hostNames := make([]string, 0, len(cfg.Hosts))
	if options.hostName != "" {
		hostNames = append(hostNames, options.hostName)
	} else {
		for _, host := range cfg.Hosts {
			hostNames = append(hostNames, host.Host)
		}
	}
	selections, found, err := dependencies.loadLatest(ctx, repo, hostNames)
	if err != nil {
		return verifySelectionResult{}, err
	}
	if !found {
		return verifySelectionResult{}, fmt.Errorf("restic 仓库中不存在 manifest 快照")
	}
	result := verifySelectionResult{latestSnapshotID: selections.Latest.Snapshot.ID}
	for index := range cfg.Hosts {
		host := &cfg.Hosts[index]
		if options.hostName != "" && host.Host != options.hostName {
			continue
		}
		selection, selected := selections.ByHost[host.Host]
		if !selected {
			result.failures = append(result.failures, verify.Failure{
				Host: host.Host, RunID: selections.Latest.Manifest.RunID,
				ManifestSnapshotID: selections.Latest.Snapshot.ID,
				Error:              "restic 仓库中不存在该 host 的 manifest",
			})
			continue
		}
		result.hosts = append(result.hosts, verifyHostSelection{
			host: host, manifest: selection.Manifest, snapshot: selection.Snapshot,
		})
	}
	if options.hostName == "" {
		result.failures = append(result.failures, removedHostFailures(cfg, selections.Latest, result.hosts)...)
	}
	return result, nil
}

func removedHostFailures(
	cfg *config.Config,
	latest backup.ManifestSelection,
	hosts []verifyHostSelection,
) []verify.Failure {
	candidates := make(map[string]backup.ManifestSelection)
	addSelection := func(selection backup.ManifestSelection) {
		for _, hostName := range unmatchedManifestHosts(cfg, selection.Manifest) {
			current, exists := candidates[hostName]
			if !exists || manifestSnapshotNewer(selection.Snapshot, current.Snapshot) {
				candidates[hostName] = selection
			}
		}
	}
	addSelection(latest)
	for _, host := range hosts {
		addSelection(backup.ManifestSelection{Manifest: host.manifest, Snapshot: host.snapshot})
	}
	hostNames := make([]string, 0, len(candidates))
	for hostName := range candidates {
		hostNames = append(hostNames, hostName)
	}
	sort.Strings(hostNames)
	result := make([]verify.Failure, 0, len(hostNames))
	for _, hostName := range hostNames {
		selection := candidates[hostName]
		result = append(result, verify.Failure{
			Host: hostName, RunID: selection.Manifest.RunID, ManifestSnapshotID: selection.Snapshot.ID,
			Targets: verifyTargetEvidence(selection.Manifest, hostName), Error: "manifest host 已不在当前清单",
		})
	}
	return result
}

func manifestSnapshotNewer(left restic.Snapshot, right restic.Snapshot) bool {
	if left.Time.Equal(right.Time) {
		return left.ID > right.ID
	}
	return left.Time.After(right.Time)
}

func resolveExplicitVerifySelection(
	ctx context.Context,
	cfg *config.Config,
	repo *restic.Repo,
	options verifyCommandOptions,
	dependencies verifyDependencies,
) (verifySelectionResult, error) {
	manifest, snapshot, found, err := dependencies.loadManifest(ctx, repo, options.snapshot)
	if err != nil {
		return verifySelectionResult{}, err
	}
	if !found {
		return verifySelectionResult{}, fmt.Errorf("restic 仓库中不存在 manifest 快照")
	}
	result := verifySelectionResult{latestSnapshotID: snapshot.ID}
	hosts, selectionErr := selectVerifyHosts(cfg, manifest, options.hostName)
	if selectionErr != nil {
		if options.hostName == "" {
			return verifySelectionResult{}, selectionErr
		}
		result.failures = append(result.failures, verify.Failure{
			Host: options.hostName, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
			Targets: verifyTargetEvidence(manifest, options.hostName),
			Error:   "所选 manifest 不包含该 host",
		})
		return result, nil
	}
	for _, host := range hosts {
		result.hosts = append(result.hosts, verifyHostSelection{host: host, manifest: manifest, snapshot: snapshot})
	}
	for _, hostName := range unmatchedManifestHosts(cfg, manifest) {
		result.failures = append(result.failures, verify.Failure{
			Host: hostName, RunID: manifest.RunID, ManifestSnapshotID: snapshot.ID,
			Targets: verifyTargetEvidence(manifest, hostName), Error: "manifest host 已不在当前清单",
		})
	}
	return result, nil
}

func verifyTargetEvidence(manifest backup.Manifest, hostName string) []verify.TargetEvidence {
	seen := make(map[string]struct{})
	var result []verify.TargetEvidence
	for _, host := range manifest.Hosts {
		if host.Host != hostName {
			continue
		}
		for _, target := range host.Targets {
			if target.SnapshotID == "" {
				continue
			}
			key := target.TargetID + "\x00" + target.SnapshotID
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, verify.TargetEvidence{
				TargetID: target.TargetID, TargetType: string(target.TargetType), SnapshotID: target.SnapshotID,
			})
		}
	}
	return result
}

func selectVerifyHosts(cfg *config.Config, manifest backup.Manifest, hostName string) ([]*config.Host, error) {
	manifestHosts := make(map[string]struct{}, len(manifest.Hosts))
	for _, host := range manifest.Hosts {
		manifestHosts[host.Host] = struct{}{}
	}
	if hostName != "" {
		host := findRestoreHost(cfg, hostName)
		if host == nil {
			return nil, fmt.Errorf("清单中不存在验证 host %q", hostName)
		}
		if _, found := manifestHosts[hostName]; !found {
			return nil, fmt.Errorf("所选 manifest 中不存在 host %q", hostName)
		}
		return []*config.Host{host}, nil
	}
	hosts := make([]*config.Host, 0, len(cfg.Hosts))
	for index := range cfg.Hosts {
		if _, found := manifestHosts[cfg.Hosts[index].Host]; found {
			hosts = append(hosts, &cfg.Hosts[index])
		}
	}
	return hosts, nil
}

func unmatchedManifestHosts(cfg *config.Config, manifest backup.Manifest) []string {
	if cfg == nil {
		return nil
	}
	configured := make(map[string]struct{}, len(cfg.Hosts))
	for _, host := range cfg.Hosts {
		configured[host.Host] = struct{}{}
	}
	var result []string
	for _, host := range manifest.Hosts {
		if _, found := configured[host.Host]; !found {
			result = append(result, host.Host)
		}
	}
	return result
}

func summarizeVerifyResults(results []verify.Result, forcedFailure bool) (store.Status, string) {
	status := store.StatusOK
	var safeErrors []string
	for _, result := range results {
		switch result.Status {
		case store.StatusFail:
			status = store.StatusFail
		case store.StatusWarn:
			if status == store.StatusOK {
				status = store.StatusWarn
			}
		}
		if result.Error != "" {
			safeErrors = append(safeErrors, fmt.Sprintf("host %s: %s", result.Host, result.Error))
		}
	}
	if forcedFailure {
		status = store.StatusFail
	}
	return status, strings.Join(safeErrors, "；")
}

func appendVerifyError(current string, next string) string {
	if current == "" {
		return next
	}
	return current + "；" + next
}

func printVerifySummary(cmd *cobra.Command, asJSON bool, summary verifyCommandSummary) error {
	if asJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "恢复演练: %s\n", summary.Status)
	fmt.Fprintf(out, "  manifest snapshot: %s\n", summary.ManifestSnapshotID)
	for _, result := range summary.Results {
		fmt.Fprintf(out, "  host %s: %s\n", result.Host, result.Status)
		fmt.Fprintf(out, "    verification ID: %s\n", result.ID)
		fmt.Fprintf(out, "    run: %s\n", result.RunID)
		fmt.Fprintf(out, "    manifest snapshot: %s\n", result.ManifestSnapshotID)
		if result.Restore.Isolation != nil {
			fmt.Fprintf(out, "    compose project: %s\n", result.Restore.Isolation.ProjectName)
		}
		if result.Cleanup != nil {
			fmt.Fprintf(out, "    cleanup: %s\n", result.Cleanup.Status)
		}
		if result.KeptOwnership != nil {
			fmt.Fprintf(out, "    已保留: %s\n", result.KeptOwnership.ProjectName)
			fmt.Fprintf(out, "    清理命令: %s\n", result.KeptOwnership.CleanupCommand)
		}
		if len(result.Baseline.Differences) > 0 {
			fmt.Fprintf(out, "    生产基线差异: %s\n", strings.Join(result.Baseline.Differences, ", "))
		}
		if result.Error != "" {
			fmt.Fprintf(out, "    失败阶段: %s\n", result.Error)
		}
	}
	if summary.Error != "" {
		fmt.Fprintf(out, "  演练问题: %s\n", summary.Error)
	}
	return nil
}
