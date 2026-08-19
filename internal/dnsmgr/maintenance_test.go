package dnsmgr

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type taskActiveCall struct {
	taskID      int64
	active      bool
	contextDone bool
}

type scriptedTaskActivator struct {
	calls   []taskActiveCall
	results []error
}

func (a *scriptedTaskActivator) SetTaskActive(ctx context.Context, taskID int64, active bool) error {
	a.calls = append(a.calls, taskActiveCall{taskID: taskID, active: active, contextDone: ctx.Err() != nil})
	if len(a.results) == 0 {
		return nil
	}
	err := a.results[0]
	a.results = a.results[1:]
	return err
}

func TestPauseTasks_按序暂停(t *testing.T) {
	activator := &scriptedTaskActivator{}
	result, err := PauseTasks(context.Background(), activator, MaintenancePlan{TaskIDs: []int64{21, 34}})
	if err != nil || result.Status != "paused" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	want := []taskActiveCall{
		{taskID: 21, active: false},
		{taskID: 34, active: false},
	}
	if !reflect.DeepEqual(activator.calls, want) {
		t.Fatalf("调用 = %#v，期望 %#v", activator.calls, want)
	}
}

func TestPauseTasks_中途失败后逆序恢复(t *testing.T) {
	activator := &scriptedTaskActivator{results: []error{nil, nil, errors.New("pause failed"), nil, nil}}
	result, err := PauseTasks(context.Background(), activator, MaintenancePlan{TaskIDs: []int64{21, 34, 55}})
	if err == nil || result.Status != "rolled_back" || len(result.ManualTaskIDs) != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	want := []taskActiveCall{
		{taskID: 21, active: false},
		{taskID: 34, active: false},
		{taskID: 55, active: false},
		{taskID: 55, active: true},
		{taskID: 34, active: true},
		{taskID: 21, active: true},
	}
	if !reflect.DeepEqual(activator.calls, want) {
		t.Fatalf("调用 = %#v，期望 %#v", activator.calls, want)
	}
	if result.Tasks[0].ResumeStatus != "restored" || result.Tasks[1].ResumeStatus != "restored" ||
		result.Tasks[2].PauseStatus != "failed" || result.Tasks[2].ResumeStatus != "restored" {
		t.Fatalf("任务结果 = %#v", result.Tasks)
	}
}

func TestPauseTasks_补偿失败继续并返回人工任务(t *testing.T) {
	activator := &scriptedTaskActivator{results: []error{
		nil, nil, errors.New("pause failed"), errors.New("resume 55 failed"),
		errors.New("resume 34 failed"), errors.New("resume 21 failed"),
	}}
	result, err := PauseTasks(context.Background(), activator, MaintenancePlan{TaskIDs: []int64{21, 34, 55}})
	if err == nil || result.Status != "rollback_failed" || !reflect.DeepEqual(result.ManualTaskIDs, []int64{55, 34, 21}) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(activator.calls) != 6 {
		t.Fatalf("恢复失败后仍应处理全部任务，调用=%#v", activator.calls)
	}
}

func TestPauseTasks_NilContext返回结构化失败(t *testing.T) {
	activator := &scriptedTaskActivator{}
	result, err := PauseTasks(nil, activator, MaintenancePlan{TaskIDs: []int64{21, 34}})
	if err == nil || !strings.Contains(err.Error(), "context 不能为空") || result.Status != "rollback_failed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(activator.calls) != 0 || !reflect.DeepEqual(result.Tasks, []MaintenanceTaskResult{
		{TaskID: 21, PauseStatus: "not_attempted"},
		{TaskID: 34, PauseStatus: "not_attempted"},
	}) {
		t.Fatalf("nil context 不应发请求，result=%#v calls=%#v", result, activator.calls)
	}
}

func TestResumeTasks_取消后仍逆序恢复且继续收集失败(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	activator := &scriptedTaskActivator{results: []error{errors.New("resume 34 failed"), nil}}
	paused := MaintenanceResult{Status: "paused", Tasks: []MaintenanceTaskResult{
		{TaskID: 21, PauseStatus: "paused"},
		{TaskID: 34, PauseStatus: "paused"},
	}}
	result, err := ResumeTasks(ctx, activator, paused)
	if err == nil || result.Status != "restore_failed" || !reflect.DeepEqual(result.ManualTaskIDs, []int64{34}) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	want := []taskActiveCall{
		{taskID: 34, active: true, contextDone: false},
		{taskID: 21, active: true, contextDone: false},
	}
	if !reflect.DeepEqual(activator.calls, want) {
		t.Fatalf("调用 = %#v，期望 %#v", activator.calls, want)
	}
}

func TestResumeTasks_NilContext标记全部人工恢复(t *testing.T) {
	activator := &scriptedTaskActivator{}
	paused := MaintenanceResult{Status: "paused", Tasks: []MaintenanceTaskResult{
		{TaskID: 21, PauseStatus: "paused"},
		{TaskID: 34, PauseStatus: "paused"},
	}}
	result, err := ResumeTasks(nil, activator, paused)
	if err == nil || !strings.Contains(err.Error(), "context 不能为空") || result.Status != "restore_failed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(activator.calls) != 0 || !reflect.DeepEqual(result.ManualTaskIDs, []int64{34, 21}) ||
		result.Tasks[0].ResumeStatus != "failed" || result.Tasks[1].ResumeStatus != "failed" {
		t.Fatalf("nil context 结果不完整: result=%#v calls=%#v", result, activator.calls)
	}
}

func TestMaintenancePlanValidate(t *testing.T) {
	for _, plan := range []MaintenancePlan{
		{},
		{TaskIDs: []int64{0}},
		{TaskIDs: []int64{21, 21}},
	} {
		if err := plan.Validate(); err == nil {
			t.Fatalf("无效计划通过校验: %#v", plan)
		}
	}
}
