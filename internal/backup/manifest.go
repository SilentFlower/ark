package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/store"
)

const (
	// ManifestSchemaVersion 是当前支持的备份清单格式版本。
	ManifestSchemaVersion = 1
	// ManifestFilename 是 manifest 在 restic 快照中的稳定文件名。
	ManifestFilename = "ark-manifest.json"
	// ManifestTag 标识只包含 ark manifest 的 restic 快照。
	ManifestTag = "ark-manifest"
	// LatestManifestSelector 表示按创建时间与 ID 选择最新 manifest 快照。
	LatestManifestSelector = "latest"

	manifestMaximumBytes = 16 << 20
)

// Manifest 描述一次 backup run 产生的全部 target 最终结果。
type Manifest struct {
	// SchemaVersion 是 manifest 恢复契约版本。
	SchemaVersion int
	// RunID 是本次整体 backup 的稳定标识。
	RunID string
	// ArkVersion 是 hub 执行本次 backup 时的 ark 版本。
	ArkVersion string
	// StartedAt 是本次 run 的 UTC 开始时间。
	StartedAt time.Time
	// FinishedAt 是本次 run 的 UTC 结束时间。
	FinishedAt time.Time
	// Hosts 按清单执行顺序保存各 host 的 target 结果。
	Hosts []ManifestHost
}

// ManifestHost 保存一台 host 的全部 target 最终结果。
type ManifestHost struct {
	// Host 是清单中的 host 标识。
	Host string
	// Targets 按清单执行顺序保存 P2-4 产生的最终结果。
	Targets []TargetResult
}

type manifestWire struct {
	SchemaVersion int                `json:"schema_version"`
	RunID         string             `json:"run_id"`
	ArkVersion    string             `json:"ark_version"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at"`
	Hosts         []manifestHostWire `json:"hosts"`
}

type manifestHostWire struct {
	Host    string               `json:"host"`
	Targets []manifestTargetWire `json:"targets"`
}

type manifestTargetWire struct {
	ID           string            `json:"id"`
	Type         config.TargetType `json:"type"`
	SnapshotID   string            `json:"snapshot_id"`
	Bytes        int64             `json:"bytes"`
	Duration     string            `json:"duration"`
	Status       store.Status      `json:"status"`
	Error        string            `json:"error"`
	ImageDigests map[string]string `json:"image_digests"`
}

type manifestRepository struct {
	backupStdin    func(context.Context, io.Reader, string, []string) (restic.Snapshot, error)
	forgetSnapshot func(context.Context, string) error
	snapshots      func(context.Context, []string) ([]restic.Snapshot, error)
	dump           func(context.Context, string, string) (io.ReadCloser, error)
}

// Validate 校验 manifest 的版本、主键、时间和 target 结果约束。
// @return error schema 不支持、字段缺失、时间非法、数值为负或 target 重复时的错误。
func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return unsupportedManifestSchema(m.SchemaVersion)
	}
	if strings.TrimSpace(m.RunID) == "" {
		return fmt.Errorf("manifest run_id 不能为空")
	}
	if strings.TrimSpace(m.ArkVersion) == "" {
		return fmt.Errorf("manifest ark_version 不能为空")
	}
	if err := validateManifestTime("started_at", m.StartedAt); err != nil {
		return err
	}
	if err := validateManifestTime("finished_at", m.FinishedAt); err != nil {
		return err
	}
	if m.FinishedAt.Before(m.StartedAt) {
		return fmt.Errorf("manifest finished_at 不能早于 started_at")
	}

	seenTargets := make(map[string]struct{})
	for hostIndex, host := range m.Hosts {
		if strings.TrimSpace(host.Host) == "" {
			return fmt.Errorf("manifest hosts[%d].host 不能为空", hostIndex)
		}
		for targetIndex, target := range host.Targets {
			field := fmt.Sprintf("manifest hosts[%d].targets[%d]", hostIndex, targetIndex)
			if target.Host != host.Host {
				return fmt.Errorf("%s.host %q 与所属 host %q 不一致", field, target.Host, host.Host)
			}
			if strings.TrimSpace(target.TargetID) == "" {
				return fmt.Errorf("%s.id 不能为空", field)
			}
			if !validManifestTargetType(target.TargetType) {
				return fmt.Errorf("%s.type %q 不受支持", field, target.TargetType)
			}
			if !validManifestStatus(target.Status) {
				return fmt.Errorf("%s.status %q 不受支持", field, target.Status)
			}
			if target.Bytes < 0 {
				return fmt.Errorf("%s.bytes 不能为负", field)
			}
			if target.Duration < 0 {
				return fmt.Errorf("%s.duration 不能为负", field)
			}
			if target.Status != store.StatusFail && strings.TrimSpace(target.SnapshotID) == "" {
				return fmt.Errorf("%s.snapshot_id 在状态 %q 下不能为空", field, target.Status)
			}
			for service, digest := range target.ImageDigests {
				if strings.TrimSpace(service) == "" || strings.TrimSpace(digest) == "" {
					return fmt.Errorf("%s.image_digests 的 service 和 digest 不能为空", field)
				}
			}

			key := host.Host + "\x00" + target.TargetID
			if _, exists := seenTargets[key]; exists {
				return fmt.Errorf("manifest target 重复: host=%q target=%q", host.Host, target.TargetID)
			}
			seenTargets[key] = struct{}{}
		}
	}
	return nil
}

