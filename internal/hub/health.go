package hub

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/schedule"
	"github.com/silentflower/ark/internal/store"
)

type alertResponse struct {
	ID        string `json:"id"`
	Host      string `json:"host"`
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

type hostProjection struct {
	summary hostSummaryResponse
	detail  hostDetailResponse
}

type scheduleResult struct {
	window schedule.Window
	err    error
}

func (application *application) projectHosts(
	ctx context.Context,
	cfg *config.Config,
) ([]hostProjection, []alertResponse, error) {
	now := application.now().UTC()
	schedules := make(map[string]scheduleResult)
	for index := range cfg.Hosts {
		expression := cfg.ScheduleFor(&cfg.Hosts[index]).OnCalendar
		if _, exists := schedules[expression]; exists {
			continue
		}
		window, err := application.analyzeSchedule(ctx, expression, now)
		schedules[expression] = scheduleResult{window: window, err: err}
	}

	projections := make([]hostProjection, 0, len(cfg.Hosts))
	alerts := make([]alertResponse, 0)
	for index := range cfg.Hosts {
		host := &cfg.Hosts[index]
		expression := cfg.ScheduleFor(host).OnCalendar
		hostRuns, err := application.state.ListHostRuns(ctx, host.Host, 100)
		if err != nil {
			return nil, nil, err
		}
		doctorReport, doctorFound, err := application.state.LatestDoctorReport(
			ctx, store.DoctorScopeHost, host.Host,
		)
		if err != nil {
			return nil, nil, err
		}
		verifications, err := application.state.ListVerifications(ctx, host.Host, 20)
		if err != nil {
			return nil, nil, err
		}

		summary := hostSummaryResponse{
			Host: host.Host, Local: host.Local, Project: host.Project.Name,
			TargetCount: len(host.Targets), Schedule: expression,
			Diagnostics: make([]string, 0),
		}
		if len(hostRuns) > 0 {
			status := hostRuns[0].Status
			summary.LastBackupStatus = &status
		}
		var lastSuccessful time.Time
		for _, run := range hostRuns {
			if run.Status == store.StatusOK || run.Status == store.StatusWarn {
				lastSuccessful = run.Run.FinishedAt
				break
			}
		}
		summary.LastSuccessfulBackupAt = optionalTime(lastSuccessful)
		summary.RecentBackupSizes, summary.LastBackupBytes = deriveBackupSizes(hostRuns)
		if len(verifications) > 0 {
			status := verifications[0].Status
			summary.LastVerificationStatus = &status
		}

		scheduleState := schedules[expression]
		if scheduleState.err == nil {
			summary.NextRunAt = optionalTime(scheduleState.window.NextRunAt)
		} else {
			summary.Diagnostics = append(summary.Diagnostics, "schedule_unavailable")
			if doctorFound {
				summary.NextRunAt = optionalTime(doctorReport.NextRunAt)
			}
		}
		hostAlerts := deriveAlerts(
			host.Host, now, lastSuccessful, hostRuns, verifications, scheduleState,
		)
		alerts = append(alerts, hostAlerts...)
		summary.Health = deriveHealth(summary, doctorReport, doctorFound, hostAlerts, scheduleState.err)

		targets := make([]targetResponse, 0, len(host.Targets))
		for _, target := range host.Targets {
			targets = append(targets, targetResponse{ID: target.ID(), Type: target.Type})
		}
		runResponses := make([]hostRunResponse, 0, len(hostRuns))
		for _, run := range hostRuns {
			runResponses = append(runResponses, newHostRunResponse(run))
		}
		verificationResponses := make([]verificationResponse, 0, len(verifications))
		for _, verification := range verifications {
			verificationResponses = append(verificationResponses, newVerificationResponse(verification))
		}
		var doctorResponse *doctorReportResponse
		if doctorFound {
			doctorResponse, err = newDoctorReportResponse(doctorReport, host)
			if err != nil {
				return nil, nil, fmt.Errorf("投影 host %q doctor 报告失败: %w", host.Host, err)
			}
		}
		projections = append(projections, hostProjection{
			summary: summary,
			detail: hostDetailResponse{
				Summary: summary, Targets: targets, Runs: runResponses,
				Doctor: doctorResponse, Verifications: verificationResponses,
			},
		})
	}
	sort.Slice(alerts, func(left int, right int) bool { return alerts[left].ID < alerts[right].ID })
	return projections, alerts, nil
}

// maximumSizePoints 是大小趋势保留的采样点数。14 个点覆盖两周的日备，
// 既能看出体积突变，又不会让总览响应变大。
const maximumSizePoints = 14

// deriveBackupSizes 从 host 的运行历史里提取成功备份的体积序列。
//
// 数据来源就是投影已经取到的 hostRuns，不额外查库。只统计 ok 与 warn 的 run：
// 失败 run 的字节数是半截数据，放进趋势里会把体积腰斩误判成正常波动，
// 而体积突变正是 ADR-011 的第二道防线要盯的信号。
// @param hostRuns 按开始时间倒序的运行记录，已排除 running。
// @return []backupSizePoint 按时间正序、最多 maximumSizePoints 个采样点。
// @return *int64 最近一次成功备份的总字节；没有成功记录时为 nil。
func deriveBackupSizes(hostRuns []store.HostRun) ([]backupSizePoint, *int64) {
	points := make([]backupSizePoint, 0, maximumSizePoints)
	for _, run := range hostRuns {
		if run.Status != store.StatusOK && run.Status != store.StatusWarn {
			continue
		}
		if len(points) == maximumSizePoints {
			break
		}
		var total int64
		for _, target := range run.Targets {
			total += target.Bytes
		}
		points = append(points, backupSizePoint{
			RunID: run.Run.ID, FinishedAt: formatTime(run.Run.FinishedAt), Bytes: total,
		})
	}
	if len(points) == 0 {
		return points, nil
	}
	// hostRuns 是倒序，采样点要按时间正序交给前端画折线。
	for left, right := 0, len(points)-1; left < right; left, right = left+1, right-1 {
		points[left], points[right] = points[right], points[left]
	}
	latest := points[len(points)-1].Bytes
	return points, &latest
}

func deriveAlerts(
	host string,
	now time.Time,
	lastSuccessful time.Time,
	hostRuns []store.HostRun,
	verifications []store.Verification,
	scheduleState scheduleResult,
) []alertResponse {
	alerts := make([]alertResponse, 0, 3)
	appendAlert := func(kind string, message string) {
		alerts = append(alerts, alertResponse{
			ID: host + ":" + kind, Host: host, Kind: kind, Severity: "fail",
			Message: message, CreatedAt: formatTime(now),
		})
	}
	if scheduleState.err == nil && (lastSuccessful.IsZero() ||
		now.Sub(lastSuccessful) > 2*scheduleState.window.Interval) {
		appendAlert("backup_overdue", "最近成功备份已超过有效计划周期的两倍")
	}
	if len(hostRuns) >= 2 && hostRuns[0].Status == store.StatusFail && hostRuns[1].Status == store.StatusFail {
		appendAlert("backup_consecutive_failures", "最近两次备份均失败")
	}
	if len(verifications) > 0 && verifications[0].Status == store.StatusFail {
		appendAlert("verification_failed", "最近一次恢复演练失败")
	}
	return alerts
}

func deriveHealth(
	summary hostSummaryResponse,
	doctor store.DoctorReport,
	doctorFound bool,
	alerts []alertResponse,
	scheduleErr error,
) string {
	if scheduleErr != nil {
		return "unknown"
	}
	if len(alerts) > 0 || doctor.Status == store.StatusFail ||
		(summary.LastBackupStatus != nil && *summary.LastBackupStatus == store.StatusFail) ||
		(summary.LastVerificationStatus != nil && *summary.LastVerificationStatus == store.StatusFail) {
		return "fail"
	}
	if !doctorFound {
		return "unknown"
	}
	if doctor.Status == store.StatusWarn ||
		(summary.LastBackupStatus != nil && *summary.LastBackupStatus == store.StatusWarn) ||
		(summary.LastVerificationStatus != nil && *summary.LastVerificationStatus == store.StatusWarn) {
		return "warn"
	}
	return "ok"
}

func scheduleFailureMessage(host string, err error) error {
	return fmt.Errorf("计算 host %q 调度窗口失败: %w", host, err)
}
