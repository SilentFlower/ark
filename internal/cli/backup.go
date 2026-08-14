package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

const (
	defaultBackupLockPath = "/run/ark.lock"
	backupCleanupTimeout  = 10 * time.Second
)

type backupCommandOptions struct {
	configPath string
	hostName   string
	dryRun     bool
	skipDoctor bool
	asJSON     bool
}

type backupDependencies struct {
	loadConfig      func(string) (*config.Config, error)
	acquireLock     func(string) (io.Closer, error)
	runLocalDoctor  runLocalFunc
	runHostDoctor   runHostFunc
	openStore       func(context.Context, string) (*store.Store, error)
	closeStore      func(*store.Store) error
	createRun       func(context.Context, *store.Store, store.Run) error
	finishRun       func(context.Context, *store.Store, string, store.RunResult) error
	recordRunTarget func(context.Context, *store.Store, store.RunTarget) error
	newRepo         func(*config.Repo) (*restic.Repo, error)
	ensureRepo      func(context.Context, *restic.Repo) error
	newRunner       func(*config.Host) (sshexec.Runner, error)
	executeTarget   func(context.Context, config.Host, config.Target, sshexec.Runner) (*backup.Result, error)
	exportState     func(context.Context, *store.Store) (io.ReadCloser, error)
	backupTarget    func(context.Context, string, *backup.Result, *restic.Repo, *store.Store) (backup.TargetResult, error)
	saveManifest    func(context.Context, *restic.Repo, backup.Manifest) (restic.Snapshot, error)
	forgetPolicy    func(context.Context, *restic.Repo, config.Retention, []string) error
	prune           func(context.Context, *restic.Repo) error
	now             func() time.Time
	newRunID        func(time.Time) (string, error)
	statePath       string
}

type backupPlan struct {
	ConfigPath string           `json:"config_path"`
	Hosts      []backupPlanHost `json:"hosts"`
	Manifest   backupPlanFile   `json:"manifest"`
}

type backupPlanHost struct {
	Host      string              `json:"host"`
	Targets   []backupPlanTarget  `json:"targets"`
	Retention backupPlanRetention `json:"retention"`
}

type backupPlanRetention struct {
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
}

type backupPlanTarget struct {
	ID       string            `json:"id"`
	Type     config.TargetType `json:"type"`
	Filename string            `json:"filename"`
	Tags     []string          `json:"tags"`
}

type backupPlanFile struct {
	Filename string   `json:"filename"`
	Tags     []string `json:"tags"`
}

type backupRunSummary struct {
	RunID              string           `json:"run_id"`
	Status             store.Status     `json:"status"`
	Manifest           *backup.Manifest `json:"manifest,omitempty"`
	ManifestSnapshotID string           `json:"manifest_snapshot_id"`
	Error              string           `json:"error"`
}

type backupFailureSet struct {
	causes   []error
	safe     []string
	warnings bool
}

type fileLock struct {
	file *os.File
}

// newBackupCmd 构建完整备份命令。
func newBackupCmd(configPath *string) *cobra.Command {
	return newBackupCmdWithDependencies(configPath, defaultBackupDependencies())
}

func newBackupCmdWithDependencies(configPath *string, dependencies backupDependencies) *cobra.Command {
	options := backupCommandOptions{}
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "执行一次完整备份",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.configPath = *configPath
			cfg, hosts, err := loadBackupSelection(options, dependencies)
			if err != nil {
				return err
			}
			if options.dryRun {
				plan := buildBackupPlan(cfg, hosts, dependencies.statePath)
				return printBackupValue(cmd, options.asJSON, plan, printBackupPlan)
			}

			summary, runErr := runBackup(cmd.Context(), cfg, hosts, options, dependencies)
			if summary.Status == "" {
				return runErr
			}
			if printErr := printBackupValue(cmd, options.asJSON, summary, printBackupSummary); printErr != nil {
				return errors.Join(runErr, printErr)
			}
			if runErr != nil {
				return errors.Join(errBackupFailed, runErr)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&options.hostName, "host", "", "只备份指定 host")
	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "只打印脱敏计划，不执行任何外部操作")
	cmd.Flags().BoolVar(&options.skipDoctor, "skip-doctor", false, "应急跳过本地和 host 环境检查")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出结果")
	return cmd
}

