package verify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

const (
	verificationDetailSchemaVersion = 1
	verificationFinishTimeout       = 30 * time.Second
)

// Options 控制演练失败后的资源保留与 hub 状态库原始文件映射。
type Options struct {
	// KeepOnFailure 只在恢复失败且资源归属完整可证时保留隔离环境。
	KeepOnFailure bool
	// RawFileTargets 把原始单文件 files target 映射到生产目标路径；演练会再映射到隔离根目录。
	RawFileTargets map[string]string
}

// Failure 描述尚未进入隔离恢复前已确认的一次 host 演练失败。
type Failure struct {
	// Host 是计划验证的来源 host。
	Host string
	// RunID 是所选 manifest 的 backup run ID。
	RunID string
	// ManifestSnapshotID 是承载所选 manifest 的精确 snapshot ID。
	ManifestSnapshotID string
	// Targets 是前置失败发生前已知的 target snapshot 关联。
	Targets []TargetEvidence
	// Error 是不含底层命令输出和敏感值的阶段级摘要。
	Error string
}

// BaselineEvidence 保存演练前后生产基线指纹与差异类别。
type BaselineEvidence struct {
	// BeforeFingerprint 是首次目标写入前的生产基线指纹。
	BeforeFingerprint string `json:"before_fingerprint,omitempty"`
	// AfterFingerprint 是清理或保留决策后的生产基线指纹。
	AfterFingerprint string `json:"after_fingerprint,omitempty"`
	// Differences 是发生变化的资源类别。
	Differences []string `json:"differences"`
}

// TargetEvidence 是本次恢复演练实际依赖的 target snapshot 关联。
type TargetEvidence struct {
	// TargetID 是 config.Target.ID 的稳定值。
	TargetID string `json:"target_id"`
	// TargetType 是 target 类型。
	TargetType string `json:"target_type"`
	// SnapshotID 是 restore Plan 选中的精确 target snapshot。
	SnapshotID string `json:"snapshot_id"`
}

// Result 是一台 host 的完整恢复演练结果。
type Result struct {
	// ID 是本次演练及其 isolation instance 的稳定标识。
	ID string `json:"id"`
	// Host 是备份来源和原机演练目标 host。
	Host string `json:"host"`
	// RunID 是来源 backup run ID。
	RunID string `json:"run_id"`
	// ManifestSnapshotID 是本次使用的精确 manifest snapshot。
	ManifestSnapshotID string `json:"manifest_snapshot_id"`
	// StartedAt 是 UTC 开始时间。
	StartedAt time.Time `json:"started_at"`
	// FinishedAt 是 UTC 完成时间。
	FinishedAt time.Time `json:"finished_at"`
	// Duration 是完整演练耗时。
	Duration time.Duration `json:"duration"`
	// Status 是 ok、warn 或 fail。
	Status store.Status `json:"status"`
	// Restore 是底层恢复步骤与隔离资源摘要。
	Restore restore.Result `json:"restore"`
	// Targets 是本次演练使用的全部 target snapshot 关联。
	Targets []TargetEvidence `json:"targets"`
	// Baseline 是生产资源不变证据。
	Baseline BaselineEvidence `json:"baseline"`
	// Cleanup 是默认清理结果；失败保留时为空。
	Cleanup *restore.CleanupResult `json:"cleanup,omitempty"`
	// KeptOwnership 是 keep-on-failure 成功后的只读归属证据。
	KeptOwnership *restore.IsolationOwnership `json:"kept_ownership,omitempty"`
	// KeptOnFailure 表示失败资源已按显式参数安全保留。
	KeptOnFailure bool `json:"kept_on_failure"`
	// Error 是阶段级脱敏失败摘要。
	Error string `json:"error,omitempty"`
}

type executeDependencies struct {
	now               func() time.Time
	newID             func(time.Time) (string, error)
	captureBaseline   func(context.Context, sshexec.Runner, restore.Plan) (Baseline, error)
	isolate           func(restore.Plan, restore.IsolationOptions) (restore.Plan, error)
	executeRestore    func(context.Context, restore.Plan, *restic.Repo, sshexec.Runner, restore.ExecuteOptions) (restore.Result, error)
	cleanup           func(context.Context, sshexec.Runner, string, string) (restore.CleanupResult, error)
	validateOwnership func(context.Context, sshexec.Runner, string, string) (restore.IsolationOwnership, error)
	record            func(context.Context, store.Verification) error
}