// MarshalJSON 按 schema v1 的稳定字段编码 manifest，并在编码前执行完整校验。
// @return []byte 可存入 restic 的 JSON。
// @return error manifest 校验或 JSON 编码失败时的错误。
func (m Manifest) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(manifestToWire(m))
}

// UnmarshalJSON 解码并校验 schema v1 manifest，拒绝不支持版本。
// @param data manifest JSON。
// @return error JSON、schema 或字段校验失败时的错误。
func (m *Manifest) UnmarshalJSON(data []byte) error {
	if m == nil {
		return fmt.Errorf("manifest 接收对象不能为空")
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("解析 manifest schema 失败: %w", err)
	}
	if header.SchemaVersion != ManifestSchemaVersion {
		return unsupportedManifestSchema(header.SchemaVersion)
	}

	var wire manifestWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("解析 manifest v%d 失败: %w", ManifestSchemaVersion, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	decoded, err := manifestFromWire(wire)
	if err != nil {
		return err
	}
	if err := decoded.Validate(); err != nil {
		return err
	}
	*m = decoded
	return nil
}

// SaveManifest 校验并把 manifest 保存为带固定标签的独立 restic 快照。
// @param ctx 控制 restic 备份取消；本方法不附加固定超时。
// @param repo 已初始化的 restic 仓库。
// @param manifest 本次 run 的完整最终结果。
// @return restic.Snapshot 新 manifest 快照的稳定字段。
// @return error 参数、manifest 校验、JSON 编码、restic 存储或失败快照撤销错误。
func SaveManifest(ctx context.Context, repo *restic.Repo, manifest Manifest) (restic.Snapshot, error) {
	if repo == nil {
		return restic.Snapshot{}, fmt.Errorf("保存 manifest 失败: restic repo 不能为空")
	}
	return saveManifest(ctx, manifest, manifestRepository{
		backupStdin:    repo.BackupStdin,
		forgetSnapshot: repo.ForgetSnapshot,
	})
}

// LoadLatestManifest 按 manifest 标签读取时间与 ID 排序后的最新一份清单。
// @param ctx 控制 restic 快照查询和 dump 取消；本方法不附加固定超时。
// @param repo 已初始化的 restic 仓库。
// @return Manifest 最新且通过一致性校验的 manifest。
// @return bool 是否存在 manifest；不存在是正常分支。
// @return error 参数、快照元数据、dump、JSON 或一致性校验失败时的错误。
func LoadLatestManifest(ctx context.Context, repo *restic.Repo) (Manifest, bool, error) {
	if repo == nil {
		return Manifest{}, false, fmt.Errorf("读取 manifest 失败: restic repo 不能为空")
	}
	return loadLatestManifest(ctx, manifestRepository{
		snapshots: repo.Snapshots,
		dump:      repo.Dump,
	})
}

// LoadManifestSelection 按 latest 或唯一 snapshot ID 前缀读取 manifest，并返回精确快照元数据。
// @param ctx 控制 restic 快照查询和 dump 取消；本方法不附加固定超时。
// @param repo 已初始化的 restic 仓库。
// @param selector latest、完整 snapshot ID 或可唯一匹配的 snapshot ID 前缀。
// @return Manifest 通过元数据、内容与 run 一致性校验的 manifest。
// @return restic.Snapshot 实际读取的精确 manifest 快照元数据。
// @return bool 是否存在 manifest；仅 latest 且仓库无 manifest 时返回 false。
// @return error 参数、选择歧义、快照元数据、dump、JSON 或一致性校验失败时的错误。
func LoadManifestSelection(
	ctx context.Context,
	repo *restic.Repo,
	selector string,
) (Manifest, restic.Snapshot, bool, error) {
	if repo == nil {
		return Manifest{}, restic.Snapshot{}, false, fmt.Errorf("读取 manifest 失败: restic repo 不能为空")
	}
	return loadManifestSelection(ctx, selector, manifestRepository{
		snapshots: repo.Snapshots,
		dump:      repo.Dump,
	})
}

func saveManifest(
	ctx context.Context,
	manifest Manifest,
	repository manifestRepository,
) (restic.Snapshot, error) {
	if ctx == nil {
		return restic.Snapshot{}, fmt.Errorf("保存 manifest 失败: context 不能为空")
	}
	if repository.backupStdin == nil || repository.forgetSnapshot == nil {
		return restic.Snapshot{}, fmt.Errorf("保存 manifest 失败: 仓库依赖不完整")
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return restic.Snapshot{}, fmt.Errorf("编码 manifest 失败: %w", err)
	}
	payload = append(payload, '\n')
	snapshot, err := repository.backupStdin(
		ctx,
		bytes.NewReader(payload),
		ManifestFilename,
		[]string{ManifestTag, manifestRunTag(manifest.RunID)},
	)
	if err != nil {
		backupErr := fmt.Errorf("保存 manifest 到 restic 失败: %w", err)
		if strings.TrimSpace(snapshot.ID) == "" {
			return restic.Snapshot{}, backupErr
		}
		// restic 可能已经提交 manifest 后才返回非零；失败结果不能留作最新恢复清单。
		forgetErr := repository.forgetSnapshot(ctx, snapshot.ID)
		if forgetErr != nil {
			return restic.Snapshot{}, errors.Join(
				backupErr,
				fmt.Errorf("撤销失败的 manifest 快照 %q 失败: %w", snapshot.ID, forgetErr),
			)
		}
		return restic.Snapshot{}, backupErr
	}
	if strings.TrimSpace(snapshot.ID) == "" {
		return restic.Snapshot{}, fmt.Errorf("保存 manifest 到 restic 失败: 未返回 snapshot ID")
	}
	return snapshot, nil
}

func loadLatestManifest(
	ctx context.Context,
	repository manifestRepository,
) (Manifest, bool, error) {
	manifest, _, found, err := loadManifestSelection(ctx, LatestManifestSelector, repository)
	return manifest, found, err
}

func loadManifestSelection(
	ctx context.Context,
	selector string,
	repository manifestRepository,
) (Manifest, restic.Snapshot, bool, error) {
	if ctx == nil {
		return Manifest{}, restic.Snapshot{}, false, fmt.Errorf("读取 manifest 失败: context 不能为空")
	}
	if repository.snapshots == nil || repository.dump == nil {
		return Manifest{}, restic.Snapshot{}, false, fmt.Errorf("读取 manifest 失败: 仓库依赖不完整")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Manifest{}, restic.Snapshot{}, false, fmt.Errorf("读取 manifest 失败: snapshot 选择器不能为空")
	}
	snapshots, err := repository.snapshots(ctx, []string{ManifestTag})
	if err != nil {
		return Manifest{}, restic.Snapshot{}, false, fmt.Errorf("查询 manifest 快照失败: %w", err)
	}
	candidate, found, err := selectManifestSnapshot(snapshots, selector)
	if err != nil || !found {
		return Manifest{}, restic.Snapshot{}, found, err
	}
	manifest, err := readManifestSnapshot(ctx, candidate, repository)
	if err != nil {
		return Manifest{}, restic.Snapshot{}, false, err
	}
	return manifest, candidate, true, nil
}

func selectManifestSnapshot(
	snapshots []restic.Snapshot,
	selector string,
) (restic.Snapshot, bool, error) {
	if len(snapshots) == 0 {
		if selector == LatestManifestSelector {
			return restic.Snapshot{}, false, nil
		}
		return restic.Snapshot{}, false, fmt.Errorf("manifest snapshot %q 不存在", selector)
	}

	if selector != LatestManifestSelector {
		var prefixMatches []restic.Snapshot
		for _, snapshot := range snapshots {
			if snapshot.ID == selector {
				return snapshot, true, nil
			}
			if strings.HasPrefix(snapshot.ID, selector) {
				prefixMatches = append(prefixMatches, snapshot)
			}
		}
		switch len(prefixMatches) {
		case 0:
			return restic.Snapshot{}, false, fmt.Errorf("manifest snapshot %q 不存在", selector)
		case 1:
			return prefixMatches[0], true, nil
		default:
			return restic.Snapshot{}, false, fmt.Errorf(
				"manifest snapshot %q 匹配多个候选，必须提供更长的 ID",
				selector,
			)
		}
	}

	ordered := append([]restic.Snapshot(nil), snapshots...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Time.Equal(ordered[j].Time) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Time.Before(ordered[j].Time)
	})
	return ordered[len(ordered)-1], true, nil
}

