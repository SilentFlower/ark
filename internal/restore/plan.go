// Package restore 把备份 manifest 与当前清单映射为可审计的纯数据恢复计划。
//
// 本包不读取文件、不访问 restic、不创建 Runner，也不执行任何目标机命令。
// 外部资源选择与真实恢复编排分别由 cli 和后续执行层负责。
package restore

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/store"
)

// Phase 是恢复步骤所属的稳定阶段。
type Phase string

const (
	// PhaseFiles 表示恢复 compose、环境文件和其它宿主机文件。
	PhaseFiles Phase = "files"
	// PhaseImageDigest 表示核对并准备备份时记录的镜像 digest。
	PhaseImageDigest Phase = "image_digest"
	// PhaseVolume 表示恢复 Docker volume 内容。
	PhaseVolume Phase = "volume"
	// PhaseDatabasePrepare 表示在写入数据前准备数据库服务。
	PhaseDatabasePrepare Phase = "database_prepare"
	// PhaseDatabaseData 表示导入 PostgreSQL 或 Redis 数据。
	PhaseDatabaseData Phase = "database_data"
	// PhaseApplication 表示启动完整应用。
	PhaseApplication Phase = "application"
	// PhaseHealth 表示执行恢复后的健康检查。
	PhaseHealth Phase = "health"
)

const defaultConflictPolicy = "refuse_existing"

var defaultManualChecks = []string{
	"暂停 dnsmgr 对目标主机的检测",
	"确认 DNS 指向目标主机",
	"确认 TLS 证书适配目标环境",
	"确认防火墙端口已放行",
	"复核 .env 中需要按新环境调整的配置",
}

// Project 是恢复时必须保持不变的 compose 项目定位副本。
type Project struct {
	// Name 是项目的逻辑名称。
	Name string `json:"name"`
	// ComposeFile 是目标机上的 compose 文件绝对路径。
	ComposeFile string `json:"compose_file"`
	// EnvFile 是 compose 使用的环境文件绝对路径，可为空。
	EnvFile string `json:"env_file,omitempty"`
	// ProjectName 是传给 docker compose 的显式项目名，可为空。
	ProjectName string `json:"project_name,omitempty"`
}

// Target 是恢复步骤使用的清单 target 配置副本。
type Target struct {
	// Type 是 target 类型。
	Type config.TargetType `json:"type"`
	// Service 是数据库 target 使用的 compose service。
	Service string `json:"service,omitempty"`
	// Database 是 PostgreSQL target 使用的数据库名。
	Database string `json:"database,omitempty"`
	// User 是 PostgreSQL target 使用的数据库用户。
	User string `json:"user,omitempty"`
	// Name 是 volume 或 files target 的名称。
	Name string `json:"name,omitempty"`
	// Paths 是 files target 的宿主机路径副本。
	Paths []string `json:"paths,omitempty"`
	// Services 是 image_digest target 的 compose service 副本。
	Services []string `json:"services,omitempty"`
}

// Step 描述一个稳定阶段中的单个恢复动作，不包含命令字符串或运行时依赖。
type Step struct {
	// Phase 是步骤所属阶段。
	Phase Phase `json:"phase"`
	// TargetID 是 config.Target.ID 返回的稳定标识；项目级步骤为空。
	TargetID string `json:"target_id,omitempty"`
	// TargetType 是 target 类型；项目级步骤为空。
	TargetType config.TargetType `json:"target_type,omitempty"`
	// SnapshotID 是该 target 的 restic 快照 ID；准备和项目级步骤可为空。
	SnapshotID string `json:"snapshot_id,omitempty"`
	// Target 是执行阶段需要的 target 配置副本；项目级步骤为空。
	Target *Target `json:"target,omitempty"`
	// ImageDigests 是 image_digest target 记录的 service 到不可变 digest 映射。
	ImageDigests map[string]string `json:"image_digests,omitempty"`
}

