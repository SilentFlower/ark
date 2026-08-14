package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/silentflower/ark/internal/sshexec"
	"github.com/silentflower/ark/internal/store"
)

const composeServiceLabel = "com.docker.compose.service"

// CleanupResult 是隔离资源清理的结构化结果。
type CleanupResult struct {
	// IsolationID 是本次清理定位的完整隔离标识。
	IsolationID string `json:"isolation_id"`
	// DestinationHost 是清单中的目标 host。
	DestinationHost string `json:"destination_host"`
	// Status 是 ok 或 fail。
	Status store.Status `json:"status"`
	// Removed 是已删除的资源摘要。
	Removed []string `json:"removed"`
	// Error 是脱敏后的失败摘要。
	Error string `json:"error,omitempty"`
}

// IsolationOwnership 是一份通过 state、project label、isolation label 和路径校验的只读资源摘要。
type IsolationOwnership struct {
	// IsolationID 是已校验的完整隔离标识。
	IsolationID string `json:"isolation_id"`
	// DestinationHost 是清单中的目标 host。
	DestinationHost string `json:"destination_host"`
	// ProjectName 是隔离 Compose project 名。
	ProjectName string `json:"project_name"`
	// Root 是受保护的隔离根目录。
	Root string `json:"root"`
	// Containers 是已校验归属的容器。
	Containers []IsolationContainer `json:"containers"`
	// Networks 是已校验归属的 network 名称。
	Networks []string `json:"networks"`
	// Volumes 是已校验归属的 volume 名称。
	Volumes []string `json:"volumes"`
	// CleanupCommand 是对应的幂等安全清理命令。
	CleanupCommand string `json:"cleanup_command"`
}

// ValidateIsolationOwnership 只读校验一份隔离状态及全部 Docker 资源归属。
// @param ctx 控制目标机状态、路径与标签检查的取消。
// @param runner destination 的本地或 SSH Runner。
// @param destinationHost 清单中的恢复目标 host。
// @param isolationID 完整的 64 位小写十六进制 isolation ID。
// @return IsolationOwnership 已证明属于该 isolation 的安全资源摘要。
// @return error 状态目录缺失、字段漂移、路径越界或资源标签不匹配时的错误。
func ValidateIsolationOwnership(
	ctx context.Context,
	runner sshexec.Runner,
	destinationHost string,
	isolationID string,
) (IsolationOwnership, error) {
	if ctx == nil || runner == nil {
		return IsolationOwnership{}, fmt.Errorf("校验隔离资源归属失败: context 或 runner 为空")
	}
	if strings.TrimSpace(destinationHost) == "" || !ValidIsolationID(isolationID) {
		return IsolationOwnership{}, fmt.Errorf("校验隔离资源归属失败: destination host 或 isolation ID 无效")
	}
	root := path.Join(isolationBase, isolationID)
	exists, err := targetPathExists(ctx, runner, root)
	if err != nil {
		return IsolationOwnership{}, err
	}
	if !exists {
		return IsolationOwnership{}, fmt.Errorf("校验隔离资源归属失败: isolation root 不存在")
	}
	state, containers, networks, volumes, err := loadOwnedIsolationResources(
		ctx, runner, destinationHost, isolationID, root,
	)
	if err != nil {
		return IsolationOwnership{}, err
	}
	result := IsolationOwnership{
		IsolationID:     isolationID,
		DestinationHost: destinationHost,
		ProjectName:     state.ProjectName,
		Root:            root,
		Networks:        append([]string(nil), networks...),
		Volumes:         append([]string(nil), volumes...),
		CleanupCommand:  fmt.Sprintf("ark restore cleanup --host %s --isolation %s", destinationHost, isolationID),
	}
	for _, container := range containers {
		result.Containers = append(result.Containers, IsolationContainer{ID: container.ID, Service: container.Service})
	}
	return result, nil
}

