package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/schedule"
	"github.com/silentflower/ark/internal/store"
)

const validBackupOperationJSON = `{"run_id":"run-manual","status":"ok","manifest":{"schema_version":1,"run_id":"run-manual","ark_version":"test","started_at":"2026-08-17T12:00:00Z","finished_at":"2026-08-17T12:01:00Z","hosts":[]},"manifest_snapshot_id":"manifest-backup","heartbeat_status":"sent","error":""}`

func TestAPI_HostsAlertsRuns与Operations稳定投影(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	state := openHubTestStore(t)
	cfg := hubTestConfig()
	recordHubTestRun(t, state, "run-success", store.StatusOK, now.Add(-72*time.Hour))
	recordHubTestRun(t, state, "run-fail-1", store.StatusFail, now.Add(-2*time.Hour))
	recordHubTestRun(t, state, "run-fail-2", store.StatusFail, now.Add(-time.Hour))
	if err := state.RecordDoctorReport(context.Background(), store.DoctorReport{
		Scope: store.DoctorScopeHost, Host: "web-01", CreatedAt: now.Add(-time.Hour),
		Status: store.StatusOK, NextRunAt: now.Add(12 * time.Hour), ReportJSON: []byte(`{"checks":[{"name":"web-01 / project.compose_file","status":"ok","detail":"/secret/compose.yaml"},{"name":"web-01 / connection","status":"fail","detail":"SSH 登录失败: identity /secret/id known_hosts /secret/known"},{"name":"web-01 / target files/config","status":"fail","detail":"以下路径无法访问: /secret/data"}]}`),
	}); err != nil {
		t.Fatalf("记录 doctor 失败: %v", err)
	}
	if err := state.RecordVerification(context.Background(), store.Verification{
		ID: "verify-1", Host: "web-01", RunID: "run-fail-2", SnapshotID: "manifest-1",
		StartedAt: now.Add(-30 * time.Minute), FinishedAt: now.Add(-29 * time.Minute),
		Duration: time.Minute, Status: store.StatusFail, Error: "恢复演练失败",
		DetailJSON: []byte(`{"status":"fail"}`),
	}); err != nil {
		t.Fatalf("记录 verification 失败: %v", err)
	}
	if err := state.CreateManualOperation(context.Background(), store.ManualOperation{
		ID: "operation-1", Kind: store.OperationKindBackup, Host: "web-01",
		Status: store.OperationStatusRunning, StartedAt: now, RequestJSON: []byte(`{}`),
	}); err != nil {
		t.Fatalf("创建 operation 失败: %v", err)
	}

	application, _ := newHTTPTestApplication(t, false, func() time.Time { return now })
	application.state = state
	application.configPath = "/secret/config/ark.yaml"
	application.loadConfig = func(string) (*config.Config, error) { return cfg, nil }
	application.analyzeSchedule = func(context.Context, string, time.Time) (schedule.Window, error) {
		return schedule.Window{NextRunAt: now.Add(12 * time.Hour), Interval: 24 * time.Hour}, nil
	}
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)

	hostsResponse := serveAuthenticated(t, handler, sessionCookie, http.MethodGet, "/api/hosts", nil)
	if hostsResponse.Code != http.StatusOK {
		t.Fatalf("hosts status=%d body=%s", hostsResponse.Code, hostsResponse.Body.String())
	}
	var hosts struct {
		Items []hostSummaryResponse `json:"items"`
	}
	decodeResponseJSON(t, hostsResponse, &hosts)
	if len(hosts.Items) != 1 || hosts.Items[0].Health != "fail" ||
		hosts.Items[0].LastBackupStatus == nil || *hosts.Items[0].LastBackupStatus != store.StatusFail ||
		hosts.Items[0].LastSuccessfulBackupAt == nil {
		t.Fatalf("hosts = %#v", hosts.Items)
	}
	if strings.Contains(hostsResponse.Body.String(), "/secret/") || strings.Contains(hostsResponse.Body.String(), "identity") {
		t.Fatalf("hosts 响应泄漏敏感路径: %s", hostsResponse.Body.String())
	}

	alertsResponse := serveAuthenticated(t, handler, sessionCookie, http.MethodGet, "/api/alerts", nil)
	var alerts struct {
		Items []alertResponse `json:"items"`
	}
	decodeResponseJSON(t, alertsResponse, &alerts)
	if len(alerts.Items) != 3 || alerts.Items[0].Host != "web-01" {
		t.Fatalf("alerts = %#v", alerts.Items)
	}
	alertKinds := make(map[string]bool, len(alerts.Items))
	for _, alert := range alerts.Items {
		alertKinds[alert.Kind] = true
	}
	if !alertKinds["backup_consecutive_failures"] {
		t.Fatalf("alerts 缺少稳定连续失败 kind: %#v", alerts.Items)
	}

	detailResponse := serveAuthenticated(t, handler, sessionCookie, http.MethodGet, "/api/hosts/web-01", nil)
	var detail hostDetailResponse
	decodeResponseJSON(t, detailResponse, &detail)
	if len(detail.Targets) != 1 || len(detail.Runs) != 3 || detail.Doctor == nil || len(detail.Verifications) != 1 {
		t.Fatalf("host detail = %#v", detail)
	}
	for _, secret := range []string{"/secret/compose.yaml", "/secret/id", "/secret/known", "/secret/data"} {
		if strings.Contains(detailResponse.Body.String(), secret) {
			t.Fatalf("host detail 泄漏敏感路径 %q: %s", secret, detailResponse.Body.String())
		}
	}

	runsResponse := serveAuthenticated(t, handler, sessionCookie, http.MethodGet, "/api/runs?host=web-01&limit=2", nil)
	var runs struct {
		Items      []runResponse `json:"items"`
		NextCursor *string       `json:"next_cursor"`
	}
	decodeResponseJSON(t, runsResponse, &runs)
	if len(runs.Items) != 2 || runs.NextCursor == nil {
		t.Fatalf("runs 第一页 = %#v", runs)
	}
	secondRuns := serveAuthenticated(
		t, handler, sessionCookie, http.MethodGet,
		"/api/runs?host=web-01&limit=2&cursor="+*runs.NextCursor, nil,
	)
	var secondPage struct {
		Items []runResponse `json:"items"`
	}
	decodeResponseJSON(t, secondRuns, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != "run-success" {
		t.Fatalf("runs 第二页 = %#v", secondPage.Items)
	}

	operationsResponse := serveAuthenticated(t, handler, sessionCookie, http.MethodGet, "/api/operations", nil)
	var operations struct {
		Items []operationResponse `json:"items"`
	}
	decodeResponseJSON(t, operationsResponse, &operations)
	if len(operations.Items) != 1 || operations.Items[0].ID != "operation-1" ||
		string(operations.Items[0].Result) != "null" {
		t.Fatalf("operations = %#v", operations.Items)
	}
}

