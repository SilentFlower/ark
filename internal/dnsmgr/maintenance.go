package dnsmgr

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const maintenanceCleanupTimeout = 30 * time.Second

// MaintenancePlan 是可稳定序列化且不含凭证的 dmonitor 维护计划。
type MaintenancePlan struct {
	// TaskIDs 按清单顺序保存维护窗口内需要暂停的任务。
	TaskIDs []int64 `json:"task_ids"`
}

// Validate 校验维护计划中的任务 ID 与重复项。
// @return error 计划为空、任务 ID 非正数或重复时的错误。
func (p MaintenancePlan) Validate() error {
	if len(p.TaskIDs) == 0 {
		return fmt.Errorf("dmonitor 维护计划至少需要一个任务")
	}
	seen := make(map[int64]struct{}, len(p.TaskIDs))
	for index, taskID := range p.TaskIDs {
		if taskID <= 0 {
			return fmt.Errorf("dmonitor 维护计划 task_ids[%d] 必须大于 0", index)
		}
		if _, exists := seen[taskID]; exists {
			return fmt.Errorf("dmonitor 维护计划 task_ids[%d] 重复", index)
		}
		seen[taskID] = struct{}{}
	}
	return nil
}

// MaintenanceTaskResult 是单个 dmonitor 任务的暂停与恢复结果。
type MaintenanceTaskResult struct {
	// TaskID 是 dnsmgr dmtask 表中的任务 ID。
	TaskID int64 `json:"task_id"`
	// PauseStatus 是 paused、failed 或 not_attempted。
	PauseStatus string `json:"pause_status"`
	// ResumeStatus 是 restored 或 failed；尚未恢复时为空。
	ResumeStatus string `json:"resume_status,omitempty"`
}

// MaintenanceResult 是一次 dmonitor 维护窗口的结构化结果。
type MaintenanceResult struct {
	// Status 是 not_started、paused、restored、rolled_back、rollback_failed 或 restore_failed。
	Status string `json:"status"`
	// Tasks 按清单顺序保存每个任务的暂停与恢复状态。
	Tasks []MaintenanceTaskResult `json:"tasks"`
	// ManualTaskIDs 是恢复失败后需要人工启用的任务。
	ManualTaskIDs []int64 `json:"manual_task_ids,omitempty"`
	// Error 是不含外部响应或凭证的失败摘要。
	Error string `json:"error,omitempty"`
}

// TaskActivator 是维护窗口编排所需的最小 dmonitor client 契约。
type TaskActivator interface {
	// SetTaskActive 启用或暂停单个 dmonitor 任务。
	// @param ctx 控制调用取消与超时。
	// @param taskID dnsmgr dmtask 表中的任务 ID。
	// @param active true 表示启用，false 表示暂停。
	// @return error 调用或返回契约失败时的脱敏错误。
	SetTaskActive(context.Context, int64, bool) error
}

// RestoreClient 是恢复流程复用的 dnsmgr client 契约。
type RestoreClient interface {
	ValueSetter
	TaskActivator
}

// PauseTasks 按计划顺序暂停任务，并在中途失败时逆序恢复已暂停任务。
// @param ctx 控制前向暂停；补偿会脱离已取消的 ctx 并使用固定总超时。
// @param activator dmonitor 任务启停 client。
// @param plan 不含凭证的维护计划。
// @return MaintenanceResult 每个任务的暂停、补偿和人工处理状态。
// @return error 计划、暂停或补偿失败时的脱敏错误。
func PauseTasks(ctx context.Context, activator TaskActivator, plan MaintenancePlan) (MaintenanceResult, error) {
	result := newMaintenanceResult(plan)
	if err := plan.Validate(); err != nil {
		result.Status = "rollback_failed"
		result.Error = "dmonitor 维护计划无效"
		return result, fmt.Errorf("暂停 dmonitor 任务失败: %w", err)
	}
	if ctx == nil {
		result.Status = "rollback_failed"
		result.Error = "dmonitor 任务暂停未执行"
		return result, fmt.Errorf("暂停 dmonitor 任务失败: context 不能为空")
	}
	if activator == nil {
		result.Status = "rollback_failed"
		result.Error = "dmonitor 任务暂停未执行"
		return result, fmt.Errorf("暂停 dmonitor 任务失败: client 不能为空")
	}

	for index, taskID := range plan.TaskIDs {
		item := &result.Tasks[index]
		if err := activator.SetTaskActive(ctx, taskID, false); err != nil {
			item.PauseStatus = "failed"
			// HTTP 失败不能证明服务端未完成写入，因此当前失败项也必须幂等恢复为 active=1。
			resumeErr := resumeMaintenanceTasks(ctx, activator, &result, true)
			if resumeErr == nil {
				result.Status = "rolled_back"
				result.Error = "dmonitor 任务暂停失败，已恢复本轮可能暂停的任务"
			} else {
				result.Status = "rollback_failed"
				result.Error = "dmonitor 任务暂停失败且恢复不完整"
			}
			return result, errors.Join(
				fmt.Errorf("暂停 dnsmgr dmonitor 任务 %d 失败: %w", taskID, err),
				resumeErr,
			)
		}
		item.PauseStatus = "paused"
	}
	result.Status = "paused"
	return result, nil
}

