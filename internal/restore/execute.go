package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

const (
	restoreMarkerBase       = "/var/lib/ark/restore"
	composeProjectLabel     = "com.docker.compose.project"
	composeVolumeLabel      = "com.docker.compose.volume"
	restoreManifestLabel    = "io.ark.restore.manifest"
	defaultRestorePollDelay = 250 * time.Millisecond
	restoreCleanupTimeout   = 10 * time.Second
)

// ExecuteOptions 控制真实恢复的覆盖授权与破坏前备份回调。
type ExecuteOptions struct {
	// Force 允许处理当前 Plan 精确声明且归属正确的冲突资源。
	Force bool
	// RawFileTargets 把源端以原始单文件保存的 files target ID 映射到目标绝对路径。
	RawFileTargets map[string]string
	// SafetyBackup 在任何破坏性步骤前备份 destination；存在冲突且 Force=true 时必填。
	SafetyBackup func(context.Context) error
	// OnPlanReady 在全部预检与可选 safety backup 成功后、首次目标写入前调用。
	OnPlanReady func(Plan) error
}

// StepResult 是一个恢复步骤的脱敏执行结果。
type StepResult struct {
	// Phase 是步骤所属阶段。
	Phase Phase `json:"phase"`
	// TargetID 是 target 步骤的稳定 ID；项目级步骤为空。
	TargetID string `json:"target_id,omitempty"`
	// Status 是 ok、skipped、warn 或 fail。
	Status string `json:"status"`
	// Detail 是不含命令输出和业务数据的阶段摘要。
	Detail string `json:"detail,omitempty"`
}

// Result 是一次真实恢复的结构化最终结果。
type Result struct {
	// ManifestSnapshotID 是本次执行使用的精确 manifest snapshot。
	ManifestSnapshotID string `json:"manifest_snapshot_id"`
	// RunID 是恢复来源 backup run ID。
	RunID string `json:"run_id"`
	// SourceHost 是备份来源 host。
	SourceHost string `json:"source_host"`
	// DestinationHost 是恢复目标 host。
	DestinationHost string `json:"destination_host"`
	// Status 是整体 ok、warn 或 fail。
	Status store.Status `json:"status"`
	// Steps 按 Plan 顺序保存已执行步骤。
	Steps []StepResult `json:"steps"`
	// ManualChecks 是恢复后仍需管理员完成的事项。
	ManualChecks []string `json:"manual_checks"`
	// Error 是脱敏后的整体失败摘要。
	Error string `json:"error,omitempty"`
	// Isolation 是隔离恢复的资源、端口和清理摘要；原位恢复为空。
	Isolation *IsolationResult `json:"isolation,omitempty"`
}

// Execute 严格按 Plan 顺序流式执行恢复，并在默认模式下拒绝未知既有资源。
// @param ctx 控制 restic、目标机命令和就绪轮询的取消。
// @param plan 已由 BuildPlan 生成并通过兼容校验的完整计划。
// @param repo 已配置的 hub restic 仓库。
// @param runner destination 的本地或 SSH Runner。
// @param options 覆盖授权和可选破坏前备份回调。
// @return Result 已完成步骤、整体状态和人工确认项。
// @return error 参数、冲突、备份、数据流、命令、健康或 digest 校验失败时的错误。
func Execute(
	ctx context.Context,
	plan Plan,
	repo *restic.Repo,
	runner sshexec.Runner,
	options ExecuteOptions,
) (Result, error) {
	if repo == nil {
		return Result{}, fmt.Errorf("执行恢复失败: restic repo 不能为空")
	}
	return execute(ctx, plan, runner, options, executeDependencies{
		dump:         repo.Dump,
		pollInterval: defaultRestorePollDelay,
	})
}

type executeDependencies struct {
	dump         func(context.Context, string, string) (io.ReadCloser, error)
	pollInterval time.Duration
}

type conflict struct {
	resource   string
	detail     string
	authorized bool
}

type preflight struct {
	resume            bool
	conflicts         []conflict
	projectContainers []composeState
}

type composeState struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

type dockerMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Destination string `json:"Destination"`
}

type composeConfig struct {
	Services map[string]json.RawMessage `json:"services"`
	Volumes  map[string]composeVolume   `json:"volumes"`
}

type composeVolume struct {
	Name string `json:"name"`
}

func execute(
	ctx context.Context,
	plan Plan,
	runner sshexec.Runner,
	options ExecuteOptions,
	dependencies executeDependencies,
) (result Result, runErr error) {
	result = newExecuteResult(plan)
	if ctx == nil {
		return failResult(result, fmt.Errorf("执行恢复失败: context 不能为空"))
	}
	if runner == nil || dependencies.dump == nil || dependencies.pollInterval <= 0 {
		return failResult(result, fmt.Errorf("执行恢复失败: 内部依赖不完整"))
	}
	if strings.TrimSpace(plan.ManifestSnapshotID) == "" || strings.TrimSpace(plan.SourceHost) == "" ||
		strings.TrimSpace(plan.DestinationHost) == "" || len(plan.Steps) == 0 {
		return failResult(result, fmt.Errorf("执行恢复失败: Plan 不完整"))
	}
	if strings.TrimSpace(plan.Project.ProjectName) == "" {
		return failResult(result, fmt.Errorf("执行恢复失败: project_name 不能为空，真实恢复不能猜测 Compose project 标签"))
	}
	if plan.Isolation != nil && options.Force {
		return failResult(result, fmt.Errorf("执行恢复失败: isolation Plan 不允许 force"))
	}
	if err := validateExecutePlan(plan); err != nil {
		return failResult(result, err)
	}
	if err := validateIsolationPlan(plan); err != nil {
		return failResult(result, err)
	}
	isolationRootExists := false
	if plan.Isolation != nil {
		var err error
		isolationRootExists, err = validateExistingIsolationRoot(ctx, plan, runner)
		if err != nil {
			return failResult(result, err)
		}
	}
	if err := validateRawFileTargets(plan, options.RawFileTargets); err != nil {
		return failResult(result, err)
	}
	executionID := executionIdentity(plan, options.RawFileTargets)
	var isolationRuntime isolationState
	if plan.Isolation != nil {
		state, found, err := loadIsolationState(ctx, plan, runner)
		if err != nil {
			return failResult(result, err)
		}
		if isolationRootExists && !found {
			return failResult(result, fmt.Errorf("隔离恢复目录已存在但缺少合法 state.json，拒绝接管"))
		}
		if found {
			if err := validateIsolationState(plan, executionID, state); err != nil {
				return failResult(result, err)
			}
			isolationRuntime = state
		}
	}

	inspection, err := inspectDestination(ctx, plan, executionID, runner)
	if err != nil {
		return failResult(result, err)
	}
	if err := authorizeConflicts(ctx, inspection, options); err != nil {
		return failResult(result, err)
	}
	if options.OnPlanReady != nil {
		if err := options.OnPlanReady(plan); err != nil {
			return failResult(result, fmt.Errorf("输出恢复计划失败: %w", err))
		}
	}
	if !inspection.resume {
		if plan.Isolation != nil {
			if err := ensureIsolationRoot(ctx, plan, runner); err != nil {
				return failResult(result, err)
			}
			isolationRuntime = newIsolationState(plan, executionID)
			if err := writeIsolationState(ctx, plan, runner, isolationRuntime); err != nil {
				return failResult(result, err)
			}
		}
		if err := writeMarker(ctx, runner, planStatePath(plan), executionID); err != nil {
			return failResult(result, fmt.Errorf("记录恢复计划状态失败: %w", err))
		}
	}
	if options.Force && len(inspection.conflicts) > 0 && len(inspection.projectContainers) > 0 {
		if err := stopProjectContainers(ctx, runner, inspection.projectContainers); err != nil {
			return failResult(result, err)
		}
	} else if inspection.resume && len(inspection.projectContainers) > 0 {
		needsStop, err := resumeRequiresStop(ctx, plan, options, runner)
		if err != nil {
			return failResult(result, err)
		}
		if needsStop {
			if err := stopProjectContainers(ctx, runner, inspection.projectContainers); err != nil {
				return failResult(result, err)
			}
		}
	}
	isolationPrepared := plan.Isolation == nil
	for _, step := range plan.Steps {
		if !isolationPrepared && step.Phase != PhaseFiles {
			isolationRuntime, err = prepareIsolation(ctx, plan, runner, executionID)
			if err != nil {
				return failResult(result, err)
			}
			isolationPrepared = true
			result.Isolation = isolationResult(plan, isolationRuntime)
		}
		stepResult, err := executeStep(ctx, plan, step, runner, options, dependencies)
		result.Steps = append(result.Steps, stepResult)
		if stepResult.Phase == PhaseHealth && stepResult.Status == "warn" && stepResult.Detail != "" {
			result.ManualChecks = append(result.ManualChecks, "复核健康检查: "+stepResult.Detail)
		}
		if err != nil {
			return failResult(result, err)
		}
		if plan.Isolation != nil && step.Phase == PhaseApplication {
			isolationRuntime, err = inspectIsolationPorts(ctx, plan, runner, isolationRuntime)
			if err != nil {
				return failResult(result, err)
			}
			result.Isolation = isolationResult(plan, isolationRuntime)
		} else if plan.Isolation != nil &&
			(step.Phase == PhaseDatabasePrepare || step.Phase == PhaseDatabaseData) {
			isolationRuntime, err = inspectIsolationContainers(ctx, plan, runner, isolationRuntime)
			if err != nil {
				return failResult(result, err)
			}
			result.Isolation = isolationResult(plan, isolationRuntime)
		}
	}
	if err := writeMarker(ctx, runner, planCompletePath(plan), executionID); err != nil {
		return failResult(result, fmt.Errorf("记录恢复完成状态失败: %w", err))
	}
	result.Status = store.StatusOK
	result.Isolation = isolationResult(plan, isolationRuntime)
	for _, step := range result.Steps {
		if step.Status == "warn" {
			result.Status = store.StatusWarn
			break
		}
	}
	return result, nil
}