func TestActionAPI_CSRF互斥与恢复确认单次消费(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	state := openHubTestStore(t)
	cfg := hubTestConfig()
	cfg.Hosts = append(cfg.Hosts, config.Host{
		Host: "web-02", Local: true,
		Project: config.Project{Name: "web-02", ComposeFile: "/srv/web-02/compose.yaml"},
	})
	directory := t.TempDir()
	capturePath := filepath.Join(directory, "capture")
	releasePath := filepath.Join(directory, "release")
	binaryPath := filepath.Join(directory, "ark")
	script := `#!/bin/sh
printf 'CALL\n' >> "$ARK_HUB_TEST_CAPTURE"
for arg in "$@"; do printf '[%s]\n' "$arg" >> "$ARK_HUB_TEST_CAPTURE"; done
while [ ! -f "$ARK_HUB_TEST_RELEASE" ]; do sleep 0.01; done
inspect=0
command_name=""
for arg in "$@"; do
  [ "$arg" = "--inspect" ] && inspect=1
  case "$arg" in backup|verify|restore) command_name="$arg";; esac
done
if [ "$inspect" = "1" ]; then
  printf '{"plan":{"manifest_snapshot_id":"manifest-exact","run_id":"run-restore","source_host":"web-01","destination_host":"web-01","project":{"name":"web","compose_file":"/srv/web/compose.yaml"},"conflict_policy":"reject","steps":[],"manual_checks":[]},"force":true,"resume":false,"destructive":true,"conflicts":[],"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n'
else
  case "$command_name" in
    backup)
      printf '{"run_id":"run-manual","status":"ok","manifest":{"schema_version":1,"run_id":"run-manual","ark_version":"test","started_at":"2026-08-17T12:00:00Z","finished_at":"2026-08-17T12:01:00Z","hosts":[]},"manifest_snapshot_id":"manifest-backup","heartbeat_status":"sent","error":""}\n'
      ;;
    verify)
      printf '{"manifest_snapshot_id":"manifest-selected","status":"ok","results":[]}\n'
      ;;
    restore)
      printf '{"manifest_snapshot_id":"manifest-exact","run_id":"run-restore","source_host":"web-01","destination_host":"web-01","status":"ok","steps":[],"manual_checks":[]}\n'
      ;;
  esac
fi
`
	if err := os.WriteFile(binaryPath, []byte(script), 0o700); err != nil {
		t.Fatalf("写入 Ark 测试脚本失败: %v", err)
	}
	t.Setenv("ARK_HUB_TEST_CAPTURE", capturePath)
	t.Setenv("ARK_HUB_TEST_RELEASE", releasePath)

	application, _ := newHTTPTestApplication(t, false, func() time.Time { return now })
	manager, err := newOperationManager(state, binaryPath, "/etc/ark/ark.yaml", application.random, application.now)
	if err != nil {
		t.Fatalf("创建 operation manager 失败: %v", err)
	}
	t.Cleanup(func() { _ = manager.close() })
	application.state = state
	application.configPath = "/etc/ark/ark.yaml"
	application.arkBinaryPath = binaryPath
	application.loadConfig = func(string) (*config.Config, error) { return cfg, nil }
	application.analyzeSchedule = func(context.Context, string, time.Time) (schedule.Window, error) {
		return schedule.Window{NextRunAt: now.Add(24 * time.Hour), Interval: 24 * time.Hour}, nil
	}
	application.operations = manager
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)
	sessionValue, ok := application.sessions.get(sessionCookie.Value)
	if !ok {
		t.Fatal("测试会话不存在")
	}

	missingCSRF := serveAuthenticated(t, handler, sessionCookie, http.MethodPost, "/api/hosts/web-01/backup", []byte(`{}`))
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("缺失 CSRF status=%d body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}

	backupResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/backup", []byte(`{}`),
	)
	if backupResponse.Code != http.StatusAccepted {
		t.Fatalf("backup status=%d body=%s", backupResponse.Code, backupResponse.Body.String())
	}
	var backupOperation operationResponse
	decodeResponseJSON(t, backupResponse, &backupOperation)
	conflictResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/verify", []byte(`{"snapshot":"latest"}`),
	)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("并发冲突 status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("释放 Ark 测试进程失败: %v", err)
	}
	waitOperationStatus(t, state, backupOperation.ID, store.OperationStatusOK)
	verifyResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/verify", []byte(`{"snapshot":"manifest-selected"}`),
	)
	if verifyResponse.Code != http.StatusAccepted {
		t.Fatalf("verify status=%d body=%s", verifyResponse.Code, verifyResponse.Body.String())
	}
	var verifyOperation operationResponse
	decodeResponseJSON(t, verifyResponse, &verifyOperation)
	waitOperationStatus(t, state, verifyOperation.ID, store.OperationStatusOK)

	previewResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/restore",
		[]byte(`{"action":"preview","destination_host":"web-01","snapshot":"latest","mode":"force"}`),
	)
	if previewResponse.Code != http.StatusAccepted {
		t.Fatalf("preview status=%d body=%s", previewResponse.Code, previewResponse.Body.String())
	}
	var previewOperation operationResponse
	decodeResponseJSON(t, previewResponse, &previewOperation)
	waitOperationStatus(t, state, previewOperation.ID, store.OperationStatusOK)

	previewDetail := serveAuthenticated(
		t, handler, sessionCookie, http.MethodGet, "/api/operations/"+previewOperation.ID, nil,
	)
	var previewDetailBody operationResponse
	decodeResponseJSON(t, previewDetail, &previewDetailBody)
	if previewDetailBody.ConfirmationToken == nil {
		t.Fatalf("首次读取 preview 未返回确认 token: %s", previewDetail.Body.String())
	}
	secondPreviewDetail := serveAuthenticated(
		t, handler, sessionCookie, http.MethodGet, "/api/operations/"+previewOperation.ID, nil,
	)
	if strings.Contains(secondPreviewDetail.Body.String(), "confirmation_token") {
		t.Fatalf("确认 token 被重复返回: %s", secondPreviewDetail.Body.String())
	}

	executeBody, _ := json.Marshal(restoreExecuteRequest{
		Action: "execute", PreviewOperationID: previewOperation.ID,
		ConfirmationToken: *previewDetailBody.ConfirmationToken,
	})
	crossHostResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-02/restore", executeBody,
	)
	if crossHostResponse.Code != http.StatusConflict {
		t.Fatalf("跨 host 旧确认 status=%d body=%s", crossHostResponse.Code, crossHostResponse.Body.String())
	}
	executeResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/restore", executeBody,
	)
	if executeResponse.Code != http.StatusAccepted {
		t.Fatalf("execute status=%d body=%s", executeResponse.Code, executeResponse.Body.String())
	}
	var executeOperation operationResponse
	decodeResponseJSON(t, executeResponse, &executeOperation)
	waitOperationStatus(t, state, executeOperation.ID, store.OperationStatusOK)

	repeatedResponse := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/restore", executeBody,
	)
	if repeatedResponse.Code != http.StatusConflict {
		t.Fatalf("重复确认 status=%d body=%s", repeatedResponse.Code, repeatedResponse.Body.String())
	}
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("读取 argv 捕获失败: %v", err)
	}
	calls := strings.Split(string(captured), "CALL\n")
	if len(calls) < 5 {
		t.Fatalf("捕获的 Ark 调用数量不足: %#v", calls)
	}
	for callIndex, expected := range map[int]string{
		1: "[--config]\n[/etc/ark/ark.yaml]\n[backup]\n[--host]\n[web-01]\n[--json]",
		2: "[--config]\n[/etc/ark/ark.yaml]\n[verify]\n[--host]\n[web-01]\n[--snapshot]\n[manifest-selected]\n[--json]",
	} {
		if !strings.Contains(calls[callIndex], expected) {
			t.Fatalf("Ark 调用 %d argv 不匹配: %s", callIndex, calls[callIndex])
		}
	}
	lastCall := calls[len(calls)-1]
	for _, expected := range []string{
		"[--snapshot]\n[manifest-exact]", "[--force]",
		"[--expected-preview-sha256]\n[aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa]",
	} {
		if !strings.Contains(lastCall, expected) {
			t.Fatalf("真实恢复 argv 缺少 %q: %s", expected, lastCall)
		}
	}
	if strings.Contains(lastCall, "[latest]") {
		t.Fatalf("真实恢复错误复用了 latest: %s", lastCall)
	}
}

