package restore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/silentflower/ark/internal/backup"
	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
	"gopkg.in/yaml.v3"
)

const (
	isolationSchemaVersion         = 1
	isolationLabel                 = "io.ark.restore.isolation"
	isolationBase                  = restoreMarkerBase + "/isolations"
	isolationProjectMaximumLength  = 63
	isolationResourceMaximumLength = 255
)

const (
	// IsolationPurposeRestore 是人工隔离恢复的默认用途。
	IsolationPurposeRestore = "restore"
	// IsolationPurposeVerify 是自动恢复演练使用的隔离用途。
	IsolationPurposeVerify = "verify"
	// IsolationPortRuntimeAuto 保留原 host IP 语义，并由 Docker 原子分配宿主机端口。
	IsolationPortRuntimeAuto = "runtime_auto"
	// IsolationPortDisabled 删除全部 published ports，供不应暴露宿主机端口的自动演练使用。
	IsolationPortDisabled = "disabled"
)

var (
	isolationProjectCharacter = regexp.MustCompile(`[^a-z0-9_-]+`)
	isolationPurposePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	isolationInstancePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// IsolationOptions 控制稳定隔离身份的用途、实例键与端口策略。
type IsolationOptions struct {
	// Purpose 区分 restore、verify 等调用方，并进入资源名和稳定 ID。
	Purpose string `json:"purpose"`
	// InstanceKey 允许同一恢复事实按调用方实例继续细分；普通 restore 为空。
	InstanceKey string `json:"instance_key,omitempty"`
	// PortAllocation 控制隔离 Compose 的 published ports；为空时保持普通恢复的自动端口行为。
	PortAllocation string `json:"port_allocation,omitempty"`
}

// IsolationSpec 描述 dry-run 与真实执行共享的隔离恢复身份和派生路径。
type IsolationSpec struct {
	// SchemaVersion 是隔离状态模型版本。
	SchemaVersion int `json:"schema_version"`
	// ID 是由恢复事实稳定派生的完整隔离标识。
	ID string `json:"id"`
	// ShortID 是用于 Compose 资源名的短标识。
	ShortID string `json:"short_id"`
	// Purpose 区分人工 restore 与后续 verify 编排。
	Purpose string `json:"purpose"`
	// InstanceKey 是调用方提供的稳定实例键；普通 restore 为空。
	InstanceKey string `json:"instance_key,omitempty"`
	// ProjectName 是隔离 Compose project 名。
	ProjectName string `json:"project_name"`
	// Root 是目标机上的 root-only 隔离状态根目录。
	Root string `json:"root"`
	// FilesRoot 是 files target 的安全恢复根目录。
	FilesRoot string `json:"files_root"`
	// SourceProject 保存生成隔离 Compose 配置所需的原项目定位。
	SourceProject Project `json:"source_project"`
	// SourceComposeFile 是原 compose 文件在 FilesRoot 内的映射路径。
	SourceComposeFile string `json:"source_compose_file"`
	// SourceEnvFile 是原 env 文件在 FilesRoot 内的映射路径，可为空。
	SourceEnvFile string `json:"source_env_file,omitempty"`
	// GeneratedComposeFile 是结构化改写后的 root-only Compose 文件。
	GeneratedComposeFile string `json:"generated_compose_file"`
	// PortAllocation 表示宿主机端口的隔离策略。
	PortAllocation string `json:"port_allocation"`
	// PathMappings 是原 files 路径到隔离路径的稳定映射。
	PathMappings []IsolationPathMapping `json:"path_mappings"`
	// VolumeMappings 是原 volume 名到隔离 volume 名的稳定映射。
	VolumeMappings []IsolationResourceMapping `json:"volume_mappings"`
	// Ports 是备份时记录的完整 published port 映射；隔离宿主机端口在执行前为 auto。
	Ports []IsolationPort `json:"ports"`
}

// IsolationPathMapping 描述一个原宿主机路径到隔离路径的映射。
type IsolationPathMapping struct {
	// Source 是原恢复目标绝对路径。
	Source string `json:"source"`
	// Destination 是 isolation files 根内的目标路径。
	Destination string `json:"destination"`
}

// IsolationResourceMapping 描述原 Docker 资源名到隔离资源名的映射。
type IsolationResourceMapping struct {
	// Source 是原物理资源名。
	Source string `json:"source"`
	// Destination 是隔离物理资源名。
	Destination string `json:"destination"`
}

// IsolationPort 描述隔离恢复前后的单个 published port 映射。
type IsolationPort struct {
	// Service 是 Compose service 名。
	Service string `json:"service"`
	// HostIP 是原 Compose 的宿主机绑定地址，可为空。
	HostIP string `json:"host_ip,omitempty"`
	// OriginalPublished 是原宿主机端口；原配置未指定时为空。
	OriginalPublished string `json:"original_published,omitempty"`
	// AllocatedPort 是 Docker 实际分配的宿主机端口；dry-run 为 auto。
	AllocatedPort string `json:"allocated_port"`
	// Target 是容器端口。
	Target uint16 `json:"target"`
	// Protocol 是 tcp 或 udp。
	Protocol string `json:"protocol"`
	// AppProtocol 是 Compose 可选应用协议提示。
	AppProtocol string `json:"app_protocol,omitempty"`
	// Mode 是 Compose 端口发布模式，可为空。
	Mode string `json:"mode,omitempty"`
}

// IsolationResult 是隔离恢复可安全输出的资源和访问摘要。
type IsolationResult struct {
	// ID 是完整 isolation ID。
	ID string `json:"id"`
	// ProjectName 是隔离 Compose project 名。
	ProjectName string `json:"project_name"`
	// Root 是目标机上的隔离根目录。
	Root string `json:"root"`
	// GeneratedComposeFile 是结构化改写后的 Compose 配置路径。
	GeneratedComposeFile string `json:"generated_compose_file"`
	// Containers 是已创建且完成归属校验的容器。
	Containers []IsolationContainer `json:"containers"`
	// Volumes 是隔离 Compose 使用的具名 volume。
	Volumes []string `json:"volumes"`
	// Networks 是隔离 Compose 使用的具名 network。
	Networks []string `json:"networks"`
	// Ports 是启动后确认的实际端口映射。
	Ports []IsolationPort `json:"ports"`
	// CleanupCommand 是幂等安全清理命令。
	CleanupCommand string `json:"cleanup_command"`
}

// IsolationContainer 描述一个已创建的隔离 Compose 容器。
type IsolationContainer struct {
	// ID 是 Docker 容器完整 ID。
	ID string `json:"id"`
	// Service 是对应的 Compose service 名。
	Service string `json:"service"`
}

type isolationState struct {
	SchemaVersion int                  `json:"schema_version"`
	ID            string               `json:"id"`
	Purpose       string               `json:"purpose"`
	ExecutionID   string               `json:"execution_id"`
	Destination   string               `json:"destination_host"`
	ProjectName   string               `json:"project_name"`
	Root          string               `json:"root"`
	ComposeFile   string               `json:"compose_file"`
	Services      []string             `json:"services,omitempty"`
	Containers    []IsolationContainer `json:"containers,omitempty"`
	Volumes       []string             `json:"volumes,omitempty"`
	Networks      []string             `json:"networks,omitempty"`
	Ports         []IsolationPort      `json:"ports,omitempty"`
}

// WithIsolation 把普通恢复 Plan 转换为稳定、可续跑的隔离恢复 Plan。
// @param plan 已由 BuildPlan 构建的普通恢复计划。
// @return Plan project、files 和 volume 已派生到隔离资源的计划副本。
// @return error Plan 已隔离、字段不完整或 compose/env 未包含在 files target 时返回错误。
func WithIsolation(plan Plan) (Plan, error) {
	return WithIsolationOptions(plan, IsolationOptions{
		Purpose:        IsolationPurposeRestore,
		PortAllocation: IsolationPortRuntimeAuto,
	})
}

// WithIsolationOptions 按用途、实例键和端口策略把普通恢复 Plan 转换为稳定隔离 Plan。
// @param plan 已由 BuildPlan 构建的普通恢复计划。
// @param options 非空且合法的 purpose，以及可选稳定 instance key 和 published ports 策略。
// @return Plan project、files 和 volume 已派生到隔离资源的计划副本。
// @return error Plan 或 options 无效、compose/env 未进入 files target 时返回错误。
func WithIsolationOptions(plan Plan, options IsolationOptions) (Plan, error) {
	if plan.Isolation != nil {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: Plan 已包含 isolation")
	}
	if !isolationPurposePattern.MatchString(options.Purpose) {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: isolation purpose %q 无效", options.Purpose)
	}
	if options.InstanceKey != "" && !isolationInstancePattern.MatchString(options.InstanceKey) {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: isolation instance key 无效")
	}
	if options.PortAllocation == "" {
		options.PortAllocation = IsolationPortRuntimeAuto
	}
	if options.PortAllocation != IsolationPortRuntimeAuto && options.PortAllocation != IsolationPortDisabled {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: isolation port allocation %q 无效", options.PortAllocation)
	}
	if strings.TrimSpace(plan.ManifestSnapshotID) == "" || strings.TrimSpace(plan.SourceHost) == "" ||
		strings.TrimSpace(plan.DestinationHost) == "" || strings.TrimSpace(plan.Project.Name) == "" ||
		strings.TrimSpace(plan.Project.ComposeFile) == "" {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: Plan 不完整")
	}
	if !planPathCovered(plan, plan.Project.ComposeFile) {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: compose_file %q 未包含在 files target 中", plan.Project.ComposeFile)
	}
	if plan.Project.EnvFile != "" && !planPathCovered(plan, plan.Project.EnvFile) {
		return Plan{}, fmt.Errorf("构建隔离恢复计划失败: env_file %q 未包含在 files target 中", plan.Project.EnvFile)
	}
	ports, err := isolationPlanPorts(plan, options.PortAllocation)
	if err != nil {
		return Plan{}, err
	}

	originalProject := plan.Project
	id := isolationID(plan, options.Purpose, options.InstanceKey)
	shortID := id[:12]
	root := path.Join(isolationBase, id)
	filesRoot := path.Join(root, "files")
	projectName := isolationProjectName(effectiveProjectName(originalProject), options.Purpose, shortID)
	generatedComposeFile := path.Join(root, "compose.generated.json")
	isolated := copyPlan(plan)
	isolated.Isolation = &IsolationSpec{
		SchemaVersion:        isolationSchemaVersion,
		ID:                   id,
		ShortID:              shortID,
		Purpose:              options.Purpose,
		InstanceKey:          options.InstanceKey,
		ProjectName:          projectName,
		Root:                 root,
		FilesRoot:            filesRoot,
		SourceProject:        originalProject,
		SourceComposeFile:    isolationPath(filesRoot, originalProject.ComposeFile),
		GeneratedComposeFile: generatedComposeFile,
		PortAllocation:       options.PortAllocation,
		Ports:                ports,
	}
	if originalProject.EnvFile != "" {
		isolated.Isolation.SourceEnvFile = isolationPath(filesRoot, originalProject.EnvFile)
	}
	isolated.Project = Project{
		Name:        originalProject.Name,
		ComposeFile: generatedComposeFile,
		ProjectName: projectName,
	}
	for index := range isolated.Steps {
		step := &isolated.Steps[index]
		if step.Target == nil {
			continue
		}
		switch step.TargetType {
		case config.TargetFiles:
			for pathIndex, targetPath := range step.Target.Paths {
				mapped := isolationPath(filesRoot, targetPath)
				isolated.Isolation.PathMappings = append(isolated.Isolation.PathMappings, IsolationPathMapping{
					Source: targetPath, Destination: mapped,
				})
				step.Target.Paths[pathIndex] = mapped
			}
		case config.TargetVolume:
			mapped := isolationResourceName(step.Target.Name, options.Purpose, shortID)
			isolated.Isolation.VolumeMappings = append(isolated.Isolation.VolumeMappings, IsolationResourceMapping{
				Source: step.Target.Name, Destination: mapped,
			})
			step.Target.Name = mapped
		}
	}
	return isolated, nil
}

func validateIsolationPlan(plan Plan) error {
	spec := plan.Isolation
	if spec == nil {
		return nil
	}
	if spec.SchemaVersion != isolationSchemaVersion || !ValidIsolationID(spec.ID) ||
		spec.ShortID != spec.ID[:12] || !isolationPurposePattern.MatchString(spec.Purpose) ||
		(spec.InstanceKey != "" && !isolationInstancePattern.MatchString(spec.InstanceKey)) {
		return fmt.Errorf("执行恢复失败: isolation 身份无效")
	}
	if spec.PortAllocation != IsolationPortRuntimeAuto && spec.PortAllocation != IsolationPortDisabled {
		return fmt.Errorf("执行恢复失败: isolation 端口策略无效")
	}
	identityPlan := plan
	identityPlan.Project = spec.SourceProject
	identityPlan.Isolation = nil
	if spec.ID != isolationID(identityPlan, spec.Purpose, spec.InstanceKey) ||
		spec.ProjectName != isolationProjectName(effectiveProjectName(spec.SourceProject), spec.Purpose, spec.ShortID) {
		return fmt.Errorf("执行恢复失败: isolation 身份派生不一致")
	}
	expectedRoot := path.Join(isolationBase, spec.ID)
	if spec.Root != expectedRoot || spec.FilesRoot != path.Join(expectedRoot, "files") ||
		spec.GeneratedComposeFile != path.Join(expectedRoot, "compose.generated.json") {
		return fmt.Errorf("执行恢复失败: isolation 路径模型无效")
	}
	if plan.Project.ProjectName != spec.ProjectName || plan.Project.ComposeFile != spec.GeneratedComposeFile ||
		plan.Project.EnvFile != "" || strings.TrimSpace(spec.ProjectName) == "" {
		return fmt.Errorf("执行恢复失败: isolation project 与 Plan 不一致")
	}
	if spec.SourceComposeFile != isolationPath(spec.FilesRoot, spec.SourceProject.ComposeFile) ||
		(spec.SourceProject.EnvFile == "" && spec.SourceEnvFile != "") ||
		(spec.SourceProject.EnvFile != "" && spec.SourceEnvFile != isolationPath(spec.FilesRoot, spec.SourceProject.EnvFile)) {
		return fmt.Errorf("执行恢复失败: isolation Compose 路径映射无效")
	}
	for _, mapping := range spec.PathMappings {
		if path.Clean(mapping.Source) == "/" || !strings.HasPrefix(path.Clean(mapping.Source), "/") ||
			mapping.Destination != isolationPath(spec.FilesRoot, mapping.Source) {
			return fmt.Errorf("执行恢复失败: isolation files 路径映射无效")
		}
	}
	if err := validateIsolationPorts(spec.Ports, spec.PortAllocation); err != nil {
		return err
	}
	for _, step := range plan.Steps {
		if step.Target == nil {
			continue
		}
		switch step.TargetType {
		case config.TargetFiles:
			for _, targetPath := range step.Target.Paths {
				if targetPath == spec.FilesRoot || !strings.HasPrefix(path.Clean(targetPath), spec.FilesRoot+"/") {
					return fmt.Errorf("执行恢复失败: isolation files target 越过专用根目录")
				}
			}
		case config.TargetVolume:
			matched := false
			for _, mapping := range spec.VolumeMappings {
				if mapping.Destination == step.Target.Name &&
					mapping.Destination == isolationResourceName(mapping.Source, spec.Purpose, spec.ShortID) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("执行恢复失败: isolation volume 映射无效")
			}
		}
	}
	return nil
}

func isolationPlanPorts(plan Plan, allocation string) ([]IsolationPort, error) {
	var metadata *backup.ComposeMetadata
	for _, step := range plan.Steps {
		if step.Phase != PhaseImageDigest {
			continue
		}
		if metadata != nil {
			return nil, fmt.Errorf("构建隔离恢复计划失败: 存在多个 image digest 步骤")
		}
		metadata = step.composeMetadata
	}
	if metadata == nil {
		return nil, fmt.Errorf("构建隔离恢复计划失败: 备份 manifest 缺少 Compose 端口元数据，请先重新执行 backup")
	}
	ports := make([]IsolationPort, 0, len(metadata.PublishedPorts))
	allocated := "auto"
	if allocation == IsolationPortDisabled {
		allocated = IsolationPortDisabled
	}
	for _, source := range metadata.PublishedPorts {
		ports = append(ports, IsolationPort{
			Service:           source.Service,
			HostIP:            source.HostIP,
			OriginalPublished: source.Published,
			AllocatedPort:     allocated,
			Target:            source.Target,
			Protocol:          source.Protocol,
			AppProtocol:       source.AppProtocol,
			Mode:              source.Mode,
		})
	}
	if err := validateIsolationPorts(ports, allocation); err != nil {
		return nil, fmt.Errorf("构建隔离恢复计划失败: %w", err)
	}
	sortIsolationPorts(ports)
	return ports, nil
}

func validateIsolationPorts(ports []IsolationPort, allocation string) error {
	seen := make(map[string]struct{}, len(ports))
	expectedAllocated := "auto"
	if allocation == IsolationPortDisabled {
		expectedAllocated = IsolationPortDisabled
	}
	for index, port := range ports {
		if strings.TrimSpace(port.Service) == "" || port.Target == 0 ||
			(port.Protocol != "tcp" && port.Protocol != "udp") || port.AllocatedPort != expectedAllocated {
			return fmt.Errorf("isolation ports[%d] 无效", index)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", port.Service, port.HostIP, port.Target, port.Protocol)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("isolation ports[%d] 与已有端口映射重复", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func sortIsolationPorts(ports []IsolationPort) {
	sort.Slice(ports, func(left int, right int) bool {
		leftPort := ports[left]
		rightPort := ports[right]
		if leftPort.Service != rightPort.Service {
			return leftPort.Service < rightPort.Service
		}
		if leftPort.HostIP != rightPort.HostIP {
			return leftPort.HostIP < rightPort.HostIP
		}
		if leftPort.Target != rightPort.Target {
			return leftPort.Target < rightPort.Target
		}
		if leftPort.Protocol != rightPort.Protocol {
			return leftPort.Protocol < rightPort.Protocol
		}
		if leftPort.OriginalPublished != rightPort.OriginalPublished {
			return leftPort.OriginalPublished < rightPort.OriginalPublished
		}
		if leftPort.AppProtocol != rightPort.AppProtocol {
			return leftPort.AppProtocol < rightPort.AppProtocol
		}
		return leftPort.Mode < rightPort.Mode
	})
}

// IsolationPath 把原宿主机绝对路径映射到 Plan 的隔离 files 根目录。
// @param plan 已启用隔离恢复的 Plan。
// @param originalPath 原恢复目标绝对路径。
// @return string 隔离根目录内的目标路径。
// @return error Plan 未启用隔离或路径非法时返回错误。
func IsolationPath(plan Plan, originalPath string) (string, error) {
	if plan.Isolation == nil {
		return "", fmt.Errorf("映射隔离路径失败: Plan 未启用 isolation")
	}
	if !strings.HasPrefix(path.Clean(originalPath), "/") || path.Clean(originalPath) == "/" {
		return "", fmt.Errorf("映射隔离路径失败: 路径 %q 必须是非根绝对路径", originalPath)
	}
	return isolationPath(plan.Isolation.FilesRoot, originalPath), nil
}

func copyPlan(plan Plan) Plan {
	copied := plan
	copied.Project = plan.Project
	copied.ManualChecks = append([]string(nil), plan.ManualChecks...)
	copied.Steps = make([]Step, len(plan.Steps))
	for index, step := range plan.Steps {
		copied.Steps[index] = step
		if step.Target != nil {
			target := *step.Target
			target.Paths = append([]string(nil), step.Target.Paths...)
			target.Services = append([]string(nil), step.Target.Services...)
			copied.Steps[index].Target = &target
		}
		if step.ImageDigests != nil {
			copied.Steps[index].ImageDigests = make(map[string]string, len(step.ImageDigests))
			for service, digest := range step.ImageDigests {
				copied.Steps[index].ImageDigests[service] = digest
			}
		}
		copied.Steps[index].composeMetadata = copyComposeMetadata(step.composeMetadata)
	}
	if plan.Isolation != nil {
		isolation := *plan.Isolation
		isolation.PathMappings = append([]IsolationPathMapping(nil), plan.Isolation.PathMappings...)
		isolation.VolumeMappings = append([]IsolationResourceMapping(nil), plan.Isolation.VolumeMappings...)
		isolation.Ports = append([]IsolationPort(nil), plan.Isolation.Ports...)
		copied.Isolation = &isolation
	}
	return copied
}

func isolationID(plan Plan, purpose string, instanceKey string) string {
	value := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		isolationSchemaVersion,
		purpose,
		instanceKey,
		plan.ManifestSnapshotID,
		plan.SourceHost,
		plan.DestinationHost,
		plan.Project.Name,
		plan.Project.ProjectName,
		plan.Project.ComposeFile,
	)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func isolationProjectName(original string, purpose string, shortID string) string {
	prefix := strings.ToLower(strings.TrimSpace(original))
	prefix = isolationProjectCharacter.ReplaceAllString(prefix, "-")
	prefix = strings.Trim(prefix, "-_")
	if prefix == "" || !isIsolationProjectStart(prefix[0]) {
		prefix = "ark"
	}
	suffix := "-" + purpose + "-" + shortID
	if maximum := isolationProjectMaximumLength - len(suffix); len(prefix) > maximum {
		prefix = strings.Trim(prefix[:maximum], "-_")
		if prefix == "" {
			prefix = "ark"
		}
	}
	return prefix + suffix
}

func isolationResourceName(original string, purpose string, shortID string) string {
	value := strings.TrimSpace(original)
	if value == "" {
		value = "resource"
	}
	suffix := "-" + purpose + "-" + shortID
	if maximum := isolationResourceMaximumLength - len(suffix); len(value) > maximum {
		value = value[:maximum]
	}
	return value + suffix
}

func isolationPath(filesRoot string, originalPath string) string {
	return path.Join(filesRoot, strings.TrimPrefix(path.Clean(originalPath), "/"))
}

func planPathCovered(plan Plan, targetPath string) bool {
	cleaned := path.Clean(targetPath)
	for _, step := range plan.Steps {
		if step.Phase != PhaseFiles || step.Target == nil {
			continue
		}
		for _, sourcePath := range step.Target.Paths {
			if pathCoveredBy(cleaned, sourcePath) {
				return true
			}
		}
	}
	return false
}

func isIsolationProjectStart(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

func pathCoveredBy(targetPath string, sourcePath string) bool {
	targetPath = path.Clean(targetPath)
	sourcePath = path.Clean(sourcePath)
	return targetPath == sourcePath || strings.HasPrefix(targetPath, sourcePath+"/")
}

func isolationStatePath(plan Plan) string {
	return path.Join(plan.Isolation.Root, "state.json")
}

func newIsolationState(plan Plan, executionID string) isolationState {
	return isolationState{
		SchemaVersion: isolationSchemaVersion,
		ID:            plan.Isolation.ID,
		Purpose:       plan.Isolation.Purpose,
		ExecutionID:   executionID,
		Destination:   plan.DestinationHost,
		ProjectName:   plan.Isolation.ProjectName,
		Root:          plan.Isolation.Root,
		ComposeFile:   plan.Isolation.GeneratedComposeFile,
	}
}

func loadIsolationState(ctx context.Context, plan Plan, runner sshexec.Runner) (isolationState, bool, error) {
	var state isolationState
	statePath := isolationStatePath(plan)
	found, err := targetPathExists(ctx, runner, statePath)
	if err != nil || !found {
		return state, found, err
	}
	if err := validateExactIsolationPath(ctx, runner, statePath); err != nil {
		return state, true, err
	}
	content, err := runner.Run(ctx, "cat", "--", statePath)
	if err != nil {
		return state, true, fmt.Errorf("读取隔离恢复状态失败: %w", err)
	}
	if err := json.Unmarshal([]byte(content), &state); err != nil {
		return state, true, fmt.Errorf("解析隔离恢复状态失败: %w", err)
	}
	return state, true, nil
}

func validateIsolationState(plan Plan, executionID string, state isolationState) error {
	if state.SchemaVersion != isolationSchemaVersion || state.ID != plan.Isolation.ID ||
		state.Purpose != plan.Isolation.Purpose ||
		state.ExecutionID != executionID || state.Destination != plan.DestinationHost ||
		state.ProjectName != plan.Isolation.ProjectName || state.Root != plan.Isolation.Root ||
		state.ComposeFile != plan.Isolation.GeneratedComposeFile {
		return fmt.Errorf("隔离恢复状态与当前 Plan 不一致，拒绝接管")
	}
	return nil
}

func writeIsolationState(ctx context.Context, plan Plan, runner sshexec.Runner, state isolationState) error {
	content, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("编码隔离恢复状态失败: %w", err)
	}
	if err := writeRootOnlyFile(ctx, runner, isolationStatePath(plan), string(content)+"\n"); err != nil {
		return fmt.Errorf("记录隔离恢复状态失败: %w", err)
	}
	return nil
}

func validateExistingIsolationRoot(ctx context.Context, plan Plan, runner sshexec.Runner) (bool, error) {
	exists, err := targetPathExists(ctx, runner, plan.Isolation.Root)
	if err != nil || !exists {
		return exists, err
	}
	return true, validateExactIsolationPath(ctx, runner, plan.Isolation.Root)
}

func ensureIsolationRoot(ctx context.Context, plan Plan, runner sshexec.Runner) error {
	for _, directory := range []string{isolationBase, plan.Isolation.Root} {
		exists, err := targetPathExists(ctx, runner, directory)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := runner.Run(ctx, "install", "-d", "-m", "0700", directory); err != nil {
				return fmt.Errorf("创建隔离恢复目录失败: %w", err)
			}
		}
		if err := validateExactIsolationPath(ctx, runner, directory); err != nil {
			return err
		}
	}
	return nil
}

func validateExactIsolationPath(ctx context.Context, runner sshexec.Runner, targetPath string) error {
	if _, err := runner.Run(ctx, "test", "!", "-L", targetPath); err != nil {
		return fmt.Errorf("隔离恢复路径 %q 是符号链接或无法验证", targetPath)
	}
	resolved, err := runner.Run(ctx, "readlink", "-f", "--", targetPath)
	if err != nil || strings.TrimSpace(resolved) != targetPath {
		return fmt.Errorf("隔离恢复路径 %q 真实路径不匹配", targetPath)
	}
	return nil
}

func prepareIsolation(
	ctx context.Context,
	plan Plan,
	runner sshexec.Runner,
	executionID string,
) (isolationState, error) {
	state, found, err := loadIsolationState(ctx, plan, runner)
	if err != nil {
		return state, err
	}
	if found {
		if err := validateIsolationState(plan, executionID, state); err != nil {
			return state, err
		}
	} else {
		state = newIsolationState(plan, executionID)
	}
	if err := validateIsolationFilesTree(ctx, runner, plan.Isolation); err != nil {
		return state, err
	}
	if err := validateIsolationComposeSources(ctx, runner, plan.Isolation); err != nil {
		return state, err
	}

	sourceProject := plan.Isolation.SourceProject
	sourceProject.ComposeFile = plan.Isolation.SourceComposeFile
	sourceProject.EnvFile = plan.Isolation.SourceEnvFile
	argv := composeBaseArgv(sourceProject)
	argv = append(argv, "config", "--format", "json", "--no-env-resolution")
	canonical, err := readIsolationComposeCanonical(ctx, runner, argv)
	if err != nil {
		return state, err
	}
	generated, services, volumes, networks, ports, hostIPs, err := transformIsolationCompose(
		canonical, plan.Isolation,
	)
	if err != nil {
		return state, err
	}
	if !equalIsolationPortDeclarations(ports, plan.Isolation.Ports) {
		return state, fmt.Errorf("备份时记录的 Compose 端口与恢复材料不一致，拒绝启动隔离副本")
	}
	if err := validateIsolationHostIPs(ctx, runner, hostIPs); err != nil {
		return state, err
	}
	mappedPaths, err := isolationComposeMappedPaths(generated)
	if err != nil {
		return state, err
	}
	if err := validateIsolationMappedPaths(ctx, runner, plan.Isolation, mappedPaths); err != nil {
		return state, err
	}
	if err := writeRootOnlyFile(ctx, runner, plan.Isolation.GeneratedComposeFile, string(generated)+"\n"); err != nil {
		return state, fmt.Errorf("写入隔离 Compose 配置失败: %w", err)
	}
	if _, err := runner.Run(ctx, append(composeBaseArgv(plan.Project), "config", "--format", "json")...); err != nil {
		return state, fmt.Errorf("校验生成的隔离 Compose 配置失败")
	}
	state.Services = services
	state.Volumes = volumes
	state.Networks = networks
	state.Ports = ports
	if err := writeIsolationState(ctx, plan, runner, state); err != nil {
		return state, err
	}
	return state, nil
}

func readIsolationComposeCanonical(
	ctx context.Context,
	runner sshexec.Runner,
	argv []string,
) ([]byte, error) {
	payload, err := sshexec.ReadAllStdout(ctx, runner, argv...)
	if err != nil {
		return nil, isolationComposeCommandError{cause: err}
	}
	return payload, nil
}

type isolationComposeCommandError struct {
	cause error
}

func (e isolationComposeCommandError) Error() string {
	return "生成隔离 Compose 配置失败"
}

func (e isolationComposeCommandError) Unwrap() error {
	return e.cause
}

func equalIsolationPortDeclarations(left []IsolationPort, right []IsolationPort) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateIsolationFilesTree(ctx context.Context, runner sshexec.Runner, spec *IsolationSpec) error {
	if err := validateExactIsolationPath(ctx, runner, spec.FilesRoot); err != nil {
		return err
	}
	if err := validateIsolationFilesTreeLinks(ctx, runner, spec.FilesRoot); err != nil {
		return err
	}
	return validateIsolationMappedPaths(ctx, runner, spec, []string{
		spec.SourceComposeFile,
		spec.SourceEnvFile,
	})
}

func validateIsolationFilesTreeLinks(ctx context.Context, runner sshexec.Runner, filesRoot string) error {
	out, err := runner.Run(ctx, "find", filesRoot, "-type", "l", "-print")
	if err != nil {
		return fmt.Errorf("检查 isolation files 符号链接失败")
	}
	for _, linkPath := range nonEmptyLines(out) {
		resolved, err := runner.Run(ctx, "readlink", "-f", "--", linkPath)
		if err != nil {
			return fmt.Errorf("解析 isolation files 符号链接失败")
		}
		resolved = strings.TrimSpace(resolved)
		if resolved == filesRoot || !strings.HasPrefix(resolved, filesRoot+"/") {
			return fmt.Errorf("isolation files 包含越过专用根目录的符号链接")
		}
	}
	return nil
}

func validateIsolationComposeSources(ctx context.Context, runner sshexec.Runner, spec *IsolationSpec) error {
	visited := make(map[string]bool)
	var walk func(composeSourceReference) error
	walk = func(source composeSourceReference) error {
		composeFile := source.Path
		composeFile = path.Clean(composeFile)
		visitKey := composeFile + "\x00" + source.BaseDirectory
		if visited[visitKey] {
			return nil
		}
		visited[visitKey] = true
		if err := validateIsolationMappedPaths(ctx, runner, spec, []string{composeFile}); err != nil {
			return err
		}
		content, err := runner.Run(ctx, "cat", "--", composeFile)
		if err != nil {
			return fmt.Errorf("读取隔离 Compose 源文件失败")
		}
		var document map[string]any
		if err := yaml.Unmarshal([]byte(content), &document); err != nil {
			return fmt.Errorf("解析隔离 Compose 源文件失败: %w", err)
		}
		baseDirectory := source.BaseDirectory
		if baseDirectory == "" {
			baseDirectory = path.Dir(composeFile)
		}
		references, nestedComposeFiles, changed, err := rewriteComposePreReadPaths(document, spec, baseDirectory)
		if err != nil {
			return err
		}
		if err := validateIsolationMappedPaths(ctx, runner, spec, references); err != nil {
			return err
		}
		if changed {
			rewritten, err := yaml.Marshal(document)
			if err != nil {
				return fmt.Errorf("编码隔离 Compose 源文件失败: %w", err)
			}
			if err := writeRootOnlyFile(ctx, runner, composeFile, string(rewritten)); err != nil {
				return fmt.Errorf("写入隔离 Compose 源文件失败: %w", err)
			}
		}
		for _, nestedFile := range nestedComposeFiles {
			if err := walk(nestedFile); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(composeSourceReference{Path: spec.SourceComposeFile, BaseDirectory: path.Dir(spec.SourceComposeFile)})
}

type composeSourceReference struct {
	Path          string
	BaseDirectory string
}

func rewriteComposePreReadPaths(
	document map[string]any,
	spec *IsolationSpec,
	baseDirectory string,
) ([]string, []composeSourceReference, bool, error) {
	var references []string
	var nestedComposeFiles []composeSourceReference
	changed := false
	if rawInclude, exists := document["include"]; exists {
		rewritten, includeReferences, includeComposeFiles, err := rewriteComposeInclude(rawInclude, spec, baseDirectory)
		if err != nil {
			return nil, nil, false, err
		}
		document["include"] = rewritten
		references = append(references, includeReferences...)
		nestedComposeFiles = append(nestedComposeFiles, includeComposeFiles...)
		changed = true
	}
	services, _ := document["services"].(map[string]any)
	for serviceName, rawService := range services {
		service, _ := rawService.(map[string]any)
		if service == nil {
			continue
		}
		if rawLabelFiles, exists := service["label_file"]; exists {
			rewritten, paths, err := rewriteComposePathValue(rawLabelFiles, spec, baseDirectory)
			if err != nil {
				return nil, nil, false, fmt.Errorf("Compose service %q label_file 无法隔离: %w", serviceName, err)
			}
			service["label_file"] = rewritten
			references = append(references, paths...)
			changed = true
		}
		extends, _ := service["extends"].(map[string]any)
		if extendsFile := strings.TrimSpace(stringValue(extends["file"])); extendsFile != "" {
			resolved, err := resolveComposePreReadPath(spec, baseDirectory, extendsFile)
			if err != nil {
				return nil, nil, false, err
			}
			extends["file"] = resolved
			references = append(references, resolved)
			nestedComposeFiles = append(nestedComposeFiles, composeSourceReference{
				Path: resolved, BaseDirectory: path.Dir(resolved),
			})
			changed = true
		}
	}
	sort.Slice(nestedComposeFiles, func(left int, right int) bool {
		if nestedComposeFiles[left].Path == nestedComposeFiles[right].Path {
			return nestedComposeFiles[left].BaseDirectory < nestedComposeFiles[right].BaseDirectory
		}
		return nestedComposeFiles[left].Path < nestedComposeFiles[right].Path
	})
	return uniqueSorted(references), uniqueComposeSourceReferences(nestedComposeFiles), changed, nil
}

func rewriteComposeInclude(
	value any,
	spec *IsolationSpec,
	baseDirectory string,
) (any, []string, []composeSourceReference, error) {
	if value == nil {
		return nil, nil, nil, nil
	}
	items, isList := value.([]any)
	if !isList {
		items = []any{value}
	}
	var references []string
	var nestedComposeFiles []composeSourceReference
	for index, item := range items {
		switch typed := item.(type) {
		case string:
			resolved, err := resolveComposePreReadPath(spec, baseDirectory, typed)
			if err != nil {
				return nil, nil, nil, err
			}
			items[index] = resolved
			references = append(references, resolved)
			nestedComposeFiles = append(nestedComposeFiles, composeSourceReference{
				Path: resolved, BaseDirectory: path.Dir(resolved),
			})
		case map[string]any:
			paths, exists := typed["path"]
			if !exists {
				return nil, nil, nil, fmt.Errorf("Compose include 缺少 path")
			}
			rewritten, resolvedPaths, err := rewriteComposePathValue(paths, spec, baseDirectory)
			if err != nil {
				return nil, nil, nil, err
			}
			typed["path"] = rewritten
			references = append(references, resolvedPaths...)
			projectDirectory := ""
			for _, field := range []string{"env_file", "project_directory"} {
				if rawPath, exists := typed[field]; exists {
					rewritten, resolved, err := rewriteComposePathValue(rawPath, spec, baseDirectory)
					if err != nil {
						return nil, nil, nil, err
					}
					typed[field] = rewritten
					references = append(references, resolved...)
					if field == "project_directory" && len(resolved) == 1 {
						projectDirectory = resolved[0]
					}
				}
			}
			for _, resolvedPath := range resolvedPaths {
				base := projectDirectory
				if base == "" {
					base = path.Dir(resolvedPath)
				}
				nestedComposeFiles = append(nestedComposeFiles, composeSourceReference{
					Path: resolvedPath, BaseDirectory: base,
				})
			}
		default:
			return nil, nil, nil, fmt.Errorf("Compose include 结构无法安全解析")
		}
	}
	if isList {
		return items, references, nestedComposeFiles, nil
	}
	return items[0], references, nestedComposeFiles, nil
}

func uniqueComposeSourceReferences(values []composeSourceReference) []composeSourceReference {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		previous := result[len(result)-1]
		if value.Path != previous.Path || value.BaseDirectory != previous.BaseDirectory {
			result = append(result, value)
		}
	}
	return result
}

func rewriteComposePathValue(value any, spec *IsolationSpec, baseDirectory string) (any, []string, error) {
	switch typed := value.(type) {
	case string:
		resolved, err := resolveComposePreReadPath(spec, baseDirectory, typed)
		return resolved, []string{resolved}, err
	case []any:
		resolvedPaths := make([]string, 0, len(typed))
		for index, item := range typed {
			text := strings.TrimSpace(stringValue(item))
			resolved, err := resolveComposePreReadPath(spec, baseDirectory, text)
			if err != nil {
				return nil, nil, err
			}
			typed[index] = resolved
			resolvedPaths = append(resolvedPaths, resolved)
		}
		return typed, resolvedPaths, nil
	default:
		return nil, nil, fmt.Errorf("Compose 路径字段结构无法安全解析")
	}
}

func resolveComposePreReadPath(spec *IsolationSpec, baseDirectory string, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "$") {
		return "", fmt.Errorf("Compose canonical 前置读取路径 %q 无法证明隔离", value)
	}
	resolved := value
	if !path.IsAbs(resolved) {
		resolved = path.Join(baseDirectory, resolved)
	}
	mapped, ok := mappedIsolationPath(spec, path.Clean(resolved))
	if !ok {
		return "", fmt.Errorf("Compose canonical 前置读取路径 %q 未包含在 files target 中", value)
	}
	return mapped, nil
}

func isolationComposeMappedPaths(generated []byte) ([]string, error) {
	var document map[string]any
	if err := json.Unmarshal(generated, &document); err != nil {
		return nil, fmt.Errorf("解析生成的隔离 Compose 配置失败: %w", err)
	}
	var result []string
	services, err := objectField(document, "services", true)
	if err != nil {
		return nil, err
	}
	for _, serviceName := range sortedKeys(services) {
		service, ok := services[serviceName].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Compose service %q 结构无效", serviceName)
		}
		if rawVolumes, ok := service["volumes"].([]any); ok {
			for _, rawVolume := range rawVolumes {
				volume, ok := rawVolume.(map[string]any)
				if ok && stringValue(volume["type"]) == "bind" {
					result = append(result, strings.TrimSpace(stringValue(volume["source"])))
				}
			}
		}
		if rawEnvFiles, ok := service["env_file"].([]any); ok {
			for _, rawEnvFile := range rawEnvFiles {
				envFile, ok := rawEnvFile.(map[string]any)
				if ok {
					result = append(result, strings.TrimSpace(stringValue(envFile["path"])))
				}
			}
		}
	}
	for _, field := range []string{"configs", "secrets"} {
		resources, err := objectField(document, field, false)
		if err != nil {
			return nil, err
		}
		for _, key := range sortedKeys(resources) {
			resource, ok := resources[key].(map[string]any)
			if ok {
				result = append(result, strings.TrimSpace(stringValue(resource["file"])))
			}
		}
	}
	return uniqueSorted(result), nil
}

func validateIsolationMappedPaths(
	ctx context.Context,
	runner sshexec.Runner,
	spec *IsolationSpec,
	paths []string,
) error {
	for _, targetPath := range paths {
		if targetPath == "" {
			continue
		}
		cleaned := path.Clean(targetPath)
		if cleaned == spec.FilesRoot || !strings.HasPrefix(cleaned, spec.FilesRoot+"/") {
			return fmt.Errorf("Compose 引用路径越过 isolation files 根目录")
		}
		exists, err := targetPathExists(ctx, runner, cleaned)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("Compose 引用的隔离恢复路径不存在")
		}
		resolved, err := runner.Run(ctx, "readlink", "-f", "--", cleaned)
		if err != nil {
			return fmt.Errorf("解析 Compose 隔离路径失败")
		}
		resolved = strings.TrimSpace(resolved)
		if resolved == spec.FilesRoot || !strings.HasPrefix(resolved, spec.FilesRoot+"/") {
			return fmt.Errorf("Compose 引用路径的真实位置越过 isolation files 根目录")
		}
	}
	return nil
}

