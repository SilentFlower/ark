package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/silentflower/ark/internal/doctor"
	"github.com/silentflower/ark/internal/store"
)

type recordDoctorReportFunc func(context.Context, *store.Store, store.DoctorReport) error
type analyzeScheduleFunc func(context.Context, string, time.Time) (time.Time, error)

func persistDoctorReport(
	ctx context.Context,
	state *store.Store,
	scope store.DoctorScope,
	host string,
	report *doctor.Report,
	createdAt time.Time,
	nextRunAt time.Time,
	record recordDoctorReportFunc,
) error {
	if record == nil {
		return nil
	}
	if report == nil {
		return fmt.Errorf("记录 doctor 报告失败: 报告为空")
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("序列化 doctor 报告失败: %w", err)
	}
	if err := record(ctx, state, store.DoctorReport{
		Scope: scope, Host: host, CreatedAt: createdAt.UTC(), Status: doctorStoreStatus(report),
		NextRunAt: nextRunAt.UTC(), ReportJSON: payload,
	}); err != nil {
		return err
	}
	return nil
}

func doctorStoreStatus(report *doctor.Report) store.Status {
	status := store.StatusOK
	if report == nil {
		return store.StatusFail
	}
	for _, check := range report.Checks {
		switch check.Status {
		case doctor.StatusFail:
			return store.StatusFail
		case doctor.StatusWarn:
			status = store.StatusWarn
		}
	}
	return status
}