func TestAPI_Schedule失败返回诊断和最后已知时间(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	state := openHubTestStore(t)
	nextRunAt := now.Add(6 * time.Hour)
	if err := state.RecordDoctorReport(context.Background(), store.DoctorReport{
		Scope: store.DoctorScopeHost, Host: "web-01", CreatedAt: now.Add(-time.Hour),
		Status: store.StatusOK, NextRunAt: nextRunAt, ReportJSON: []byte(`{"checks":[]}`),
	}); err != nil {
		t.Fatalf("记录 doctor 失败: %v", err)
	}
	application, _ := newHTTPTestApplication(t, false, func() time.Time { return now })
	application.state = state
	application.analyzeSchedule = func(context.Context, string, time.Time) (schedule.Window, error) {
		return schedule.Window{}, context.DeadlineExceeded
	}
	projections, alerts, err := application.projectHosts(context.Background(), hubTestConfig())
	if err != nil {
		t.Fatalf("投影 host 失败: %v", err)
	}
	if len(projections) != 1 || projections[0].summary.Health != "unknown" ||
		projections[0].summary.NextRunAt == nil || *projections[0].summary.NextRunAt != formatTime(nextRunAt) ||
		len(projections[0].summary.Diagnostics) != 1 ||
		projections[0].summary.Diagnostics[0] != "schedule_unavailable" {
		t.Fatalf("schedule failure summary=%#v", projections)
	}
	if len(alerts) != 0 {
		t.Fatalf("schedule failure 不应伪造 overdue: %#v", alerts)
	}
}