func transformIsolationCompose(
	canonical []byte,
	spec *IsolationSpec,
) ([]byte, []string, []string, []string, []IsolationPort, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("解析 Compose 配置失败: %w", err)
	}
	document["name"] = spec.ProjectName
	services, err := objectField(document, "services", true)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	serviceNames := sortedKeys(services)
	var ports []IsolationPort
	portKeys := make(map[string]struct{})
	var hostIPs []string
	for _, serviceName := range serviceNames {
		service, ok := services[serviceName].(map[string]any)
		if !ok {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q 结构无效", serviceName)
		}
		if err := validateIsolationService(serviceName, service); err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
		delete(service, "container_name")
		addIsolationLabel(service, spec.ID)
		if err := transformServicePathList(serviceName, service, "env_file", spec); err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
		if rawVolumes, ok := service["volumes"].([]any); ok {
			for index, rawVolume := range rawVolumes {
				volume, ok := rawVolume.(map[string]any)
				if !ok {
					return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q volume[%d] 结构无效", serviceName, index)
				}
				mountType, _ := volume["type"].(string)
				switch mountType {
				case "bind":
					source, _ := volume["source"].(string)
					mapped, ok := mappedIsolationPath(spec, source)
					if !ok {
						return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q bind path %q 未包含在 files target 中", serviceName, source)
					}
					volume["source"] = mapped
				case "volume":
					if strings.TrimSpace(stringValue(volume["source"])) == "" {
						return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q 使用匿名 volume，无法证明隔离清理范围", serviceName)
					}
				case "tmpfs":
				default:
					return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q mount type %q 不支持隔离恢复", serviceName, mountType)
				}
			}
		}
		if rawPorts, ok := service["ports"].([]any); ok {
			for index, rawPort := range rawPorts {
				portObject, ok := rawPort.(map[string]any)
				if !ok {
					return nil, nil, nil, nil, nil, nil, fmt.Errorf("Compose service %q port[%d] 结构无效", serviceName, index)
				}
				portMapping, err := isolationPortFromCompose(serviceName, portObject)
				if err != nil {
					return nil, nil, nil, nil, nil, nil, err
				}
				if spec.PortAllocation == IsolationPortDisabled {
					portMapping.AllocatedPort = IsolationPortDisabled
				}
				ports = append(ports, portMapping)
				portKey := fmt.Sprintf("%s\x00%s\x00%d\x00%s", serviceName, portMapping.HostIP, portMapping.Target, portMapping.Protocol)
				if _, exists := portKeys[portKey]; exists {
					return nil, nil, nil, nil, nil, nil, fmt.Errorf(
						"Compose service %q 的 port %d/%s 在同一 host IP 上重复，无法稳定映射",
						serviceName, portMapping.Target, portMapping.Protocol,
					)
				}
				portKeys[portKey] = struct{}{}
				if spec.PortAllocation == IsolationPortRuntimeAuto && concreteIsolationHostIP(portMapping.HostIP) {
					hostIPs = append(hostIPs, portMapping.HostIP)
				}
				if spec.PortAllocation == IsolationPortRuntimeAuto {
					delete(portObject, "published")
				}
			}
			if spec.PortAllocation == IsolationPortDisabled {
				// 自动演练只需要容器内部 health/数据库检查。彻底删除端口声明，避免意外扩大网络暴露面。
				delete(service, "ports")
			}
		}
	}

	volumeNames, err := transformNamedResources(document, "volumes", spec, true)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	networkNames, err := transformNamedResources(document, "networks", spec, true)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	for _, field := range []string{"configs", "secrets"} {
		if err := transformFileResources(document, field, spec); err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
	}
	sortIsolationPorts(ports)
	generated, err := json.Marshal(document)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("编码隔离 Compose 配置失败: %w", err)
	}
	return generated, serviceNames, volumeNames, networkNames, ports, uniqueSorted(hostIPs), nil
}

