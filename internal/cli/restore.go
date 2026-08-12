package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
)

type restoreCommandOptions struct {
	configPath      string
	sourceHost      string
	destinationHost string
	snapshot        string
	dryRun          bool
	asJSON          bool
}

type restoreDependencies struct {
	loadConfig   func(string) (*config.Config, error)
	newRepo      func(*config.Repo) (*restic.Repo, error)
	loadManifest func(context.Context, *restic.Repo, string) (backup.Manifest, restic.Snapshot, bool, error)
}

// newRestoreCmd 构建恢复计划命令；真实恢复执行由 P3-2 在相同入口上扩展。
func newRestoreCmd(configPath *string) *cobra.Command {
	return newRestoreCmdWithDependencies(configPath, restoreDependencies{
		loadConfig:   config.LoadAndValidate,
		newRepo:      restic.New,
		loadManifest: backup.LoadManifestSelection,
	})
}

func newRestoreCmdWithDependencies(
	configPath *string,
	dependencies restoreDependencies,
) *cobra.Command {
	options := restoreCommandOptions{snapshot: backup.LatestManifestSelector}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "生成恢复计划",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(options.sourceHost) == "" {
				return fmt.Errorf("--host 不能为空")
			}
			if !options.dryRun {
				return fmt.Errorf("当前 restore 仅支持 --dry-run；真实恢复将在 P3-2 提供")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.configPath = *configPath
			plan, err := buildRestoreDryRun(cmd.Context(), options, dependencies)
			if err != nil {
				return err
			}
			if options.asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(plan)
			}
			printRestorePlan(cmd, plan)
			return nil
		},
	}
	cmd.Flags().StringVar(&options.sourceHost, "host", "", "manifest 中的备份来源 host")
	cmd.Flags().StringVar(&options.destinationHost, "to", "", "当前清单中的恢复目标 host，默认与来源相同")
	cmd.Flags().StringVar(&options.snapshot, "snapshot", backup.LatestManifestSelector, "manifest snapshot ID 或 latest")
	cmd.Flags().BoolVar(&options.dryRun, "dry-run", false, "只读取 manifest 并输出恢复计划")
	cmd.Flags().BoolVar(&options.asJSON, "json", false, "以纯 JSON 输出恢复计划")
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

func printRestorePlan(cmd *cobra.Command, plan restore.Plan) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "恢复计划")
	fmt.Fprintf(out, "  manifest snapshot: %s\n", plan.ManifestSnapshotID)
	fmt.Fprintf(out, "  backup run: %s\n", plan.RunID)
	fmt.Fprintf(out, "  来源: %s\n", plan.SourceHost)
	fmt.Fprintf(out, "  目标: %s\n", plan.DestinationHost)
	fmt.Fprintf(out, "  项目: %s\n", plan.Project.Name)
	fmt.Fprintf(out, "  compose: %s\n", plan.Project.ComposeFile)
	if plan.Project.EnvFile != "" {
		fmt.Fprintf(out, "  env: %s\n", plan.Project.EnvFile)
	}
	if plan.Project.ProjectName != "" {
		fmt.Fprintf(out, "  compose project: %s\n", plan.Project.ProjectName)
	}
	fmt.Fprintf(out, "  冲突策略: %s（默认拒绝覆盖；真实恢复需显式 --force）\n", plan.ConflictPolicy)

	var current restore.Phase
	for _, step := range plan.Steps {
		if step.Phase != current {
			current = step.Phase
			fmt.Fprintf(out, "\n阶段 %s\n", restorePhaseLabel(current))
		}
		printRestoreStep(cmd, step)
	}

	fmt.Fprintln(out, "\n人工确认")
	for _, item := range plan.ManualChecks {
		fmt.Fprintf(out, "  - %s\n", item)
	}
}

func printRestoreStep(cmd *cobra.Command, step restore.Step) {
	out := cmd.OutOrStdout()
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
		printRestoreTargetConfig(cmd, *step.Target)
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

func printRestoreTargetConfig(cmd *cobra.Command, target restore.Target) {
	out := cmd.OutOrStdout()
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