func newExecuteResult(plan Plan) Result {
	result := Result{
		ManifestSnapshotID: plan.ManifestSnapshotID,
		RunID:              plan.RunID,
		SourceHost:         plan.SourceHost,
		DestinationHost:    plan.DestinationHost,
		Steps:              make([]StepResult, 0, len(plan.Steps)),
		ManualChecks:       append([]string(nil), plan.ManualChecks...),
	}
	if plan.Isolation != nil {
		result.Isolation = isolationResult(plan, isolationState{})
	}
	return result
}

func failResult(result Result, err error) (Result, error) {
	result.Status = store.StatusFail
	result.Error = "恢复未完成"
	return result, err
}

func inspectDestination(ctx context.Context, plan Plan, executionID string, runner sshexec.Runner) (preflight, error) {
	inspection := preflight{}
	state, found, err := readMarker(ctx, runner, planStatePath(plan))
	if err != nil {
		return inspection, fmt.Errorf("读取目标恢复状态失败: %w", err)
	}
	if found {
		if state == executionID {
			inspection.resume = true
		} else {
			complete, completeFound, err := readMarker(ctx, runner, planCompletePath(plan))
			if err != nil {
				return inspection, fmt.Errorf("读取目标恢复完成状态失败: %w", err)
			}
			// 已完整结束的旧 Plan 只是历史状态；只有无法证明完成的陌生 Plan 才禁止接管。
			if !completeFound || complete != state {
				inspection.conflicts = append(inspection.conflicts, conflict{
					resource: "restore_state", detail: "目标机存在另一份未完成恢复计划", authorized: false,
				})
			}
		}
	}

	projectName := effectiveProjectName(plan.Project)
	containers, err := listProjectContainers(ctx, runner, projectName)
	if err != nil {
		return inspection, err
	}
	if len(containers) > 0 {
		inspection.projectContainers = append(inspection.projectContainers, containers...)
	}
	if plan.Isolation != nil {
		for _, container := range containers {
			labels, labelErr := inspectResourceLabels(ctx, runner, "container", container.ID)
			if labelErr != nil || !isolationLabelsMatch(plan, labels) {
				inspection.conflicts = append(inspection.conflicts, conflict{
					resource: container.ID, detail: "容器 isolation 标签不匹配", authorized: false,
				})
			}
		}
	}
	if len(containers) > 0 && !inspection.resume {
		inspection.conflicts = append(inspection.conflicts, conflict{
			resource: "compose_project", detail: fmt.Sprintf("项目 %q 已有 %d 个容器", projectName, len(containers)), authorized: true,
		})
	}
	volumes, err := listProjectVolumes(ctx, runner, projectName)
	if err != nil {
		return inspection, err
	}
	if len(volumes) > 0 && !inspection.resume {
		inspection.conflicts = append(inspection.conflicts, conflict{
			resource: "compose_volumes", detail: fmt.Sprintf("项目 %q 已有 %d 个 volume", projectName, len(volumes)), authorized: true,
		})
	}
	if plan.Isolation != nil {
		for _, volumeName := range volumes {
			_, labels, labelErr := inspectVolume(ctx, runner, volumeName)
			if labelErr != nil || !isolationLabelsMatch(plan, labels) {
				inspection.conflicts = append(inspection.conflicts, conflict{
					resource: volumeName, detail: "volume isolation 标签不匹配", authorized: false,
				})
			}
		}
	}

	for _, step := range plan.Steps {
		if step.Target == nil {
			continue
		}
		switch step.TargetType {
		case config.TargetFiles:
			if inspection.resume {
				continue
			}
			for _, targetPath := range step.Target.Paths {
				exists, err := targetPathExists(ctx, runner, targetPath)
				if err != nil {
					return inspection, err
				}
				if exists {
					inspection.conflicts = append(inspection.conflicts, conflict{
						resource: targetPath, detail: "目标文件或目录已存在", authorized: true,
					})
				}
			}
		case config.TargetVolume:
			exists, labels, err := inspectVolume(ctx, runner, step.Target.Name)
			if err != nil {
				return inspection, err
			}
			if !exists {
				continue
			}
			authorized := labels[composeProjectLabel] == projectName && isolationLabelsMatch(plan, labels)
			if !inspection.resume || !authorized {
				detail := "volume 已存在"
				if !authorized {
					detail = "volume 标签不属于目标 Compose Project"
				}
				inspection.conflicts = append(inspection.conflicts, conflict{
					resource: step.Target.Name, detail: detail, authorized: authorized,
				})
			}
		}
	}
	return inspection, nil
}