// Execute 在原 host 上执行一次端口不暴露的隔离恢复演练并持久化最终结果。
// @param ctx 控制恢复主路径的取消；取消后的清理、基线复核与记录使用有界收尾 context。
// @param plan 尚未隔离、source=destination 的完整恢复 Plan。
// @param repo 已配置的 restic 仓库。
// @param runner source 原 host 的本地或 SSH Runner。
// @param state 已打开的 ark 状态库。
// @param options 失败保留和原始单文件映射选项。
// @return Result 恢复、清理、基线和持久化结果。
// @return error 恢复、资源归属、基线、清理或持久化失败时保留的错误链。
func Execute(
	ctx context.Context,
	plan restore.Plan,
	repo *restic.Repo,
	runner sshexec.Runner,
	state *store.Store,
	options Options,
) (Result, error) {
	if repo == nil || state == nil {
		return Result{}, fmt.Errorf("执行恢复演练失败: repo 或 store 为空")
	}
	return execute(ctx, plan, repo, runner, options, executeDependencies{
		now:               time.Now,
		newID:             generateVerificationID,
		captureBaseline:   CaptureBaseline,
		isolate:           restore.WithIsolationOptions,
		executeRestore:    restore.Execute,
		cleanup:           restore.CleanupIsolation,
		validateOwnership: restore.ValidateIsolationOwnership,
		record: func(ctx context.Context, verification store.Verification) error {
			return state.RecordVerification(ctx, verification)
		},
	})
}

// RecordFailure 为执行前已失败的 host 生成 verification ID 并持久化脱敏结果。
// @param ctx 控制结果写入；调用方取消后仍会使用有界收尾 context。
// @param state 已打开的 ark 状态库。
// @param failure host、manifest/run 关联和阶段级安全摘要。
// @return Result 已持久化或尝试持久化的失败结果。
// @return error 输入、ID 生成、结果编码或状态库写入失败。
func RecordFailure(ctx context.Context, state *store.Store, failure Failure) (Result, error) {
	if ctx == nil || state == nil || strings.TrimSpace(failure.Host) == "" ||
		strings.TrimSpace(failure.RunID) == "" || strings.TrimSpace(failure.ManifestSnapshotID) == "" ||
		strings.TrimSpace(failure.Error) == "" {
		return Result{}, fmt.Errorf("记录恢复演练前置失败失败: 参数不完整")
	}
	startedAt := time.Now().UTC()
	id, err := generateVerificationID(startedAt)
	if err != nil {
		return Result{}, fmt.Errorf("生成 verification ID 失败: %w", err)
	}
	result := Result{
		ID: id, Host: failure.Host, RunID: failure.RunID,
		ManifestSnapshotID: failure.ManifestSnapshotID,
		StartedAt:          startedAt, FinishedAt: startedAt, Status: store.StatusFail,
		Targets:  append([]TargetEvidence(nil), failure.Targets...),
		Baseline: BaselineEvidence{Differences: []string{}}, Error: failure.Error,
	}
	recordErr := recordVerification(context.WithoutCancel(ctx), result, func(ctx context.Context, verification store.Verification) error {
		return state.RecordVerification(ctx, verification)
	})
	if recordErr != nil {
		result.Error = appendResultError(result.Error, "记录演练结果失败")
		return result, recordErr
	}
	return result, nil
}

