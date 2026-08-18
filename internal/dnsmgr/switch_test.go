package dnsmgr

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type setCall struct {
	domainID int64
	recordID string
	value    string
	expected *string
}

type scriptedResult struct {
	result ValueResult
	err    error
}

type scriptedSetter struct {
	calls   []setCall
	results []scriptedResult
}

func (s *scriptedSetter) SetRecordValue(
	_ context.Context,
	domainID int64,
	recordID string,
	value string,
	expected *string,
) (ValueResult, error) {
	var copiedExpected *string
	if expected != nil {
		value := *expected
		copiedExpected = &value
	}
	s.calls = append(s.calls, setCall{domainID: domainID, recordID: recordID, value: value, expected: copiedExpected})
	result := s.results[0]
	s.results = s.results[1:]
	return result.result, result.err
}

func TestSwitch_顺序更新且幂等项不进入补偿栈(t *testing.T) {
	setter := &scriptedSetter{results: []scriptedResult{
		{result: ValueResult{PreviousValue: "203.0.113.10", Value: "203.0.113.10", Changed: false}},
		{result: ValueResult{PreviousValue: "198.51.100.2", Value: "203.0.113.10", Changed: true}},
	}}
	plan := Plan{Value: "203.0.113.10", Records: []Record{
		{DomainID: 1, RecordID: "a"},
		{DomainID: 1, RecordID: "b"},
	}}
	result, err := Switch(context.Background(), setter, plan)
	if err != nil || result.Status != "ok" {
		t.Fatalf("结果 = %#v, err=%v", result, err)
	}
	if got := []string{result.Records[0].Status, result.Records[1].Status}; !reflect.DeepEqual(got, []string{"unchanged", "updated"}) {
		t.Fatalf("记录状态 = %v", got)
	}
}

func TestSwitch_失败后逆序补偿(t *testing.T) {
	setter := &scriptedSetter{results: []scriptedResult{
		{result: ValueResult{PreviousValue: "198.51.100.1", Value: "203.0.113.10", Changed: true}},
		{result: ValueResult{PreviousValue: "198.51.100.2", Value: "203.0.113.10", Changed: true}},
		{err: errors.New("forward failed")},
		{result: ValueResult{PreviousValue: "203.0.113.10", Value: "198.51.100.2", Changed: true}},
		{result: ValueResult{PreviousValue: "203.0.113.10", Value: "198.51.100.1", Changed: true}},
	}}
	plan := Plan{Value: "203.0.113.10", Records: []Record{
		{DomainID: 1, RecordID: "a"},
		{DomainID: 1, RecordID: "b"},
		{DomainID: 1, RecordID: "c"},
	}}
	result, err := Switch(context.Background(), setter, plan)
	if err == nil || result.Status != "rolled_back" || len(result.ManualRecords) != 0 {
		t.Fatalf("结果 = %#v, err=%v", result, err)
	}
	wantOrder := []string{"a", "b", "c", "b", "a"}
	var gotOrder []string
	for _, call := range setter.calls {
		gotOrder = append(gotOrder, call.recordID)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("调用顺序 = %v，期望 %v", gotOrder, wantOrder)
	}
	for _, call := range setter.calls[3:] {
		if call.expected == nil || *call.expected != plan.Value {
			t.Fatalf("补偿 expected = %#v", call.expected)
		}
	}
}

func TestSwitch_补偿失败继续处理并返回人工记录(t *testing.T) {
	setter := &scriptedSetter{results: []scriptedResult{
		{result: ValueResult{PreviousValue: "198.51.100.1", Value: "203.0.113.10", Changed: true}},
		{result: ValueResult{PreviousValue: "198.51.100.2", Value: "203.0.113.10", Changed: true}},
		{err: errors.New("forward failed")},
		{err: errors.New("rollback failed")},
		{result: ValueResult{PreviousValue: "203.0.113.10", Value: "198.51.100.1", Changed: true}},
	}}
	plan := Plan{Value: "203.0.113.10", Records: []Record{
		{DomainID: 1, RecordID: "a"},
		{DomainID: 1, RecordID: "b"},
		{DomainID: 1, RecordID: "c"},
	}}
	result, err := Switch(context.Background(), setter, plan)
	if err == nil || result.Status != "rollback_failed" || len(result.ManualRecords) != 1 ||
		result.ManualRecords[0].RecordID != "b" {
		t.Fatalf("结果 = %#v, err=%v", result, err)
	}
	if result.Records[0].RollbackStatus != "rolled_back" || result.Records[1].RollbackStatus != "failed" {
		t.Fatalf("补偿状态 = %#v", result.Records)
	}
}