func readManifestSnapshot(
	ctx context.Context,
	candidate restic.Snapshot,
	repository manifestRepository,
) (Manifest, error) {
	runID, err := validateManifestSnapshot(candidate)
	if err != nil {
		return Manifest{}, err
	}

	reader, err := repository.dump(ctx, candidate.ID, ManifestFilename)
	if err != nil {
		return Manifest{}, fmt.Errorf("读取 manifest 快照 %q 失败: %w", candidate.ID, err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(reader, manifestMaximumBytes+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return Manifest{}, fmt.Errorf("读取 manifest 快照 %q 内容失败: %w", candidate.ID, err)
	}
	if len(payload) > manifestMaximumBytes {
		return Manifest{}, fmt.Errorf("manifest 快照 %q 超过 %d 字节限制", candidate.ID, manifestMaximumBytes)
	}

	var manifest Manifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("解析 manifest 快照 %q 失败: %w", candidate.ID, err)
	}
	if manifest.RunID != runID {
		return Manifest{}, fmt.Errorf(
			"manifest 快照 %q 的 run tag %q 与内容 run_id %q 不一致",
			candidate.ID,
			runID,
			manifest.RunID,
		)
	}
	return manifest, nil
}

func manifestToWire(manifest Manifest) manifestWire {
	wire := manifestWire{
		SchemaVersion: manifest.SchemaVersion,
		RunID:         manifest.RunID,
		ArkVersion:    manifest.ArkVersion,
		StartedAt:     manifest.StartedAt.UTC(),
		FinishedAt:    manifest.FinishedAt.UTC(),
		Hosts:         make([]manifestHostWire, len(manifest.Hosts)),
	}
	for hostIndex, host := range manifest.Hosts {
		wireHost := manifestHostWire{
			Host:    host.Host,
			Targets: make([]manifestTargetWire, len(host.Targets)),
		}
		for targetIndex, target := range host.Targets {
			wireHost.Targets[targetIndex] = manifestTargetWire{
				ID:           target.TargetID,
				Type:         target.TargetType,
				SnapshotID:   target.SnapshotID,
				Bytes:        target.Bytes,
				Duration:     target.Duration.String(),
				Status:       target.Status,
				Error:        target.Error,
				ImageDigests: cloneStringMap(target.ImageDigests),
			}
		}
		wire.Hosts[hostIndex] = wireHost
	}
	return wire
}

func manifestFromWire(wire manifestWire) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: wire.SchemaVersion,
		RunID:         wire.RunID,
		ArkVersion:    wire.ArkVersion,
		StartedAt:     wire.StartedAt,
		FinishedAt:    wire.FinishedAt,
		Hosts:         make([]ManifestHost, len(wire.Hosts)),
	}
	for hostIndex, wireHost := range wire.Hosts {
		host := ManifestHost{
			Host:    wireHost.Host,
			Targets: make([]TargetResult, len(wireHost.Targets)),
		}
		for targetIndex, wireTarget := range wireHost.Targets {
			duration, err := time.ParseDuration(wireTarget.Duration)
			if err != nil {
				return Manifest{}, fmt.Errorf(
					"manifest hosts[%d].targets[%d].duration %q 非法: %w",
					hostIndex,
					targetIndex,
					wireTarget.Duration,
					err,
				)
			}
			host.Targets[targetIndex] = TargetResult{
				Host:         wireHost.Host,
				TargetID:     wireTarget.ID,
				TargetType:   wireTarget.Type,
				Status:       wireTarget.Status,
				Bytes:        wireTarget.Bytes,
				Duration:     duration,
				SnapshotID:   wireTarget.SnapshotID,
				Error:        wireTarget.Error,
				ImageDigests: cloneStringMap(wireTarget.ImageDigests),
			}
		}
		manifest.Hosts[hostIndex] = host
	}
	return manifest, nil
}