func transformServicePathList(serviceName string, service map[string]any, field string, spec *IsolationSpec) error {
	rawValues, exists := service[field]
	if !exists {
		return nil
	}
	values, ok := rawValues.([]any)
	if !ok {
		return fmt.Errorf("Compose service %q 的 %s 结构无效", serviceName, field)
	}
	for index, rawValue := range values {
		value, ok := rawValue.(map[string]any)
		if !ok {
			return fmt.Errorf("Compose service %q 的 %s[%d] 结构无效", serviceName, field, index)
		}
		source := strings.TrimSpace(stringValue(value["path"]))
		mapped, ok := mappedIsolationPath(spec, source)
		if !ok {
			return fmt.Errorf("Compose service %q 的 %s path %q 未包含在 files target 中", serviceName, field, source)
		}
		value["path"] = mapped
	}
	return nil
}

func validateIsolationService(serviceName string, service map[string]any) error {
	for _, field := range []string{"network_mode", "ipc", "pid", "uts", "userns_mode", "cgroup"} {
		value := strings.TrimSpace(stringValue(service[field]))
		if value == "host" || strings.HasPrefix(value, "container:") || strings.HasPrefix(value, "service:") {
			return fmt.Errorf("Compose service %q 的 %s=%q 无法隔离", serviceName, field, value)
		}
	}
	if privileged, _ := service["privileged"].(bool); privileged {
		return fmt.Errorf("Compose service %q 的 privileged 无法证明隔离", serviceName)
	}
	for _, field := range []string{
		"devices", "device_cgroup_rules", "external_links", "gpus", "volumes_from", "cgroup_parent", "provider",
	} {
		if composeFieldPresent(service[field]) {
			return fmt.Errorf("Compose service %q 的 %s 无法证明隔离", serviceName, field)
		}
	}
	if useAPISocket, _ := service["use_api_socket"].(bool); useAPISocket {
		return fmt.Errorf("Compose service %q 的 use_api_socket 无法证明隔离", serviceName)
	}
	return nil
}