func validateExecutePlan(plan Plan) error {
	imageDigestSteps := 0
	projectLifecycleSteps := 0
	for index, step := range plan.Steps {
		switch step.Phase {
		case PhaseFiles, PhaseVolume, PhaseDatabasePrepare, PhaseDatabaseData:
			if step.Target == nil || step.Target.Type != step.TargetType || strings.TrimSpace(step.TargetID) == "" {
				return fmt.Errorf("执行恢复失败: Plan steps[%d] target 不完整", index)
			}
			if step.Phase == PhaseFiles {
				for _, targetPath := range step.Target.Paths {
					if err := validateRestoreFilePathForPlan(plan, targetPath); err != nil {
						return fmt.Errorf("执行恢复失败: Plan steps[%d]: %w", index, err)
					}
				}
			}
		case PhaseImageDigest:
			imageDigestSteps++
			if step.Target == nil || step.Target.Type != config.TargetImageDigest ||
				step.TargetType != config.TargetImageDigest || len(step.ImageDigests) == 0 {
				return fmt.Errorf("执行恢复失败: Plan steps[%d] image digest 不完整", index)
			}
			for service, digest := range step.ImageDigests {
				if strings.TrimSpace(service) == "" || !validImageDigest(digest) {
					return fmt.Errorf("执行恢复失败: Plan steps[%d] service %q image digest 无效", index, service)
				}
			}
		case PhaseApplication, PhaseHealth:
			projectLifecycleSteps++
			if step.Target != nil || step.TargetID != "" || step.TargetType != "" {
				return fmt.Errorf("执行恢复失败: Plan steps[%d] 项目级步骤包含 target", index)
			}
		default:
			return fmt.Errorf("执行恢复失败: Plan steps[%d] phase %q 不受支持", index, step.Phase)
		}
	}
	if projectLifecycleSteps > 0 && imageDigestSteps != 1 {
		return fmt.Errorf("执行恢复失败: Plan 的 image digest 步骤数量为 %d，期望 1", imageDigestSteps)
	}
	return nil
}

func validateRawFileTargets(plan Plan, rawFileTargets map[string]string) error {
	if len(rawFileTargets) == 0 {
		return nil
	}
	fileSteps := make(map[string]Step)
	for _, step := range plan.Steps {
		if step.Phase == PhaseFiles {
			fileSteps[step.TargetID] = step
		}
	}
	for targetID, destinationPath := range rawFileTargets {
		step, ok := fileSteps[targetID]
		if !ok || step.Target == nil || step.TargetType != config.TargetFiles || len(step.Target.Paths) != 1 {
			return fmt.Errorf("执行恢复失败: 原始文件 target %q 不对应独立 files 步骤", targetID)
		}
		if path.Clean(destinationPath) != path.Clean(step.Target.Paths[0]) {
			return fmt.Errorf("执行恢复失败: 原始文件 target %q 的目标路径与 Plan 不一致", targetID)
		}
		cleanedDestination := path.Clean(destinationPath)
		for otherTargetID, otherStep := range fileSteps {
			if otherTargetID == targetID || otherStep.Target == nil {
				continue
			}
			for _, otherPath := range otherStep.Target.Paths {
				cleanedOther := path.Clean(otherPath)
				if pathsOverlap(cleanedDestination, cleanedOther) ||
					cleanedOther == cleanedDestination+"-wal" || cleanedOther == cleanedDestination+"-shm" {
					return fmt.Errorf(
						"执行恢复失败: 原始文件 target %q 与 files target %q 的路径 %q 重叠",
						targetID,
						otherTargetID,
						otherPath,
					)
				}
			}
		}
	}
	return nil
}

func validateRestoreFilePath(targetPath string) error {
	cleaned := path.Clean(targetPath)
	if cleaned == "/" || cleaned == "." || !strings.HasPrefix(cleaned, "/") {
		return fmt.Errorf("files target 路径 %q 不允许用于恢复", targetPath)
	}
	for _, protected := range []string{restoreMarkerBase, "/run/ark.lock"} {
		if pathsOverlap(cleaned, protected) {
			return fmt.Errorf("files target 路径 %q 与 ark 运行时路径 %q 重叠", targetPath, protected)
		}
	}
	return nil
}

func validateRestoreFilePathForPlan(plan Plan, targetPath string) error {
	if plan.Isolation == nil {
		return validateRestoreFilePath(targetPath)
	}
	cleaned := path.Clean(targetPath)
	if cleaned == plan.Isolation.FilesRoot || !strings.HasPrefix(cleaned, plan.Isolation.FilesRoot+"/") {
		return fmt.Errorf("files target 路径 %q 不位于 isolation files 根目录", targetPath)
	}
	return nil
}

