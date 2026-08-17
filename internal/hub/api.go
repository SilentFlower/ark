package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/store"
)

type apiStore interface {
	ListRuns(context.Context, store.RunListOptions) ([]store.Run, bool, error)
	ListHostRuns(context.Context, string, int) ([]store.HostRun, error)
	LatestDoctorReport(context.Context, store.DoctorScope, string) (store.DoctorReport, bool, error)
	ListVerifications(context.Context, string, int) ([]store.Verification, error)
	CreateManualOperation(context.Context, store.ManualOperation) error
	FinishManualOperation(context.Context, string, store.ManualOperationResult) error
	GetManualOperation(context.Context, string) (store.ManualOperation, error)
	ListManualOperations(context.Context, store.OperationListOptions) ([]store.ManualOperation, bool, error)
}

type apiErrorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (application *application) handleHostsAPI(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	projections, _, err := application.projectHosts(request.Context(), cfg)
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	items := make([]hostSummaryResponse, 0, len(projections))
	for _, projection := range projections {
		items = append(items, projection.summary)
	}
	application.writeJSON(writer, http.StatusOK, struct {
		Items []hostSummaryResponse `json:"items"`
	}{Items: items})
}

func (application *application) handleHostAPI(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	hostName := request.PathValue("host")
	projections, _, err := application.projectHosts(request.Context(), cfg)
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	for _, projection := range projections {
		if projection.summary.Host == hostName {
			application.writeJSON(writer, http.StatusOK, projection.detail)
			return
		}
	}
	application.writeAPIError(writer, http.StatusNotFound, "not_found", "host 不存在")
}

func (application *application) handleRunsAPI(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	values := request.URL.Query()
	if err := rejectUnknownQuery(values, "host", "status", "limit", "cursor"); err != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	host := values.Get("host")
	if host != "" && findConfigHost(cfg, host) == nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	status := store.Status(values.Get("status"))
	if status != "" && status != store.StatusRunning && status != store.StatusOK &&
		status != store.StatusWarn && status != store.StatusFail {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	limit, err := parseLimit(values.Get("limit"))
	if err != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	options := store.RunListOptions{Host: host, Status: status, Limit: limit}
	filter := host + "\x00" + string(status) + "\x00" + strconv.Itoa(limit)
	if cursor := values.Get("cursor"); cursor != "" {
		options.BeforeAt, options.BeforeID, err = decodeCursor(cursor, "runs", filter)
		if err != nil {
			application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
			return
		}
	}
	runs, hasMore, err := application.state.ListRuns(request.Context(), options)
	if err != nil {
		if strings.Contains(err.Error(), "status") || strings.Contains(err.Error(), "limit") {
			application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
			return
		}
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	items := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		items = append(items, newRunResponse(run))
	}
	nextCursor := (*string)(nil)
	if hasMore && len(runs) > 0 {
		encoded, encodeErr := encodeCursor("runs", filter, runs[len(runs)-1].StartedAt, runs[len(runs)-1].ID)
		if encodeErr != nil {
			application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
			return
		}
		nextCursor = &encoded
	}
	application.writeJSON(writer, http.StatusOK, struct {
		Items      []runResponse `json:"items"`
		NextCursor *string       `json:"next_cursor"`
	}{Items: items, NextCursor: nextCursor})
}

func (application *application) handleAlertsAPI(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	_, alerts, err := application.projectHosts(request.Context(), cfg)
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	application.writeJSON(writer, http.StatusOK, struct {
		Items []alertResponse `json:"items"`
	}{Items: alerts})
}

func (application *application) handleOperationsAPI(writer http.ResponseWriter, request *http.Request) {
	if _, _, ok := application.requireSession(writer, request); !ok {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	values := request.URL.Query()
	if err := rejectUnknownQuery(values, "host", "kind", "status", "limit", "cursor"); err != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	limit, err := parseLimit(values.Get("limit"))
	if err != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	options := store.OperationListOptions{
		Host: values.Get("host"), Kind: store.OperationKind(values.Get("kind")),
		Status: store.OperationStatus(values.Get("status")), Limit: limit,
	}
	if options.Host != "" && findConfigHost(cfg, options.Host) == nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	filter := strings.Join([]string{options.Host, string(options.Kind), string(options.Status), strconv.Itoa(limit)}, "\x00")
	if cursor := values.Get("cursor"); cursor != "" {
		options.BeforeAt, options.BeforeID, err = decodeCursor(cursor, "operations", filter)
		if err != nil {
			application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
			return
		}
	}
	operations, hasMore, err := application.state.ListManualOperations(request.Context(), options)
	if err != nil {
		if strings.Contains(err.Error(), "非法") || strings.Contains(err.Error(), "limit") {
			application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
			return
		}
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	items := make([]operationResponse, 0, len(operations))
	for _, operation := range operations {
		items = append(items, newOperationResponse(operation))
	}
	nextCursor := (*string)(nil)
	if hasMore && len(operations) > 0 {
		encoded, encodeErr := encodeCursor(
			"operations", filter, operations[len(operations)-1].StartedAt, operations[len(operations)-1].ID,
		)
		if encodeErr != nil {
			application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
			return
		}
		nextCursor = &encoded
	}
	application.writeJSON(writer, http.StatusOK, struct {
		Items      []operationResponse `json:"items"`
		NextCursor *string             `json:"next_cursor"`
	}{Items: items, NextCursor: nextCursor})
}

func (application *application) handleOperationAPI(writer http.ResponseWriter, request *http.Request) {
	_, sessionToken, ok := application.requireSession(writer, request)
	if !ok {
		return
	}
	if _, ok := application.requireRuntime(writer, request); !ok {
		return
	}
	operation, err := application.state.GetManualOperation(request.Context(), request.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		application.writeAPIError(writer, http.StatusNotFound, "not_found", "operation 不存在")
		return
	}
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	response := newOperationResponse(operation)
	if application.operations != nil {
		token, issueErr := application.operations.issueConfirmation(operation, sessionToken)
		if issueErr != nil {
			application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
			return
		}
		response.ConfirmationToken = token
	}
	application.writeJSON(writer, http.StatusOK, response)
}

func (application *application) requireRuntime(
	writer http.ResponseWriter,
	request *http.Request,
) (*config.Config, bool) {
	if application.state == nil || application.loadConfig == nil || application.analyzeSchedule == nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return nil, false
	}
	cfg, err := application.loadConfig(application.configPath)
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return nil, false
	}
	return cfg, true
}

func (application *application) writeJSON(writer http.ResponseWriter, status int, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	writeBody(writer, append(payload, '\n'))
}

func (application *application) writeAPIError(writer http.ResponseWriter, status int, code string, message string) {
	payload, _ := json.Marshal(apiErrorBody{Error: apiError{Code: code, Message: message}})
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	writeBody(writer, append(payload, '\n'))
}

func parseLimit(value string) (int, error) {
	if value == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 100 {
		return 0, fmt.Errorf("limit 必须在 1 到 100 之间")
	}
	return limit, nil
}

func rejectUnknownQuery(values map[string][]string, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name, entries := range values {
		if _, ok := allowedSet[name]; !ok || len(entries) != 1 {
			return fmt.Errorf("查询参数 %q 无效", name)
		}
	}
	return nil
}

func findConfigHost(cfg *config.Config, name string) *config.Host {
	for index := range cfg.Hosts {
		if cfg.Hosts[index].Host == name {
			return &cfg.Hosts[index]
		}
	}
	return nil
}