// CleanupIsolation 校验归属后幂等删除一份隔离恢复的全部资源。
// @param ctx 控制目标机检查和删除命令的取消。
// @param runner destination 的本地或 SSH Runner。
// @param destinationHost 清单中的恢复目标 host。
// @param isolationID 完整的 64 位小写十六进制 isolation ID。
// @return CleanupResult 已删除资源和最终状态。
// @return error 状态、标签、路径校验或删除失败时返回错误。
func CleanupIsolation(
	ctx context.Context,
	runner sshexec.Runner,
	destinationHost string,
	isolationID string,
) (CleanupResult, error) {
	result := CleanupResult{
		IsolationID:     isolationID,
		DestinationHost: destinationHost,
		Removed:         []string{},
	}
	if ctx == nil || runner == nil {
		return failCleanupResult(result, fmt.Errorf("清理隔离资源失败: context 或 runner 为空"))
	}
	if strings.TrimSpace(destinationHost) == "" || !ValidIsolationID(isolationID) {
		return failCleanupResult(result, fmt.Errorf("清理隔离资源失败: destination host 或 isolation ID 无效"))
	}

	root := path.Join(isolationBase, isolationID)
	exists, err := targetPathExists(ctx, runner, root)
	if err != nil {
		return failCleanupResult(result, err)
	}
	if !exists {
		orphans, err := listIsolationLabeledResources(ctx, runner, isolationID)
		if err != nil {
			return failCleanupResult(result, err)
		}
		if len(orphans) > 0 {
			return failCleanupResult(result, fmt.Errorf(
				"隔离恢复状态目录不存在，但仍发现带 isolation 标签的 Docker 资源: %s",
				strings.Join(orphans, ", "),
			))
		}
		result.Status = store.StatusOK
		return result, nil
	}
	_, containers, networks, volumes, err := loadOwnedIsolationResources(
		ctx, runner, destinationHost, isolationID, root,
	)
	if err != nil {
		return failCleanupResult(result, err)
	}
	if len(containers) > 0 {
		ids := make([]string, 0, len(containers))
		for _, container := range containers {
			ids = append(ids, container.ID)
		}
		sort.Strings(ids)
		if _, err := runner.Run(ctx, append([]string{"docker", "rm", "-f"}, ids...)...); err != nil {
			return failCleanupResult(result, fmt.Errorf("删除隔离容器失败: %w", err))
		}
		result.Removed = append(result.Removed, "containers")
	}
	for _, networkName := range networks {
		if _, err := runner.Run(ctx, "docker", "network", "rm", networkName); err != nil {
			return failCleanupResult(result, fmt.Errorf("删除隔离 network %q 失败: %w", networkName, err))
		}
		result.Removed = append(result.Removed, "network:"+networkName)
	}
	for _, volumeName := range volumes {
		if _, err := runner.Run(ctx, "docker", "volume", "rm", volumeName); err != nil {
			return failCleanupResult(result, fmt.Errorf("删除隔离 volume %q 失败: %w", volumeName, err))
		}
		result.Removed = append(result.Removed, "volume:"+volumeName)
	}
	// 状态目录是失败重试时证明资源归属的最后凭据；确认 Docker 资源已无残留后才能删除。
	remaining, err := listIsolationLabeledResources(ctx, runner, isolationID)
	if err != nil {
		return failCleanupResult(result, err)
	}
	if len(remaining) > 0 {
		return failCleanupResult(result, fmt.Errorf(
			"清理后仍发现带 isolation 标签的 Docker 资源: %s",
			strings.Join(remaining, ", "),
		))
	}
	if _, err := runner.Run(ctx, "rm", "-rf", "--", root); err != nil {
		return failCleanupResult(result, fmt.Errorf("删除隔离恢复目录失败: %w", err))
	}
	result.Removed = append(result.Removed, "root:"+root)
	result.Status = store.StatusOK
	return result, nil
}