func pathsOverlap(left string, right string) bool {
	left = path.Clean(left)
	right = path.Clean(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func authorizeConflicts(ctx context.Context, inspection preflight, options ExecuteOptions) error {
	if len(inspection.conflicts) == 0 {
		return nil
	}
	var unauthorized []string
	var summaries []string
	for _, item := range inspection.conflicts {
		summaries = append(summaries, fmt.Sprintf("%s: %s", item.resource, item.detail))
		if !item.authorized {
			unauthorized = append(unauthorized, item.resource)
		}
	}
	if len(unauthorized) > 0 {
		return fmt.Errorf("恢复冲突包含未获授权资源 %s", strings.Join(unauthorized, ", "))
	}
	if !options.Force {
		return fmt.Errorf("目标存在恢复冲突，默认拒绝覆盖: %s", strings.Join(summaries, "；"))
	}
	if options.SafetyBackup == nil {
		return fmt.Errorf("--force 恢复前的 safety backup 依赖为空")
	}
	if err := options.SafetyBackup(ctx); err != nil {
		return fmt.Errorf("破坏前 safety backup 失败，恢复已中止: %w", err)
	}
	return nil
}

func executeStep(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	options ExecuteOptions,
	dependencies executeDependencies,
) (StepResult, error) {
	result := StepResult{Phase: step.Phase, TargetID: step.TargetID}
	markerValue := stepMarkerValue(plan, step, options.RawFileTargets)
	completed, warnings, err := stepCompleted(ctx, plan, step, markerValue, runner)
	if err != nil {
		result.Status = "fail"
		return result, err
	}
	if completed {
		if len(warnings) > 0 {
			result.Status = "warn"
			result.Detail = strings.Join(warnings, "；")
		} else {
			result.Status = "skipped"
			result.Detail = "后置条件已验证"
		}
		return result, nil
	}

	switch step.Phase {
	case PhaseFiles:
		err = restoreFiles(ctx, plan, step, runner, options.RawFileTargets, dependencies)
	case PhaseImageDigest:
		err = restoreImages(ctx, plan, step, runner)
	case PhaseVolume:
		err = restoreVolume(ctx, plan, step, runner, dependencies)
	case PhaseDatabasePrepare:
		err = prepareDatabase(ctx, plan, step, runner, dependencies.pollInterval)
	case PhaseDatabaseData:
		err = restoreDatabase(ctx, plan, step, runner, dependencies)
	case PhaseApplication:
		if err = validateComposeImageCoverage(ctx, plan, runner); err == nil {
			_, err = runner.Run(ctx, append(composeArgv(plan), "up", "-d", "--no-build", "--pull", "never")...)
		}
	case PhaseHealth:
		var warnings []string
		warnings, err = verifyHealth(ctx, plan, runner, dependencies.pollInterval)
		if err == nil && len(warnings) > 0 {
			result.Status = "warn"
			result.Detail = strings.Join(warnings, "；")
		}
	default:
		err = fmt.Errorf("不支持恢复阶段 %q", step.Phase)
	}
	if err != nil {
		result.Status = "fail"
		result.Detail = "阶段执行失败"
		return result, fmt.Errorf("恢复阶段 %q target %q 失败: %w", step.Phase, step.TargetID, err)
	}
	completedMarker, err := completedStepMarker(ctx, step, markerValue, runner)
	if err != nil {
		result.Status = "fail"
		return result, fmt.Errorf("生成阶段 %q target %q 完成状态失败: %w", step.Phase, step.TargetID, err)
	}
	if err := writeMarker(ctx, runner, stepMarkerPath(plan, step), completedMarker); err != nil {
		result.Status = "fail"
		return result, fmt.Errorf("记录阶段 %q target %q 完成状态失败: %w", step.Phase, step.TargetID, err)
	}
	if result.Status == "" {
		result.Status = "ok"
	}
	return result, nil
}

func restoreFiles(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	rawFileTargets map[string]string,
	dependencies executeDependencies,
) error {
	if step.Target == nil {
		return fmt.Errorf("files target 配置为空")
	}
	if plan.Isolation != nil {
		exists, err := targetPathExists(ctx, runner, plan.Isolation.FilesRoot)
		if err != nil {
			return err
		}
		if exists {
			if err := validateIsolationFilesTreeLinks(ctx, runner, plan.Isolation.FilesRoot); err != nil {
				return err
			}
		}
	}
	if destinationPath, ok := rawFileTargets[step.TargetID]; ok {
		return restoreRawFile(ctx, plan, step, runner, destinationPath, dependencies)
	}
	for _, targetPath := range step.Target.Paths {
		cleaned := path.Clean(targetPath)
		// 默认模式已证明路径不存在；force 或同 Plan 重跑时必须先删除精确目标，避免残留旧文件。
		if _, err := runner.Run(ctx, "rm", "-rf", "--", cleaned); err != nil {
			return fmt.Errorf("清理 files target 路径 %q 失败: %w", targetPath, err)
		}
	}
	if plan.Isolation != nil {
		if _, err := runner.Run(ctx, "install", "-d", "-m", "0700", plan.Isolation.FilesRoot); err != nil {
			return fmt.Errorf("准备 isolation files 根目录失败: %w", err)
		}
		return feedDump(ctx, dependencies.dump, runner, step.SnapshotID, snapshotPath(plan, step),
			[]string{"tar", "-xpf", "-", "-C", plan.Isolation.FilesRoot})
	}
	return feedDump(ctx, dependencies.dump, runner, step.SnapshotID, snapshotPath(plan, step),
		[]string{"tar", "-xpf", "-", "-C", "/"})
}

func restoreRawFile(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	destinationPath string,
	dependencies executeDependencies,
) error {
	directory := path.Dir(destinationPath)
	temporary := destinationPath + ".ark-restore.tmp"
	if _, err := runner.Run(ctx, "install", "-d", "-m", "0700", directory); err != nil {
		return fmt.Errorf("准备原始文件恢复目录 %q 失败: %w", directory, err)
	}
	if _, err := runner.Run(ctx, "chmod", "0700", directory); err != nil {
		return fmt.Errorf("设置原始文件恢复目录权限失败: %w", err)
	}
	if _, err := runner.Run(ctx, "rm", "-f", "--", temporary); err != nil {
		return fmt.Errorf("清理原始文件临时路径 %q 失败: %w", temporary, err)
	}

	// 原文件必须保留到完整数据流落盘并收紧权限后，避免中途失败留下半截状态库。
	reader, err := dependencies.dump(ctx, step.SnapshotID, rawFileSnapshotPath(plan, step))
	if err != nil {
		return fmt.Errorf("读取 restic snapshot %q 原始文件失败: %w", step.SnapshotID, err)
	}
	feedErr := runner.Feed(ctx, reader, "tee", temporary)
	closeErr := reader.Close()
	if err := errors.Join(feedErr, closeErr); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return fmt.Errorf("恢复原始文件数据流失败: %w", errors.Join(err, cleanupErr))
	}
	if _, err := runner.Run(ctx, "chmod", "0600", temporary); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return errors.Join(fmt.Errorf("设置原始文件权限失败: %w", err), cleanupErr)
	}
	if _, err := runner.Run(ctx, "mv", "--", temporary, destinationPath); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return errors.Join(fmt.Errorf("原子切换原始文件失败: %w", err), cleanupErr)
	}
	// Online Backup 产物是独立主库；切换成功后、应用启动前必须移除旧 WAL/SHM。
	if _, err := runner.Run(ctx, "rm", "-f", "--", destinationPath+"-wal", destinationPath+"-shm"); err != nil {
		return fmt.Errorf("清理状态库 WAL/SHM 失败: %w", err)
	}
	return nil
}

func restoreImages(ctx context.Context, plan Plan, step Step, runner sshexec.Runner) error {
	if err := validateComposeImageCoverage(ctx, plan, runner); err != nil {
		return err
	}
	services := make([]string, 0, len(step.ImageDigests))
	for service := range step.ImageDigests {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		digest := step.ImageDigests[service]
		ok, err := imageDigestPresent(ctx, runner, digest)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		if _, err := runner.Run(ctx, "docker", "pull", digest); err != nil {
			return fmt.Errorf("拉取 service %q 镜像 digest 失败: %w", service, err)
		}
		ok, err = imageDigestPresent(ctx, runner, digest)
		if err != nil || !ok {
			return errors.Join(fmt.Errorf("service %q 镜像 digest 拉取后无法验证", service), err)
		}
	}
	content, err := imageOverrideContent(plan)
	if err != nil {
		return err
	}
	if err := writeRootOnlyFile(ctx, runner, imageOverridePath(plan), content); err != nil {
		return fmt.Errorf("写入 Compose digest override 失败: %w", err)
	}
	return nil
}

