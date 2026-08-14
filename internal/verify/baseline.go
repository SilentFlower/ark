// Package verify 编排 ark 的隔离恢复演练、生产基线复核、资源清理和结果持久化。
//
// target 恢复、Compose 隔离转换和归属清理由 restore 包负责；本包只增加演练生命周期，
// 不复制第二套恢复语义。所有基线只读取资源身份与文件元数据，不读取环境变量或业务内容。
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restore"
	"github.com/silentflower/ark/internal/sshexec"
)

const composeProjectLabel = "com.docker.compose.project"

// Baseline 是演练前后用于证明生产资源未变化的稳定快照。
type Baseline struct {
	// Fingerprint 是下列安全元数据稳定 JSON 的 SHA-256。
	Fingerprint string `json:"fingerprint"`
	// Containers 是生产 Compose project 的容器身份、状态与镜像 ID。
	Containers []ContainerBaseline `json:"containers"`
	// Networks 是生产 Compose project 的 network 身份、driver 与 labels。
	Networks []NetworkBaseline `json:"networks"`
	// Volumes 是生产 Compose project 的 volume 名称、driver 与 labels。
	Volumes []VolumeBaseline `json:"volumes"`
	// Files 是 files target 声明路径的类型与 stat 元数据。
	Files []FileBaseline `json:"files"`
}

// ContainerBaseline 是一个生产容器的安全身份摘要。
type ContainerBaseline struct {
	// ID 是完整容器 ID。
	ID string `json:"id"`
	// Service 是 Compose service 标签。
	Service string `json:"service"`
	// State 是 Docker 容器状态。
	State string `json:"state"`
	// ImageID 是容器使用的不可变 Docker image ID。
	ImageID string `json:"image_id"`
	// ImageDigests 是该 image 当前记录的已排序仓库 digest。
	ImageDigests []string `json:"image_digests"`
}

// NetworkBaseline 是一个生产 network 的安全身份摘要。
type NetworkBaseline struct {
	// ID 是 Docker network ID。
	ID string `json:"id"`
	// Name 是 network 名称。
	Name string `json:"name"`
	// Driver 是 network driver。
	Driver string `json:"driver"`
	// Labels 是完整 Docker network labels，用于发现 project 元数据漂移。
	Labels map[string]string `json:"labels"`
}

// VolumeBaseline 是一个生产 volume 的安全身份摘要。
type VolumeBaseline struct {
	// Name 是 volume 名称。
	Name string `json:"name"`
	// Driver 是 volume driver。
	Driver string `json:"driver"`
	// Labels 是完整 Docker volume labels，用于发现 project 元数据漂移。
	Labels map[string]string `json:"labels"`
}

// FileBaseline 是一个生产路径的安全 stat 摘要。
type FileBaseline struct {
	// Path 是清单声明的生产绝对路径。
	Path string `json:"path"`
	// Type 是 stat 返回的文件类型。
	Type string `json:"type"`
	// Mode 是八进制权限文本。
	Mode string `json:"mode"`
	// UID 是属主用户 ID。
	UID uint64 `json:"uid"`
	// GID 是属组 ID。
	GID uint64 `json:"gid"`
	// Size 是字节数。
	Size int64 `json:"size"`
	// ModifiedUnix 是秒级修改时间。
	ModifiedUnix int64 `json:"modified_unix"`
	// EntryCount 是目录树内的条目数量；普通文件固定为 1。
	EntryCount int `json:"entry_count"`
	// EntryFingerprint 是目录树内路径与 stat 元数据的稳定 SHA-256，不读取文件内容。
	EntryFingerprint string `json:"entry_fingerprint"`
}

