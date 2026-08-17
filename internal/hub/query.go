package hub

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/store"
)

type hostSummaryResponse struct {
	Host                   string        `json:"host"`
	Local                  bool          `json:"local"`
	Project                string        `json:"project"`
	TargetCount            int           `json:"target_count"`
	Schedule               string        `json:"schedule"`
	LastBackupStatus       *store.Status `json:"last_backup_status"`
	LastSuccessfulBackupAt *string       `json:"last_successful_backup_at"`
	LastVerificationStatus *store.Status `json:"last_verification_status"`
	NextRunAt              *string       `json:"next_run_at"`
	Diagnostics            []string      `json:"diagnostics"`
	Health                 string        `json:"health"`
}

type hostDetailResponse struct {
	Summary       hostSummaryResponse    `json:"summary"`
	Targets       []targetResponse       `json:"targets"`
	Runs          []hostRunResponse      `json:"runs"`
	Doctor        *doctorReportResponse  `json:"doctor"`
	Verifications []verificationResponse `json:"verifications"`
}

type targetResponse struct {
	ID   string            `json:"id"`
	Type config.TargetType `json:"type"`
}

type runResponse struct {
	ID            string       `json:"id"`
	RequestedHost *string      `json:"requested_host"`
	Status        store.Status `json:"status"`
	StartedAt     string       `json:"started_at"`
	FinishedAt    *string      `json:"finished_at"`
	DurationMS    *int64       `json:"duration_ms"`
	ArkVersion    string       `json:"ark_version"`
	Error         string       `json:"error"`
}

type targetResultResponse struct {
	TargetID   string       `json:"target_id"`
	TargetType string       `json:"target_type"`
	Status     store.Status `json:"status"`
	Bytes      int64        `json:"bytes"`
	DurationMS int64        `json:"duration_ms"`
	SnapshotID string       `json:"snapshot_id"`
	Error      string       `json:"error"`
}

type hostRunResponse struct {
	Run     runResponse            `json:"run"`
	Status  store.Status           `json:"status"`
	Targets []targetResultResponse `json:"targets"`
}

type doctorReportResponse struct {
	CreatedAt string              `json:"created_at"`
	Status    store.Status        `json:"status"`
	NextRunAt *string             `json:"next_run_at"`
	Report    doctorReportPayload `json:"report"`
}

type doctorReportPayload struct {
	Checks []doctorCheckResponse `json:"checks"`
}

type doctorCheckResponse struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type verificationResponse struct {
	ID         string          `json:"id"`
	RunID      *string         `json:"run_id"`
	SnapshotID string          `json:"snapshot_id"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at"`
	DurationMS int64           `json:"duration_ms"`
	Status     store.Status    `json:"status"`
	Error      string          `json:"error"`
	Detail     json.RawMessage `json:"detail"`
}

type operationResponse struct {
	ID                string                `json:"id"`
	Kind              store.OperationKind   `json:"kind"`
	Host              string                `json:"host"`
	Status            store.OperationStatus `json:"status"`
	StartedAt         string                `json:"started_at"`
	FinishedAt        *string               `json:"finished_at"`
	DurationMS        *int64                `json:"duration_ms"`
	Request           json.RawMessage       `json:"request"`
	Result            json.RawMessage       `json:"result"`
	Error             string                `json:"error"`
	ExitCode          *int                  `json:"exit_code"`
	ParentOperationID *string               `json:"parent_operation_id"`
	ConfirmationToken *string               `json:"confirmation_token,omitempty"`
}

func newRunResponse(run store.Run) runResponse {
	response := runResponse{
		ID: run.ID, Status: run.Status, StartedAt: formatTime(run.StartedAt),
		ArkVersion: run.ArkVersion, Error: run.Error,
	}
	response.RequestedHost = optionalString(run.RequestedHost)
	response.FinishedAt = optionalTime(run.FinishedAt)
	if !run.FinishedAt.IsZero() {
		value := run.Duration.Milliseconds()
		response.DurationMS = &value
	}
	return response
}

func newHostRunResponse(value store.HostRun) hostRunResponse {
	targets := make([]targetResultResponse, 0, len(value.Targets))
	for _, target := range value.Targets {
		targets = append(targets, targetResultResponse{
			TargetID: target.TargetID, TargetType: target.TargetType, Status: target.Status,
			Bytes: target.Bytes, DurationMS: target.Duration.Milliseconds(),
			SnapshotID: target.SnapshotID, Error: target.Error,
		})
	}
	return hostRunResponse{Run: newRunResponse(value.Run), Status: value.Status, Targets: targets}
}

func newDoctorReportResponse(value store.DoctorReport, host *config.Host) (*doctorReportResponse, error) {
	var report *doctorReportPayload
	if err := json.Unmarshal(value.ReportJSON, &report); err != nil || report == nil {
		return nil, fmt.Errorf("解析 doctor 报告失败")
	}
	if report.Checks == nil {
		report.Checks = make([]doctorCheckResponse, 0)
	}
	sensitivePaths := hostSensitivePaths(host)
	for index := range report.Checks {
		check := &report.Checks[index]
		switch store.Status(check.Status) {
		case store.StatusOK, store.StatusWarn, store.StatusFail:
		default:
			return nil, fmt.Errorf("doctor 报告包含非法状态 %q", check.Status)
		}
		check.Name = redactDoctorText(check.Name, sensitivePaths)
		check.Detail = redactDoctorText(check.Detail, sensitivePaths)
	}
	return &doctorReportResponse{
		CreatedAt: formatTime(value.CreatedAt), Status: value.Status,
		NextRunAt: optionalTime(value.NextRunAt), Report: *report,
	}, nil
}

func hostSensitivePaths(host *config.Host) []string {
	if host == nil {
		return nil
	}
	paths := []string{host.Project.ComposeFile, host.Project.EnvFile}
	if host.SSH != nil {
		paths = append(paths, host.SSH.IdentityFile, host.SSH.KnownHostsFile)
	}
	for _, target := range host.Targets {
		paths = append(paths, target.Paths...)
	}
	return paths
}

func redactDoctorText(value string, sensitivePaths []string) string {
	for _, path := range sensitivePaths {
		if strings.TrimSpace(path) != "" {
			value = strings.ReplaceAll(value, path, "[路径已脱敏]")
		}
	}
	return value
}

func newVerificationResponse(value store.Verification) verificationResponse {
	return verificationResponse{
		ID: value.ID, RunID: optionalString(value.RunID), SnapshotID: value.SnapshotID,
		StartedAt: formatTime(value.StartedAt), FinishedAt: formatTime(value.FinishedAt),
		DurationMS: value.Duration.Milliseconds(), Status: value.Status,
		Error: value.Error, Detail: value.DetailJSON,
	}
}

func newOperationResponse(value store.ManualOperation) operationResponse {
	response := operationResponse{
		ID: value.ID, Kind: value.Kind, Host: value.Host, Status: value.Status,
		StartedAt: formatTime(value.StartedAt), Request: value.RequestJSON,
		Result: value.ResultJSON, Error: value.Error, ExitCode: value.ExitCode,
		ParentOperationID: optionalString(value.ParentOperationID),
	}
	response.FinishedAt = optionalTime(value.FinishedAt)
	if !value.FinishedAt.IsZero() {
		duration := value.Duration.Milliseconds()
		response.DurationMS = &duration
	}
	return response
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func optionalTime(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := formatTime(value)
	return &formatted
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}