func validateComposeImageCoverage(ctx context.Context, plan Plan, runner sshexec.Runner) error {
	services, err := composeBaseServices(ctx, plan.Project, runner)
	if err != nil {
		return err
	}
	planned := make(map[string]struct{})
	for _, step := range plan.Steps {
		if step.Phase != PhaseImageDigest {
			continue
		}
		for service := range step.ImageDigests {
			planned[service] = struct{}{}
		}
	}
	var missing []string
	for _, service := range services {
		if _, exists := planned[service]; !exists {
			missing = append(missing, service)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("compose 活跃 services 缺少备份时 image digest: %s", strings.Join(missing, ", "))
	}
	return nil
}

func restoreVolume(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	dependencies executeDependencies,
) error {
	if step.Target == nil {
		return fmt.Errorf("volume target 配置为空")
	}
	name := step.Target.Name
	volumeKey, err := composeVolumeKey(ctx, runner, plan.Project, name)
	if err != nil {
		return err
	}
	exists, labels, err := inspectVolume(ctx, runner, name)
	if err != nil {
		return err
	}
	projectName := effectiveProjectName(plan.Project)
	if exists && (labels[composeProjectLabel] != projectName || labels[composeVolumeLabel] != volumeKey ||
		!isolationLabelsMatch(plan, labels)) {
		return fmt.Errorf("volume %q 不属于目标项目 %q", name, projectName)
	}
	if !exists {
		argv := []string{
			"docker", "volume", "create",
			"--label", composeProjectLabel + "=" + projectName,
			"--label", composeVolumeLabel + "=" + volumeKey,
			"--label", restoreManifestLabel + "=" + plan.ManifestSnapshotID,
		}
		if plan.Isolation != nil {
			argv = append(argv, "--label", isolationLabel+"="+plan.Isolation.ID)
		}
		argv = append(argv, name)
		if _, err := runner.Run(ctx, argv...); err != nil {
			return fmt.Errorf("创建 volume %q 失败: %w", name, err)
		}
	} else if _, err := runner.Run(ctx,
		"docker", "run", "--rm", "-v", name+":/dst", "alpine",
		"find", "/dst", "-mindepth", "1", "-maxdepth", "1", "-exec", "rm", "-rf", "--", "{}", "+",
	); err != nil {
		return fmt.Errorf("清空 volume %q 旧内容失败: %w", name, err)
	}
	if err := feedDump(ctx, dependencies.dump, runner, step.SnapshotID, snapshotPath(plan, step), []string{
		"docker", "run", "--rm", "-i", "-v", name + ":/dst", "alpine",
		"tar", "-xpf", "-", "-C", "/dst",
	}); err != nil {
		return err
	}
	exists, labels, err = inspectVolume(ctx, runner, name)
	if err != nil || !exists || labels[composeProjectLabel] != projectName || labels[composeVolumeLabel] != volumeKey ||
		!isolationLabelsMatch(plan, labels) {
		return errors.Join(fmt.Errorf("volume %q 恢复后归属校验失败", name), err)
	}
	return nil
}

func prepareDatabase(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	pollInterval time.Duration,
) error {
	if step.Target == nil {
		return fmt.Errorf("database target 配置为空")
	}
	argv := composeArgv(plan)
	if step.TargetType == config.TargetRedis {
		if _, err := runner.Run(ctx,
			append(argv, "up", "--no-start", "--no-build", "--pull", "never", "--no-deps", step.Target.Service)...,
		); err != nil {
			return fmt.Errorf("创建 Redis service %q 失败: %w", step.Target.Service, err)
		}
		if _, err := runner.Run(ctx, append(argv, "stop", step.Target.Service)...); err != nil {
			return fmt.Errorf("停止 Redis service %q 失败: %w", step.Target.Service, err)
		}
		return nil
	}
	if _, err := runner.Run(ctx,
		append(argv, "up", "-d", "--no-build", "--pull", "never", "--no-deps", step.Target.Service)...,
	); err != nil {
		return fmt.Errorf("启动数据库 service %q 失败: %w", step.Target.Service, err)
	}
	return waitDatabaseReady(ctx, plan, step, runner, pollInterval)
}

func restoreDatabase(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	dependencies executeDependencies,
) error {
	if step.Target == nil {
		return fmt.Errorf("database target 配置为空")
	}
	switch step.TargetType {
	case config.TargetPostgres:
		argv := append(composeArgv(plan), "exec", "-T", step.Target.Service, "psql")
		if step.Target.User != "" {
			argv = append(argv, "-U", step.Target.User)
		}
		// psql 默认遇到 SQL 错误仍可能继续并返回 0，必须强制首个错误终止导入。
		argv = append(argv, "-d", step.Target.Database, "--set", "ON_ERROR_STOP=1")
		return feedDump(ctx, dependencies.dump, runner, step.SnapshotID, snapshotPath(plan, step), argv)
	case config.TargetRedis:
		return restoreRedis(ctx, plan, step, runner, dependencies)
	default:
		return fmt.Errorf("不支持数据库 target 类型 %q", step.TargetType)
	}
}

func restoreRedis(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	dependencies executeDependencies,
) error {
	argv := composeArgv(plan)
	if _, err := runner.Run(ctx, append(argv, "stop", step.Target.Service)...); err != nil {
		return fmt.Errorf("停止 Redis service %q 失败: %w", step.Target.Service, err)
	}
	containerID, err := composeServiceContainerID(ctx, runner, plan, step.Target.Service)
	if err != nil {
		return err
	}
	volumeName, err := redisDataVolume(ctx, runner, containerID)
	if err != nil {
		return err
	}
	exists, labels, err := inspectVolume(ctx, runner, volumeName)
	if err != nil || !exists || labels[composeProjectLabel] != effectiveProjectName(plan.Project) ||
		!isolationLabelsMatch(plan, labels) {
		return errors.Join(fmt.Errorf("Redis 数据 volume %q 不属于目标项目", volumeName), err)
	}
	owner, err := volumeDataOwner(ctx, runner, volumeName)
	if err != nil {
		return err
	}
	temporary := "/data/.ark-restore-dump.rdb"
	if _, err := runner.Run(ctx,
		"docker", "run", "--rm", "-v", volumeName+":/data", "alpine", "rm", "-f", "--", temporary,
	); err != nil {
		return fmt.Errorf("清理 Redis 临时 RDB 失败: %w", err)
	}
	if err := feedDump(ctx, dependencies.dump, runner, step.SnapshotID, snapshotPath(plan, step), []string{
		"docker", "run", "--rm", "-i", "--user", owner,
		"-v", volumeName + ":/data", "alpine", "tee", temporary,
	}); err != nil {
		return err
	}
	if _, err := runner.Run(ctx,
		"docker", "run", "--rm", "-v", volumeName+":/data", "alpine",
		"mv", "--", temporary, "/data/dump.rdb",
	); err != nil {
		return fmt.Errorf("原子切换 Redis RDB 失败: %w", err)
	}
	if _, err := runner.Run(ctx,
		append(argv, "up", "-d", "--no-build", "--pull", "never", "--no-deps", step.Target.Service)...,
	); err != nil {
		return fmt.Errorf("启动 Redis service %q 失败: %w", step.Target.Service, err)
	}
	return waitDatabaseReady(ctx, plan, step, runner, dependencies.pollInterval)
}

func feedDump(
	ctx context.Context,
	dump func(context.Context, string, string) (io.ReadCloser, error),
	runner sshexec.Runner,
	snapshotID string,
	snapshotPath string,
	argv []string,
) error {
	reader, err := dump(ctx, snapshotID, snapshotPath)
	if err != nil {
		return fmt.Errorf("读取 restic snapshot %q 路径 %q 失败: %w", snapshotID, snapshotPath, err)
	}
	feedErr := runner.Feed(ctx, reader, argv...)
	closeErr := reader.Close()
	if err := errors.Join(feedErr, closeErr); err != nil {
		return fmt.Errorf("恢复数据流失败: %w", err)
	}
	return nil
}

func waitDatabaseReady(
	ctx context.Context,
	plan Plan,
	step Step,
	runner sshexec.Runner,
	pollInterval time.Duration,
) error {
	for {
		var argv []string
		switch step.TargetType {
		case config.TargetPostgres:
			argv = append(composeArgv(plan), "exec", "-T", step.Target.Service, "pg_isready")
			if step.Target.User != "" {
				argv = append(argv, "-U", step.Target.User)
			}
			argv = append(argv, "-d", step.Target.Database)
		case config.TargetRedis:
			argv = append(composeArgv(plan), "exec", "-T", step.Target.Service, "redis-cli", "PING")
		default:
			return fmt.Errorf("不支持数据库 readiness 类型 %q", step.TargetType)
		}
		out, err := runner.Run(ctx, argv...)
		if err == nil && (step.TargetType != config.TargetRedis || strings.TrimSpace(out) == "PONG") {
			return nil
		}
		if err := waitPoll(ctx, pollInterval); err != nil {
			return fmt.Errorf("等待数据库 target %q 就绪失败: %w", step.TargetID, err)
		}
	}
}

func verifyHealth(
	ctx context.Context,
	plan Plan,
	runner sshexec.Runner,
	pollInterval time.Duration,
) ([]string, error) {
	services, err := composeServices(ctx, plan, runner)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("compose project 未声明任何 service")
	}
	for {
		states, err := composeStates(ctx, runner, plan, true)
		if err != nil {
			return nil, err
		}
		byService := make(map[string][]composeState, len(states))
		pending := false
		for _, state := range states {
			if state.Service != "" {
				byService[state.Service] = append(byService[state.Service], state)
			}
		}
		for _, service := range services {
			serviceStates := byService[service]
			if len(serviceStates) == 0 {
				pending = true
				break
			}
			for _, state := range serviceStates {
				if state.State == "created" || state.State == "restarting" {
					pending = true
					break
				}
				if state.State != "running" {
					return nil, fmt.Errorf("service %q container %q state=%q", service, state.ID, state.State)
				}
				if state.Health == "starting" {
					pending = true
					break
				}
				if state.Health != "" && state.Health != "healthy" {
					return nil, fmt.Errorf("service %q container %q health=%q", service, state.ID, state.Health)
				}
			}
			if pending {
				break
			}
		}
		if pending {
			if err := waitPoll(ctx, pollInterval); err != nil {
				return nil, fmt.Errorf("等待 compose services 运行失败: %w", err)
			}
			continue
		}

		var warnings []string
		for _, service := range services {
			withoutHealth := false
			for _, state := range byService[service] {
				if state.Health == "" {
					withoutHealth = true
				}
			}
			if withoutHealth {
				warnings = append(warnings, fmt.Sprintf("service %q 未声明 healthcheck", service))
			}
		}
		if err := verifyImageDigests(ctx, plan, runner, byService); err != nil {
			return nil, err
		}
		return warnings, nil
	}
}