func transformNamedResources(document map[string]any, field string, spec *IsolationSpec, labels bool) ([]string, error) {
	resources, err := objectField(document, field, false)
	if err != nil || resources == nil {
		return nil, err
	}
	keys := sortedKeys(resources)
	physicalNames := make([]string, 0, len(keys))
	for _, key := range keys {
		resource, ok := resources[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Compose %s %q 结构无效", field, key)
		}
		if externalResource(resource) {
			return nil, fmt.Errorf("Compose %s %q 是 external 资源，无法隔离", field, key)
		}
		if field == "volumes" &&
			(strings.TrimSpace(stringValue(resource["driver"])) != "" || composeFieldPresent(resource["driver_opts"])) {
			return nil, fmt.Errorf("Compose volume %q 的 driver/driver_opts 无法证明隔离", key)
		}
		if field == "networks" {
			driver := strings.TrimSpace(stringValue(resource["driver"]))
			if driver != "" && driver != "bridge" {
				return nil, fmt.Errorf("Compose network %q 使用 driver %q，无法证明隔离", key, driver)
			}
			if composeFieldPresent(resource["driver_opts"]) {
				return nil, fmt.Errorf("Compose network %q 的 driver_opts 无法证明隔离", key)
			}
		}
		original := strings.TrimSpace(stringValue(resource["name"]))
		if original == "" {
			original = effectiveProjectResourceName(spec.SourceProject, key)
		}
		mapped := isolationResourceName(original, spec.Purpose, spec.ShortID)
		resource["name"] = mapped
		if labels {
			addIsolationLabel(resource, spec.ID)
		}
		physicalNames = append(physicalNames, mapped)
	}
	return physicalNames, nil
}