func TestAPI_拒绝未知Host非法Filter未知字段和超大Body(t *testing.T) {
	state := openHubTestStore(t)
	application, _ := newHTTPTestApplication(t, false, time.Now)
	application.state = state
	application.configPath = "/etc/ark/ark.yaml"
	application.loadConfig = func(string) (*config.Config, error) { return hubTestConfig(), nil }
	application.analyzeSchedule = func(context.Context, string, time.Time) (schedule.Window, error) {
		return schedule.Window{NextRunAt: time.Now().Add(24 * time.Hour), Interval: 24 * time.Hour}, nil
	}
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)
	sessionValue, ok := application.sessions.get(sessionCookie.Value)
	if !ok {
		t.Fatal("测试会话不存在")
	}

	tests := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{name: "未知详情 host", method: http.MethodGet, path: "/api/hosts/missing", want: http.StatusNotFound},
		{name: "非法 run status", method: http.MethodGet, path: "/api/runs?status=unknown", want: http.StatusBadRequest},
		{name: "未知操作 host", method: http.MethodPost, path: "/api/hosts/missing/backup", body: []byte(`{}`), want: http.StatusNotFound},
		{name: "未知 JSON 字段", method: http.MethodPost, path: "/api/hosts/web-01/backup", body: []byte(`{"force":true}`), want: http.StatusBadRequest},
		{name: "超大 JSON body", method: http.MethodPost, path: "/api/hosts/web-01/backup", body: []byte(`{"padding":"` + strings.Repeat("x", maximumJSONBodyBytes) + `"}`), want: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			if test.method == http.MethodPost {
				response = serveAction(
					t, handler, sessionCookie, sessionValue.csrfToken, test.path, test.body,
				)
			} else {
				response = serveAuthenticated(t, handler, sessionCookie, test.method, test.path, test.body)
			}
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s，期望 %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
	operations, _, err := state.ListManualOperations(context.Background(), store.OperationListOptions{Limit: 10})
	if err != nil || len(operations) != 0 {
		t.Fatalf("非法请求创建了 operation=%#v err=%v", operations, err)
	}
}