func composeServices(ctx context.Context, plan Plan, runner sshexec.Runner) ([]string, error) {
	return readComposeServices(ctx, runner, composeArgv(plan))
}

func composeBaseServices(ctx context.Context, project Project, runner sshexec.Runner) ([]string, error) {
	return readComposeServices(ctx, runner, composeBaseArgv(project))
}

func readComposeServices(ctx context.Context, runner sshexec.Runner, argv []string) ([]string, error) {
	out, err := runner.Run(ctx, append(argv, "config", "--format", "json")...)
	if err != nil {
		return nil, fmt.Errorf("读取 compose services 失败: %w", err)
	}
	var parsed composeConfig
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		return nil, fmt.Errorf("解析 compose services 失败: %w", err)
	}
	services := make([]string, 0, len(parsed.Services))
	for service := range parsed.Services {
		services = append(services, service)
	}
	sort.Strings(services)
	return services, nil
}

func verifyImageDigests(
	ctx context.Context,
	plan Plan,
	runner sshexec.Runner,
	states map[string][]composeState,
) error {
	for _, step := range plan.Steps {
		if step.Phase != PhaseImageDigest {
			continue
		}
		services := make([]string, 0, len(step.ImageDigests))
		for service := range step.ImageDigests {
			services = append(services, service)
		}
		sort.Strings(services)
		for _, service := range services {
			serviceStates := states[service]
			if len(serviceStates) == 0 {
				return fmt.Errorf("service %q 缺少运行容器，无法核对 digest", service)
			}
			for _, state := range serviceStates {
				if state.ID == "" {
					return fmt.Errorf("service %q 容器缺少 ID，无法核对 digest", service)
				}
				out, err := runner.Run(ctx, "docker", "container", "inspect", "--format", "{{.Image}}", state.ID)
				if err != nil {
					return fmt.Errorf("查询 service %q image ID 失败: %w", service, err)
				}
				digests, err := imageRepoDigests(ctx, runner, strings.TrimSpace(out))
				if err != nil {
					return err
				}
				if !containsString(digests, step.ImageDigests[service]) {
					return fmt.Errorf("service %q 容器 %q 实际 image digest 与 Plan 不一致", service, state.ID)
				}
			}
		}
	}
	return nil
}

func stepCompleted(
	ctx context.Context,
	plan Plan,
	step Step,
	markerValue string,
	runner sshexec.Runner,
) (bool, []string, error) {
	value, found, err := readMarker(ctx, runner, stepMarkerPath(plan, step))
	if err != nil || !found {
		return false, nil, err
	}
	switch step.Phase {
	case PhaseFiles:
		expected, err := completedStepMarker(ctx, step, markerValue, runner)
		if err != nil || value != expected {
			return false, nil, err
		}
	case PhaseImageDigest:
		if value != markerValue {
			return false, nil, nil
		}
		for _, digest := range step.ImageDigests {
			ok, err := imageDigestPresent(ctx, runner, digest)
			if err != nil || !ok {
				return false, nil, err
			}
		}
		matches, err := imageOverrideMatches(ctx, plan, runner)
		if err != nil || !matches {
			return false, nil, err
		}
	case PhaseVolume:
		if value != markerValue {
			return false, nil, nil
		}
		exists, labels, err := inspectVolume(ctx, runner, step.Target.Name)
		if err != nil || !exists {
			return false, nil, err
		}
		volumeKey, err := composeVolumeKey(ctx, runner, plan.Project, step.Target.Name)
		if err != nil || labels[composeProjectLabel] != effectiveProjectName(plan.Project) ||
			labels[composeVolumeLabel] != volumeKey || !isolationLabelsMatch(plan, labels) {
			return false, nil, err
		}
	case PhaseDatabasePrepare, PhaseDatabaseData:
		if value != markerValue {
			return false, nil, nil
		}
		if step.TargetType == config.TargetPostgres || step.Phase == PhaseDatabaseData {
			if err := waitDatabaseReadyOnce(ctx, plan, step, runner); err != nil {
				return false, nil, nil
			}
		}
	case PhaseApplication, PhaseHealth:
		if value != markerValue {
			return false, nil, nil
		}
		warnings, err := verifyHealth(ctx, plan, runner, defaultRestorePollDelay)
		if err != nil {
			return false, nil, nil
		}
		if step.Phase == PhaseHealth {
			return true, warnings, nil
		}
	}
	return true, nil, nil
}

func isolationLabelsMatch(plan Plan, labels map[string]string) bool {
	if plan.Isolation == nil {
		return true
	}
	return labels[isolationLabel] == plan.Isolation.ID
}

func completedStepMarker(
	ctx context.Context,
	step Step,
	markerValue string,
	runner sshexec.Runner,
) (string, error) {
	if step.Phase != PhaseFiles {
		return markerValue, nil
	}
	if step.Target == nil {
		return "", fmt.Errorf("files target 配置为空")
	}
	fingerprint, err := fileMetadataFingerprint(ctx, runner, step.Target.Paths)
	if err != nil {
		return "", err
	}
	return markerValue + "\nmetadata=" + fingerprint, nil
}