// Plan 是由 manifest 事实与当前清单共同确定的完整恢复计划。
type Plan struct {
	// ManifestSnapshotID 是承载本计划 manifest 的精确 restic snapshot ID。
	ManifestSnapshotID string `json:"manifest_snapshot_id"`
	// RunID 是 manifest 对应的完整备份运行 ID。
	RunID string `json:"run_id"`
	// SourceHost 是 manifest 中的备份来源 host。
	SourceHost string `json:"source_host"`
	// DestinationHost 是当前清单中的恢复目标 host。
	DestinationHost string `json:"destination_host"`
	// Project 是 source 与 destination 完全一致的 compose 项目定位。
	Project Project `json:"project"`
	// ConflictPolicy 是真实执行阶段必须遵守的默认冲突策略。
	ConflictPolicy string `json:"conflict_policy"`
	// Steps 按固定阶段和清单 target 顺序保存全部动作。
	Steps []Step `json:"steps"`
	// ManualChecks 是执行或切流前必须由管理员复核的固定事项。
	ManualChecks []string `json:"manual_checks"`
}

// BuildPlan 校验 manifest 与 source/destination 清单定义并构建稳定恢复计划。
// @param cfg 已完成静态校验的当前 ark 清单。
// @param manifest 已完成 schema 解码的备份 manifest。
// @param manifestSnapshotID 实际承载 manifest 的 restic snapshot ID。
// @param sourceHost manifest 中的备份来源 host。
// @param destinationHost 当前清单中的恢复目标 host。
// @return Plan 只包含结构化值、可稳定序列化的完整恢复计划。
// @return error 参数、host、Project、Target 或备份结果不一致时的聚合错误。
func BuildPlan(
	cfg *config.Config,
	manifest backup.Manifest,
	manifestSnapshotID string,
	sourceHost string,
	destinationHost string,
) (Plan, error) {
	if cfg == nil {
		return Plan{}, fmt.Errorf("构建恢复计划失败: config 不能为空")
	}
	if err := manifest.Validate(); err != nil {
		return Plan{}, fmt.Errorf("构建恢复计划失败: manifest 无效: %w", err)
	}
	manifestSnapshotID = strings.TrimSpace(manifestSnapshotID)
	sourceHost = strings.TrimSpace(sourceHost)
	destinationHost = strings.TrimSpace(destinationHost)
	if destinationHost == "" {
		destinationHost = sourceHost
	}

	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if manifestSnapshotID == "" {
		add("manifest_snapshot_id: 不能为空")
	}
	if sourceHost == "" {
		add("source_host: 不能为空")
	}

	source := findConfigHost(cfg, sourceHost)
	destination := findConfigHost(cfg, destinationHost)
	manifestSource, manifestSourceCount := findManifestHost(manifest, sourceHost)
	if source == nil && sourceHost != "" {
		add("source_host: 当前清单中不存在 host %q", sourceHost)
	}
	if destination == nil && destinationHost != "" {
		add("destination_host: 当前清单中不存在 host %q", destinationHost)
	}
	switch manifestSourceCount {
	case 0:
		if sourceHost != "" {
			add("source_host: manifest 中不存在 host %q", sourceHost)
		}
	case 1:
	default:
		add("source_host: manifest 中 host %q 出现 %d 次", sourceHost, manifestSourceCount)
	}
	if err := errors.Join(errs...); err != nil {
		return Plan{}, fmt.Errorf("构建恢复计划失败: %w", err)
	}

	errs = nil
	compareProjects(source.Project, destination.Project, add)
	compareTargets(source.Targets, destination.Targets, add)
	results := validateManifestTargets(*source, *manifestSource, add)
	if err := errors.Join(errs...); err != nil {
		return Plan{}, fmt.Errorf("构建恢复计划失败: %w", err)
	}

	steps := buildSteps(source.Targets, results)
	return Plan{
		ManifestSnapshotID: manifestSnapshotID,
		RunID:              manifest.RunID,
		SourceHost:         sourceHost,
		DestinationHost:    destinationHost,
		Project:            copyProject(source.Project),
		ConflictPolicy:     defaultConflictPolicy,
		Steps:              steps,
		ManualChecks:       append([]string(nil), defaultManualChecks...),
	}, nil
}