func TestConfirmation_过期和会话不匹配不创建Operation(t *testing.T) {
	tests := []struct {
		name         string
		sessionToken string
		expiresAt    time.Time
		want         error
	}{
		{name: "会话不匹配", sessionToken: "other-session", expiresAt: time.Date(2026, 8, 17, 12, 10, 0, 0, time.UTC), want: errConfirmationInvalid},
		{name: "确认过期", sessionToken: "session", expiresAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), want: errConfirmationExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
			state := openHubTestStore(t)
			manager, err := newOperationManager(
				state, os.Args[0], "/etc/ark/ark.yaml",
				strings.NewReader(strings.Repeat("r", 256)), func() time.Time { return now },
			)
			if err != nil {
				t.Fatalf("创建 operation manager 失败: %v", err)
			}
			t.Cleanup(func() { _ = manager.close() })
			manager.confirmations["preview-1"] = &confirmation{
				sessionHash: hashSessionToken("session"),
				request: restorePreviewRequest{
					Action: "preview", SourceHost: "web-01", DestinationHost: "web-01",
					Snapshot: "latest", Mode: "normal",
				},
				tokenHash: hashToken("confirmation"), issued: true,
				expiresAt: test.expiresAt, digest: strings.Repeat("a", 64), exactSnapshot: "manifest-1",
			}
			_, err = manager.startConfirmedRestore(
				context.Background(), "web-01", "preview-1", "confirmation", test.sessionToken,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("确认错误=%v，期望 %v", err, test.want)
			}
			operations, _, listErr := state.ListManualOperations(
				context.Background(), store.OperationListOptions{Limit: 10},
			)
			if listErr != nil || len(operations) != 0 {
				t.Fatalf("无效确认创建了 operation=%#v err=%v", operations, listErr)
			}
		})
	}
}

