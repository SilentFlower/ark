package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_AlertStateRoundTrip与Upsert(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 18, 8, 0, 0, 123000000, time.FixedZone("CST", 8*60*60))
	active := AlertState{
		ID: "web-01:backup_overdue", Host: "web-01", Kind: "backup_overdue", Active: true,
		FirstSeenAt: base, LastSeenAt: base.Add(time.Minute),
	}
	if err := state.SaveAlertState(context.Background(), active); err != nil {
		t.Fatalf("保存活动告警失败: %v", err)
	}

	states, err := state.ListAlertStates(context.Background())
	if err != nil || len(states) != 1 {
		t.Fatalf("告警状态 = %#v err=%v", states, err)
	}
	got := states[0]
	if !got.Active || !got.FirstSeenAt.Equal(base.UTC().Truncate(time.Millisecond)) ||
		!got.LastAlertSentAt.IsZero() || !got.ResolvedAt.IsZero() || !got.RecoverySentAt.IsZero() {
		t.Fatalf("活动告警还原错误: %#v", got)
	}

	active.LastAlertSentAt = base.Add(2 * time.Minute)
	active.LastSeenAt = base.Add(2 * time.Minute)
	active.Active = false
	active.ResolvedAt = base.Add(3 * time.Minute)
	active.RecoverySentAt = base.Add(4 * time.Minute)
	if err := state.SaveAlertState(context.Background(), active); err != nil {
		t.Fatalf("更新恢复告警失败: %v", err)
	}
	states, err = state.ListAlertStates(context.Background())
	if err != nil || len(states) != 1 || states[0].Active || states[0].RecoverySentAt.IsZero() {
		t.Fatalf("恢复告警状态 = %#v err=%v", states, err)
	}
}

func TestStore_SaveAlertState拒绝非法状态(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	base := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	valid := AlertState{
		ID: "web-01:backup_overdue", Host: "web-01", Kind: "backup_overdue",
		Active: true, FirstSeenAt: base, LastSeenAt: base,
	}
	tests := []struct {
		name    string
		mutate  func(*AlertState)
		wantSub string
	}{
		{name: "ID 不稳定", mutate: func(value *AlertState) { value.ID = "other" }, wantSub: "host:kind"},
		{name: "kind 非法", mutate: func(value *AlertState) { value.Kind = "unknown" }, wantSub: "kind"},
		{name: "观察时间倒序", mutate: func(value *AlertState) { value.LastSeenAt = base.Add(-time.Second) }, wantSub: "last_seen_at"},
		{name: "活动状态含恢复时间", mutate: func(value *AlertState) { value.ResolvedAt = base }, wantSub: "活动告警"},
		{name: "恢复状态缺少时间", mutate: func(value *AlertState) { value.Active = false }, wantSub: "resolved_at"},
		{name: "恢复通知早于恢复", mutate: func(value *AlertState) {
			value.Active = false
			value.ResolvedAt = base.Add(time.Minute)
			value.RecoverySentAt = base
		}, wantSub: "recovery_sent_at"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value := valid
			tc.mutate(&value)
			err := state.SaveAlertState(context.Background(), value)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("SaveAlertState 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestStore_AlertStateContext取消(t *testing.T) {
	state := openTestStore(t, filepath.Join(t.TempDir(), "ark.db"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := state.ListAlertStates(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("取消查询错误 = %v", err)
	}
}
