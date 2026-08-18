package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/silentflower/ark/internal/monitoring"
	"github.com/silentflower/ark/internal/store"
)

const (
	maximumOperationStdoutBytes = 4 * 1024 * 1024
	maximumOperationStderrBytes = 64 * 1024
	operationStopTimeout        = 10 * time.Second
	confirmationTTL             = 10 * time.Minute
)

var (
	errOperationConflict    = errors.New("已有手工任务正在运行")
	errOperationStartFailed = errors.New("Ark 子进程启动失败")
	errConfirmationInvalid  = errors.New("恢复确认无效")
	errConfirmationExpired  = errors.New("恢复确认已过期")
)

type operationManager struct {
	ctx           context.Context
	cancel        context.CancelFunc
	state         apiStore
	binaryPath    string
	configPath    string
	random        io.Reader
	now           func() time.Time
	mu            sync.Mutex
	activeID      string
	closed        bool
	confirmations map[string]*confirmation
	wait          sync.WaitGroup
	closeOnce     sync.Once
}

type operationStart struct {
	Kind              store.OperationKind
	Host              string
	RequestJSON       json.RawMessage
	Arguments         []string
	ParentOperationID string
	Preview           *previewRegistration
}

type previewRegistration struct {
	SessionHash string
	Request     restorePreviewRequest
}

type confirmation struct {
	sessionHash   string
	request       restorePreviewRequest
	tokenHash     string
	issued        bool
	consumed      bool
	expiresAt     time.Time
	digest        string
	exactSnapshot string
}

type previewResult struct {
	Plan      restorePlanWire   `json:"plan"`
	Conflicts []json.RawMessage `json:"conflicts"`
	Digest    string            `json:"digest"`
}