func defaultBackupDependencies() backupDependencies {
	return backupDependencies{
		loadConfig:     config.LoadAndValidate,
		acquireLock:    acquireBackupLock,
		runLocalDoctor: doctor.RunLocal,
		runHostDoctor:  doctor.RunHost,
		openStore:      store.Open,
		closeStore: func(state *store.Store) error {
			return state.Close()
		},
		createRun: func(ctx context.Context, state *store.Store, run store.Run) error {
			return state.CreateRun(ctx, run)
		},
		finishRun: func(ctx context.Context, state *store.Store, id string, result store.RunResult) error {
			return state.FinishRun(ctx, id, result)
		},
		recordRunTarget: func(ctx context.Context, state *store.Store, target store.RunTarget) error {
			return state.RecordRunTarget(ctx, target)
		},
		newRepo:    restic.New,
		ensureRepo: func(ctx context.Context, repo *restic.Repo) error { return repo.EnsureInit(ctx) },
		newRunner:  backupRunnerForHost,
		executeTarget: func(
			ctx context.Context,
			host config.Host,
			target config.Target,
			runner sshexec.Runner,
		) (*backup.Result, error) {
			return backup.Execute(ctx, host, target, runner)
		},
		exportState: func(ctx context.Context, state *store.Store) (io.ReadCloser, error) {
			return state.ExportSnapshot(ctx)
		},
		backupTarget: backup.BackupTarget,
		saveManifest: backup.SaveManifest,
		forgetPolicy: func(
			ctx context.Context,
			repo *restic.Repo,
			policy config.Retention,
			tags []string,
		) error {
			return repo.ForgetPolicy(ctx, policy, tags)
		},
		prune:     func(ctx context.Context, repo *restic.Repo) error { return repo.Prune(ctx) },
		now:       time.Now,
		newRunID:  generateRunID,
		statePath: store.DefaultPath,
	}
}

func loadBackupSelection(
	options backupCommandOptions,
	dependencies backupDependencies,
) (*config.Config, []*config.Host, error) {
	if dependencies.loadConfig == nil {
		return nil, nil, fmt.Errorf("执行 backup 失败: 清单加载依赖为空")
	}
	cfg, err := dependencies.loadConfig(options.configPath)
	if err != nil {
		return nil, nil, err
	}
	hosts, err := selectBackupHosts(cfg, options.hostName)
	if err != nil {
		return nil, nil, err
	}
	if err := validateStateDatabaseTargets(hosts, dependencies.statePath); err != nil {
		return nil, nil, err
	}
	return cfg, hosts, nil
}

func runBackup(
	ctx context.Context,
	cfg *config.Config,
	hosts []*config.Host,
	options backupCommandOptions,
	dependencies backupDependencies,
) (summary backupRunSummary, runErr error) {
	if err := validateBackupDependencies(dependencies); err != nil {
		return summary, err
	}
	if err := validateStateDatabaseTargets(hosts, dependencies.statePath); err != nil {
		return summary, err
	}
	lock, err := dependencies.acquireLock(defaultBackupLockPath)
	if err != nil {
		return summary, err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			summary.Status = store.StatusFail
			summary.Error = appendSafeSummary(summary.Error, "释放备份锁失败")
			runErr = errors.Join(runErr, fmt.Errorf("释放备份锁失败: %w", closeErr))
		}
	}()
	return runBackupLocked(ctx, cfg, hosts, options, dependencies)
}

