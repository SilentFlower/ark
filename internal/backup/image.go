package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

const (
	containerInspectFormat = `{"image_id":{{json .Image}},"image_ref":{{json .Config.Image}}}`
	imageInspectFormat     = `{{json .RepoDigests}}`
)

type composeContainer struct {
	ID      string `json:"ID"`
	Service string `json:"Service"`
	State   string `json:"State"`
}

type containerImage struct {
	ImageID  string `json:"image_id"`
	ImageRef string `json:"image_ref"`
}

func executeImageDigest(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
) (*Result, error) {
	configArgv := append(composeArgv(host.Project), "config", "--format", "json", "--no-env-resolution")
	canonical, err := readComposeMetadataCanonical(ctx, runner, configArgv)
	if err != nil {
		return nil, fmt.Errorf("读取 target %q Compose 恢复元数据失败: %w", target.ID(), err)
	}
	composeMetadata, err := parseComposeMetadata(canonical)
	if err != nil {
		return nil, fmt.Errorf("解析 target %q Compose 恢复元数据失败: %w", target.ID(), err)
	}

	psArgv := append(composeArgv(host.Project), "ps", "--format", "json")
	out, err := runner.Run(ctx, psArgv...)
	if err != nil {
		return nil, fmt.Errorf("查询 target %q 运行容器失败: %w", target.ID(), err)
	}
	containers, err := parseComposeContainers(out)
	if err != nil {
		return nil, fmt.Errorf("解析 target %q 运行容器失败: %w", target.ID(), err)
	}

	digests := make(map[string]string, len(target.Services))
	services := append([]string(nil), target.Services...)
	sort.Strings(services)
	for _, service := range services {
		serviceDigest, err := resolveServiceDigest(ctx, target.ID(), service, containers, runner)
		if err != nil {
			return nil, err
		}
		digests[service] = serviceDigest
	}

	data, err := json.Marshal(digests)
	if err != nil {
		return nil, fmt.Errorf("编码 target %q image digest 失败: %w", target.ID(), err)
	}
	data = append(data, '\n')
	return memoryResult(
		host,
		target,
		".json",
		io.NopCloser(bytes.NewReader(data)),
		digests,
		composeMetadata,
	), nil
}

func readComposeMetadataCanonical(
	ctx context.Context,
	runner sshexec.Runner,
	argv []string,
) ([]byte, error) {
	payload, err := sshexec.ReadAllStdout(ctx, runner, argv...)
	if err != nil {
		return nil, composeMetadataCommandError{cause: err}
	}
	return payload, nil
}

type composeMetadataCommandError struct {
	cause error
}

func (e composeMetadataCommandError) Error() string {
	return "Compose canonical 命令失败"
}

func (e composeMetadataCommandError) Unwrap() error {
	return e.cause
}