func transformFileResources(document map[string]any, field string, spec *IsolationSpec) error {
	resources, err := objectField(document, field, false)
	if err != nil || resources == nil {
		return err
	}
	for _, key := range sortedKeys(resources) {
		resource, ok := resources[key].(map[string]any)
		if !ok {
			return fmt.Errorf("Compose %s %q 结构无效", field, key)
		}
		if externalResource(resource) {
			return fmt.Errorf("Compose %s %q 是 external 资源，无法隔离", field, key)
		}
		filePath := strings.TrimSpace(stringValue(resource["file"]))
		if filePath != "" {
			mapped, ok := mappedIsolationPath(spec, filePath)
			if !ok {
				return fmt.Errorf("Compose %s %q path %q 未包含在 files target 中", field, key, filePath)
			}
			resource["file"] = mapped
		}
		if original := strings.TrimSpace(stringValue(resource["name"])); original != "" {
			resource["name"] = isolationResourceName(original, spec.Purpose, spec.ShortID)
		}
	}
	return nil
}

func isolationPortFromCompose(service string, value map[string]any) (IsolationPort, error) {
	targetNumber, err := uint16Value(value["target"])
	if err != nil || targetNumber == 0 {
		return IsolationPort{}, fmt.Errorf("Compose service %q port target 无效", service)
	}
	protocol := strings.TrimSpace(stringValue(value["protocol"]))
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return IsolationPort{}, fmt.Errorf("Compose service %q port protocol %q 不支持", service, protocol)
	}
	hostIP := strings.TrimSpace(stringValue(value["host_ip"]))
	if hostIP != "" && hostIP != "0.0.0.0" && hostIP != "::" && net.ParseIP(hostIP) == nil {
		return IsolationPort{}, fmt.Errorf("Compose service %q port host IP %q 无效", service, hostIP)
	}
	return IsolationPort{
		Service:           service,
		HostIP:            hostIP,
		OriginalPublished: strings.TrimSpace(stringValue(value["published"])),
		AllocatedPort:     "auto",
		Target:            targetNumber,
		Protocol:          protocol,
		AppProtocol:       strings.TrimSpace(stringValue(value["app_protocol"])),
		Mode:              strings.TrimSpace(stringValue(value["mode"])),
	}, nil
}