func validateManifestTime(field string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("manifest %s 不能为空", field)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("manifest %s 必须使用 UTC，实际偏移 %d 秒", field, offset)
	}
	return nil
}

func validManifestTargetType(targetType config.TargetType) bool {
	switch targetType {
	case config.TargetPostgres, config.TargetRedis, config.TargetVolume,
		config.TargetFiles, config.TargetImageDigest:
		return true
	default:
		return false
	}
}

func validManifestStatus(status store.Status) bool {
	switch status {
	case store.StatusOK, store.StatusWarn, store.StatusFail:
		return true
	default:
		return false
	}
}

func validateManifestSnapshot(snapshot restic.Snapshot) (string, error) {
	if strings.TrimSpace(snapshot.ID) == "" {
		return "", fmt.Errorf("manifest 候选快照缺少 snapshot ID")
	}
	if snapshot.Time.IsZero() {
		return "", fmt.Errorf("manifest 候选快照 %q 缺少创建时间", snapshot.ID)
	}
	if len(snapshot.Paths) != 1 || strings.TrimPrefix(snapshot.Paths[0], "/") != ManifestFilename {
		return "", fmt.Errorf(
			"manifest 候选快照 %q 的路径 %#v 与固定文件名 %q 不一致",
			snapshot.ID,
			snapshot.Paths,
			ManifestFilename,
		)
	}

	hasManifestTag := false
	var runID string
	for _, tag := range snapshot.Tags {
		switch {
		case tag == ManifestTag:
			hasManifestTag = true
		case strings.HasPrefix(tag, "run:"):
			value := strings.TrimPrefix(tag, "run:")
			if strings.TrimSpace(value) == "" {
				return "", fmt.Errorf("manifest 候选快照 %q 包含空 run tag", snapshot.ID)
			}
			if runID != "" {
				return "", fmt.Errorf("manifest 候选快照 %q 包含多个 run tag", snapshot.ID)
			}
			runID = value
		}
	}
	if !hasManifestTag {
		return "", fmt.Errorf("manifest 候选快照 %q 缺少标签 %q", snapshot.ID, ManifestTag)
	}
	if runID == "" {
		return "", fmt.Errorf("manifest 候选快照 %q 缺少 run tag", snapshot.ID)
	}
	return runID, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("解析 manifest 尾部失败: %w", err)
	}
	return fmt.Errorf("manifest JSON 包含多个顶层值")
}

func unsupportedManifestSchema(actual int) error {
	return fmt.Errorf(
		"不支持 manifest schema_version: 实际 %d，当前支持 %d",
		actual,
		ManifestSchemaVersion,
	)
}

func manifestRunTag(runID string) string {
	return "run:" + runID
}
