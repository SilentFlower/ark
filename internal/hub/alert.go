package hub

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/monitoring"
	"github.com/silentflower/ark/internal/store"
)

const (
	alertEvaluationInterval = time.Minute
	alertSilenceInterval    = 24 * time.Hour
)

type alertStore interface {
	ListAlertStates(context.Context) ([]store.AlertState, error)
	SaveAlertState(context.Context, store.AlertState) error
}

type alertProjector func(context.Context, *config.Config) ([]alertResponse, error)

type alertManager struct {
	mutex          sync.Mutex
	state          alertStore
	configPath     string
	loadConfig     func(string) (*config.Config, error)
	loadMonitoring func(string) (monitoring.Settings, error)
	project        alertProjector
	send           func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error
	now            func() time.Time
	interval       time.Duration
	report         func(error)
}

type alertEvent struct {
	state    store.AlertState
	alert    *alertResponse
	recovery bool
}

func newAlertManager(
	state alertStore,
	configPath string,
	loadConfig func(string) (*config.Config, error),
	loadMonitoring func(string) (monitoring.Settings, error),
	project alertProjector,
	send func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error,
	now func() time.Time,
	interval time.Duration,
	report func(error),
) (*alertManager, error) {
	if state == nil || loadConfig == nil || loadMonitoring == nil || project == nil || send == nil ||
		now == nil || interval <= 0 || report == nil {
		return nil, errors.New("创建告警管理器失败: 内部依赖不完整")
	}
	return &alertManager{
		state: state, configPath: configPath, loadConfig: loadConfig,
		loadMonitoring: loadMonitoring, project: project, send: send,
		now: now, interval: interval, report: report,
	}, nil
}

func (manager *alertManager) run(ctx context.Context) {
	manager.evaluateAndReport(ctx)
	ticker := time.NewTicker(manager.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.evaluateAndReport(ctx)
		}
	}
}

func (manager *alertManager) evaluateAndReport(ctx context.Context) {
	if err := manager.evaluate(ctx); err != nil && ctx.Err() == nil {
		manager.report(err)
	}
}

func (manager *alertManager) evaluate(ctx context.Context) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	cfg, err := manager.loadConfig(manager.configPath)
	if err != nil {
		return fmt.Errorf("重新加载清单失败: %w", err)
	}
	if cfg.Monitoring == nil {
		return nil
	}
	settings, err := manager.loadMonitoring(cfg.Monitoring.EnvFile)
	if err != nil {
		return fmt.Errorf("加载监控配置失败: %w", err)
	}
	if settings.DingTalk == nil {
		return nil
	}
	alerts, err := manager.project(ctx, cfg)
	if err != nil {
		return fmt.Errorf("计算当前告警投影失败: %w", err)
	}
	stored, err := manager.state.ListAlertStates(ctx)
	if err != nil {
		return err
	}

	now := manager.now().UTC()
	hosts := make(map[string]struct{}, len(cfg.Hosts))
	for index := range cfg.Hosts {
		hosts[cfg.Hosts[index].Host] = struct{}{}
	}
	current := make(map[string]alertResponse, len(alerts))
	for _, alert := range alerts {
		current[alert.ID] = alert
	}
	states := make(map[string]store.AlertState, len(stored)+len(alerts))
	for _, state := range stored {
		states[state.ID] = state
	}

	events := make([]alertEvent, 0, len(alerts)+len(stored))
	for index := range alerts {
		alert := alerts[index]
		state, exists := states[alert.ID]
		if !exists || !state.Active {
			state = store.AlertState{
				ID: alert.ID, Host: alert.Host, Kind: alert.Kind, Active: true,
				FirstSeenAt: now, LastSeenAt: now,
			}
		} else {
			state.LastSeenAt = now
		}
		if err := manager.state.SaveAlertState(ctx, state); err != nil {
			return err
		}
		states[state.ID] = state
		if state.LastAlertSentAt.IsZero() || now.Sub(state.LastAlertSentAt) >= alertSilenceInterval {
			currentAlert := alert
			events = append(events, alertEvent{state: state, alert: &currentAlert})
		}
	}

	for _, previous := range stored {
		if _, exists := current[previous.ID]; exists {
			continue
		}
		_, hostExists := hosts[previous.Host]
		if previous.Active {
			previous.Active = false
			previous.ResolvedAt = now
			previous.RecoverySentAt = time.Time{}
			if err := manager.state.SaveAlertState(ctx, previous); err != nil {
				return err
			}
			states[previous.ID] = previous
			if hostExists && !previous.LastAlertSentAt.IsZero() {
				events = append(events, alertEvent{state: previous, recovery: true})
			}
			continue
		}
		if hostExists && !previous.LastAlertSentAt.IsZero() && previous.RecoverySentAt.IsZero() {
			events = append(events, alertEvent{state: previous, recovery: true})
		}
	}

	if len(events) == 0 {
		return nil
	}
	sort.Slice(events, func(left int, right int) bool {
		if events[left].state.ID == events[right].state.ID {
			return !events[left].recovery && events[right].recovery
		}
		return events[left].state.ID < events[right].state.ID
	})
	message := buildAlertMessage(events, now)
	if err := manager.send(ctx, *settings.DingTalk, message); err != nil {
		return err
	}

	var saveErrors []error
	for _, event := range events {
		state := event.state
		if event.recovery {
			state.RecoverySentAt = now
		} else {
			state.LastAlertSentAt = now
		}
		// 先发送再提交投递时间选择了 at-least-once：若进程在这里退出，重启后可能重复一条，
		// 但不会把一次实际未发送的告警永久静默 24 小时。
		if err := manager.state.SaveAlertState(ctx, state); err != nil {
			saveErrors = append(saveErrors, err)
		}
	}
	return errors.Join(saveErrors...)
}

func buildAlertMessage(events []alertEvent, detectedAt time.Time) monitoring.MarkdownMessage {
	var builder strings.Builder
	builder.WriteString("### Ark 主动告警\n\n")
	for _, event := range events {
		description := alertKindDescription(event.state.Kind)
		if event.recovery {
			fmt.Fprintf(&builder, "- **%s** · %s已恢复（`%s`）\n", event.state.Host, description, event.state.Kind)
			fmt.Fprintf(&builder, "  - 恢复时间：%s\n", detectedAt.Format(time.RFC3339))
			continue
		}
		fmt.Fprintf(&builder, "- **%s** · %s（`%s`）\n", event.state.Host, description, event.state.Kind)
		fmt.Fprintf(&builder, "  - 检测时间：%s\n", detectedAt.Format(time.RFC3339))
		fmt.Fprintf(&builder, "  - 当前状态：%s\n", event.alert.Message)
	}
	return monitoring.MarkdownMessage{Title: "Ark 主动告警", Text: builder.String()}
}

func alertKindDescription(kind string) string {
	switch kind {
	case "backup_overdue":
		return "备份超时"
	case "backup_consecutive_failures":
		return "连续备份失败"
	case "verification_failed":
		return "恢复演练失败"
	default:
		return kind
	}
}