// runBackupLocked 执行已由调用方持有全局 ark 锁的完整备份编排。
// restore 的破坏前备份必须复用这条边界，避免在同一进程内再次获取非重入文件锁。
func runBackupLocked(
	ctx context.Context,
	cfg *config.Config,
	hosts []*config.Host,
	options backupCommandOptions,
	dependencies backupDependencies,
) (summary backupRunSummary, runErr error) {
	if err := validateBackupDependencies(dependencies); err != nil {
		return summary, err
	}
	if err := validateStateDatabaseTargets(hosts, dependencies.statePath); err != nil {
		return summary, err
	}
	failures := &backupFailureSet{}
	if !options.skipDoctor {
		report := dependencies.runLocalDoctor(ctx, cfg)
		if failureNames := doctorFailureNames(report); len(failureNames) > 0 {
			return summary, fmt.Errorf(
				"本地 doctor 未通过（失败项: %s），备份已在创建快照前中止",
				strings.Join(failureNames, ", "),
			)
		}
		if reportHasWarnings(report) {
			failures.warnings = true
		}
	}

	startedAt := dependencies.now().UTC()
	runID, err := dependencies.newRunID(startedAt)
	if err != nil {
		return summary, fmt.Errorf("生成 backup run ID 失败: %w", err)
	}
	summary.RunID = runID

	state, err := dependencies.openStore(ctx, dependencies.statePath)
	if err != nil {
		return summary, fmt.Errorf("打开 backup 状态库失败: %w", err)
	}
	defer func() {
		if closeErr := dependencies.closeStore(state); closeErr != nil {
			summary.Status = store.StatusFail
			summary.Error = appendSafeSummary(summary.Error, "关闭状态库失败")
			runErr = errors.Join(runErr, closeErr)
		}
	}()
	if err := dependencies.createRun(ctx, state, store.Run{
		ID:         runID,
		Status:     store.StatusRunning,
		StartedAt:  startedAt,
		ArkVersion: Version,
		RequestedHost: func() string {
			return options.hostName
		}(),
	}); err != nil {
		return summary, err
	}

	repo, err := dependencies.newRepo(&cfg.Repo)
	if err != nil {
		failures.add(fmt.Errorf("创建 restic repo 失败: %w", err), "创建 restic repo 失败")
		return finishBackupRun(ctx, state, startedAt, summary, failures, dependencies)
	}
	if err := dependencies.ensureRepo(ctx, repo); err != nil {
		failures.add(fmt.Errorf("初始化 restic repo 失败: %w", err), "初始化 restic repo 失败")
		return finishBackupRun(ctx, state, startedAt, summary, failures, dependencies)
	}

	manifest := backup.Manifest{
		SchemaVersion: backup.ManifestSchemaVersion,
		RunID:         runID,
		ArkVersion:    Version,
		StartedAt:     startedAt,
		Hosts:         make([]backup.ManifestHost, 0, len(hosts)),
	}
	retentionHosts := make([]*config.Host, 0, len(hosts))
	for _, host := range hosts {
		hostResult, hostCanPrune := runBackupHost(
			ctx,
			cfg,
			host,
			options.skipDoctor,
			runID,
			repo,
			state,
			failures,
			dependencies,
		)
		manifest.Hosts = append(manifest.Hosts, hostResult)
		if hostCanPrune {
			retentionHosts = append(retentionHosts, host)
		}
	}

	manifest.FinishedAt = dependencies.now().UTC()
	if manifest.FinishedAt.Before(manifest.StartedAt) {
		manifest.FinishedAt = manifest.StartedAt
	}
	summary.Manifest = &manifest
	manifestSnapshot, err := dependencies.saveManifest(ctx, repo, manifest)
	if err != nil {
		failures.add(fmt.Errorf("保存 manifest 失败: %w", err), "保存 manifest 失败")
		return finishBackupRun(ctx, state, startedAt, summary, failures, dependencies)
	}
	summary.ManifestSnapshotID = manifestSnapshot.ID

	for _, host := range retentionHosts {
		if err := dependencies.forgetPolicy(
			ctx,
			repo,
			cfg.RetentionFor(host),
			[]string{"host:" + host.Host},
		); err != nil {
			failures.add(
				fmt.Errorf("应用 host %q 保留策略失败: %w", host.Host, err),
				fmt.Sprintf("host %q 保留策略失败", host.Host),
			)
		}
	}
	if err := dependencies.forgetPolicy(
		ctx,
		repo,
		cfg.RetentionFor(nil),
		[]string{backup.ManifestTag},
	); err != nil {
		failures.add(fmt.Errorf("应用 manifest 保留策略失败: %w", err), "manifest 保留策略失败")
	}
	// 所有 host 的 forget 都结束后只执行一次 prune，避免反复争抢仓库排他锁。
	if err := dependencies.prune(ctx, repo); err != nil {
		failures.add(fmt.Errorf("统一 prune 失败: %w", err), "统一 prune 失败")
	}

	return finishBackupRun(ctx, state, startedAt, summary, failures, dependencies)
}