func mappedIsolationPath(spec *IsolationSpec, original string) (string, bool) {
	original = path.Clean(original)
	if strings.HasPrefix(original, spec.FilesRoot+"/") {
		restoredSource := "/" + strings.TrimPrefix(original, spec.FilesRoot+"/")
		for _, mapping := range spec.PathMappings {
			if pathCoveredBy(restoredSource, mapping.Source) {
				return original, true
			}
		}
		return "", false
	}
	for _, mapping := range spec.PathMappings {
		if pathCoveredBy(original, mapping.Source) {
			return isolationPath(spec.FilesRoot, original), true
		}
	}
	return "", false
}

func validateIsolationHostIPs(ctx context.Context, runner sshexec.Runner, required []string) error {
	if len(required) == 0 {
		return nil
	}
	out, err := runner.Run(ctx, "ip", "-j", "address")
	if err != nil {
		return fmt.Errorf("检查隔离端口 host IP 失败")
	}
	var links []struct {
		Addresses []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return fmt.Errorf("解析目标机 IP 地址失败: %w", err)
	}
	available := make(map[string]struct{})
	for _, link := range links {
		for _, address := range link.Addresses {
			available[address.Local] = struct{}{}
		}
	}
	for _, requiredIP := range required {
		if _, exists := available[requiredIP]; !exists {
			return fmt.Errorf("Compose 指定的 host IP %q 在恢复目标机不存在", requiredIP)
		}
	}
	return nil
}

