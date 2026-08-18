package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

var alertKinds = map[string]struct{}{
	"backup_overdue":              {},
	"backup_consecutive_failures": {},
	"verification_failed":         {},
}

// ListAlertStates 查询全部告警生命周期状态。
// @param ctx 控制数据库查询的取消与超时。
// @return []AlertState 按稳定 ID 升序排列的全部状态，NULL 时间还原为零值。
// @return error 数据库查询或时间还原失败时的错误。
func (s *Store) ListAlertStates(ctx context.Context) ([]AlertState, error) {
	states := make([]AlertState, 0)
	err := withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		rows, queryErr := conn.QueryContext(ctx, `
			SELECT id, host, kind, active, first_seen_at, last_seen_at,
				last_alert_sent_at, resolved_at, recovery_sent_at
			FROM alert_states
			ORDER BY id ASC`)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			state, scanErr := scanAlertState(rows)
			if scanErr != nil {
				return scanErr
			}
			states = append(states, state)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("查询告警状态失败: %w", err)
	}
	return states, nil
}

// SaveAlertState 新增或覆盖一条告警生命周期状态。
// @param ctx 控制数据库写入的取消与超时。
// @param state 已完成业务对账的完整状态。
// @return error 字段校验、时间转换或数据库写入失败时的错误。
func (s *Store) SaveAlertState(ctx context.Context, state AlertState) error {
	if err := validateAlertState(state); err != nil {
		return err
	}
	firstSeenAt, err := requiredUnixMilli(state.FirstSeenAt, "alert_state.first_seen_at")
	if err != nil {
		return err
	}
	lastSeenAt, err := requiredUnixMilli(state.LastSeenAt, "alert_state.last_seen_at")
	if err != nil {
		return err
	}
	lastAlertSentAt, err := nullableUnixMilli(state.LastAlertSentAt, "alert_state.last_alert_sent_at")
	if err != nil {
		return err
	}
	resolvedAt, err := nullableUnixMilli(state.ResolvedAt, "alert_state.resolved_at")
	if err != nil {
		return err
	}
	recoverySentAt, err := nullableUnixMilli(state.RecoverySentAt, "alert_state.recovery_sent_at")
	if err != nil {
		return err
	}

	active := 0
	if state.Active {
		active = 1
	}
	err = withBusyRetry(ctx, s.db, func(conn *sql.Conn) error {
		_, execErr := conn.ExecContext(ctx, `
			INSERT INTO alert_states (
				id, host, kind, active, first_seen_at, last_seen_at,
				last_alert_sent_at, resolved_at, recovery_sent_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				host = excluded.host,
				kind = excluded.kind,
				active = excluded.active,
				first_seen_at = excluded.first_seen_at,
				last_seen_at = excluded.last_seen_at,
				last_alert_sent_at = excluded.last_alert_sent_at,
				resolved_at = excluded.resolved_at,
				recovery_sent_at = excluded.recovery_sent_at`,
			state.ID, state.Host, state.Kind, active, firstSeenAt, lastSeenAt,
			lastAlertSentAt, resolvedAt, recoverySentAt,
		)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("保存告警状态 %q 失败: %w", state.ID, err)
	}
	return nil
}

type alertStateScanner interface {
	Scan(dest ...any) error
}

func scanAlertState(scanner alertStateScanner) (AlertState, error) {
	var state AlertState
	var active int
	var firstSeenAt int64
	var lastSeenAt int64
	var lastAlertSentAt sql.NullInt64
	var resolvedAt sql.NullInt64
	var recoverySentAt sql.NullInt64
	if err := scanner.Scan(
		&state.ID, &state.Host, &state.Kind, &active, &firstSeenAt, &lastSeenAt,
		&lastAlertSentAt, &resolvedAt, &recoverySentAt,
	); err != nil {
		return AlertState{}, err
	}
	state.Active = active == 1
	state.FirstSeenAt = time.UnixMilli(firstSeenAt).UTC()
	state.LastSeenAt = time.UnixMilli(lastSeenAt).UTC()
	if lastAlertSentAt.Valid {
		state.LastAlertSentAt = time.UnixMilli(lastAlertSentAt.Int64).UTC()
	}
	if resolvedAt.Valid {
		state.ResolvedAt = time.UnixMilli(resolvedAt.Int64).UTC()
	}
	if recoverySentAt.Valid {
		state.RecoverySentAt = time.UnixMilli(recoverySentAt.Int64).UTC()
	}
	return state, nil
}

func validateAlertState(state AlertState) error {
	if strings.TrimSpace(state.ID) == "" {
		return fmt.Errorf("保存告警状态失败: alert_state.id 不能为空")
	}
	if strings.TrimSpace(state.Host) == "" {
		return fmt.Errorf("保存告警状态失败: alert_state.host 不能为空")
	}
	if _, ok := alertKinds[state.Kind]; !ok {
		return fmt.Errorf("保存告警状态失败: alert_state.kind %q 非法", state.Kind)
	}
	if state.ID != state.Host+":"+state.Kind {
		return fmt.Errorf("保存告警状态失败: alert_state.id 必须等于 host:kind")
	}
	if state.FirstSeenAt.IsZero() || state.LastSeenAt.IsZero() {
		return fmt.Errorf("保存告警状态失败: first_seen_at 与 last_seen_at 不能为空")
	}
	if state.LastSeenAt.Before(state.FirstSeenAt) {
		return fmt.Errorf("保存告警状态失败: last_seen_at 不能早于 first_seen_at")
	}
	if !state.LastAlertSentAt.IsZero() && state.LastAlertSentAt.Before(state.FirstSeenAt) {
		return fmt.Errorf("保存告警状态失败: last_alert_sent_at 不能早于 first_seen_at")
	}
	if state.Active {
		if !state.ResolvedAt.IsZero() || !state.RecoverySentAt.IsZero() {
			return fmt.Errorf("保存告警状态失败: 活动告警不能包含恢复时间")
		}
		return nil
	}
	if state.ResolvedAt.IsZero() {
		return fmt.Errorf("保存告警状态失败: 已恢复告警必须包含 resolved_at")
	}
	if state.ResolvedAt.Before(state.FirstSeenAt) {
		return fmt.Errorf("保存告警状态失败: resolved_at 不能早于 first_seen_at")
	}
	if !state.RecoverySentAt.IsZero() && state.RecoverySentAt.Before(state.ResolvedAt) {
		return fmt.Errorf("保存告警状态失败: recovery_sent_at 不能早于 resolved_at")
	}
	return nil
}