func parseComposeContainers(output string) ([]composeContainer, error) {
	decoder := json.NewDecoder(strings.NewReader(output))
	var containers []composeContainer
	for {
		var container composeContainer
		if err := decoder.Decode(&container); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("compose ps JSON 无效: %w", err)
		}
		if container.ID == "" || container.Service == "" || container.State == "" {
			return nil, fmt.Errorf("compose ps JSON 缺少 ID、Service 或 State")
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func resolveServiceDigest(
	ctx context.Context,
	targetID string,
	service string,
	containers []composeContainer,
	runner sshexec.Runner,
) (string, error) {
	var serviceContainers []composeContainer
	for _, container := range containers {
		if container.Service == service && container.State == "running" {
			serviceContainers = append(serviceContainers, container)
		}
	}
	if len(serviceContainers) == 0 {
		return "", fmt.Errorf("解析 target %q image digest 失败: service %q 没有运行中的容器", targetID, service)
	}

	resolved := ""
	for _, container := range serviceContainers {
		containerImage, err := inspectContainerImage(ctx, targetID, service, container.ID, runner)
		if err != nil {
			return "", err
		}
		repoDigests, err := inspectRepoDigests(ctx, targetID, service, containerImage.ImageID, runner)
		if err != nil {
			return "", err
		}
		candidate, err := selectRepoDigest(containerImage.ImageRef, repoDigests)
		if err != nil {
			return "", fmt.Errorf("解析 target %q image digest 失败: service %q: %w", targetID, service, err)
		}
		if resolved != "" && resolved != candidate {
			return "", fmt.Errorf(
				"解析 target %q image digest 失败: service %q 的运行容器使用多个 digest",
				targetID,
				service,
			)
		}
		resolved = candidate
	}
	return resolved, nil
}

func inspectContainerImage(
	ctx context.Context,
	targetID string,
	service string,
	containerID string,
	runner sshexec.Runner,
) (containerImage, error) {
	out, err := runner.Run(ctx,
		"docker", "container", "inspect", "--format", containerInspectFormat, containerID)
	if err != nil {
		return containerImage{}, fmt.Errorf(
			"查询 target %q service %q 运行镜像失败: %w",
			targetID,
			service,
			err,
		)
	}
	var image containerImage
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &image); err != nil {
		return containerImage{}, fmt.Errorf(
			"解析 target %q service %q 运行镜像失败: %w",
			targetID,
			service,
			err,
		)
	}
	if image.ImageID == "" || image.ImageRef == "" {
		return containerImage{}, fmt.Errorf(
			"解析 target %q service %q 运行镜像失败: image ID 或引用为空",
			targetID,
			service,
		)
	}
	return image, nil
}

func inspectRepoDigests(
	ctx context.Context,
	targetID string,
	service string,
	imageID string,
	runner sshexec.Runner,
) ([]string, error) {
	out, err := runner.Run(ctx,
		"docker", "image", "inspect", "--format", imageInspectFormat, imageID)
	if err != nil {
		return nil, fmt.Errorf(
			"查询 target %q service %q RepoDigests 失败: %w",
			targetID,
			service,
			err,
		)
	}
	var digests []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &digests); err != nil {
		return nil, fmt.Errorf(
			"解析 target %q service %q RepoDigests 失败: %w",
			targetID,
			service,
			err,
		)
	}
	if len(digests) == 0 {
		return nil, fmt.Errorf("解析 target %q service %q RepoDigests 失败: RepoDigests 为空", targetID, service)
	}
	return digests, nil
}

func selectRepoDigest(imageRef string, repoDigests []string) (string, error) {
	repository, err := repositoryFromReference(imageRef)
	if err != nil {
		return "", err
	}
	normalized := normalizeRepository(repository)
	var candidates []string
	for _, repoDigest := range repoDigests {
		at := strings.LastIndex(repoDigest, "@")
		if at <= 0 || at == len(repoDigest)-1 {
			continue
		}
		if normalizeRepository(repoDigest[:at]) == normalized {
			candidates = append(candidates, repoDigest)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("RepoDigests 无法对应运行镜像仓库 %q", repository)
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("运行镜像仓库 %q 对应多个 RepoDigest", repository)
	}
	return candidates[0], nil
}

func repositoryFromReference(imageRef string) (string, error) {
	value := strings.TrimSpace(imageRef)
	if value == "" {
		return "", fmt.Errorf("运行镜像引用为空")
	}
	if at := strings.LastIndex(value, "@"); at >= 0 {
		value = value[:at]
	}
	lastSlash := strings.LastIndex(value, "/")
	if colon := strings.LastIndex(value, ":"); colon > lastSlash {
		value = value[:colon]
	}
	if value == "" || strings.HasPrefix(value, "sha256") {
		return "", fmt.Errorf("运行镜像引用 %q 不包含可确定的仓库", imageRef)
	}
	return value, nil
}

func normalizeRepository(repository string) string {
	value := strings.ToLower(strings.TrimSpace(repository))
	value = strings.TrimPrefix(value, "index.docker.io/")
	value = strings.TrimPrefix(value, "docker.io/")
	value = strings.TrimPrefix(value, "library/")
	return value
}