// ResumeTasks 逆序恢复本轮已暂停任务，单项失败时继续处理其余任务。
// @param ctx 原命令 context；函数会脱离其取消信号并使用固定总超时。
// @param activator dmonitor 任务启停 client。
// @param result PauseTasks 返回的维护结果。
// @return MaintenanceResult 更新后的恢复状态和人工任务。
// @return error 任一任务恢复失败时的聚合脱敏错误。
func ResumeTasks(ctx context.Context, activator TaskActivator, result MaintenanceResult) (MaintenanceResult, error) {
	if ctx == nil {
		markPendingResumesFailed(&result, false)
		result.Status = "restore_failed"
		result.Error = "dmonitor 任务恢复未执行"
		return result, fmt.Errorf("恢复 dmonitor 任务失败: context 不能为空")
	}
	if activator == nil {
		markPendingResumesFailed(&result, false)
		result.Status = "restore_failed"
		result.Error = "dmonitor 任务恢复未执行"
		return result, fmt.Errorf("恢复 dmonitor 任务失败: client 不能为空")
	}
	if err := resumeMaintenanceTasks(ctx, activator, &result, false); err != nil {
		result.Status = "restore_failed"
		result.Error = "dmonitor 任务恢复不完整"
		return result, err
	}
	result.Status = "restored"
	result.Error = ""
	return result, nil
}

func newMaintenanceResult(plan MaintenancePlan) MaintenanceResult {
	tasks := make([]MaintenanceTaskResult, len(plan.TaskIDs))
	for index, taskID := range plan.TaskIDs {
		tasks[index] = MaintenanceTaskResult{TaskID: taskID, PauseStatus: "not_attempted"}
	}
	return MaintenanceResult{Tasks: tasks}
}

func resumeMaintenanceTasks(
	ctx context.Context,
	activator TaskActivator,
	result *MaintenanceResult,
	includePauseFailure bool,
) error {
	if ctx == nil {
		return fmt.Errorf("恢复 dmonitor 任务失败: context 不能为空")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maintenanceCleanupTimeout)
	defer cancel()

	result.ManualTaskIDs = nil
	var errs []error
	for index := len(result.Tasks) - 1; index >= 0; index-- {
		item := &result.Tasks[index]
		if !shouldResumeMaintenanceTask(*item, includePauseFailure) || item.ResumeStatus == "restored" {
			continue
		}
		if err := activator.SetTaskActive(cleanupCtx, item.TaskID, true); err != nil {
			item.ResumeStatus = "failed"
			result.ManualTaskIDs = append(result.ManualTaskIDs, item.TaskID)
			errs = append(errs, fmt.Errorf("恢复 dnsmgr dmonitor 任务 %d 失败: %w", item.TaskID, err))
			continue
		}
		item.ResumeStatus = "restored"
	}
	return errors.Join(errs...)
}

func markPendingResumesFailed(result *MaintenanceResult, includePauseFailure bool) {
	result.ManualTaskIDs = nil
	for index := len(result.Tasks) - 1; index >= 0; index-- {
		item := &result.Tasks[index]
		if !shouldResumeMaintenanceTask(*item, includePauseFailure) || item.ResumeStatus == "restored" {
			continue
		}
		item.ResumeStatus = "failed"
		result.ManualTaskIDs = append(result.ManualTaskIDs, item.TaskID)
	}
}

func shouldResumeMaintenanceTask(item MaintenanceTaskResult, includePauseFailure bool) bool {
	return item.PauseStatus == "paused" || includePauseFailure && item.PauseStatus == "failed"
}