func findConfigHost(cfg *config.Config, name string) *config.Host {
	for index := range cfg.Hosts {
		if cfg.Hosts[index].Host == name {
			return &cfg.Hosts[index]
		}
	}
	return nil
}

func findManifestHost(manifest backup.Manifest, name string) (*backup.ManifestHost, int) {
	var found *backup.ManifestHost
	count := 0
	for index := range manifest.Hosts {
		if manifest.Hosts[index].Host == name {
			found = &manifest.Hosts[index]
			count++
		}
	}
	return found, count
}

func compareProjects(source config.Project, destination config.Project, add func(string, ...any)) {
	compareStringField("project.name", source.Name, destination.Name, add)
	compareStringField("project.compose_file", source.ComposeFile, destination.ComposeFile, add)
	compareStringField("project.env_file", source.EnvFile, destination.EnvFile, add)
	compareStringField("project.project_name", source.ProjectName, destination.ProjectName, add)
}

func compareTargets(source []config.Target, destination []config.Target, add func(string, ...any)) {
	destinationByID := make(map[string]config.Target, len(destination))
	for _, target := range destination {
		id := target.ID()
		if _, exists := destinationByID[id]; exists {
			add("destination targets: target %q 重复", id)
			continue
		}
		destinationByID[id] = target
	}
	seenSource := make(map[string]struct{}, len(source))
	for _, sourceTarget := range source {
		id := sourceTarget.ID()
		if _, exists := seenSource[id]; exists {
			add("source targets: target %q 重复", id)
			continue
		}
		seenSource[id] = struct{}{}
		destinationTarget, exists := destinationByID[id]
		if !exists {
			add("destination targets: 缺少 target %q", id)
			continue
		}
		compareTarget(id, sourceTarget, destinationTarget, add)
	}
	for _, destinationTarget := range destination {
		id := destinationTarget.ID()
		if _, exists := seenSource[id]; !exists {
			add("destination targets: 存在 source 未定义的 target %q", id)
		}
	}
}

func compareTarget(id string, source config.Target, destination config.Target, add func(string, ...any)) {
	compareStringField("targets["+id+"].type", string(source.Type), string(destination.Type), add)
	compareStringField("targets["+id+"].service", source.Service, destination.Service, add)
	compareStringField("targets["+id+"].database", source.Database, destination.Database, add)
	compareStringField("targets["+id+"].user", source.User, destination.User, add)
	compareStringField("targets["+id+"].name", source.Name, destination.Name, add)
	compareStringSliceField("targets["+id+"].paths", source.Paths, destination.Paths, add)
	compareStringSliceField("targets["+id+"].services", source.Services, destination.Services, add)
}

func compareStringField(field string, source string, destination string, add func(string, ...any)) {
	if source != destination {
		add("%s: source=%q destination=%q", field, source, destination)
	}
}

func compareStringSliceField(field string, source []string, destination []string, add func(string, ...any)) {
	if !slices.Equal(source, destination) {
		add("%s: source=%#v destination=%#v", field, source, destination)
	}
}