// CaptureBaseline 采集 Plan 对应生产 Compose project 与 files target 的只读元数据。
// @param ctx 控制 Docker 和 stat 命令取消。
// @param runner source 原 host 的本地或 SSH Runner。
// @param plan 尚未隔离的生产恢复 Plan。
// @return Baseline 已排序且带稳定指纹的生产资源快照。
// @return error Plan、资源标签、Docker 输出或文件 stat 无效时的错误。
func CaptureBaseline(ctx context.Context, runner sshexec.Runner, plan restore.Plan) (Baseline, error) {
	if ctx == nil || runner == nil {
		return Baseline{}, fmt.Errorf("采集生产基线失败: context 或 runner 为空")
	}
	projectName := effectiveProjectName(plan.Project)
	if strings.TrimSpace(projectName) == "" || plan.Isolation != nil {
		return Baseline{}, fmt.Errorf("采集生产基线失败: Plan 必须是未隔离的生产项目")
	}
	containers, err := captureContainers(ctx, runner, projectName)
	if err != nil {
		return Baseline{}, err
	}
	networks, err := captureNetworks(ctx, runner, projectName)
	if err != nil {
		return Baseline{}, err
	}
	volumes, err := captureVolumes(ctx, runner, projectName)
	if err != nil {
		return Baseline{}, err
	}
	files, err := captureFiles(ctx, runner, plan)
	if err != nil {
		return Baseline{}, err
	}
	baseline := Baseline{Containers: containers, Networks: networks, Volumes: volumes, Files: files}
	payload, err := json.Marshal(struct {
		Containers []ContainerBaseline `json:"containers"`
		Networks   []NetworkBaseline   `json:"networks"`
		Volumes    []VolumeBaseline    `json:"volumes"`
		Files      []FileBaseline      `json:"files"`
	}{baseline.Containers, baseline.Networks, baseline.Volumes, baseline.Files})
	if err != nil {
		return Baseline{}, fmt.Errorf("编码生产基线失败: %w", err)
	}
	digest := sha256.Sum256(payload)
	baseline.Fingerprint = hex.EncodeToString(digest[:])
	return baseline, nil
}

// CompareBaselines 返回发生变化的资源类别；空切片表示生产基线完全一致。
// @param before 演练首次目标写入前的生产基线。
// @param after 清理或保留决策后的生产基线。
// @return []string 稳定排序的 containers、networks、volumes 或 files 差异类别。
func CompareBaselines(before Baseline, after Baseline) []string {
	var differences []string
	if !equalJSON(before.Containers, after.Containers) {
		differences = append(differences, "containers")
	}
	if !equalJSON(before.Networks, after.Networks) {
		differences = append(differences, "networks")
	}
	if !equalJSON(before.Volumes, after.Volumes) {
		differences = append(differences, "volumes")
	}
	if !equalJSON(before.Files, after.Files) {
		differences = append(differences, "files")
	}
	return differences
}