func loadOwnedIsolationResources(
	ctx context.Context,
	runner sshexec.Runner,
	destinationHost string,
	isolationID string,
	root string,
) (isolationState, []composeState, []string, []string, error) {
	if err := validateCleanupPath(ctx, runner, root, root); err != nil {
		return isolationState{}, nil, nil, nil, err
	}
	statePath := path.Join(root, "state.json")
	if err := validateCleanupPath(ctx, runner, root, statePath); err != nil {
		return isolationState{}, nil, nil, nil, err
	}
	content, found, err := readFileIfExists(ctx, runner, statePath)
	if err != nil {
		return isolationState{}, nil, nil, nil, fmt.Errorf("读取隔离恢复状态失败: %w", err)
	}
	if !found {
		return isolationState{}, nil, nil, nil, fmt.Errorf("隔离恢复目录缺少 state.json，拒绝处理")
	}
	var state isolationState
	if err := json.Unmarshal([]byte(content), &state); err != nil {
		return isolationState{}, nil, nil, nil, fmt.Errorf("解析隔离恢复状态失败: %w", err)
	}
	if err := validateCleanupState(state, destinationHost, isolationID, root); err != nil {
		return isolationState{}, nil, nil, nil, err
	}
	containers, networks, volumes, err := inspectCleanupResources(ctx, runner, state)
	if err != nil {
		return isolationState{}, nil, nil, nil, err
	}
	if err := validateIsolationLabeledResourceSet(ctx, runner, state.ID, containers, networks, volumes); err != nil {
		return isolationState{}, nil, nil, nil, err
	}
	return state, containers, networks, volumes, nil
}

func validateIsolationLabeledResourceSet(
	ctx context.Context,
	runner sshexec.Runner,
	isolationID string,
	containers []composeState,
	networks []string,
	volumes []string,
) error {
	expected := make([]string, 0, len(containers)+len(networks)+len(volumes))
	for _, container := range containers {
		expected = append(expected, "container:"+container.ID)
	}
	for _, network := range networks {
		expected = append(expected, "network:"+network)
	}
	for _, volume := range volumes {
		expected = append(expected, "volume:"+volume)
	}
	expected = uniqueSorted(expected)
	actual, err := listIsolationLabeledResources(ctx, runner, isolationID)
	if err != nil {
		return err
	}
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf(
			"isolation 标签资源与已校验状态不一致: 实际 %s，期望 %s",
			strings.Join(actual, ", "),
			strings.Join(expected, ", "),
		)
	}
	return nil
}

func listIsolationLabeledResources(
	ctx context.Context,
	runner sshexec.Runner,
	isolationID string,
) ([]string, error) {
	queries := []struct {
		resource string
		argv     []string
	}{
		{
			resource: "container",
			argv: []string{
				"docker", "ps", "-a", "--no-trunc",
				"--filter", "label=" + isolationLabel + "=" + isolationID,
				"--format", "{{.ID}}",
			},
		},
		{
			resource: "network",
			argv: []string{
				"docker", "network", "ls",
				"--filter", "label=" + isolationLabel + "=" + isolationID,
				"--format", "{{.Name}}",
			},
		},
		{
			resource: "volume",
			argv: []string{
				"docker", "volume", "ls",
				"--filter", "label=" + isolationLabel + "=" + isolationID,
				"--format", "{{.Name}}",
			},
		},
	}
	var resources []string
	for _, query := range queries {
		out, err := runner.Run(ctx, query.argv...)
		if err != nil {
			return nil, fmt.Errorf("查询 isolation 标签关联的 %s 失败: %w", query.resource, err)
		}
		for _, name := range nonEmptyLines(out) {
			resources = append(resources, query.resource+":"+name)
		}
	}
	return uniqueSorted(resources), nil
}

func failCleanupResult(result CleanupResult, err error) (CleanupResult, error) {
	result.Status = store.StatusFail
	result.Error = "隔离资源清理未完成"
	return result, err
}