func concreteIsolationHostIP(value string) bool {
	if value == "" || value == "0.0.0.0" || value == "::" {
		return false
	}
	return net.ParseIP(value) != nil
}

func inspectIsolationPorts(ctx context.Context, plan Plan, runner sshexec.Runner, state isolationState) (isolationState, error) {
	if plan.Isolation != nil && plan.Isolation.PortAllocation == IsolationPortDisabled {
		return inspectIsolationContainers(ctx, plan, runner, state)
	}
	states, err := composeStates(ctx, runner, plan, true)
	if err != nil {
		return state, err
	}
	portsByService := make(map[string][]IsolationPort)
	for _, item := range state.Ports {
		portsByService[item.Service] = append(portsByService[item.Service], item)
	}
	state.Containers = state.Containers[:0]
	expectedServices := cleanupStringSet(state.Services)
	seenServices := make(map[string]bool, len(state.Services))
	for _, container := range states {
		if container.ID == "" {
			return state, fmt.Errorf("隔离 service %q 容器缺少 ID，无法读取端口", container.Service)
		}
		if !expectedServices[container.Service] {
			return state, fmt.Errorf("隔离 Compose project 包含未记录 service %q", container.Service)
		}
		if seenServices[container.Service] {
			return state, fmt.Errorf("隔离 service %q 存在多个容器，端口映射不唯一", container.Service)
		}
		seenServices[container.Service] = true
		labels, err := inspectResourceLabels(ctx, runner, "container", container.ID)
		if err != nil || labels[composeProjectLabel] != plan.Isolation.ProjectName ||
			labels[isolationLabel] != plan.Isolation.ID || labels[composeServiceLabel] != container.Service {
			return state, fmt.Errorf("隔离 service %q 容器归属校验失败", container.Service)
		}
		state.Containers = append(state.Containers, IsolationContainer{ID: container.ID, Service: container.Service})
		out, err := runner.Run(ctx, "docker", "container", "inspect", "--format", "{{json .NetworkSettings.Ports}}", container.ID)
		if err != nil {
			return state, fmt.Errorf("读取隔离 service %q 端口失败: %w", container.Service, err)
		}
		var bindings map[string][]isolationPortBinding
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &bindings); err != nil {
			return state, fmt.Errorf("解析隔离 service %q 端口失败: %w", container.Service, err)
		}
		servicePorts := portsByService[container.Service]
		for index := range servicePorts {
			key := strconv.Itoa(int(servicePorts[index].Target)) + "/" + servicePorts[index].Protocol
			binding, err := selectIsolationPortBinding(servicePorts[index].HostIP, bindings[key])
			if err != nil {
				return state, fmt.Errorf("隔离 service %q 的 port %s: %w", container.Service, key, err)
			}
			servicePorts[index].AllocatedPort = binding.HostPort
		}
		portsByService[container.Service] = servicePorts
	}
	for _, service := range state.Services {
		if !seenServices[service] {
			return state, fmt.Errorf("隔离 service %q 缺少容器，无法读取端口", service)
		}
	}
	state.Ports = state.Ports[:0]
	for _, service := range state.Services {
		state.Ports = append(state.Ports, portsByService[service]...)
	}
	if err := writeIsolationState(ctx, plan, runner, state); err != nil {
		return state, err
	}
	return state, nil
}

