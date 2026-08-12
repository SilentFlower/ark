package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/store"
)

const targetRecordTimeout = 10 * time.Second

// TargetResult 是单个 target 完成完整性判定后的最终业务结果。
type TargetResult struct {
	// Host 是清单中的 host 标识。
	Host string
	// TargetID 是 config.Target.ID 返回的稳定 target 标识。
	TargetID string
	// TargetType 是 target 的清单类型。
	TargetType config.TargetType
	// Status 是最终 ok、warn 或 fail 状态。
	Status store.Status
	// Bytes 是实际从执行器数据流读取并交给 restic 的字节数。
	Bytes int64
	// Duration 是备份、上游校验、必要撤销和历史比较的总耗时。
	Duration time.Duration
	// SnapshotID 是 restic 返回的快照 ID；失败撤销后仍保留用于审计。
	SnapshotID string
	// Error 是可写入状态库和 manifest 的脱敏错误或警告原因。
	Error string
	// ImageDigests 仅对 image_digest target 非空。
	ImageDigests map[string]string
}

// BackupTarget 把执行器数据流写入 restic，完成完整性判定并持久化最终状态。
// @param ctx 控制 restic、上游 Wait、坏快照撤销和状态库操作。
// @param runID 已由调用方创建的整体运行 ID。
// @param source Execute 返回的 target 数据流和稳定元数据。
// @param repo 已初始化的 restic 仓库。
// @param state 已创建当前 run 的状态库。
// @return TargetResult 可直接供 manifest 和整体运行聚合使用的最终结果。
// @return error target 失败、撤销失败、状态库失败或 context 取消时的组合错误。
func BackupTarget(
	ctx context.Context,
	runID string,
	source *Result,
	repo *restic.Repo,
	state *store.Store,
) (TargetResult, error) {
	if repo == nil {
		return TargetResult{}, fmt.Errorf("备份 target 失败: restic repo 不能为空")
	}
	if state == nil {
		return TargetResult{}, fmt.Errorf("备份 target 失败: store 不能为空")
	}
	return backupTarget(ctx, runID, source, targetDependencies{
		backupStdin:               repo.BackupStdin,
		forgetSnapshot:            repo.ForgetSnapshot,
		lastSuccessfulTargetBytes: state.LastSuccessfulTargetBytes,
		recordRunTarget:           state.RecordRunTarget,
		now:                       time.Now,
	})
}

type targetDependencies struct {
	backupStdin               func(context.Context, io.Reader, string, []string) (restic.Snapshot, error)
	forgetSnapshot            func(context.Context, string) error
	lastSuccessfulTargetBytes func(context.Context, string, string) (int64, bool, error)
	recordRunTarget           func(context.Context, store.RunTarget) error
	now                       func() time.Time
}

type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