func runBackupHost(
	ctx context.Context,
	cfg *config.Config,
	host *config.Host,
	skipDoctor bool,
	runID string,
	repo *restic.Repo,
	state *store.Store,
	failures *backupFailureSet,
	dependencies backupDependencies,
) (backup.ManifestHost, bool) {
	manifestHost := backup.ManifestHost{Host: host.Host, Targets: make([]backup.TargetResult, 0, len(host.Targets))}
	if !skipDoctor {
		report := dependencies.runHostDoctor(ctx, cfg, host)
		if failureNames := doctorFailureNames(report); len(failureNames) > 0 {
			cause := fmt.Errorf(
				"host %q doctor 未通过（失败项: %s）",
				host.Host,
				strings.Join(failureNames, ", "),
			)
			failures.add(cause, cause.Error())
			return recordSkippedTargets(
				ctx, runID, host, "host doctor 未通过，未执行", state, manifestHost, failures, dependencies,
			), false
		}
		if reportHasWarnings(report) {
			failures.warnings = true
		}
	}

	runner, err := dependencies.newRunner(host)
	if err != nil {
		failures.add(fmt.Errorf("创建 host %q 执行器失败: %w", host.Host, err),
			fmt.Sprintf("host %q 执行器创建失败", host.Host))
		return recordSkippedTargets(
			ctx, runID, host, "host 执行器创建失败，未执行", state, manifestHost, failures, dependencies,
		), false
	}

	hostFailed := false
	for _, target := range host.Targets {
		source, err := executeBackupSource(ctx, *host, target, runner, state, dependencies)
		if err != nil {
			hostFailed = true
			result := backup.TargetResult{
				Host:       host.Host,
				TargetID:   target.ID(),
				TargetType: target.Type,
				Status:     store.StatusFail,
				Error:      fmt.Sprintf("target %q 失败: 启动数据流失败", target.ID()),
			}
			if recordErr := recordSyntheticTarget(ctx, runID, state, result, dependencies); recordErr != nil {
				result.Error += "；最终结果持久化失败"
				failures.add(recordErr, fmt.Sprintf("target %q 最终结果持久化失败", target.ID()))
			}
			manifestHost.Targets = append(manifestHost.Targets, result)
			failures.add(fmt.Errorf("执行 target %q 失败: %w", target.ID(), err), result.Error)
			continue
		}

		result, targetErr := dependencies.backupTarget(ctx, runID, source, repo, state)
		manifestHost.Targets = append(manifestHost.Targets, result)
		switch result.Status {
		case store.StatusWarn:
			failures.warnings = true
		case store.StatusFail:
			hostFailed = true
			if targetErr == nil {
				safe := result.Error
				if safe == "" {
					safe = fmt.Sprintf("target %q 失败", target.ID())
				}
				failures.add(fmt.Errorf("target %q 返回失败状态", target.ID()), safe)
			}
		}
		if targetErr != nil {
			hostFailed = true
			safe := result.Error
			if safe == "" {
				safe = fmt.Sprintf("target %q 失败", target.ID())
			}
			failures.add(targetErr, safe)
		}
	}
	return manifestHost, !hostFailed
}