func inspectIsolationContainers(
	ctx context.Context,
	plan Plan,
	runner sshexec.Runner,
	state isolationState,
) (isolationState, error) {
	states, err := composeStates(ctx, runner, plan, true)
	if err != nil {
		return state, err
	}
	expectedServices := cleanupStringSet(state.Services)
	seenServices := make(map[string]bool, len(state.Services))
	containers := make([]IsolationContainer, 0, len(states))
	for _, container := range states {
		if container.ID == "" || !expectedServices[container.Service] {
			return state, fmt.Errorf("隔离 Compose project 包含未记录容器")
		}
		if seenServices[container.Service] {
			return state, fmt.Errorf("隔离 service %q 存在多个容器", container.Service)
		}
		seenServices[container.Service] = true
		labels, err := inspectResourceLabels(ctx, runner, "container", container.ID)
		if err != nil || labels[composeProjectLabel] != plan.Isolation.ProjectName ||
			labels[isolationLabel] != plan.Isolation.ID || labels[composeServiceLabel] != container.Service {
			return state, fmt.Errorf("隔离 service %q 容器归属校验失败", container.Service)
		}
		containers = append(containers, IsolationContainer{ID: container.ID, Service: container.Service})
	}
	state.Containers = containers
	if err := writeIsolationState(ctx, plan, runner, state); err != nil {
		return state, err
	}
	return state, nil
}

type isolationPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func selectIsolationPortBinding(requiredHostIP string, bindings []isolationPortBinding) (isolationPortBinding, error) {
	var matches []isolationPortBinding
	for _, binding := range bindings {
		if strings.TrimSpace(binding.HostPort) == "" {
			continue
		}
		if requiredHostIP != "" && binding.HostIP != requiredHostIP {
			continue
		}
		matches = append(matches, binding)
	}
	if len(matches) == 0 {
		return isolationPortBinding{}, fmt.Errorf("缺少实际宿主机端口")
	}
	for _, match := range matches[1:] {
		if match.HostPort != matches[0].HostPort {
			return isolationPortBinding{}, fmt.Errorf("同一声明得到多个不同宿主机端口")
		}
	}
	return matches[0], nil
}

func isolationResult(plan Plan, state isolationState) *IsolationResult {
	if plan.Isolation == nil {
		return nil
	}
	return &IsolationResult{
		ID:                   plan.Isolation.ID,
		ProjectName:          plan.Isolation.ProjectName,
		Root:                 plan.Isolation.Root,
		GeneratedComposeFile: plan.Isolation.GeneratedComposeFile,
		Containers:           append([]IsolationContainer{}, state.Containers...),
		Volumes:              append([]string{}, state.Volumes...),
		Networks:             append([]string{}, state.Networks...),
		Ports:                append([]IsolationPort{}, state.Ports...),
		CleanupCommand:       fmt.Sprintf("ark restore cleanup --host %s --isolation %s", plan.DestinationHost, plan.Isolation.ID),
	}
}

func objectField(document map[string]any, field string, required bool) (map[string]any, error) {
	raw, exists := document[field]
	if !exists {
		if required {
			return nil, fmt.Errorf("Compose 配置缺少 %s", field)
		}
		return nil, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Compose %s 结构无效", field)
	}
	return object, nil
}

func addIsolationLabel(resource map[string]any, isolationID string) {
	labels, ok := resource["labels"].(map[string]any)
	if !ok {
		labels = make(map[string]any)
		resource["labels"] = labels
	}
	labels[isolationLabel] = isolationID
}

func externalResource(resource map[string]any) bool {
	switch value := resource["external"].(type) {
	case bool:
		return value
	case map[string]any:
		return true
	default:
		return false
	}
}

func composeFieldPresent(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	case bool:
		return typed
	default:
		return true
	}
}

func effectiveProjectResourceName(project Project, key string) string {
	return effectiveProjectName(project) + "_" + key
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func uint16Value(value any) (uint16, error) {
	parsed, err := strconv.ParseUint(stringValue(value), 10, 16)
	return uint16(parsed), err
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
