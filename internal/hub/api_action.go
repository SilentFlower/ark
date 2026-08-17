package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/store"
)

const maximumJSONBodyBytes = 16 * 1024

type backupActionRequest struct{}

type verifyActionRequest struct {
	Snapshot string `json:"snapshot"`
}

type restorePreviewRequest struct {
	Action          string `json:"action"`
	SourceHost      string `json:"-"`
	DestinationHost string `json:"destination_host"`
	Snapshot        string `json:"snapshot"`
	Mode            string `json:"mode"`
}

type restoreExecuteRequest struct {
	Action             string `json:"action"`
	PreviewOperationID string `json:"preview_operation_id"`
	ConfirmationToken  string `json:"confirmation_token"`
}

func (application *application) handleBackupAction(writer http.ResponseWriter, request *http.Request) {
	value, _, ok := application.requireSession(writer, request)
	if !ok || !application.requireCSRF(writer, request, value) {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	host := request.PathValue("host")
	if findConfigHost(cfg, host) == nil {
		application.writeAPIError(writer, http.StatusNotFound, "not_found", "host 不存在")
		return
	}
	var body backupActionRequest
	if err := decodeJSONBody(writer, request, &body); err != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	payload := json.RawMessage("{}")
	application.startOperation(writer, request, operationStart{
		Kind: store.OperationKindBackup, Host: host, RequestJSON: payload,
		Arguments: []string{"--config", application.configPath, "backup", "--host", host, "--json"},
	})
}

func (application *application) handleVerifyAction(writer http.ResponseWriter, request *http.Request) {
	value, _, ok := application.requireSession(writer, request)
	if !ok || !application.requireCSRF(writer, request, value) {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	host := request.PathValue("host")
	if findConfigHost(cfg, host) == nil {
		application.writeAPIError(writer, http.StatusNotFound, "not_found", "host 不存在")
		return
	}
	body := verifyActionRequest{Snapshot: "latest"}
	if err := decodeJSONBody(writer, request, &body); err != nil || strings.TrimSpace(body.Snapshot) == "" {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	payload, _ := json.Marshal(body)
	application.startOperation(writer, request, operationStart{
		Kind: store.OperationKindVerify, Host: host, RequestJSON: payload,
		Arguments: []string{
			"--config", application.configPath, "verify", "--host", host,
			"--snapshot", body.Snapshot, "--json",
		},
	})
}

func (application *application) handleRestoreAction(writer http.ResponseWriter, request *http.Request) {
	value, sessionToken, ok := application.requireSession(writer, request)
	if !ok || !application.requireCSRF(writer, request, value) {
		return
	}
	cfg, ok := application.requireRuntime(writer, request)
	if !ok {
		return
	}
	sourceHost := request.PathValue("host")
	if findConfigHost(cfg, sourceHost) == nil {
		application.writeAPIError(writer, http.StatusNotFound, "not_found", "host 不存在")
		return
	}
	var actionProbe struct {
		Action string `json:"action"`
	}
	body, err := readJSONBody(writer, request)
	if err != nil || json.Unmarshal(body, &actionProbe) != nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	switch actionProbe.Action {
	case "preview":
		application.handleRestorePreview(writer, request, cfg, sourceHost, sessionToken, body)
	case "execute":
		application.handleRestoreExecute(writer, request, sourceHost, sessionToken, body)
	default:
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
	}
}

func (application *application) handleRestorePreview(
	writer http.ResponseWriter,
	request *http.Request,
	cfg *config.Config,
	sourceHost string,
	sessionToken string,
	body []byte,
) {
	preview := restorePreviewRequest{
		Action: "preview", SourceHost: sourceHost, DestinationHost: sourceHost,
		Snapshot: "latest", Mode: "normal",
	}
	if err := decodeStrictJSON(body, &preview); err != nil || preview.Action != "preview" ||
		strings.TrimSpace(preview.DestinationHost) == "" || strings.TrimSpace(preview.Snapshot) == "" ||
		!validRestoreMode(preview.Mode) || findConfigHost(cfg, preview.DestinationHost) == nil {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	preview.SourceHost = sourceHost
	payload, _ := json.Marshal(preview)
	arguments := restorePreviewArguments(application.configPath, preview)
	application.startOperation(writer, request, operationStart{
		Kind: store.OperationKindRestorePreview, Host: sourceHost, RequestJSON: payload,
		Arguments: arguments,
		Preview:   &previewRegistration{SessionHash: hashSessionToken(sessionToken), Request: preview},
	})
}

func (application *application) handleRestoreExecute(
	writer http.ResponseWriter,
	request *http.Request,
	sourceHost string,
	sessionToken string,
	body []byte,
) {
	var execute restoreExecuteRequest
	if err := decodeStrictJSON(body, &execute); err != nil || execute.Action != "execute" ||
		strings.TrimSpace(execute.PreviewOperationID) == "" || strings.TrimSpace(execute.ConfirmationToken) == "" {
		application.writeAPIError(writer, http.StatusBadRequest, "invalid_request", "请求参数无效")
		return
	}
	if application.operations == nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	operation, err := application.operations.startConfirmedRestore(
		request.Context(), sourceHost, execute.PreviewOperationID, execute.ConfirmationToken, sessionToken,
	)
	if err != nil {
		application.writeOperationStartError(writer, err)
		return
	}
	application.writeJSON(writer, http.StatusAccepted, newOperationResponse(operation))
}

func (application *application) startOperation(
	writer http.ResponseWriter,
	request *http.Request,
	start operationStart,
) {
	if application.operations == nil {
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
		return
	}
	operation, err := application.operations.start(request.Context(), start)
	if err != nil {
		application.writeOperationStartError(writer, err)
		return
	}
	application.writeJSON(writer, http.StatusAccepted, newOperationResponse(operation))
}

func (application *application) writeOperationStartError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOperationConflict):
		application.writeAPIError(writer, http.StatusConflict, "conflict", "已有手工任务正在运行")
	case errors.Is(err, errConfirmationExpired):
		application.writeAPIError(writer, http.StatusUnprocessableEntity, "confirmation_expired", "恢复确认已过期")
	case errors.Is(err, errConfirmationInvalid):
		application.writeAPIError(writer, http.StatusConflict, "confirmation_required", "恢复确认无效")
	case errors.Is(err, errOperationStartFailed):
		application.writeAPIError(writer, http.StatusInternalServerError, "operation_failed", "Ark 子进程启动失败")
	default:
		application.writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "服务暂不可用")
	}
}