func backupTarget(
	ctx context.Context,
	runID string,
	source *Result,
	dependencies targetDependencies,
) (TargetResult, error) {
	if err := validateTargetBackupInput(ctx, runID, source, dependencies); err != nil {
		return TargetResult{}, err
	}

	result := TargetResult{
		Host:         source.Host,
		TargetID:     source.TargetID,
		TargetType:   source.TargetType,
		ImageDigests: cloneStringMap(source.ImageDigests),
	}
	startedAt := dependencies.now()
	counter := &countingReader{reader: source.Reader}
	tags := []string{"host:" + source.Host, "target:" + source.TargetID, "run:" + runID}
	snapshot, backupErr := dependencies.backupStdin(
		ctx,
		counter,
		source.StdinFilename,
		tags,
	)
	result.Bytes = counter.bytes
	result.SnapshotID = snapshot.ID

	var waitErr error
	var closeErr error
	if backupErr != nil {
		// restic 提前失败时先关闭上游 stdout，避免远程进程继续阻塞写入，再执行 Wait 回收。
		closeErr = source.Reader.Close()
		waitErr = source.Wait()
	} else {
		// restic 已经读到 EOF，必须先确认上游真实退出状态，再释放 reader。
		waitErr = source.Wait()
		closeErr = source.Reader.Close()
	}

	if backupErr != nil {
		var forgetErr error
		if strings.TrimSpace(snapshot.ID) != "" {
			// restic 可能在提交 snapshot 并输出 summary 后，才因上游流错误返回非零。
			// 只要拿到了精确 ID，就必须撤销，不能留下状态库不可见的截断快照。
			forgetErr = dependencies.forgetSnapshot(ctx, snapshot.ID)
		}
		failure := errors.Join(
			fmt.Errorf("备份 target %q 到 restic 失败: %w", source.TargetID, backupErr),
			wrapTargetCloseError(source.TargetID, closeErr),
			wrapTargetWaitError(source.TargetID, waitErr),
			wrapForgetError(snapshot.ID, forgetErr),
		)
		safeMessage := targetFailureSummary(
			source.TargetID,
			"restic 备份失败",
			stageIfError(closeErr, "关闭数据流失败"),
			stageIfError(waitErr, "上游命令失败"),
			stageIfError(forgetErr, "撤销坏快照失败"),
		)
		return persistTargetResult(
			ctx, runID, startedAt, result, store.StatusFail, failure, safeMessage, dependencies,
		)
	}

	if strings.TrimSpace(snapshot.ID) == "" {
		failure := errors.Join(
			fmt.Errorf("备份 target %q 失败: restic 未返回 snapshot ID", source.TargetID),
			wrapTargetCloseError(source.TargetID, closeErr),
			wrapTargetWaitError(source.TargetID, waitErr),
		)
		safeMessage := targetFailureSummary(
			source.TargetID,
			"restic 未返回 snapshot ID",
			stageIfError(closeErr, "关闭数据流失败"),
			stageIfError(waitErr, "上游命令失败"),
		)
		return persistTargetResult(
			ctx, runID, startedAt, result, store.StatusFail, failure, safeMessage, dependencies,
		)
	}

	if waitErr != nil {
		forgetErr := dependencies.forgetSnapshot(ctx, snapshot.ID)
		failure := errors.Join(
			wrapTargetWaitError(source.TargetID, waitErr),
			wrapTargetCloseError(source.TargetID, closeErr),
			wrapForgetError(snapshot.ID, forgetErr),
		)
		safeMessage := targetFailureSummary(
			source.TargetID,
			"上游命令失败",
			stageIfError(closeErr, "关闭数据流失败"),
			stageIfError(forgetErr, "撤销坏快照失败"),
		)
		return persistTargetResult(
			ctx, runID, startedAt, result, store.StatusFail, failure, safeMessage, dependencies,
		)
	}
	if closeErr != nil {
		return persistTargetResult(
			ctx,
			runID,
			startedAt,
			result,
			store.StatusFail,
			wrapTargetCloseError(source.TargetID, closeErr),
			targetFailureSummary(source.TargetID, "关闭数据流失败"),
			dependencies,
		)
	}

	previousBytes, found, historyErr := dependencies.lastSuccessfulTargetBytes(
		ctx,
		source.Host,
		source.TargetID,
	)
	if historyErr != nil {
		failure := fmt.Errorf("查询 target %q 历史字节数失败: %w", source.TargetID, historyErr)
		return persistTargetResult(
			ctx,
			runID,
			startedAt,
			result,
			store.StatusFail,
			failure,
			targetFailureSummary(source.TargetID, "查询历史字节数失败"),
			dependencies,
		)
	}
	if found && previousBytes > 0 && belowHalf(result.Bytes, previousBytes) {
		warning := fmt.Sprintf(
			"target %q 当前字节数 %d 低于上次成功值 %d 的 50%%",
			source.TargetID,
			result.Bytes,
			previousBytes,
		)
		result.Error = warning
		return persistTargetResult(ctx, runID, startedAt, result, store.StatusWarn, nil, "", dependencies)
	}

	return persistTargetResult(ctx, runID, startedAt, result, store.StatusOK, nil, "", dependencies)
}