func execute(
	ctx context.Context,
	plan restore.Plan,
	repo *restic.Repo,
	runner sshexec.Runner,
	options Options,
	dependencies executeDependencies,
) (result Result, runErr error) {
	if ctx == nil || runner == nil || dependencies.now == nil || dependencies.newID == nil ||
		dependencies.captureBaseline == nil || dependencies.isolate == nil || dependencies.executeRestore == nil ||
		dependencies.cleanup == nil || dependencies.validateOwnership == nil || dependencies.record == nil {
		return Result{}, fmt.Errorf("执行恢复演练失败: 参数或内部依赖不完整")
	}
	if strings.TrimSpace(plan.ManifestSnapshotID) == "" || strings.TrimSpace(plan.RunID) == "" ||
		strings.TrimSpace(plan.SourceHost) == "" || plan.SourceHost != plan.DestinationHost || plan.Isolation != nil {
		return Result{}, fmt.Errorf("执行恢复演练失败: 必须使用 source=destination 的未隔离完整 Plan")
	}
	startedAt := dependencies.now().UTC()
	verificationID, err := dependencies.newID(startedAt)
	if err != nil {
		return Result{}, fmt.Errorf("生成 verification ID 失败: %w", err)
	}
	result = Result{
		ID: verificationID, Host: plan.SourceHost, RunID: plan.RunID,
		ManifestSnapshotID: plan.ManifestSnapshotID, StartedAt: startedAt,
		Status: store.StatusFail, Targets: targetEvidence(plan),
		Baseline: BaselineEvidence{Differences: []string{}},
	}

	before, err := dependencies.captureBaseline(ctx, runner, plan)
	if err != nil {
		result.Error = "采集演练前生产基线失败"
		return finishVerification(ctx, result, errors.Join(runErr, err), dependencies)
	}
	result.Baseline.BeforeFingerprint = before.Fingerprint

	isolationPlan, err := dependencies.isolate(plan, restore.IsolationOptions{
		Purpose: restore.IsolationPurposeVerify, InstanceKey: verificationID,
		PortAllocation: restore.IsolationPortDisabled,
	})
	if err != nil {
		result.Error = "构建演练隔离计划失败"
		return finishVerification(ctx, result, errors.Join(runErr, err), dependencies)
	}
	rawFileTargets, err := isolatedRawFileTargets(isolationPlan, options.RawFileTargets)
	if err != nil {
		result.Error = "映射演练原始文件失败"
		return finishVerification(ctx, result, errors.Join(runErr, err), dependencies)
	}
	result.Restore, runErr = dependencies.executeRestore(ctx, isolationPlan, repo, runner, restore.ExecuteOptions{
		RawFileTargets: rawFileTargets,
	})
	if runErr == nil && result.Restore.Status == store.StatusFail {
		runErr = fmt.Errorf("隔离恢复返回失败状态")
	}
	if runErr != nil {
		result.Error = isolationRestoreFailureSummary(result.Restore.Error)
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verificationFinishTimeout)
	defer cancel()
	isolationID := isolationPlan.Isolation.ID
	if runErr != nil && options.KeepOnFailure {
		ownership, ownershipErr := dependencies.validateOwnership(finishCtx, runner, plan.DestinationHost, isolationID)
		if ownershipErr == nil {
			result.KeptOwnership = &ownership
			result.KeptOnFailure = true
		} else {
			cleanupResult, cleanupErr := dependencies.cleanup(finishCtx, runner, plan.DestinationHost, isolationID)
			result.Cleanup = &cleanupResult
			runErr = errors.Join(runErr, ownershipErr, cleanupErr)
		}
	} else {
		cleanupResult, cleanupErr := dependencies.cleanup(finishCtx, runner, plan.DestinationHost, isolationID)
		result.Cleanup = &cleanupResult
		if cleanupErr != nil {
			result.Error = "清理演练资源失败"
			runErr = errors.Join(runErr, cleanupErr)
		}
	}

	after, afterErr := dependencies.captureBaseline(finishCtx, runner, plan)
	if afterErr != nil {
		result.Error = "采集演练后生产基线失败"
		runErr = errors.Join(runErr, afterErr)
	} else {
		result.Baseline.AfterFingerprint = after.Fingerprint
		result.Baseline.Differences = CompareBaselines(before, after)
		if len(result.Baseline.Differences) > 0 {
			result.Error = "生产资源基线发生变化"
			runErr = errors.Join(runErr, fmt.Errorf("生产资源基线发生变化: %s", strings.Join(result.Baseline.Differences, ", ")))
		}
	}

	if runErr == nil {
		result.Status = result.Restore.Status
		if result.Status != store.StatusWarn {
			result.Status = store.StatusOK
		}
	} else {
		result.Status = store.StatusFail
	}
	return finishVerification(finishCtx, result, runErr, dependencies)
}