// ValidIsolationID 判断输入是否为完整的 64 位小写十六进制隔离标识。
// @param value 待校验的 isolation ID。
// @return bool 格式完全符合约束时为 true。
func ValidIsolationID(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if character >= '0' && character <= '9' {
			continue
		}
		if character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validateCleanupPath(ctx context.Context, runner sshexec.Runner, root string, targetPath string) error {
	if root == isolationBase || !strings.HasPrefix(root, isolationBase+"/") ||
		(targetPath != root && !strings.HasPrefix(targetPath, root+"/")) {
		return fmt.Errorf("隔离恢复路径 %q 越过允许范围", targetPath)
	}
	return validateExactIsolationPath(ctx, runner, targetPath)
}

func validateCleanupState(state isolationState, destinationHost string, isolationID string, root string) error {
	if state.SchemaVersion != isolationSchemaVersion || state.ID != isolationID ||
		!isolationPurposePattern.MatchString(state.Purpose) ||
		state.Destination != destinationHost || state.Root != root ||
		state.ComposeFile != path.Join(root, "compose.generated.json") ||
		!strings.HasSuffix(state.ProjectName, "-"+state.Purpose+"-"+isolationID[:12]) {
		return fmt.Errorf("隔离恢复状态与清理参数不一致，拒绝删除")
	}
	return nil
}

func inspectCleanupResources(
	ctx context.Context,
	runner sshexec.Runner,
	state isolationState,
) ([]composeState, []string, []string, error) {
	containerIDs, err := listCleanupContainers(ctx, runner, state.ProjectName)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, expected := range state.Containers {
		exists, err := cleanupContainerExists(ctx, runner, expected.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		if exists {
			containerIDs = append(containerIDs, expected.ID)
		}
	}
	containerIDs = uniqueSorted(containerIDs)
	expectedServices := cleanupStringSet(state.Services)
	expectedContainers := make(map[string]string, len(state.Containers))
	for _, container := range state.Containers {
		expectedContainers[container.ID] = container.Service
	}
	seenServices := make(map[string]int, len(state.Services))
	containers := make([]composeState, 0, len(containerIDs))
	for _, containerID := range containerIDs {
		labels, err := inspectResourceLabels(ctx, runner, "container", containerID)
		if err != nil || labels[composeProjectLabel] != state.ProjectName || labels[isolationLabel] != state.ID {
			return nil, nil, nil, errors.Join(fmt.Errorf("容器 %q 隔离标签不匹配", containerID), err)
		}
		service := labels[composeServiceLabel]
		if !expectedServices[service] {
			return nil, nil, nil, fmt.Errorf("Compose project %q 包含未记录容器", state.ProjectName)
		}
		if expectedService, recorded := expectedContainers[containerID]; recorded && expectedService != service {
			return nil, nil, nil, fmt.Errorf("容器 %q service 与隔离状态不匹配", containerID)
		}
		seenServices[service]++
		if seenServices[service] > 1 {
			return nil, nil, nil, fmt.Errorf("Compose service %q 存在多个容器，拒绝清理", service)
		}
		containers = append(containers, composeState{ID: containerID, Service: service})
	}
	networks, err := listCleanupResources(ctx, runner, "network", state.ProjectName)
	if err != nil {
		return nil, nil, nil, err
	}
	networks, err = includeExistingCleanupResources(ctx, runner, "network", networks, state.Networks)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateCleanupResources(ctx, runner, "network", networks, state.Networks, state); err != nil {
		return nil, nil, nil, err
	}
	volumes, err := listProjectVolumes(ctx, runner, state.ProjectName)
	if err != nil {
		return nil, nil, nil, err
	}
	volumes, err = includeExistingCleanupResources(ctx, runner, "volume", volumes, state.Volumes)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := validateCleanupResources(ctx, runner, "volume", volumes, state.Volumes, state); err != nil {
		return nil, nil, nil, err
	}
	sort.Strings(networks)
	sort.Strings(volumes)
	return containers, networks, volumes, nil
}

func cleanupContainerExists(ctx context.Context, runner sshexec.Runner, containerID string) (bool, error) {
	if strings.TrimSpace(containerID) == "" {
		return false, fmt.Errorf("隔离状态包含空容器 ID")
	}
	out, err := runner.Run(ctx,
		"docker", "ps", "-aq", "--no-trunc", "--filter", "id="+containerID, "--format", "{{.ID}}")
	if err != nil {
		return false, fmt.Errorf("查询隔离容器 %q 失败: %w", containerID, err)
	}
	for _, actualID := range nonEmptyLines(out) {
		if actualID == containerID {
			return true, nil
		}
	}
	return false, nil
}

func includeExistingCleanupResources(
	ctx context.Context,
	runner sshexec.Runner,
	resource string,
	actual []string,
	expected []string,
) ([]string, error) {
	result := append([]string(nil), actual...)
	for _, name := range expected {
		exists, err := cleanupResourceExists(ctx, runner, resource, name)
		if err != nil {
			return nil, err
		}
		if exists {
			result = append(result, name)
		}
	}
	return uniqueSorted(result), nil
}

func cleanupResourceExists(
	ctx context.Context,
	runner sshexec.Runner,
	resource string,
	name string,
) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, fmt.Errorf("隔离状态包含空 %s 名称", resource)
	}
	out, err := runner.Run(ctx,
		"docker", resource, "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false, fmt.Errorf("查询隔离 %s %q 失败: %w", resource, name, err)
	}
	for _, actualName := range nonEmptyLines(out) {
		if actualName == name {
			return true, nil
		}
	}
	return false, nil
}