func captureContainers(ctx context.Context, runner sshexec.Runner, projectName string) ([]ContainerBaseline, error) {
	ids, err := listResourceNames(ctx, runner, []string{
		"docker", "ps", "-a", "--no-trunc", "--filter", "label=" + composeProjectLabel + "=" + projectName,
		"--format", "{{.ID}}",
	}, "容器")
	if err != nil {
		return nil, err
	}
	result := make([]ContainerBaseline, 0, len(ids))
	for _, id := range ids {
		project, err := inspectValue(ctx, runner, "container", id, `{{index .Config.Labels "com.docker.compose.project"}}`)
		if err != nil || project != projectName {
			return nil, fmt.Errorf("采集生产基线失败: 容器 %q project 标签不匹配", id)
		}
		service, err := inspectValue(ctx, runner, "container", id, `{{index .Config.Labels "com.docker.compose.service"}}`)
		if err != nil || strings.TrimSpace(service) == "" {
			return nil, fmt.Errorf("采集生产基线失败: 容器 %q 缺少 Compose service 标签", id)
		}
		state, err := inspectValue(ctx, runner, "container", id, "{{.State.Status}}")
		if err != nil {
			return nil, err
		}
		imageID, err := inspectValue(ctx, runner, "container", id, "{{.Image}}")
		if err != nil {
			return nil, err
		}
		digests, err := inspectStringList(ctx, runner, "image", imageID, "{{json .RepoDigests}}")
		if err != nil {
			return nil, err
		}
		result = append(result, ContainerBaseline{
			ID: id, Service: service, State: state, ImageID: imageID, ImageDigests: digests,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func captureNetworks(ctx context.Context, runner sshexec.Runner, projectName string) ([]NetworkBaseline, error) {
	names, err := listResourceNames(ctx, runner, []string{
		"docker", "network", "ls", "--filter", "label=" + composeProjectLabel + "=" + projectName,
		"--format", "{{.Name}}",
	}, "network")
	if err != nil {
		return nil, err
	}
	result := make([]NetworkBaseline, 0, len(names))
	for _, name := range names {
		labels, err := inspectStringMap(ctx, runner, "network", name, "{{json .Labels}}")
		if err != nil || labels[composeProjectLabel] != projectName {
			return nil, fmt.Errorf("采集生产基线失败: network %q project 标签不匹配", name)
		}
		id, err := inspectValue(ctx, runner, "network", name, "{{.ID}}")
		if err != nil {
			return nil, err
		}
		driver, err := inspectValue(ctx, runner, "network", name, "{{.Driver}}")
		if err != nil {
			return nil, err
		}
		result = append(result, NetworkBaseline{ID: id, Name: name, Driver: driver, Labels: labels})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func captureVolumes(ctx context.Context, runner sshexec.Runner, projectName string) ([]VolumeBaseline, error) {
	names, err := listResourceNames(ctx, runner, []string{
		"docker", "volume", "ls", "--filter", "label=" + composeProjectLabel + "=" + projectName,
		"--format", "{{.Name}}",
	}, "volume")
	if err != nil {
		return nil, err
	}
	result := make([]VolumeBaseline, 0, len(names))
	for _, name := range names {
		labels, err := inspectStringMap(ctx, runner, "volume", name, "{{json .Labels}}")
		if err != nil || labels[composeProjectLabel] != projectName {
			return nil, fmt.Errorf("采集生产基线失败: volume %q project 标签不匹配", name)
		}
		driver, err := inspectValue(ctx, runner, "volume", name, "{{.Driver}}")
		if err != nil {
			return nil, err
		}
		result = append(result, VolumeBaseline{Name: name, Driver: driver, Labels: labels})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func captureFiles(ctx context.Context, runner sshexec.Runner, plan restore.Plan) ([]FileBaseline, error) {
	paths := make(map[string]struct{})
	for _, step := range plan.Steps {
		if step.Phase != restore.PhaseFiles || step.Target == nil || step.TargetType != config.TargetFiles {
			continue
		}
		for _, item := range step.Target.Paths {
			paths[item] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(paths))
	for item := range paths {
		ordered = append(ordered, item)
	}
	sort.Strings(ordered)
	result := make([]FileBaseline, 0, len(ordered))
	for _, item := range ordered {
		out, err := runner.Run(ctx, "stat", "--printf=%F\n%a\n%u\n%g\n%s\n%Y\n", "--", item)
		if err != nil {
			return nil, fmt.Errorf("采集生产基线失败: stat 路径 %q 失败: %w", item, err)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != 6 {
			return nil, fmt.Errorf("采集生产基线失败: stat 路径 %q 输出无效", item)
		}
		uid, uidErr := strconv.ParseUint(lines[2], 10, 64)
		gid, gidErr := strconv.ParseUint(lines[3], 10, 64)
		size, sizeErr := strconv.ParseInt(lines[4], 10, 64)
		modified, modifiedErr := strconv.ParseInt(lines[5], 10, 64)
		if err := firstError(uidErr, gidErr, sizeErr, modifiedErr); err != nil {
			return nil, fmt.Errorf("采集生产基线失败: 解析路径 %q stat 失败: %w", item, err)
		}
		entryCount := 1
		entryFingerprint := fingerprintStrings([]string{strings.Join(lines, "\x00")})
		if lines[0] == "directory" {
			entryCount, entryFingerprint, err = captureDirectoryMetadata(ctx, runner, item)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, FileBaseline{
			Path: item, Type: lines[0], Mode: lines[1], UID: uid, GID: gid, Size: size, ModifiedUnix: modified,
			EntryCount: entryCount, EntryFingerprint: entryFingerprint,
		})
	}
	return result, nil
}

func effectiveProjectName(project restore.Project) string {
	if project.ProjectName != "" {
		return project.ProjectName
	}
	return project.Name
}

func listResourceNames(ctx context.Context, runner sshexec.Runner, argv []string, resource string) ([]string, error) {
	out, err := runner.Run(ctx, argv...)
	if err != nil {
		return nil, fmt.Errorf("采集生产基线失败: 查询 %s 失败: %w", resource, err)
	}
	seen := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			seen[line] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for item := range seen {
		result = append(result, item)
	}
	sort.Strings(result)
	return result, nil
}

func inspectValue(ctx context.Context, runner sshexec.Runner, resource string, name string, format string) (string, error) {
	out, err := runner.Run(ctx, "docker", resource, "inspect", "--format", format, name)
	if err != nil {
		return "", fmt.Errorf("采集生产基线失败: 查询 %s %q 失败: %w", resource, name, err)
	}
	return strings.TrimSpace(out), nil
}

func inspectStringList(
	ctx context.Context,
	runner sshexec.Runner,
	resource string,
	name string,
	format string,
) ([]string, error) {
	out, err := inspectValue(ctx, runner, resource, name, format)
	if err != nil {
		return nil, err
	}
	if out == "null" || out == "" {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(out), &values); err != nil {
		return nil, fmt.Errorf("采集生产基线失败: 解析 %s %q 列表失败: %w", resource, name, err)
	}
	sort.Strings(values)
	return values, nil
}

func inspectStringMap(
	ctx context.Context,
	runner sshexec.Runner,
	resource string,
	name string,
	format string,
) (map[string]string, error) {
	out, err := inspectValue(ctx, runner, resource, name, format)
	if err != nil {
		return nil, err
	}
	if out == "null" || out == "" {
		return map[string]string{}, nil
	}
	values := make(map[string]string)
	if err := json.Unmarshal([]byte(out), &values); err != nil {
		return nil, fmt.Errorf("采集生产基线失败: 解析 %s %q 标签失败: %w", resource, name, err)
	}
	return values, nil
}

func captureDirectoryMetadata(
	ctx context.Context,
	runner sshexec.Runner,
	root string,
) (int, string, error) {
	// find 只输出路径和 stat 元数据；本地排序后哈希，避免文件系统遍历顺序影响基线。
	out, err := runner.Run(
		ctx,
		"find", root, "-xdev", "-printf", `%P\0%y\0%m\0%U\0%G\0%s\0%T@\0%l\0`,
	)
	if err != nil {
		return 0, "", fmt.Errorf("采集生产基线失败: 遍历目录 %q 失败: %w", root, err)
	}
	fields := strings.Split(out, "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	const metadataFieldCount = 8
	if len(fields)%metadataFieldCount != 0 {
		return 0, "", fmt.Errorf("采集生产基线失败: 目录 %q 元数据输出无效", root)
	}
	entries := make([]string, 0, len(fields)/metadataFieldCount)
	for index := 0; index < len(fields); index += metadataFieldCount {
		entries = append(entries, strings.Join(fields[index:index+metadataFieldCount], "\x00"))
	}
	sort.Strings(entries)
	return len(entries), fingerprintStrings(entries), nil
}

func fingerprintStrings(values []string) string {
	payload, _ := json.Marshal(values)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func equalJSON(left any, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
