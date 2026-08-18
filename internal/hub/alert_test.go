package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/monitoring"
	"github.com/silentflower/ark/internal/store"
)

type memoryAlertStore struct {
	mutex  sync.Mutex
	states map[string]store.AlertState
}

func newMemoryAlertStore() *memoryAlertStore {
	return &memoryAlertStore{states: make(map[string]store.AlertState)}
}

func (state *memoryAlertStore) ListAlertStates(context.Context) ([]store.AlertState, error) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	result := make([]store.AlertState, 0, len(state.states))
	for _, value := range state.states {
		result = append(result, value)
	}
	sort.Slice(result, func(left int, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

func (state *memoryAlertStore) SaveAlertState(_ context.Context, value store.AlertState) error {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.states[value.ID] = value
	return nil
}

func loadTestMonitoring(t *testing.T) monitoring.Settings {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitoring.env")
	if err := os.WriteFile(path, []byte("ARK_DINGTALK_WEBHOOK_URL=https://example.com/webhook\n"), 0o600); err != nil {
		t.Fatalf("写入监控配置失败: %v", err)
	}
	settings, err := monitoring.Load(path)
	if err != nil {
		t.Fatalf("加载监控配置失败: %v", err)
	}
	return settings
}

func newTestAlertManager(
	t *testing.T,
	state alertStore,
	cfg **config.Config,
	currentAlerts *[]alertResponse,
	now *time.Time,
	send func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error,
) *alertManager {
	t.Helper()
	settings := loadTestMonitoring(t)
	manager, err := newAlertManager(
		state,
		"/etc/ark/ark.yaml",
		func(string) (*config.Config, error) { return *cfg, nil },
		func(string) (monitoring.Settings, error) { return settings, nil },
		func(context.Context, *config.Config) ([]alertResponse, error) {
			return append([]alertResponse(nil), (*currentAlerts)...), nil
		},
		send,
		func() time.Time { return *now },
		time.Millisecond,
		func(error) {},
	)
	if err != nil {
		t.Fatalf("创建告警管理器失败: %v", err)
	}
	return manager
}

func TestAlertManager_首次静默恢复与复发(t *testing.T) {
	state := newMemoryAlertStore()
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Monitoring: &config.Monitoring{EnvFile: "/etc/ark/monitoring.env"},
		Hosts:      []config.Host{{Host: "web-01"}},
	}
	alerts := []alertResponse{{
		ID: "web-01:backup_overdue", Host: "web-01", Kind: "backup_overdue",
		Message: "最近成功备份已超过有效计划周期的两倍",
	}}
	var messages []monitoring.MarkdownMessage
	manager := newTestAlertManager(t, state, &cfg, &alerts, &now,
		func(_ context.Context, _ monitoring.DingTalkSettings, message monitoring.MarkdownMessage) error {
			messages = append(messages, message)
			return nil
		})

	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("首次评估失败: %v", err)
	}
	now = now.Add(23*time.Hour + 59*time.Minute)
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("静默期评估失败: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("静默期消息数 = %d，期望 1", len(messages))
	}
	now = now.Add(time.Minute)
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("24 小时边界评估失败: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("24 小时重发消息数 = %d，期望 2", len(messages))
	}

	alerts = nil
	now = now.Add(time.Minute)
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("恢复评估失败: %v", err)
	}
	if len(messages) != 3 || !strings.Contains(messages[2].Text, "已恢复") {
		t.Fatalf("恢复消息 = %#v", messages)
	}
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("重复恢复评估失败: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("恢复消息重复发送，数量 = %d", len(messages))
	}

	alerts = []alertResponse{{
		ID: "web-01:backup_overdue", Host: "web-01", Kind: "backup_overdue",
		Message: "最近成功备份已超过有效计划周期的两倍",
	}}
	now = now.Add(time.Minute)
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("复发评估失败: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("复发应立即发送，消息数 = %d", len(messages))
	}
	stored := state.states["web-01:backup_overdue"]
	if !stored.Active || !stored.FirstSeenAt.Equal(now) {
		t.Fatalf("复发状态未重置: %#v", stored)
	}
}

func TestAlertManager_发送失败重试且重启不重复(t *testing.T) {
	state := newMemoryAlertStore()
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Monitoring: &config.Monitoring{EnvFile: "/etc/ark/monitoring.env"},
		Hosts:      []config.Host{{Host: "web-01"}},
	}
	alerts := []alertResponse{{
		ID: "web-01:verification_failed", Host: "web-01", Kind: "verification_failed",
		Message: "最近一次恢复演练失败",
	}}
	attempts := 0
	manager := newTestAlertManager(t, state, &cfg, &alerts, &now,
		func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error {
			attempts++
			if attempts == 1 {
				return errors.New("发送失败")
			}
			return nil
		})
	if err := manager.evaluate(context.Background()); err == nil {
		t.Fatal("首次发送失败应返回错误")
	}
	if !state.states[alerts[0].ID].LastAlertSentAt.IsZero() {
		t.Fatal("发送失败不能进入静默期")
	}
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("下一周期重试失败: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("发送尝试次数 = %d，期望 2", attempts)
	}

	restartedAttempts := 0
	restarted := newTestAlertManager(t, state, &cfg, &alerts, &now,
		func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error {
			restartedAttempts++
			return nil
		})
	if err := restarted.evaluate(context.Background()); err != nil {
		t.Fatalf("重启后评估失败: %v", err)
	}
	if restartedAttempts != 0 {
		t.Fatal("Hub 重启不应丢失静默状态")
	}
}

func TestAlertManager_并发评估只发送一次(t *testing.T) {
	state := newMemoryAlertStore()
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Monitoring: &config.Monitoring{EnvFile: "/etc/ark/monitoring.env"},
		Hosts:      []config.Host{{Host: "web-01"}},
	}
	alerts := []alertResponse{{
		ID: "web-01:backup_consecutive_failures", Host: "web-01", Kind: "backup_consecutive_failures",
		Message: "最近两次备份均失败",
	}}
	var sends atomic.Int32
	manager := newTestAlertManager(t, state, &cfg, &alerts, &now,
		func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error {
			sends.Add(1)
			return nil
		})
	var wait sync.WaitGroup
	for range 10 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := manager.evaluate(context.Background()); err != nil {
				t.Errorf("并发评估失败: %v", err)
			}
		}()
	}
	wait.Wait()
	if sends.Load() != 1 {
		t.Fatalf("并发发送次数 = %d，期望 1", sends.Load())
	}
}

func TestAlertManager_Host删除不发送恢复(t *testing.T) {
	state := newMemoryAlertStore()
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	cfg := &config.Config{
		Monitoring: &config.Monitoring{EnvFile: "/etc/ark/monitoring.env"},
		Hosts:      []config.Host{{Host: "web-01"}},
	}
	alerts := []alertResponse{{
		ID: "web-01:backup_overdue", Host: "web-01", Kind: "backup_overdue",
		Message: "最近成功备份已超过有效计划周期的两倍",
	}}
	sends := 0
	manager := newTestAlertManager(t, state, &cfg, &alerts, &now,
		func(context.Context, monitoring.DingTalkSettings, monitoring.MarkdownMessage) error {
			sends++
			return nil
		})
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("首次评估失败: %v", err)
	}
	cfg.Hosts = nil
	alerts = nil
	now = now.Add(time.Minute)
	if err := manager.evaluate(context.Background()); err != nil {
		t.Fatalf("删除 host 后评估失败: %v", err)
	}
	if sends != 1 || state.states["web-01:backup_overdue"].Active {
		t.Fatalf("host 删除后的状态 sends=%d state=%#v", sends, state.states)
	}
}