func recordSkippedTargets(
	ctx context.Context,
	runID string,
	host *config.Host,
	reason string,
	state *store.Store,
	manifestHost backup.ManifestHost,
	failures *backupFailureSet,
	dependencies backupDependencies,
) backup.ManifestHost {
	for _, target := range host.Targets {
		result := backup.TargetResult{
			Host:       host.Host,
			TargetID:   target.ID(),
			TargetType: target.Type,
			Status:     store.StatusFail,
			Error:      fmt.Sprintf("target %q 失败: %s", target.ID(), reason),
		}
		if err := recordSyntheticTarget(ctx, runID, state, result, dependencies); err != nil {
			result.Error += "；最终结果持久化失败"
			failures.add(err, fmt.Sprintf("target %q 最终结果持久化失败", target.ID()))
		}
		manifestHost.Targets = append(manifestHost.Targets, result)
	}
	return manifestHost
}

func recordSyntheticTarget(
	ctx context.Context,
	runID string,
	state *store.Store,
	result backup.TargetResult,
	dependencies backupDependencies,
) error {
	recordCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		// 取消不能抹掉已经发生的 skipped/启动失败事实，沿用 target 完整性层的短时收尾边界。
		recordCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), backupCleanupTimeout)
	}
	defer cancel()
	return dependencies.recordRunTarget(recordCtx, state, store.RunTarget{
		RunID:      runID,
		Host:       result.Host,
		TargetID:   result.TargetID,
		TargetType: string(result.TargetType),
		Status:     result.Status,
		Bytes:      result.Bytes,
		Duration:   result.Duration,
		SnapshotID: result.SnapshotID,
		Error:      result.Error,
	})
}

func finishBackupRun(
	ctx context.Context,
	state *store.Store,
	startedAt time.Time,
	summary backupRunSummary,
	failures *backupFailureSet,
	dependencies backupDependencies,
) (backupRunSummary, error) {
	finishedAt := dependencies.now().UTC()
	if finishedAt.Before(startedAt) {
		finishedAt = startedAt
	}
	status := failures.status()
	safeError := strings.Join(failures.safe, "；")
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupCleanupTimeout)
	defer cancel()
	if err := dependencies.finishRun(finishCtx, state, summary.RunID, store.RunResult{
		Status:     status,
		FinishedAt: finishedAt,
		Duration:   finishedAt.Sub(startedAt),
		Error:      safeError,
	}); err != nil {
		failures.add(err, "完成 run 状态写入失败")
		status = store.StatusFail
		safeError = strings.Join(failures.safe, "；")
	}
	summary.Status = status
	summary.Error = safeError
	return summary, errors.Join(failures.causes...)
}

func (f *backupFailureSet) add(cause error, safe string) {
	if cause != nil {
		f.causes = append(f.causes, cause)
	}
	if strings.TrimSpace(safe) != "" {
		f.safe = append(f.safe, safe)
	}
}

func (f *backupFailureSet) status() store.Status {
	if len(f.causes) > 0 {
		return store.StatusFail
	}
	if f.warnings {
		return store.StatusWarn
	}
	return store.StatusOK
}

func validateBackupDependencies(dependencies backupDependencies) error {
	if dependencies.acquireLock == nil || dependencies.runLocalDoctor == nil ||
		dependencies.runHostDoctor == nil || dependencies.openStore == nil ||
		dependencies.closeStore == nil || dependencies.createRun == nil ||
		dependencies.finishRun == nil || dependencies.recordRunTarget == nil ||
		dependencies.newRepo == nil || dependencies.ensureRepo == nil ||
		dependencies.newRunner == nil || dependencies.executeTarget == nil ||
		dependencies.exportState == nil ||
		dependencies.backupTarget == nil || dependencies.saveManifest == nil ||
		dependencies.forgetPolicy == nil || dependencies.prune == nil ||
		dependencies.now == nil || dependencies.newRunID == nil ||
		strings.TrimSpace(dependencies.statePath) == "" {
		return fmt.Errorf("执行 backup 失败: 内部依赖不完整")
	}
	return nil
}

func selectBackupHosts(cfg *config.Config, hostName string) ([]*config.Host, error) {
	if cfg == nil {
		return nil, fmt.Errorf("执行 backup 失败: config 不能为空")
	}
	if hostName == "" {
		hosts := make([]*config.Host, 0, len(cfg.Hosts))
		for i := range cfg.Hosts {
			hosts = append(hosts, &cfg.Hosts[i])
		}
		return hosts, nil
	}
	for i := range cfg.Hosts {
		if cfg.Hosts[i].Host == hostName {
			return []*config.Host{&cfg.Hosts[i]}, nil
		}
	}
	return nil, fmt.Errorf("清单中不存在 host %q", hostName)
}