type backupManifestWire struct {
	SchemaVersion int               `json:"schema_version"`
	RunID         string            `json:"run_id"`
	ArkVersion    string            `json:"ark_version"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    time.Time         `json:"finished_at"`
	Hosts         []json.RawMessage `json:"hosts"`
}

type backupOperationResult struct {
	RunID              string                     `json:"run_id"`
	Status             store.Status               `json:"status"`
	Manifest           *backupManifestWire        `json:"manifest,omitempty"`
	ManifestSnapshotID string                     `json:"manifest_snapshot_id"`
	HeartbeatStatus    monitoring.HeartbeatStatus `json:"heartbeat_status"`
	Error              string                     `json:"error"`
}

type verifyOperationResult struct {
	ManifestSnapshotID string            `json:"manifest_snapshot_id"`
	Status             store.Status      `json:"status"`
	Results            []json.RawMessage `json:"results"`
	Error              string            `json:"error,omitempty"`
}

type restorePlanWire struct {
	ManifestSnapshotID string            `json:"manifest_snapshot_id"`
	RunID              string            `json:"run_id"`
	SourceHost         string            `json:"source_host"`
	DestinationHost    string            `json:"destination_host"`
	Project            json.RawMessage   `json:"project"`
	Steps              []json.RawMessage `json:"steps"`
	ManualChecks       []string          `json:"manual_checks"`
}

type restoreOperationResult struct {
	ManifestSnapshotID string            `json:"manifest_snapshot_id"`
	RunID              string            `json:"run_id"`
	SourceHost         string            `json:"source_host"`
	DestinationHost    string            `json:"destination_host"`
	Status             store.Status      `json:"status"`
	Steps              []json.RawMessage `json:"steps"`
	ManualChecks       []string          `json:"manual_checks"`
}

func newOperationManager(
	state apiStore,
	binaryPath string,
	configPath string,
	random io.Reader,
	now func() time.Time,
) (*operationManager, error) {
	if state == nil || random == nil || now == nil || strings.TrimSpace(binaryPath) == "" ||
		strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("创建手工任务管理器失败: 内部依赖不完整")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &operationManager{
		ctx: ctx, cancel: cancel, state: state, binaryPath: binaryPath, configPath: configPath,
		random: random, now: now, confirmations: make(map[string]*confirmation),
	}, nil
}

func (manager *operationManager) start(ctx context.Context, start operationStart) (store.ManualOperation, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.startLocked(ctx, start)
}

func (manager *operationManager) startLocked(
	ctx context.Context,
	start operationStart,
) (store.ManualOperation, error) {
	if manager.closed {
		return store.ManualOperation{}, fmt.Errorf("手工任务管理器已停止")
	}
	if manager.activeID != "" {
		return store.ManualOperation{}, errOperationConflict
	}
	idToken, err := randomToken(manager.random)
	if err != nil {
		return store.ManualOperation{}, fmt.Errorf("生成手工任务 ID 失败: %w", err)
	}
	startedAt := manager.now().UTC()
	operation := store.ManualOperation{
		Kind: start.Kind, Host: start.Host, Status: store.OperationStatusRunning,
		StartedAt: startedAt, RequestJSON: start.RequestJSON,
		ParentOperationID: start.ParentOperationID,
	}
	// randomToken 已提供高熵；再做 SHA-256 只为了得到固定长度、URL 无关的持久化 ID。
	digest := sha256.Sum256([]byte(idToken))
	operation.ID = hex.EncodeToString(digest[:16])
	if err := manager.state.CreateManualOperation(ctx, operation); err != nil {
		return store.ManualOperation{}, err
	}
	stdout := &boundedOperationBuffer{limit: maximumOperationStdoutBytes}
	stderr := &boundedOperationBuffer{limit: maximumOperationStderrBytes}
	command := exec.CommandContext(manager.ctx, manager.binaryPath, start.Arguments...)
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	command.WaitDelay = operationStopTimeout
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(syscall.SIGTERM)
	}

	// HTTP 只有在进程真正启动后才能返回 202，否则客户端会把 exec 失败误认为已受理。
	if err := command.Start(); err != nil {
		finishedAt := manager.now().UTC()
		if finishedAt.Before(operation.StartedAt) {
			finishedAt = operation.StartedAt
		}
		finishErr := manager.finishOperation(operation.ID, store.ManualOperationResult{
			Status: store.OperationStatusFail, FinishedAt: finishedAt,
			Duration: finishedAt.Sub(operation.StartedAt), Error: "Ark 子进程启动失败",
		})
		return store.ManualOperation{}, errors.Join(
			errOperationStartFailed,
			fmt.Errorf("启动 Ark 子进程失败: %w", err),
			finishErr,
		)
	}
	if start.Preview != nil {
		manager.confirmations[operation.ID] = &confirmation{
			sessionHash: start.Preview.SessionHash,
			request:     start.Preview.Request,
		}
	}
	manager.activeID = operation.ID
	manager.wait.Add(1)
	go manager.run(operation, command, stdout, stderr)
	return operation, nil
}

func (manager *operationManager) run(
	operation store.ManualOperation,
	command *exec.Cmd,
	stdout *boundedOperationBuffer,
	stderr *boundedOperationBuffer,
) {
	defer manager.wait.Done()
	runErr := command.Wait()
	finishedAt := manager.now().UTC()
	if finishedAt.Before(operation.StartedAt) {
		finishedAt = operation.StartedAt
	}
	result := store.ManualOperationResult{
		Status: store.OperationStatusOK, FinishedAt: finishedAt,
		Duration: finishedAt.Sub(operation.StartedAt),
	}
	if command.ProcessState != nil {
		exitCode := command.ProcessState.ExitCode()
		result.ExitCode = &exitCode
	}
	parsed, parseErr := parseOperationResult(operation.Kind, stdout)
	if parseErr == nil {
		result.ResultJSON = parsed
	}
	if manager.ctx.Err() != nil {
		result.Status = store.OperationStatusInterrupted
		result.Error = "Hub 正在停止，手工任务已中断"
	} else if runErr != nil {
		result.Status = store.OperationStatusFail
		result.Error = "Ark 子进程执行失败"
	} else if parseErr != nil {
		result.Status = store.OperationStatusFail
		result.Error = "Ark JSON 结果无效"
	}
	if stdout.overflow {
		result.Status = store.OperationStatusFail
		result.ResultJSON = nil
		result.Error = "Ark JSON 输出超过安全上限"
	}
	if stderr.overflow && result.Status == store.OperationStatusOK {
		result.Status = store.OperationStatusFail
		result.Error = "Ark 错误输出超过安全上限"
	}

	_ = manager.finishOperation(operation.ID, result)
	manager.mu.Lock()
	if manager.activeID == operation.ID {
		manager.activeID = ""
	}
	manager.mu.Unlock()
}

func (manager *operationManager) finishOperation(id string, result store.ManualOperationResult) error {
	finishCtx, cancel := context.WithTimeout(context.Background(), operationStopTimeout)
	finishErr := manager.state.FinishManualOperation(finishCtx, id, result)
	cancel()
	if finishErr != nil {
		if recovery, ok := manager.state.(operationRecoveryStore); ok {
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), operationStopTimeout)
			_, _ = recovery.InterruptRunningOperations(recoveryCtx, result.FinishedAt)
			recoveryCancel()
		}
	}
	return finishErr
}

func (manager *operationManager) issueConfirmation(
	operation store.ManualOperation,
	sessionToken string,
) (*string, error) {
	if operation.Kind != store.OperationKindRestorePreview || operation.Status != store.OperationStatusOK {
		return nil, nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry, ok := manager.confirmations[operation.ID]
	if !ok || !constantTimeStringEqual(entry.sessionHash, hashSessionToken(sessionToken)) ||
		entry.issued || entry.consumed {
		return nil, nil
	}
	var preview previewResult
	if err := json.Unmarshal(operation.ResultJSON, &preview); err != nil ||
		strings.TrimSpace(preview.Plan.ManifestSnapshotID) == "" || strings.TrimSpace(preview.Digest) == "" {
		return nil, nil
	}
	token, err := randomToken(manager.random)
	if err != nil {
		return nil, fmt.Errorf("生成恢复确认 token 失败: %w", err)
	}
	entry.tokenHash = hashToken(token)
	entry.issued = true
	entry.expiresAt = manager.now().UTC().Add(confirmationTTL)
	entry.digest = preview.Digest
	entry.exactSnapshot = preview.Plan.ManifestSnapshotID
	return &token, nil
}

func (manager *operationManager) startConfirmedRestore(
	ctx context.Context,
	sourceHost string,
	previewOperationID string,
	confirmationToken string,
	sessionToken string,
) (store.ManualOperation, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.activeID != "" {
		return store.ManualOperation{}, errOperationConflict
	}
	entry, ok := manager.confirmations[previewOperationID]
	if !ok || !entry.issued || entry.consumed ||
		entry.request.SourceHost != sourceHost ||
		!constantTimeStringEqual(entry.sessionHash, hashSessionToken(sessionToken)) ||
		!constantTimeStringEqual(entry.tokenHash, hashToken(confirmationToken)) {
		return store.ManualOperation{}, errConfirmationInvalid
	}
	if !manager.now().UTC().Before(entry.expiresAt) {
		return store.ManualOperation{}, errConfirmationExpired
	}
	requestPayload, err := json.Marshal(struct {
		PreviewOperationID string `json:"preview_operation_id"`
	}{PreviewOperationID: previewOperationID})
	if err != nil {
		return store.ManualOperation{}, err
	}
	arguments := restoreExecuteArguments(manager.configPath, entry.request, entry.exactSnapshot, entry.digest)
	operation, err := manager.startLocked(ctx, operationStart{
		Kind: store.OperationKindRestore, Host: entry.request.SourceHost,
		RequestJSON: requestPayload, Arguments: arguments, ParentOperationID: previewOperationID,
	})
	if err != nil {
		return store.ManualOperation{}, err
	}
	entry.consumed = true
	return operation, nil
}

func (manager *operationManager) close() error {
	manager.closeOnce.Do(func() {
		manager.mu.Lock()
		manager.closed = true
		manager.cancel()
		manager.mu.Unlock()
	})
	manager.wait.Wait()
	return nil
}

func parseOperationResult(kind store.OperationKind, output *boundedOperationBuffer) (json.RawMessage, error) {
	if output.overflow || len(bytes.TrimSpace(output.buffer.Bytes())) == 0 {
		return nil, fmt.Errorf("Ark JSON 输出为空或过大")
	}
	decoder := json.NewDecoder(bytes.NewReader(output.buffer.Bytes()))
	var value map[string]json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("Ark JSON 结果必须是非空对象")
	}
	allowed := operationResultFields(kind)
	if len(allowed) == 0 {
		return nil, fmt.Errorf("Ark JSON 结果类型不受支持")
	}
	for field := range value {
		if _, ok := allowed[field]; !ok {
			return nil, fmt.Errorf("Ark JSON 结果包含未知字段 %q", field)
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("Ark JSON 结果包含尾随内容")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := validateOperationResult(kind, encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

func validateOperationResult(kind store.OperationKind, encoded json.RawMessage) error {
	switch kind {
	case store.OperationKindBackup:
		var result backupOperationResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return err
		}
		if strings.TrimSpace(result.RunID) == "" || !validFinalStatus(result.Status) ||
			!validHeartbeatStatus(result.HeartbeatStatus) {
			return fmt.Errorf("backup JSON 结果缺少有效 run_id、status 或 heartbeat_status")
		}
		if result.Manifest != nil && result.Manifest.RunID != result.RunID {
			return fmt.Errorf("backup JSON 结果的 manifest run_id 不一致")
		}
		if result.Manifest != nil {
			if result.Manifest.SchemaVersion != 1 || strings.TrimSpace(result.Manifest.ArkVersion) == "" ||
				result.Manifest.StartedAt.IsZero() || result.Manifest.FinishedAt.IsZero() ||
				result.Manifest.FinishedAt.Before(result.Manifest.StartedAt) {
				return fmt.Errorf("backup JSON 结果的 manifest 不完整")
			}
			if err := validateJSONObjectList("backup manifest hosts", result.Manifest.Hosts); err != nil {
				return err
			}
		}
		if result.Status != store.StatusFail &&
			(result.Manifest == nil || strings.TrimSpace(result.ManifestSnapshotID) == "") {
			return fmt.Errorf("backup JSON 成功结果缺少 manifest 或 manifest_snapshot_id")
		}
	case store.OperationKindVerify:
		var result verifyOperationResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return err
		}
		if strings.TrimSpace(result.ManifestSnapshotID) == "" || !validFinalStatus(result.Status) ||
			result.Results == nil {
			return fmt.Errorf("verify JSON 结果缺少有效 manifest_snapshot_id、status 或 results")
		}
		if err := validateJSONObjectList("verify results", result.Results); err != nil {
			return err
		}
	case store.OperationKindRestorePreview:
		var result previewResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return err
		}
		if strings.TrimSpace(result.Plan.ManifestSnapshotID) == "" ||
			strings.TrimSpace(result.Plan.RunID) == "" || strings.TrimSpace(result.Plan.SourceHost) == "" ||
			strings.TrimSpace(result.Plan.DestinationHost) == "" || result.Plan.Steps == nil ||
			result.Plan.ManualChecks == nil || result.Conflicts == nil || !validPreviewDigest(result.Digest) {
			return fmt.Errorf("restore preview JSON 结果不完整")
		}
		if err := validateJSONObject("restore preview project", result.Plan.Project); err != nil {
			return err
		}
		if err := validateJSONObjectList("restore preview steps", result.Plan.Steps); err != nil {
			return err
		}
		if err := validateJSONObjectList("restore preview conflicts", result.Conflicts); err != nil {
			return err
		}
	case store.OperationKindRestore:
		var result restoreOperationResult
		if err := json.Unmarshal(encoded, &result); err != nil {
			return err
		}
		if strings.TrimSpace(result.ManifestSnapshotID) == "" || strings.TrimSpace(result.RunID) == "" ||
			strings.TrimSpace(result.SourceHost) == "" || strings.TrimSpace(result.DestinationHost) == "" ||
			!validFinalStatus(result.Status) || result.Steps == nil || result.ManualChecks == nil {
			return fmt.Errorf("restore JSON 结果不完整")
		}
		if err := validateJSONObjectList("restore steps", result.Steps); err != nil {
			return err
		}
	default:
		return fmt.Errorf("Ark JSON 结果类型不受支持")
	}
	return nil
}

func validateJSONObject(field string, value json.RawMessage) error {
	var object map[string]json.RawMessage
	if len(value) == 0 || json.Unmarshal(value, &object) != nil || object == nil {
		return fmt.Errorf("%s 必须是 JSON 对象", field)
	}
	return nil
}

func validateJSONObjectList(field string, values []json.RawMessage) error {
	if values == nil {
		return fmt.Errorf("%s 不能为空", field)
	}
	for index, value := range values {
		if err := validateJSONObject(fmt.Sprintf("%s[%d]", field, index), value); err != nil {
			return err
		}
	}
	return nil
}

func validFinalStatus(status store.Status) bool {
	switch status {
	case store.StatusOK, store.StatusWarn, store.StatusFail:
		return true
	default:
		return false
	}
}

func validHeartbeatStatus(status monitoring.HeartbeatStatus) bool {
	switch status {
	case monitoring.HeartbeatDisabled, monitoring.HeartbeatSent, monitoring.HeartbeatFailed:
		return true
	default:
		return false
	}
}

func validPreviewDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func operationResultFields(kind store.OperationKind) map[string]struct{} {
	fields := make(map[string]struct{})
	var names []string
	switch kind {
	case store.OperationKindBackup:
		names = []string{"run_id", "status", "manifest", "manifest_snapshot_id", "heartbeat_status", "error"}
	case store.OperationKindVerify:
		names = []string{"manifest_snapshot_id", "status", "results", "error"}
	case store.OperationKindRestorePreview:
		names = []string{"plan", "force", "resume", "destructive", "conflicts", "digest"}
	case store.OperationKindRestore:
		names = []string{
			"manifest_snapshot_id", "run_id", "source_host", "destination_host",
			"status", "steps", "manual_checks", "error", "isolation",
		}
	}
	for _, name := range names {
		fields[name] = struct{}{}
	}
	return fields
}

type boundedOperationBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *boundedOperationBuffer) Write(payload []byte) (int, error) {
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		length := len(payload)
		if length > remaining {
			length = remaining
		}
		_, _ = buffer.buffer.Write(payload[:length])
	}
	if len(payload) > remaining {
		buffer.overflow = true
	}
	return len(payload), nil
}

func hashSessionToken(value string) string {
	return hashToken("session\x00" + value)
}

func hashToken(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