func listCleanupContainers(ctx context.Context, runner sshexec.Runner, projectName string) ([]string, error) {
	out, err := runner.Run(ctx,
		"docker", "ps", "-a", "--no-trunc", "--filter", "label="+composeProjectLabel+"="+projectName, "--format", "{{.ID}}")
	if err != nil {
		return nil, fmt.Errorf("查询 Compose project 容器失败: %w", err)
	}
	return nonEmptyLines(out), nil
}

func listCleanupResources(ctx context.Context, runner sshexec.Runner, resource string, projectName string) ([]string, error) {
	out, err := runner.Run(ctx,
		"docker", resource, "ls", "--filter", "label="+composeProjectLabel+"="+projectName, "--format", "{{.Name}}")
	if err != nil {
		return nil, fmt.Errorf("查询 Compose project %s 失败: %w", resource, err)
	}
	return nonEmptyLines(out), nil
}

func validateCleanupResources(
	ctx context.Context,
	runner sshexec.Runner,
	resource string,
	actual []string,
	expected []string,
	state isolationState,
) error {
	expectedSet := cleanupStringSet(expected)
	for _, name := range actual {
		if !expectedSet[name] {
			return fmt.Errorf("Compose project %q 包含未记录 %s %q", state.ProjectName, resource, name)
		}
		labels, err := inspectResourceLabels(ctx, runner, resource, name)
		if err != nil || labels[composeProjectLabel] != state.ProjectName || labels[isolationLabel] != state.ID {
			return errors.Join(fmt.Errorf("%s %q 隔离标签不匹配", resource, name), err)
		}
	}
	return nil
}

func inspectResourceLabels(ctx context.Context, runner sshexec.Runner, resource string, name string) (map[string]string, error) {
	format := "{{json .Labels}}"
	if resource == "container" {
		format = "{{json .Config.Labels}}"
	}
	out, err := runner.Run(ctx, "docker", resource, "inspect", "--format", format, name)
	if err != nil {
		return nil, fmt.Errorf("查询 %s %q 标签失败: %w", resource, name, err)
	}
	labels := make(map[string]string)
	if trimmed := strings.TrimSpace(out); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
			return nil, fmt.Errorf("解析 %s %q 标签失败: %w", resource, name, err)
		}
	}
	return labels, nil
}

func cleanupStringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
