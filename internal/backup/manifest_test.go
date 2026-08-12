package backup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restic"
	"github.com/silentflower/ark/internal/store"
)

func testManifest() Manifest {
	startedAt := time.Date(2026, 8, 12, 4, 17, 0, 123, time.UTC)
	return Manifest{
		SchemaVersion: ManifestSchemaVersion,
		RunID:         "run-1",
		ArkVersion:    "v0.2.0",
		StartedAt:     startedAt,
		FinishedAt:    startedAt.Add(3*time.Second + time.Nanosecond),
		Hosts: []ManifestHost{
			{
				Host: "web-01",
				Targets: []TargetResult{
					{
						Host:         "web-01",
						TargetID:     "image_digest",
						TargetType:   config.TargetImageDigest,
						Status:       store.StatusWarn,
						Bytes:        128,
						Duration:     2*time.Second + time.Nanosecond,
						SnapshotID:   "snapshot-1",
						Error:        "镜像需要复核",
						ImageDigests: map[string]string{"worker": "repo/worker@sha256:222", "api": "repo/api@sha256:111"},
					},
				},
			},
		},
	}
}

func TestManifestJSON_完整往返且输出稳定(t *testing.T) {
	want := testManifest()
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	text := string(payload)
	for _, field := range []string{
		`"schema_version":1`,
		`"run_id":"run-1"`,
		`"duration":"2.000000001s"`,
		`"status":"warn"`,
		`"image_digests":{"api":"repo/api@sha256:111","worker":"repo/worker@sha256:222"}`,
	} {
		if !strings.Contains(text, field) {
			t.Errorf("JSON %s 缺少 %s", text, field)
		}
	}

	var got Manifest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("往返结果 = %#v，期望 %#v", got, want)
	}
}