func buildBackupPlan(cfg *config.Config, hosts []*config.Host, statePath string) backupPlan {
	plan := backupPlan{
		ConfigPath: cfg.Path(),
		Hosts:      make([]backupPlanHost, 0, len(hosts)),
		Manifest: backupPlanFile{
			Filename: backup.ManifestFilename,
			Tags:     []string{backup.ManifestTag, "run:<运行时生成>"},
		},
	}
	for _, host := range hosts {
		retention := cfg.RetentionFor(host)
		planHost := backupPlanHost{
			Host:    host.Host,
			Targets: make([]backupPlanTarget, 0, len(host.Targets)),
			Retention: backupPlanRetention{
				Daily: retention.Daily, Weekly: retention.Weekly, Monthly: retention.Monthly,
			},
		}
		for _, target := range host.Targets {
			planHost.Targets = append(planHost.Targets, backupPlanTarget{
				ID:       target.ID(),
				Type:     target.Type,
				Filename: plannedStdinFilename(*host, target, statePath),
				Tags: []string{
					"host:" + host.Host,
					"target:" + target.ID(),
					"run:<运行时生成>",
				},
			})
		}
		plan.Hosts = append(plan.Hosts, planHost)
	}
	return plan
}

func plannedStdinFilename(host config.Host, target config.Target, statePath string) string {
	suffix := ".tar"
	switch target.Type {
	case config.TargetPostgres:
		suffix = ".sql"
	case config.TargetRedis:
		suffix = ".rdb"
	case config.TargetImageDigest:
		suffix = ".json"
	}
	if isStateDatabaseTarget(host, target, statePath) {
		suffix = ".db"
	}
	return host.Host + "/" + target.ID() + suffix
}

func executeBackupSource(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
	state *store.Store,
	dependencies backupDependencies,
) (*backup.Result, error) {
	if !isStateDatabaseTarget(host, target, dependencies.statePath) {
		return dependencies.executeTarget(ctx, host, target, runner)
	}

	reader, err := dependencies.exportState(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("在线导出 hub 状态库失败: %w", err)
	}
	return &backup.Result{
		Host:          host.Host,
		TargetID:      target.ID(),
		TargetType:    target.Type,
		StdinFilename: plannedStdinFilename(host, target, dependencies.statePath),
		Reader:        reader,
		Wait:          func() error { return nil },
	}, nil
}

func validateStateDatabaseTargets(hosts []*config.Host, statePath string) error {
	if strings.TrimSpace(statePath) == "" {
		return fmt.Errorf("执行 backup 失败: 状态库路径不能为空")
	}
	for _, host := range hosts {
		if host == nil || !host.Local {
			continue
		}
		stateTargetCount := 0
		for _, target := range host.Targets {
			conflicts := false
			for _, path := range target.Paths {
				if conflictsWithStateDatabase(path, statePath) {
					conflicts = true
				}
			}
			if !conflicts {
				continue
			}
			if !isStateDatabaseTarget(*host, target, statePath) {
				return fmt.Errorf(
					"host %q 的状态库 %q 必须作为独立 files target 备份",
					host.Host,
					statePath,
				)
			}
			stateTargetCount++
		}
		if stateTargetCount > 1 {
			return fmt.Errorf(
				"host %q 的状态库 %q 只能声明一个独立 files target",
				host.Host,
				statePath,
			)
		}
	}
	return nil
}

func isStateDatabaseTarget(host config.Host, target config.Target, statePath string) bool {
	return host.Local && target.Type == config.TargetFiles && len(target.Paths) == 1 &&
		sameCleanAbsolutePath(target.Paths[0], statePath)
}

func sameCleanAbsolutePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func conflictsWithStateDatabase(candidate, statePath string) bool {
	candidateAbsolute, candidateErr := filepath.Abs(candidate)
	stateAbsolute, stateErr := filepath.Abs(statePath)
	if candidateErr != nil || stateErr != nil {
		return false
	}
	candidateClean := filepath.Clean(candidateAbsolute)
	stateClean := filepath.Clean(stateAbsolute)
	return candidateClean == stateClean ||
		candidateClean == stateClean+"-wal" ||
		candidateClean == stateClean+"-shm" ||
		strings.HasPrefix(stateClean, candidateClean+string(filepath.Separator)) ||
		strings.HasPrefix(candidateClean, stateClean+string(filepath.Separator))
}

func printBackupValue(
	cmd *cobra.Command,
	asJSON bool,
	value any,
	printHuman func(*cobra.Command, any),
) error {
	if asJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}
	printHuman(cmd, value)
	return nil
}

func printBackupPlan(cmd *cobra.Command, value any) {
	plan := value.(backupPlan)
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "备份计划: %s\n", plan.ConfigPath)
	for _, host := range plan.Hosts {
		fmt.Fprintf(out, "  host %s，保留 daily=%d weekly=%d monthly=%d\n",
			host.Host, host.Retention.Daily, host.Retention.Weekly, host.Retention.Monthly)
		for _, target := range host.Targets {
			fmt.Fprintf(out, "    %s (%s) -> %s [%s]\n",
				target.ID, target.Type, target.Filename, strings.Join(target.Tags, ", "))
		}
	}
	fmt.Fprintf(out, "  manifest -> %s [%s]\n", plan.Manifest.Filename, strings.Join(plan.Manifest.Tags, ", "))
}

func printBackupSummary(cmd *cobra.Command, value any) {
	summary := value.(backupRunSummary)
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "备份运行 %s: %s\n", summary.RunID, summary.Status)
	if summary.Manifest == nil {
		return
	}
	for _, host := range summary.Manifest.Hosts {
		fmt.Fprintf(out, "  host %s\n", host.Host)
		for _, target := range host.Targets {
			fmt.Fprintf(out, "    %-24s %-4s bytes=%d snapshot=%s",
				target.TargetID, target.Status, target.Bytes, target.SnapshotID)
			if target.Error != "" {
				fmt.Fprintf(out, " error=%s", target.Error)
			}
			fmt.Fprintln(out)
		}
	}
	if summary.ManifestSnapshotID != "" {
		fmt.Fprintf(out, "  manifest snapshot: %s\n", summary.ManifestSnapshotID)
	}
	if summary.Error != "" {
		fmt.Fprintf(out, "  运行问题: %s\n", summary.Error)
	}
}

func backupRunnerForHost(host *config.Host) (sshexec.Runner, error) {
	if host == nil {
		return nil, fmt.Errorf("创建 host 执行器失败: host 不能为空")
	}
	if host.Local {
		return sshexec.NewLocal(), nil
	}
	if host.SSH == nil {
		return nil, fmt.Errorf("创建 host %q 执行器失败: ssh 配置为空", host.Host)
	}
	return sshexec.NewSSH(*host.SSH)
}

func acquireBackupLock(path string) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开备份锁 %s 失败: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("已有 ark backup、restore 或 verify 正在运行，未等待锁 %s", path), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("获取 ark 全局锁 %s 失败: %w", path, err), closeErr)
	}
	return &fileLock{file: file}, nil
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}

func generateRunID(startedAt time.Time) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return startedAt.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random[:]), nil
}

func appendSafeSummary(current, next string) string {
	if current == "" {
		return next
	}
	return current + "；" + next
}

func reportHasWarnings(report *doctor.Report) bool {
	if report == nil {
		return false
	}
	_, warn, _ := report.Counts()
	return warn > 0
}

// doctorFailureNames 只返回失败检查项的名称，避免把可能含外部命令输出的 Detail 写入 journal。
func doctorFailureNames(report *doctor.Report) []string {
	if report == nil {
		return []string{"doctor 报告为空"}
	}
	failures := make([]string, 0)
	for _, check := range report.Checks {
		if check.Status == doctor.StatusFail {
			failures = append(failures, check.Name)
		}
	}
	return failures
}
