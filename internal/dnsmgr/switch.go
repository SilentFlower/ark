package dnsmgr

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Record 是恢复计划中的一条 dnsmgr 记录关联。
type Record struct {
	// DomainID 是 dnsmgr 本地 domain ID。
	DomainID int64 `json:"domain_id"`
	// RecordID 是 provider 的稳定记录 ID。
	RecordID string `json:"record_id"`
}

// Plan 是可稳定序列化且不含凭证的 DNS 切换计划。
type Plan struct {
	// Value 是所有关联记录要切换到的目标 IP。
	Value string `json:"value"`
	// Records 按清单顺序保存记录关联。
	Records []Record `json:"records"`
}

// Validate 校验 DNS 计划的目标 IP、记录标识与重复项。
// @return error 计划为空、字段无效或记录重复时的错误。
func (p Plan) Validate() error {
	if net.ParseIP(strings.TrimSpace(p.Value)) == nil {
		return fmt.Errorf("DNS 计划目标值不是合法 IP")
	}
	if len(p.Records) == 0 {
		return fmt.Errorf("DNS 计划至少需要一条记录")
	}
	seen := make(map[string]struct{}, len(p.Records))
	for index, record := range p.Records {
		if record.DomainID <= 0 || strings.TrimSpace(record.RecordID) == "" {
			return fmt.Errorf("DNS 计划 records[%d] 标识无效", index)
		}
		key := fmt.Sprintf("%d\x00%s", record.DomainID, strings.TrimSpace(record.RecordID))
		if _, exists := seen[key]; exists {
			return fmt.Errorf("DNS 计划 records[%d] 重复", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// RecordResult 是一条记录的前向更新与可选补偿结果。
type RecordResult struct {
	// DomainID 是 dnsmgr 本地 domain ID。
	DomainID int64 `json:"domain_id"`
	// RecordID 是 provider 的稳定记录 ID。
	RecordID string `json:"record_id"`
	// Status 是 updated、unchanged、failed 或 not_attempted。
	Status string `json:"status"`
	// RollbackStatus 是 rolled_back、unchanged 或 failed；未补偿时为空。
	RollbackStatus string `json:"rollback_status,omitempty"`
}

// SwitchResult 是一次多记录 DNS 切换及补偿的结构化结果。
type SwitchResult struct {
	// Status 是 ok、rolled_back 或 rollback_failed。
	Status string `json:"status"`
	// Records 按实际尝试顺序保存记录结果。
	Records []RecordResult `json:"records"`
	// ManualRecords 是补偿失败后需要人工核对的记录。
	ManualRecords []Record `json:"manual_records,omitempty"`
	// Error 是不含外部响应或凭证的失败摘要。
	Error string `json:"error,omitempty"`
}

// ValueSetter 是 DNS 切换编排所需的最小 Value-only client 契约。
type ValueSetter interface {
	// SetRecordValue 更新一条记录，并可用 expectedValue 保护补偿操作。
	// @param ctx 控制调用取消与超时。
	// @param domainID dnsmgr 本地 domain ID。
	// @param recordID provider 记录 ID。
	// @param value 新 IP。
	// @param expectedValue 可选当前值约束。
	// @return ValueResult 更新前后值与 changed 状态。
	// @return error 调用或返回契约失败时的脱敏错误。
	SetRecordValue(context.Context, int64, string, string, *string) (ValueResult, error)
}

type rollbackEntry struct {
	resultIndex   int
	record        Record
	previousValue string
}

// Switch 按计划顺序切换记录，并在部分失败时逆序补偿已变更记录。
// @param ctx 控制全部前向与补偿调用的取消和超时。
// @param setter Value-only client。
// @param plan 不含凭证的 DNS 切换计划。
// @return SwitchResult 每条前向与补偿状态，以及可能的人工处理记录。
// @return error 任一前向更新失败时的脱敏错误；即使补偿完整也返回错误。
func Switch(ctx context.Context, setter ValueSetter, plan Plan) (SwitchResult, error) {
	result := SwitchResult{Records: make([]RecordResult, 0, len(plan.Records))}
	if setter == nil {
		result.Status = "rollback_failed"
		result.Error = "DNS 切换未执行"
		return result, fmt.Errorf("执行 DNS 切换失败: client 不能为空")
	}
	if err := plan.Validate(); err != nil {
		result.Status = "rollback_failed"
		result.Error = "DNS 切换计划无效"
		return result, fmt.Errorf("执行 DNS 切换失败: %w", err)
	}
	var rollbackStack []rollbackEntry
	for recordIndex, record := range plan.Records {
		item := RecordResult{DomainID: record.DomainID, RecordID: record.RecordID}
		updated, err := setter.SetRecordValue(ctx, record.DomainID, record.RecordID, plan.Value, nil)
		if err != nil {
			item.Status = "failed"
			result.Records = append(result.Records, item)
			for _, pending := range plan.Records[recordIndex+1:] {
				result.Records = append(result.Records, RecordResult{
					DomainID: pending.DomainID, RecordID: pending.RecordID, Status: "not_attempted",
				})
			}
			rollbackDNS(ctx, setter, plan.Value, rollbackStack, &result)
			if len(result.ManualRecords) == 0 {
				result.Status = "rolled_back"
				result.Error = "DNS 切换失败，已恢复本轮变更"
			} else {
				result.Status = "rollback_failed"
				result.Error = "DNS 切换失败且回滚不完整"
			}
			return result, fmt.Errorf("更新 dnsmgr 记录 %d/%s 失败: %w", record.DomainID, record.RecordID, err)
		}
		if updated.Changed {
			item.Status = "updated"
			rollbackStack = append(rollbackStack, rollbackEntry{
				resultIndex: len(result.Records), record: record, previousValue: updated.PreviousValue,
			})
		} else {
			item.Status = "unchanged"
		}
		result.Records = append(result.Records, item)
	}
	result.Status = "ok"
	return result, nil
}

func rollbackDNS(
	ctx context.Context,
	setter ValueSetter,
	targetValue string,
	stack []rollbackEntry,
	result *SwitchResult,
) {
	for index := len(stack) - 1; index >= 0; index-- {
		entry := stack[index]
		// 主恢复 context 可能正因取消而失败；补偿必须拥有独立且有上限的时间窗口。
		rollbackCtx, cancel := context.WithTimeout(context.Background(), defaultHTTPTimeout)
		rolledBack, err := setter.SetRecordValue(
			rollbackCtx,
			entry.record.DomainID,
			entry.record.RecordID,
			entry.previousValue,
			&targetValue,
		)
		cancel()
		if err != nil {
			result.Records[entry.resultIndex].RollbackStatus = "failed"
			result.ManualRecords = append(result.ManualRecords, entry.record)
			continue
		}
		if rolledBack.Changed {
			result.Records[entry.resultIndex].RollbackStatus = "rolled_back"
		} else {
			result.Records[entry.resultIndex].RollbackStatus = "unchanged"
		}
	}
}