func TestActionAPI_子进程启动失败返回500并持久化Fail(t *testing.T) {
	state := openHubTestStore(t)
	binaryPath := filepath.Join(t.TempDir(), "missing-ark")
	application, _ := newHTTPTestApplication(t, false, time.Now)
	manager, err := newOperationManager(
		state, binaryPath, "/etc/ark/ark.yaml", application.random, application.now,
	)
	if err != nil {
		t.Fatalf("创建 operation manager 失败: %v", err)
	}
	t.Cleanup(func() { _ = manager.close() })
	application.state = state
	application.configPath = "/etc/ark/ark.yaml"
	application.loadConfig = func(string) (*config.Config, error) { return hubTestConfig(), nil }
	application.analyzeSchedule = func(context.Context, string, time.Time) (schedule.Window, error) {
		return schedule.Window{NextRunAt: time.Now().Add(24 * time.Hour), Interval: 24 * time.Hour}, nil
	}
	application.operations = manager
	handler := application.handler()
	sessionCookie := loginHTTPTestUser(t, handler)
	sessionValue, ok := application.sessions.get(sessionCookie.Value)
	if !ok {
		t.Fatal("测试会话不存在")
	}

	response := serveAction(
		t, handler, sessionCookie, sessionValue.csrfToken,
		"/api/hosts/web-01/backup", []byte(`{}`),
	)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "operation_failed") {
		t.Fatalf("启动失败 status=%d body=%s", response.Code, response.Body.String())
	}
	operations, _, err := state.ListManualOperations(context.Background(), store.OperationListOptions{Limit: 10})
	if err != nil || len(operations) != 1 || operations[0].Status != store.OperationStatusFail ||
		operations[0].Error != "Ark 子进程启动失败" {
		t.Fatalf("启动失败 operation=%#v err=%v", operations, err)
	}
}

func TestOperationManager_子进程失败路径持久化Fail(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		wantExitCode int
	}{
		{name: "非零退出", mode: "nonzero", wantExitCode: 7},
		{name: "损坏 JSON", mode: "invalid-json", wantExitCode: 0},
		{name: "stdout 超限", mode: "stdout-overflow", wantExitCode: 0},
		{name: "stderr 超限", mode: "stderr-overflow", wantExitCode: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ARK_HUB_OPERATION_HELPER", "1")
			state := openHubTestStore(t)
			manager, err := newOperationManager(
				state, os.Args[0], "/etc/ark/ark.yaml",
				strings.NewReader(strings.Repeat("r", 256)), time.Now,
			)
			if err != nil {
				t.Fatalf("创建 operation manager 失败: %v", err)
			}
			t.Cleanup(func() { _ = manager.close() })
			operation, err := manager.start(context.Background(), operationStart{
				Kind: store.OperationKindBackup, Host: "web-01", RequestJSON: []byte(`{}`),
				Arguments: operationHelperArguments(test.mode),
			})
			if err != nil {
				t.Fatalf("启动 helper 失败: %v", err)
			}
			stored := waitOperationStatus(t, state, operation.ID, store.OperationStatusFail)
			if stored.ExitCode == nil || *stored.ExitCode != test.wantExitCode || stored.Error == "" {
				t.Fatalf("失败 operation=%#v", stored)
			}
		})
	}
}

func TestOperationManager_请求取消不终止已启动任务(t *testing.T) {
	t.Setenv("ARK_HUB_OPERATION_HELPER", "1")
	state := openHubTestStore(t)
	releasePath := filepath.Join(t.TempDir(), "release")
	manager, err := newOperationManager(
		state, os.Args[0], "/etc/ark/ark.yaml",
		strings.NewReader(strings.Repeat("r", 256)), time.Now,
	)
	if err != nil {
		t.Fatalf("创建 operation manager 失败: %v", err)
	}
	t.Cleanup(func() { _ = manager.close() })
	requestContext, cancel := context.WithCancel(context.Background())
	operation, err := manager.start(requestContext, operationStart{
		Kind: store.OperationKindBackup, Host: "web-01", RequestJSON: []byte(`{}`),
		Arguments: operationHelperArguments("wait-for-release", releasePath),
	})
	if err != nil {
		t.Fatalf("启动 helper 失败: %v", err)
	}
	cancel()
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("释放 helper 失败: %v", err)
	}
	waitOperationStatus(t, state, operation.ID, store.OperationStatusOK)
}