func (application *application) requireCSRF(
	writer http.ResponseWriter,
	request *http.Request,
	value session,
) bool {
	if constantTimeStringEqual(value.csrfToken, request.Header.Get("X-CSRF-Token")) {
		return true
	}
	application.writeAPIError(writer, http.StatusForbidden, "invalid_request", "CSRF 校验失败")
	return false
}

func decodeJSONBody(writer http.ResponseWriter, request *http.Request, destination any) error {
	body, err := readJSONBody(writer, request)
	if err != nil {
		return err
	}
	return decodeStrictJSON(body, destination)
}

func readJSONBody(writer http.ResponseWriter, request *http.Request) ([]byte, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, fmt.Errorf("Content-Type 必须是 application/json")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumJSONBodyBytes)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func decodeStrictJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON 包含尾随内容")
	}
	return nil
}

func validRestoreMode(mode string) bool {
	return mode == "normal" || mode == "force" || mode == "isolate"
}

func restorePreviewArguments(configPath string, preview restorePreviewRequest) []string {
	arguments := []string{
		"--config", configPath, "restore", "--host", preview.SourceHost,
		"--to", preview.DestinationHost, "--snapshot", preview.Snapshot,
		"--dry-run", "--inspect",
	}
	if preview.Mode == "force" {
		arguments = append(arguments, "--force")
	} else if preview.Mode == "isolate" {
		arguments = append(arguments, "--isolate")
	}
	return append(arguments, "--json")
}

func restoreExecuteArguments(
	configPath string,
	preview restorePreviewRequest,
	exactSnapshot string,
	digest string,
) []string {
	arguments := []string{
		"--config", configPath, "restore", "--host", preview.SourceHost,
		"--to", preview.DestinationHost, "--snapshot", exactSnapshot,
	}
	if preview.Mode == "force" {
		arguments = append(arguments, "--force")
	} else if preview.Mode == "isolate" {
		arguments = append(arguments, "--isolate")
	}
	return append(arguments, "--expected-preview-sha256", digest, "--json")
}