func validateManifestTargets(
	source config.Host,
	manifestSource backup.ManifestHost,
	add func(string, ...any),
) map[string]backup.TargetResult {
	results := make(map[string]backup.TargetResult, len(manifestSource.Targets))
	for _, result := range manifestSource.Targets {
		if _, exists := results[result.TargetID]; exists {
			add("manifest targets: target %q 重复", result.TargetID)
			continue
		}
		results[result.TargetID] = result
	}

	configured := make(map[string]config.Target, len(source.Targets))
	for _, target := range source.Targets {
		id := target.ID()
		configured[id] = target
		result, exists := results[id]
		if !exists {
			add("manifest targets: 缺少 target %q", id)
			continue
		}
		if result.TargetType != target.Type {
			add(
				"manifest targets[%s].type: 期望 %q，实际 %q",
				id,
				target.Type,
				result.TargetType,
			)
		}
		if result.Status == store.StatusFail {
			add("manifest targets[%s].status: target 备份失败", id)
		}
		if strings.TrimSpace(result.SnapshotID) == "" {
			add("manifest targets[%s].snapshot_id: 不能为空", id)
		}
		validateImageDigests(target, result, add)
	}
	for _, result := range manifestSource.Targets {
		if _, exists := configured[result.TargetID]; !exists {
			add("manifest targets: 存在当前 source 未定义的 target %q", result.TargetID)
		}
	}
	return results
}

func validateImageDigests(
	target config.Target,
	result backup.TargetResult,
	add func(string, ...any),
) {
	if target.Type != config.TargetImageDigest {
		if len(result.ImageDigests) != 0 {
			add("manifest targets[%s].image_digests: 非 image_digest target 不应包含 digest", target.ID())
		}
		return
	}
	expected := make(map[string]struct{}, len(target.Services))
	for _, service := range target.Services {
		expected[service] = struct{}{}
		if strings.TrimSpace(result.ImageDigests[service]) == "" {
			add("manifest targets[%s].image_digests[%s]: 缺少 digest", target.ID(), service)
		}
	}
	for service := range result.ImageDigests {
		if _, exists := expected[service]; !exists {
			add("manifest targets[%s].image_digests[%s]: service 不在当前 target 配置中", target.ID(), service)
		}
	}
}

func buildSteps(targets []config.Target, results map[string]backup.TargetResult) []Step {
	steps := make([]Step, 0, len(targets)*2+2)
	appendPhase := func(phase Phase, accepted ...config.TargetType) {
		for _, target := range targets {
			if !slices.Contains(accepted, target.Type) {
				continue
			}
			result := results[target.ID()]
			step := newTargetStep(phase, target, result)
			if phase == PhaseDatabasePrepare {
				step.SnapshotID = ""
			}
			steps = append(steps, step)
		}
	}

	// 阶段分组必须独立于清单中 target 类型的交错顺序，才能让 dry-run 与真实执行共享顺序契约。
	appendPhase(PhaseFiles, config.TargetFiles)
	appendPhase(PhaseImageDigest, config.TargetImageDigest)
	appendPhase(PhaseVolume, config.TargetVolume)
	appendPhase(PhaseDatabasePrepare, config.TargetPostgres, config.TargetRedis)
	appendPhase(PhaseDatabaseData, config.TargetPostgres, config.TargetRedis)
	steps = append(steps, Step{Phase: PhaseApplication}, Step{Phase: PhaseHealth})
	return steps
}

func newTargetStep(phase Phase, target config.Target, result backup.TargetResult) Step {
	return Step{
		Phase:        phase,
		TargetID:     target.ID(),
		TargetType:   target.Type,
		SnapshotID:   result.SnapshotID,
		Target:       copyTarget(target),
		ImageDigests: copyStringMap(result.ImageDigests),
	}
}

func copyProject(project config.Project) Project {
	return Project{
		Name:        project.Name,
		ComposeFile: project.ComposeFile,
		EnvFile:     project.EnvFile,
		ProjectName: project.ProjectName,
	}
}

func copyTarget(target config.Target) *Target {
	return &Target{
		Type:     target.Type,
		Service:  target.Service,
		Database: target.Database,
		User:     target.User,
		Name:     target.Name,
		Paths:    append([]string(nil), target.Paths...),
		Services: append([]string(nil), target.Services...),
	}
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