func TestOperationHelperProcess(t *testing.T) {
	if os.Getenv("ARK_HUB_OPERATION_HELPER") != "1" {
		return
	}
	marker := -1
	for index, argument := range os.Args {
		if argument == "--" {
			marker = index
			break
		}
	}
	if marker < 0 || marker+1 >= len(os.Args) {
		os.Exit(2)
	}
	mode := os.Args[marker+1]
	switch mode {
	case "nonzero":
		_, _ = os.Stdout.WriteString(validBackupOperationJSON)
		os.Exit(7)
	case "invalid-json":
		_, _ = os.Stdout.WriteString("{")
	case "stdout-overflow":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", maximumOperationStdoutBytes+1))
	case "stderr-overflow":
		_, _ = os.Stderr.WriteString(strings.Repeat("x", maximumOperationStderrBytes+1))
		_, _ = os.Stdout.WriteString(validBackupOperationJSON)
	case "wait-for-release":
		if marker+2 >= len(os.Args) {
			os.Exit(2)
		}
		for {
			if _, err := os.Stat(os.Args[marker+2]); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = os.Stdout.WriteString(validBackupOperationJSON)
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func operationHelperArguments(mode string, extra ...string) []string {
	arguments := []string{"-test.run=^TestOperationHelperProcess$", "--", mode}
	return append(arguments, extra...)
}

func TestOperationManager_关闭时持久化Interrupted(t *testing.T) {
	state := openHubTestStore(t)
	binaryPath := filepath.Join(t.TempDir(), "ark")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatalf("写入阻塞 Ark 测试脚本失败: %v", err)
	}
	manager, err := newOperationManager(state, binaryPath, "/etc/ark/ark.yaml", strings.NewReader(strings.Repeat("r", 256)), time.Now)
	if err != nil {
		t.Fatalf("创建 operation manager 失败: %v", err)
	}
	operation, err := manager.start(context.Background(), operationStart{
		Kind: store.OperationKindBackup, Host: "web-01", RequestJSON: []byte(`{}`),
		Arguments: []string{"--config", "/etc/ark/ark.yaml", "backup", "--host", "web-01", "--json"},
	})
	if err != nil {
		t.Fatalf("启动阻塞 operation 失败: %v", err)
	}
	if err := manager.close(); err != nil {
		t.Fatalf("关闭 operation manager 失败: %v", err)
	}
	stored := waitOperationStatus(t, state, operation.ID, store.OperationStatusInterrupted)
	if stored.Error == "" {
		t.Fatalf("interrupted operation 缺少脱敏错误: %#v", stored)
	}
}

func TestParseOperationResult_拒绝不完整或未知合同(t *testing.T) {
	tests := []struct {
		name   string
		kind   store.OperationKind
		output string
	}{
		{name: "backup 缺少 run_id", kind: store.OperationKindBackup, output: `{"status":"ok"}`},
		{name: "backup 缺少心跳状态", kind: store.OperationKindBackup, output: `{"run_id":"run-1","status":"fail","manifest_snapshot_id":"","error":"failed"}`},
		{name: "backup 心跳状态非法", kind: store.OperationKindBackup, output: `{"run_id":"run-1","status":"fail","manifest_snapshot_id":"","heartbeat_status":"unknown","error":"failed"}`},
		{name: "verify 缺少 results", kind: store.OperationKindVerify, output: `{"manifest_snapshot_id":"manifest-1","status":"ok"}`},
		{name: "preview 摘要非法", kind: store.OperationKindRestorePreview, output: `{"plan":{"manifest_snapshot_id":"manifest-1","run_id":"run-1","source_host":"web-01","destination_host":"web-01","steps":[],"manual_checks":[]},"conflicts":[],"digest":"ABC"}`},
		{name: "restore 缺少来源", kind: store.OperationKindRestore, output: `{"manifest_snapshot_id":"manifest-1","run_id":"run-1","destination_host":"web-01","status":"ok","steps":[],"manual_checks":[]}`},
		{name: "verify result 不是对象", kind: store.OperationKindVerify, output: `{"manifest_snapshot_id":"manifest-1","status":"ok","results":[1]}`},
		{name: "包含未知字段", kind: store.OperationKindBackup, output: validBackupOperationJSON[:len(validBackupOperationJSON)-1] + `,"secret":true}`},
		{name: "包含尾随 JSON", kind: store.OperationKindBackup, output: validBackupOperationJSON + `{}`},
		{name: "未知 operation kind", kind: store.OperationKind("unknown"), output: `{"status":"ok"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := &boundedOperationBuffer{limit: maximumOperationStdoutBytes}
			_, _ = output.Write([]byte(test.output))
			if _, err := parseOperationResult(test.kind, output); err == nil {
				t.Fatal("期望拒绝无效 Ark JSON 结果")
			}
		})
	}
	overflow := &boundedOperationBuffer{limit: maximumOperationStdoutBytes}
	_, _ = overflow.Write([]byte(strings.Repeat("x", maximumOperationStdoutBytes+1)))
	if _, err := parseOperationResult(store.OperationKindBackup, overflow); err == nil {
		t.Fatal("期望拒绝超限 Ark JSON 结果")
	}
}

func hubTestConfig() *config.Config {
	return &config.Config{
		Defaults: config.Defaults{Schedule: &config.Schedule{OnCalendar: "daily"}},
		Hosts: []config.Host{{
			Host: "web-01", SSH: &config.SSH{IdentityFile: "/secret/id", KnownHostsFile: "/secret/known"},
			Project: config.Project{Name: "web", ComposeFile: "/secret/compose.yaml"},
			Targets: []config.Target{{Type: config.TargetFiles, Name: "config", Paths: []string{"/secret/data"}}},
		}},
	}
}

func openHubTestStore(t *testing.T) *store.Store {
	t.Helper()
	state, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ark.db"))
	if err != nil {
		t.Fatalf("打开 Hub 测试状态库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("关闭 Hub 测试状态库失败: %v", err)
		}
	})
	return state
}

func recordHubTestRun(t *testing.T, state *store.Store, id string, status store.Status, startedAt time.Time) {
	t.Helper()
	if err := state.CreateRun(context.Background(), store.Run{
		ID: id, Status: store.StatusRunning, StartedAt: startedAt, ArkVersion: "test",
	}); err != nil {
		t.Fatalf("创建 run %s 失败: %v", id, err)
	}
	if err := state.RecordRunTarget(context.Background(), store.RunTarget{
		RunID: id, Host: "web-01", TargetID: "files/config", TargetType: "files",
		Status: status, Duration: time.Second,
	}); err != nil {
		t.Fatalf("记录 run target %s 失败: %v", id, err)
	}
	if err := state.FinishRun(context.Background(), id, store.RunResult{
		Status: status, FinishedAt: startedAt.Add(time.Minute), Duration: time.Minute,
	}); err != nil {
		t.Fatalf("完成 run %s 失败: %v", id, err)
	}
}

func serveAuthenticated(
	t *testing.T,
	handler http.Handler,
	cookie *http.Cookie,
	method string,
	path string,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.AddCookie(cookie)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func serveAction(
	t *testing.T,
	handler http.Handler,
	cookie *http.Cookie,
	csrfToken string,
	path string,
	body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrfToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponseJSON(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("响应 JSON 无效: %v\n%s", err, response.Body.String())
	}
}

func waitOperationStatus(
	t *testing.T,
	state *store.Store,
	id string,
	want store.OperationStatus,
) store.ManualOperation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := state.GetManualOperation(context.Background(), id)
		if err == nil && operation.Status == want {
			return operation
		}
		time.Sleep(10 * time.Millisecond)
	}
	operation, err := state.GetManualOperation(context.Background(), id)
	t.Fatalf("等待 operation %s 状态 %s 超时: operation=%#v err=%v", id, want, operation, err)
	return store.ManualOperation{}
}