func TestManifestValidate_拒绝非法输入(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{name: "schema", mutate: func(m *Manifest) { m.SchemaVersion = 2 }, wantErr: "实际 2，当前支持 1"},
		{name: "run ID", mutate: func(m *Manifest) { m.RunID = "" }, wantErr: "run_id"},
		{name: "ark version", mutate: func(m *Manifest) { m.ArkVersion = "" }, wantErr: "ark_version"},
		{name: "开始时间", mutate: func(m *Manifest) { m.StartedAt = time.Time{} }, wantErr: "started_at"},
		{name: "非 UTC", mutate: func(m *Manifest) { m.StartedAt = m.StartedAt.In(time.FixedZone("CST", 8*60*60)) }, wantErr: "UTC"},
		{name: "时间倒序", mutate: func(m *Manifest) { m.FinishedAt = m.StartedAt.Add(-time.Second) }, wantErr: "不能早于"},
		{name: "host", mutate: func(m *Manifest) { m.Hosts[0].Host = "" }, wantErr: ".host"},
		{name: "target host", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].Host = "other" }, wantErr: "不一致"},
		{name: "target ID", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].TargetID = "" }, wantErr: ".id"},
		{name: "target type", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].TargetType = "unknown" }, wantErr: ".type"},
		{name: "status", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].Status = store.StatusRunning }, wantErr: ".status"},
		{name: "bytes", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].Bytes = -1 }, wantErr: ".bytes"},
		{name: "duration", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].Duration = -time.Second }, wantErr: ".duration"},
		{name: "成功无快照", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].SnapshotID = "" }, wantErr: ".snapshot_id"},
		{name: "空 digest", mutate: func(m *Manifest) { m.Hosts[0].Targets[0].ImageDigests["api"] = "" }, wantErr: "image_digests"},
		{
			name: "重复 target",
			mutate: func(m *Manifest) {
				m.Hosts = append(m.Hosts, ManifestHost{Host: "web-01", Targets: []TargetResult{m.Hosts[0].Targets[0]}})
			},
			wantErr: "target 重复",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manifest := testManifest()
			tc.mutate(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate 错误 = %v，期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestManifestUnmarshal_拒绝未知Schema和非法Duration(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr string
	}{
		{name: "未知 schema", payload: `{"schema_version":7}`, wantErr: "实际 7，当前支持 1"},
		{
			name:    "非法 duration",
			payload: `{"schema_version":1,"run_id":"run-1","ark_version":"v1","started_at":"2026-08-12T00:00:00Z","finished_at":"2026-08-12T00:00:01Z","hosts":[{"host":"web-01","targets":[{"id":"files/etc","type":"files","snapshot_id":"s1","bytes":1,"duration":"soon","status":"ok","error":"","image_digests":null}]}]}`,
			wantErr: "duration",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var manifest Manifest
			err := json.Unmarshal([]byte(tc.payload), &manifest)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Unmarshal 错误 = %v，期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestManifestUnmarshal_同Schema未知字段向后兼容(t *testing.T) {
	payload := `{"schema_version":1,"run_id":"run-1","ark_version":"v1","started_at":"2026-08-12T00:00:00Z","finished_at":"2026-08-12T00:00:01Z","hosts":[],"future":true}`
	var manifest Manifest
	if err := json.Unmarshal([]byte(payload), &manifest); err != nil {
		t.Fatalf("同 schema 新增可选字段应被忽略: %v", err)
	}
	if manifest.RunID != "run-1" || len(manifest.Hosts) != 0 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestSaveManifest_使用固定文件名与Tags(t *testing.T) {
	manifest := testManifest()
	var gotFilename string
	var gotTags []string
	var gotPayload []byte
	snapshot, err := saveManifest(context.Background(), manifest, manifestRepository{
		backupStdin: func(_ context.Context, reader io.Reader, filename string, tags []string) (restic.Snapshot, error) {
			gotFilename = filename
			gotTags = append([]string(nil), tags...)
			var readErr error
			gotPayload, readErr = io.ReadAll(reader)
			return restic.Snapshot{ID: "manifest-snapshot"}, readErr
		},
		forgetSnapshot: func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("saveManifest 失败: %v", err)
	}
	if snapshot.ID != "manifest-snapshot" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if gotFilename != ManifestFilename {
		t.Errorf("filename = %q，期望 %q", gotFilename, ManifestFilename)
	}
	if !reflect.DeepEqual(gotTags, []string{ManifestTag, "run:run-1"}) {
		t.Errorf("tags = %#v", gotTags)
	}
	if !strings.HasSuffix(string(gotPayload), "\n") {
		t.Errorf("payload 未以换行结尾: %q", gotPayload)
	}

	var decoded Manifest
	if err := json.Unmarshal(gotPayload, &decoded); err != nil {
		t.Fatalf("保存内容无法解码: %v", err)
	}
	if !reflect.DeepEqual(decoded, manifest) {
		t.Errorf("保存内容 = %#v，期望 %#v", decoded, manifest)
	}
}

func TestSaveManifest_备份失败时精确撤销已提交Snapshot(t *testing.T) {
	manifest := testManifest()
	backupErr := errors.New("restic 收尾失败")

	t.Run("撤销成功仍返回备份错误", func(t *testing.T) {
		var gotSnapshotID string
		_, err := saveManifest(context.Background(), manifest, manifestRepository{
			backupStdin: func(context.Context, io.Reader, string, []string) (restic.Snapshot, error) {
				return restic.Snapshot{ID: "failed-manifest"}, backupErr
			},
			forgetSnapshot: func(_ context.Context, snapshotID string) error {
				gotSnapshotID = snapshotID
				return nil
			},
		})
		if !errors.Is(err, backupErr) {
			t.Fatalf("错误 = %v，期望保留备份错误", err)
		}
		if gotSnapshotID != "failed-manifest" {
			t.Fatalf("撤销 snapshot ID = %q，期望 failed-manifest", gotSnapshotID)
		}
	})

	t.Run("撤销失败保留两条错误链", func(t *testing.T) {
		forgetErr := errors.New("forget 失败")
		_, err := saveManifest(context.Background(), manifest, manifestRepository{
			backupStdin: func(context.Context, io.Reader, string, []string) (restic.Snapshot, error) {
				return restic.Snapshot{ID: "failed-manifest"}, backupErr
			},
			forgetSnapshot: func(context.Context, string) error { return forgetErr },
		})
		if !errors.Is(err, backupErr) || !errors.Is(err, forgetErr) {
			t.Fatalf("错误 = %v，期望保留备份与撤销错误", err)
		}
	})

	t.Run("无ID时不猜测撤销", func(t *testing.T) {
		_, err := saveManifest(context.Background(), manifest, manifestRepository{
			backupStdin: func(context.Context, io.Reader, string, []string) (restic.Snapshot, error) {
				return restic.Snapshot{}, backupErr
			},
			forgetSnapshot: func(context.Context, string) error {
				t.Fatal("没有精确 snapshot ID 时不应撤销")
				return nil
			},
		})
		if !errors.Is(err, backupErr) {
			t.Fatalf("错误 = %v，期望保留备份错误", err)
		}
	})
}

func TestSaveManifest_拒绝无SnapshotID和无效输入(t *testing.T) {
	manifest := testManifest()
	_, err := saveManifest(context.Background(), manifest, manifestRepository{
		backupStdin: func(context.Context, io.Reader, string, []string) (restic.Snapshot, error) {
			return restic.Snapshot{}, nil
		},
		forgetSnapshot: func(context.Context, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot ID") {
		t.Fatalf("错误 = %v", err)
	}
	if _, err := SaveManifest(context.Background(), nil, manifest); err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("nil repo 错误 = %v", err)
	}
}

func TestLoadLatestManifest_按时间与ID确定最新并核对Run(t *testing.T) {
	manifest := testManifest()
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("编码测试 manifest 失败: %v", err)
	}
	base := time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)
	var gotTags []string
	var gotSnapshotID string
	var gotPath string
	got, found, err := loadLatestManifest(context.Background(), manifestRepository{
		snapshots: func(_ context.Context, tags []string) ([]restic.Snapshot, error) {
			gotTags = append([]string(nil), tags...)
			return []restic.Snapshot{
				{ID: "z-old", Time: base.Add(-time.Hour), Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:old"}},
				{ID: "a-same", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:old"}},
				{ID: "b-latest", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:run-1"}},
			}, nil
		},
		dump: func(_ context.Context, snapshotID, path string) (io.ReadCloser, error) {
			gotSnapshotID = snapshotID
			gotPath = path
			return io.NopCloser(strings.NewReader(string(payload))), nil
		},
	})
	if err != nil {
		t.Fatalf("loadLatestManifest 失败: %v", err)
	}
	if !found || !reflect.DeepEqual(got, manifest) {
		t.Fatalf("got=%#v found=%v", got, found)
	}
	if !reflect.DeepEqual(gotTags, []string{ManifestTag}) {
		t.Errorf("查询 tags = %#v", gotTags)
	}
	if gotSnapshotID != "b-latest" || gotPath != ManifestFilename {
		t.Errorf("dump = (%q, %q)", gotSnapshotID, gotPath)
	}
}

func TestLoadLatestManifest_无快照是正常分支(t *testing.T) {
	got, found, err := loadLatestManifest(context.Background(), manifestRepository{
		snapshots: func(context.Context, []string) ([]restic.Snapshot, error) { return nil, nil },
		dump: func(context.Context, string, string) (io.ReadCloser, error) {
			t.Fatal("无快照时不应 dump")
			return nil, nil
		},
	})
	if err != nil || found || !reflect.DeepEqual(got, Manifest{}) {
		t.Fatalf("got=%#v found=%v err=%v", got, found, err)
	}
	if _, _, err := LoadLatestManifest(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "repo") {
		t.Fatalf("nil repo 错误 = %v", err)
	}
}

func TestLoadLatestManifest_拒绝元数据与内容不一致(t *testing.T) {
	manifest := testManifest()
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("编码测试 manifest 失败: %v", err)
	}
	base := time.Date(2026, 8, 12, 5, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot restic.Snapshot
		payload  string
		dumpErr  error
		wantErr  string
	}{
		{name: "路径", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/other.json"}, Tags: []string{ManifestTag, "run:run-1"}}, payload: string(payload), wantErr: "路径"},
		{name: "缺标签", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{"run:run-1"}}, payload: string(payload), wantErr: "缺少标签"},
		{name: "多个run", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:run-1", "run:run-2"}}, payload: string(payload), wantErr: "多个 run tag"},
		{name: "run不一致", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:other"}}, payload: string(payload), wantErr: "不一致"},
		{name: "dump失败", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:run-1"}}, dumpErr: errors.New("dump failed"), wantErr: "dump failed"},
		{name: "JSON损坏", snapshot: restic.Snapshot{ID: "s1", Time: base, Paths: []string{"/" + ManifestFilename}, Tags: []string{ManifestTag, "run:run-1"}}, payload: "{", wantErr: "解析 manifest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, found, err := loadLatestManifest(context.Background(), manifestRepository{
				snapshots: func(context.Context, []string) ([]restic.Snapshot, error) {
					return []restic.Snapshot{tc.snapshot}, nil
				},
				dump: func(context.Context, string, string) (io.ReadCloser, error) {
					if tc.dumpErr != nil {
						return nil, tc.dumpErr
					}
					return io.NopCloser(strings.NewReader(tc.payload)), nil
				},
			})
			if found || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("found=%v err=%v，期望包含 %q", found, err, tc.wantErr)
			}
		})
	}
}