func validateTargetBackupInput(
	ctx context.Context,
	runID string,
	source *Result,
	dependencies targetDependencies,
) error {
	if ctx == nil {
		return fmt.Errorf("备份 target 失败: context 不能为空")
	}
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("备份 target 失败: run ID 不能为空")
	}
	if source == nil {
		return fmt.Errorf("备份 target 失败: source 不能为空")
	}
	if strings.TrimSpace(source.Host) == "" || strings.TrimSpace(source.TargetID) == "" || source.TargetType == "" {
		return fmt.Errorf("备份 target 失败: source host、target ID 和类型不能为空")
	}
	if strings.TrimSpace(source.StdinFilename) == "" || source.Reader == nil || source.Wait == nil {
		return fmt.Errorf("备份 target %q 失败: source filename、Reader 和 Wait 不能为空", source.TargetID)
	}
	if dependencies.backupStdin == nil || dependencies.forgetSnapshot == nil ||
		dependencies.lastSuccessfulTargetBytes == nil || dependencies.recordRunTarget == nil ||
		dependencies.now == nil {
		return fmt.Errorf("备份 target %q 失败: 内部依赖不完整", source.TargetID)
	}
	return nil
}

func persistTargetResult(
	ctx context.Context,
	runID string,
	startedAt time.Time,
	result TargetResult,
	status store.Status,
	cause error,
	safeMessage string,
	dependencies targetDependencies,
) (TargetResult, error) {
	result.Status = status
	result.Duration = elapsed(startedAt, dependencies.now())
	if cause != nil {
		if safeMessage == "" {
			safeMessage = targetFailureSummary(result.TargetID, "内部处理失败")
		}
		result.Error = safeMessage
	}

	recordCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		// 用户取消不能抹掉已经发生的失败事实；状态库写入使用短时收尾 context，
		// 但 restic 撤销仍保留原 context，避免取消后启动无界外部命令。
		recordCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), targetRecordTimeout)
	}
	defer cancel()
	recordErr := dependencies.recordRunTarget(recordCtx, store.RunTarget{
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
	if recordErr == nil {
		return result, cause
	}

	persistErr := fmt.Errorf("持久化 target %q 最终结果失败: %w", result.TargetID, recordErr)
	combined := errors.Join(cause, persistErr)
	result.Status = store.StatusFail
	safePersistMessage := fmt.Sprintf("target %q 最终结果持久化失败", result.TargetID)
	if result.Error == "" {
		result.Error = safePersistMessage
	} else {
		result.Error += "；" + safePersistMessage
	}
	return result, combined
}

func targetFailureSummary(targetID string, stages ...string) string {
	filtered := make([]string, 0, len(stages))
	for _, stage := range stages {
		if stage != "" {
			filtered = append(filtered, stage)
		}
	}
	return fmt.Sprintf("target %q 失败: %s", targetID, strings.Join(filtered, "；"))
}

func stageIfError(err error, stage string) string {
	if err == nil {
		return ""
	}
	return stage
}

func belowHalf(current, previous int64) bool {
	if current < 0 || previous <= 0 {
		return false
	}
	// ceil(previous/2) 等价于 current*2 < previous，但不会在大值时溢出。
	threshold := previous / 2
	if previous%2 != 0 {
		threshold++
	}
	return current < threshold
}

func elapsed(start, finish time.Time) time.Duration {
	if finish.Before(start) {
		return 0
	}
	return finish.Sub(start)
}

func wrapTargetCloseError(targetID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("关闭 target %q 数据流失败: %w", targetID, err)
}

func wrapTargetWaitError(targetID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("等待 target %q 上游命令失败: %w", targetID, err)
}

func wrapForgetError(snapshotID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("撤销坏快照 %q 失败: %w", snapshotID, err)
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