// isolationRestoreFailureSummary 只传播 restore.Result 已承诺脱敏的摘要。
// 通用 restore 文案不重复拼接，避免得到没有额外诊断价值的嵌套错误。
func isolationRestoreFailureSummary(restoreSummary string) string {
	restoreSummary = strings.TrimSpace(restoreSummary)
	if restoreSummary == "" || restoreSummary == "恢复未完成" {
		return "隔离恢复未完成"
	}
	return "隔离恢复未完成：" + restoreSummary
}

func finishVerification(
	ctx context.Context,
	result Result,
	runErr error,
	dependencies executeDependencies,
) (Result, error) {
	finishedAt := dependencies.now().UTC()
	if finishedAt.Before(result.StartedAt) {
		finishedAt = result.StartedAt
	}
	result.FinishedAt = finishedAt
	result.Duration = finishedAt.Sub(result.StartedAt)
	if runErr != nil {
		result.Status = store.StatusFail
		if result.Error == "" {
			result.Error = "恢复演练未完成"
		}
	}
	recordErr := recordVerification(ctx, result, dependencies.record)
	if recordErr != nil {
		result.Status = store.StatusFail
		result.Error = appendResultError(result.Error, "记录演练结果失败")
		runErr = errors.Join(runErr, recordErr)
	}
	return result, runErr
}

// appendResultError 保留先发生的业务阶段，并把收尾失败追加到同一脱敏摘要。
func appendResultError(current string, next string) string {
	if current == "" {
		return next
	}
	return current + "；" + next
}

func recordVerification(
	ctx context.Context,
	result Result,
	record func(context.Context, store.Verification) error,
) error {
	detail, err := verificationDetailJSON(result)
	if err != nil {
		return err
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verificationFinishTimeout)
	defer cancel()
	return record(recordCtx, store.Verification{
		ID: result.ID, Host: result.Host, RunID: result.RunID, SnapshotID: result.ManifestSnapshotID,
		StartedAt: result.StartedAt, FinishedAt: result.FinishedAt, Duration: result.Duration,
		Status: result.Status, Error: result.Error, DetailJSON: detail,
	})
}

func verificationDetailJSON(result Result) (json.RawMessage, error) {
	detail := struct {
		SchemaVersion int                         `json:"schema_version"`
		Host          string                      `json:"host"`
		SourceHost    string                      `json:"source_host"`
		RunID         string                      `json:"run_id"`
		SnapshotID    string                      `json:"manifest_snapshot_id"`
		Status        store.Status                `json:"status"`
		Targets       []TargetEvidence            `json:"targets"`
		Restore       restore.Result              `json:"restore"`
		Baseline      BaselineEvidence            `json:"baseline"`
		Cleanup       *restore.CleanupResult      `json:"cleanup,omitempty"`
		Ownership     *restore.IsolationOwnership `json:"kept_ownership,omitempty"`
		Kept          bool                        `json:"kept_on_failure"`
		Error         string                      `json:"error,omitempty"`
	}{
		verificationDetailSchemaVersion, result.Host, result.Host, result.RunID, result.ManifestSnapshotID,
		result.Status, result.Targets, result.Restore, result.Baseline, result.Cleanup, result.KeptOwnership,
		result.KeptOnFailure, result.Error,
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("编码 verification detail 失败: %w", err)
	}
	return payload, nil
}

func targetEvidence(plan restore.Plan) []TargetEvidence {
	seen := make(map[string]struct{})
	result := make([]TargetEvidence, 0)
	for _, step := range plan.Steps {
		if step.TargetID == "" || step.SnapshotID == "" {
			continue
		}
		key := step.TargetID + "\x00" + step.SnapshotID
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, TargetEvidence{
			TargetID: step.TargetID, TargetType: string(step.TargetType), SnapshotID: step.SnapshotID,
		})
	}
	return result
}

func isolatedRawFileTargets(plan restore.Plan, original map[string]string) (map[string]string, error) {
	if len(original) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(original))
	for targetID, targetPath := range original {
		mapped, err := restore.IsolationPath(plan, targetPath)
		if err != nil {
			return nil, err
		}
		result[targetID] = mapped
	}
	return result, nil
}

func generateVerificationID(startedAt time.Time) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "verify-" + startedAt.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(random[:]), nil
}