func fileMetadataFingerprint(ctx context.Context, runner sshexec.Runner, targetPaths []string) (string, error) {
	hasher := sha256.New()
	for _, targetPath := range targetPaths {
		out, err := runner.Run(ctx, "stat", "-c", "%f %a %u %g %s %Y", "--", targetPath)
		if err != nil {
			return "", fmt.Errorf("读取 files target 路径 %q 元数据失败: %w", targetPath, err)
		}
		_, _ = io.WriteString(hasher, path.Clean(targetPath))
		_, _ = hasher.Write([]byte{0})
		_, _ = io.WriteString(hasher, strings.TrimSpace(out))
		_, _ = hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func resumeRequiresStop(
	ctx context.Context,
	plan Plan,
	options ExecuteOptions,
	runner sshexec.Runner,
) (bool, error) {
	for _, step := range plan.Steps {
		switch step.Phase {
		case PhaseFiles, PhaseVolume, PhaseDatabasePrepare, PhaseDatabaseData:
		default:
			continue
		}
		markerValue := stepMarkerValue(plan, step, options.RawFileTargets)
		completed, _, err := stepCompleted(ctx, plan, step, markerValue, runner)
		if err != nil {
			return false, err
		}
		if !completed {
			return true, nil
		}
	}
	return false, nil
}

func waitDatabaseReadyOnce(ctx context.Context, plan Plan, step Step, runner sshexec.Runner) error {
	var argv []string
	if step.TargetType == config.TargetPostgres {
		argv = append(composeArgv(plan), "exec", "-T", step.Target.Service, "pg_isready")
		if step.Target.User != "" {
			argv = append(argv, "-U", step.Target.User)
		}
		argv = append(argv, "-d", step.Target.Database)
	} else {
		argv = append(composeArgv(plan), "exec", "-T", step.Target.Service, "redis-cli", "PING")
	}
	out, err := runner.Run(ctx, argv...)
	if err != nil {
		return err
	}
	if step.TargetType == config.TargetRedis && strings.TrimSpace(out) != "PONG" {
		return fmt.Errorf("Redis PING 未返回 PONG")
	}
	return nil
}

func listProjectContainers(ctx context.Context, runner sshexec.Runner, projectName string) ([]composeState, error) {
	out, err := runner.Run(ctx,
		"docker", "ps", "-a", "--filter", "label="+composeProjectLabel+"="+projectName, "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("检查目标 Compose project 冲突失败: %w", err)
	}
	return parseComposeStates(out)
}

func listProjectVolumes(ctx context.Context, runner sshexec.Runner, projectName string) ([]string, error) {
	out, err := runner.Run(ctx,
		"docker", "volume", "ls", "--filter", "label="+composeProjectLabel+"="+projectName, "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("检查目标 Compose project volume 冲突失败: %w", err)
	}
	return nonEmptyLines(out), nil
}

func composeVolumeKey(
	ctx context.Context,
	runner sshexec.Runner,
	project Project,
	physicalName string,
) (string, error) {
	out, err := runner.Run(ctx, append(composeBaseArgv(project), "config", "--format", "json")...)
	if err != nil {
		return "", fmt.Errorf("读取 compose volume 配置失败: %w", err)
	}
	var parsed composeConfig
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		return "", fmt.Errorf("解析 compose volume 配置失败: %w", err)
	}
	var matches []string
	for key, volume := range parsed.Volumes {
		resolved := volume.Name
		if resolved == "" {
			resolved = effectiveProjectName(project) + "_" + key
		}
		if resolved == physicalName {
			matches = append(matches, key)
		}
	}
	sort.Strings(matches)
	if len(matches) != 1 {
		return "", fmt.Errorf("volume %q 在 compose 配置中的逻辑定义数量为 %d", physicalName, len(matches))
	}
	return matches[0], nil
}

func stopProjectContainers(ctx context.Context, runner sshexec.Runner, containers []composeState) error {
	ids := make([]string, 0, len(containers))
	for _, container := range containers {
		if strings.TrimSpace(container.ID) == "" {
			return fmt.Errorf("目标 Compose project 容器缺少 ID，拒绝执行 --force")
		}
		ids = append(ids, container.ID)
	}
	sort.Strings(ids)
	if _, err := runner.Run(ctx, append([]string{"docker", "stop"}, ids...)...); err != nil {
		return fmt.Errorf("停止目标 Compose project 容器失败: %w", err)
	}
	return nil
}

func composeStates(ctx context.Context, runner sshexec.Runner, plan Plan, includeStopped bool) ([]composeState, error) {
	argv := append(composeArgv(plan), "ps")
	if includeStopped {
		argv = append(argv, "--all")
	}
	argv = append(argv, "--format", "json")
	out, err := runner.Run(ctx, argv...)
	if err != nil {
		return nil, fmt.Errorf("查询 compose services 状态失败: %w", err)
	}
	return parseComposeStates(out)
}

func parseComposeStates(output string) ([]composeState, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var states []composeState
		if err := json.Unmarshal([]byte(trimmed), &states); err != nil {
			return nil, fmt.Errorf("compose ps JSON 无效: %w", err)
		}
		return states, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	var states []composeState
	for {
		var state composeState
		if err := decoder.Decode(&state); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("compose ps JSON 无效: %w", err)
		}
		states = append(states, state)
	}
	return states, nil
}

func composeServiceContainerID(
	ctx context.Context,
	runner sshexec.Runner,
	plan Plan,
	service string,
) (string, error) {
	states, err := composeStates(ctx, runner, plan, true)
	if err != nil {
		return "", err
	}
	var ids []string
	for _, state := range states {
		if state.Service == service && state.ID != "" {
			ids = append(ids, state.ID)
		}
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("Redis service %q 容器数量为 %d，无法确定数据卷", service, len(ids))
	}
	return ids[0], nil
}

func redisDataVolume(ctx context.Context, runner sshexec.Runner, containerID string) (string, error) {
	out, err := runner.Run(ctx, "docker", "container", "inspect", "--format", "{{json .Mounts}}", containerID)
	if err != nil {
		return "", fmt.Errorf("查询 Redis 容器数据卷失败: %w", err)
	}
	var mounts []dockerMount
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &mounts); err != nil {
		return "", fmt.Errorf("解析 Redis 容器数据卷失败: %w", err)
	}
	var names []string
	for _, mount := range mounts {
		if mount.Type == "volume" && mount.Destination == "/data" && mount.Name != "" {
			names = append(names, mount.Name)
		}
	}
	if len(names) != 1 {
		return "", fmt.Errorf("Redis 容器 /data volume 数量为 %d，无法安全恢复", len(names))
	}
	return names[0], nil
}

func volumeDataOwner(ctx context.Context, runner sshexec.Runner, volumeName string) (string, error) {
	out, err := runner.Run(ctx,
		"docker", "run", "--rm", "-v", volumeName+":/data", "alpine",
		"stat", "-c", "%u:%g", "/data",
	)
	if err != nil {
		return "", fmt.Errorf("查询 Redis 数据目录属主失败: %w", err)
	}
	owner := strings.TrimSpace(out)
	parts := strings.Split(owner, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("Redis 数据目录属主输出无效")
	}
	for _, part := range parts {
		for _, character := range part {
			if character < '0' || character > '9' {
				return "", fmt.Errorf("Redis 数据目录属主输出无效")
			}
		}
	}
	return owner, nil
}

func inspectVolume(ctx context.Context, runner sshexec.Runner, name string) (bool, map[string]string, error) {
	out, err := runner.Run(ctx, "docker", "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false, nil, fmt.Errorf("查询 volume %q 失败: %w", name, err)
	}
	found := false
	for _, line := range nonEmptyLines(out) {
		if line == name {
			found = true
		}
	}
	if !found {
		return false, nil, nil
	}
	out, err = runner.Run(ctx, "docker", "volume", "inspect", "--format", "{{json .Labels}}", name)
	if err != nil {
		return false, nil, fmt.Errorf("查询 volume %q 标签失败: %w", name, err)
	}
	labels := make(map[string]string)
	trimmed := strings.TrimSpace(out)
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
			return false, nil, fmt.Errorf("解析 volume %q 标签失败: %w", name, err)
		}
	}
	return true, labels, nil
}

func imageDigestPresent(ctx context.Context, runner sshexec.Runner, digest string) (bool, error) {
	out, err := runner.Run(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", digest)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if commandExitedWith(err, 1) {
			return false, nil
		}
		return false, fmt.Errorf("查询镜像 digest 失败: %w", err)
	}
	var digests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &digests); err != nil {
		return false, fmt.Errorf("解析镜像 digest 失败: %w", err)
	}
	return containsString(digests, digest), nil
}

func imageRepoDigests(ctx context.Context, runner sshexec.Runner, imageID string) ([]string, error) {
	out, err := runner.Run(ctx, "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", imageID)
	if err != nil {
		return nil, fmt.Errorf("查询运行镜像 digest 失败: %w", err)
	}
	var digests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &digests); err != nil {
		return nil, fmt.Errorf("解析运行镜像 digest 失败: %w", err)
	}
	return digests, nil
}

func targetPathExists(ctx context.Context, runner sshexec.Runner, targetPath string) (bool, error) {
	_, err := runner.Run(ctx, "test", "-e", targetPath)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if commandExitedWith(err, 1) {
		_, linkErr := runner.Run(ctx, "test", "-L", targetPath)
		if linkErr == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if commandExitedWith(linkErr, 1) {
			return false, nil
		}
		return false, fmt.Errorf("检查路径 %q 是否为软链接失败: %w", targetPath, linkErr)
	}
	return false, fmt.Errorf("检查路径 %q 是否存在失败: %w", targetPath, err)
}

func commandExitedWith(err error, code int) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}

func readMarker(ctx context.Context, runner sshexec.Runner, markerPath string) (string, bool, error) {
	exists, err := targetPathExists(ctx, runner, markerPath)
	if err != nil || !exists {
		return "", false, err
	}
	out, err := runner.Run(ctx, "cat", "--", markerPath)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

func writeMarker(ctx context.Context, runner sshexec.Runner, markerPath string, value string) error {
	return writeRootOnlyFile(ctx, runner, markerPath, value+"\n")
}

func writeRootOnlyFile(ctx context.Context, runner sshexec.Runner, filePath string, content string) error {
	directory := path.Dir(filePath)
	temporary := filePath + ".tmp"
	if _, err := runner.Run(ctx, "install", "-d", "-m", "0700", directory); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "chmod", "0700", directory); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "rm", "-f", "--", temporary); err != nil {
		return err
	}
	if err := runner.Feed(ctx, strings.NewReader(content), "tee", temporary); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return errors.Join(err, cleanupErr)
	}
	if _, err := runner.Run(ctx, "chmod", "0600", temporary); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return errors.Join(err, cleanupErr)
	}
	if _, err := runner.Run(ctx, "mv", "--", temporary, filePath); err != nil {
		_, cleanupErr := cleanupRestoreTemporary(runner, temporary)
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func cleanupRestoreTemporary(runner sshexec.Runner, temporary string) (string, error) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), restoreCleanupTimeout)
	defer cancel()
	return runner.Run(cleanupCtx, "rm", "-f", "--", temporary)
}

func imageOverrideContent(plan Plan) (string, error) {
	services := make(map[string]map[string]string)
	for _, step := range plan.Steps {
		if step.Phase != PhaseImageDigest {
			continue
		}
		for service, digest := range step.ImageDigests {
			if existing, found := services[service]; found && existing["image"] != digest {
				return "", fmt.Errorf("service %q 存在多个 image digest", service)
			}
			services[service] = map[string]string{"image": digest}
		}
	}
	if len(services) == 0 {
		return "", fmt.Errorf("Plan 缺少 image digest")
	}
	// JSON 是 YAML 的严格子集，可避免手工转义 service 名或镜像引用。
	content, err := json.Marshal(map[string]any{"services": services})
	if err != nil {
		return "", fmt.Errorf("编码 Compose digest override 失败: %w", err)
	}
	return string(content) + "\n", nil
}

func imageOverrideMatches(ctx context.Context, plan Plan, runner sshexec.Runner) (bool, error) {
	expected, err := imageOverrideContent(plan)
	if err != nil {
		return false, err
	}
	actual, found, err := readFileIfExists(ctx, runner, imageOverridePath(plan))
	if err != nil || !found {
		return false, err
	}
	return actual == expected, nil
}

func readFileIfExists(ctx context.Context, runner sshexec.Runner, filePath string) (string, bool, error) {
	exists, err := targetPathExists(ctx, runner, filePath)
	if err != nil || !exists {
		return "", false, err
	}
	out, err := runner.Run(ctx, "cat", "--", filePath)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

func planMarkerRoot(plan Plan) string {
	if plan.Isolation != nil {
		return plan.Isolation.Root
	}
	value := plan.DestinationHost + "\x00" + plan.Project.Name + "\x00" +
		plan.Project.ComposeFile + "\x00" + plan.Project.ProjectName
	digest := sha256.Sum256([]byte(value))
	return path.Join(restoreMarkerBase, hex.EncodeToString(digest[:16]))
}

func executionIdentity(plan Plan, rawFileTargets map[string]string) string {
	encoded, err := json.Marshal(struct {
		Plan           Plan              `json:"plan"`
		RawFileTargets map[string]string `json:"raw_file_targets,omitempty"`
	}{Plan: plan, RawFileTargets: rawFileTargets})
	if err != nil {
		// 执行身份只含 Plan 和字符串 map；保留防御性分支避免未来字段改变后静默碰撞。
		return "invalid-plan"
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func stepMarkerValue(plan Plan, step Step, rawFileTargets map[string]string) string {
	if step.SnapshotID == "" {
		return executionIdentity(plan, rawFileTargets)
	}
	if destinationPath, ok := rawFileTargets[step.TargetID]; ok && step.Phase == PhaseFiles {
		value := step.SnapshotID + "\x00raw-file\x00" + path.Clean(destinationPath)
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	return step.SnapshotID
}

func planStatePath(plan Plan) string {
	return path.Join(planMarkerRoot(plan), "plan")
}

func planCompletePath(plan Plan) string {
	return path.Join(planMarkerRoot(plan), "complete")
}

func stepMarkerPath(plan Plan, step Step) string {
	value := string(step.Phase) + "\x00" + step.TargetID
	digest := sha256.Sum256([]byte(value))
	return path.Join(planMarkerRoot(plan), "steps", hex.EncodeToString(digest[:16]))
}

func imageOverridePath(plan Plan) string {
	return path.Join(planMarkerRoot(plan), "compose-images.json")
}

func snapshotPath(plan Plan, step Step) string {
	suffix := ".tar"
	switch step.TargetType {
	case config.TargetPostgres:
		suffix = ".sql"
	case config.TargetRedis:
		suffix = ".rdb"
	case config.TargetImageDigest:
		suffix = ".json"
	}
	return plan.SourceHost + "/" + step.TargetID + suffix
}

func rawFileSnapshotPath(plan Plan, step Step) string {
	return plan.SourceHost + "/" + step.TargetID + ".db"
}

func effectiveProjectName(project Project) string {
	if project.ProjectName != "" {
		return project.ProjectName
	}
	return project.Name
}

func composeArgv(plan Plan) []string {
	argv := []string{"docker", "compose", "-f", plan.Project.ComposeFile}
	if hasImageDigestStep(plan) {
		argv = append(argv, "-f", imageOverridePath(plan))
	}
	if plan.Project.ProjectName != "" {
		argv = append(argv, "-p", plan.Project.ProjectName)
	}
	if plan.Project.EnvFile != "" {
		argv = append(argv, "--env-file", plan.Project.EnvFile)
	}
	return argv
}

func composeBaseArgv(project Project) []string {
	argv := []string{"docker", "compose", "-f", project.ComposeFile}
	if project.ProjectName != "" {
		argv = append(argv, "-p", project.ProjectName)
	}
	if project.EnvFile != "" {
		argv = append(argv, "--env-file", project.EnvFile)
	}
	return argv
}

func hasImageDigestStep(plan Plan) bool {
	for _, step := range plan.Steps {
		if step.Phase == PhaseImageDigest && len(step.ImageDigests) > 0 {
			return true
		}
	}
	return false
}

func nonEmptyLines(value string) []string {
	var values []string
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func waitPoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
