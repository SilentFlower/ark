package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/restore"
)

type fakeRunner struct {
	run func(context.Context, ...string) (string, error)
}

func (r *fakeRunner) Run(ctx context.Context, argv ...string) (string, error) {
	return r.run(ctx, argv...)
}

func (r *fakeRunner) Stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("测试 fake 不支持 Stream")
}

func (r *fakeRunner) Feed(context.Context, io.Reader, ...string) error {
	return errors.New("测试 fake 不支持 Feed")
}

func TestCaptureBaseline_采集生产资源和目录递归元数据(t *testing.T) {
	plan := restore.Plan{
		Project: restore.Project{Name: "app", ProjectName: "app-prod"},
		Steps: []restore.Step{{
			Phase: restore.PhaseFiles, TargetType: config.TargetFiles,
			Target: &restore.Target{Type: config.TargetFiles, Paths: []string{"/srv/app"}},
		}},
	}
	runner := &fakeRunner{run: func(_ context.Context, argv ...string) (string, error) {
		joined := strings.Join(argv, "\x00")
		switch {
		case reflect.DeepEqual(argv, []string{
			"docker", "ps", "-a", "--no-trunc", "--filter", "label=com.docker.compose.project=app-prod", "--format", "{{.ID}}",
		}):
			return "container-1\n", nil
		case joined == strings.Join([]string{"docker", "container", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, "container-1"}, "\x00"):
			return "app-prod\n", nil
		case joined == strings.Join([]string{"docker", "container", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.service"}}`, "container-1"}, "\x00"):
			return "api\n", nil
		case joined == strings.Join([]string{"docker", "container", "inspect", "--format", "{{.State.Status}}", "container-1"}, "\x00"):
			return "running\n", nil
		case joined == strings.Join([]string{"docker", "container", "inspect", "--format", "{{.Image}}", "container-1"}, "\x00"):
			return "sha256:image\n", nil
		case joined == strings.Join([]string{"docker", "image", "inspect", "--format", "{{json .RepoDigests}}", "sha256:image"}, "\x00"):
			return `["registry/app@sha256:bbb","registry/app@sha256:aaa"]`, nil
		case reflect.DeepEqual(argv, []string{
			"docker", "network", "ls", "--filter", "label=com.docker.compose.project=app-prod", "--format", "{{.Name}}",
		}):
			return "app-prod_default\n", nil
		case joined == strings.Join([]string{"docker", "network", "inspect", "--format", "{{json .Labels}}", "app-prod_default"}, "\x00"):
			return `{"com.docker.compose.project":"app-prod","com.docker.compose.version":"2.30.0"}`, nil
		case joined == strings.Join([]string{"docker", "network", "inspect", "--format", "{{.ID}}", "app-prod_default"}, "\x00"):
			return "network-1\n", nil
		case joined == strings.Join([]string{"docker", "network", "inspect", "--format", "{{.Driver}}", "app-prod_default"}, "\x00"):
			return "bridge\n", nil
		case reflect.DeepEqual(argv, []string{
			"docker", "volume", "ls", "--filter", "label=com.docker.compose.project=app-prod", "--format", "{{.Name}}",
		}):
			return "app-prod_data\n", nil
		case joined == strings.Join([]string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", "app-prod_data"}, "\x00"):
			return `{"com.docker.compose.project":"app-prod","com.docker.compose.volume":"data"}`, nil
		case joined == strings.Join([]string{"docker", "volume", "inspect", "--format", "{{.Driver}}", "app-prod_data"}, "\x00"):
			return "local\n", nil
		case reflect.DeepEqual(argv, []string{"stat", "--printf=%F\n%a\n%u\n%g\n%s\n%Y\n", "--", "/srv/app"}):
			return "directory\n755\n0\n0\n4096\n1700000000\n", nil
		case reflect.DeepEqual(argv, []string{"find", "/srv/app", "-xdev", "-printf", `%P\0%y\0%m\0%U\0%G\0%s\0%T@\0%l\0`}):
			return "config.yaml\x00f\x00644\x000\x000\x0012\x001700000001.0000000000\x00\x00\x00d\x00755\x000\x000\x004096\x001700000000.0000000000\x00\x00", nil
		default:
			return "", fmt.Errorf("未配置命令响应: %#v", argv)
		}
	}}
	baseline, err := CaptureBaseline(context.Background(), runner, plan)
	if err != nil {
		t.Fatalf("CaptureBaseline 失败: %v", err)
	}
	if baseline.Fingerprint == "" || len(baseline.Containers) != 1 || len(baseline.Files) != 1 {
		t.Fatalf("baseline=%#v", baseline)
	}
	if !reflect.DeepEqual(baseline.Containers[0].ImageDigests, []string{
		"registry/app@sha256:aaa", "registry/app@sha256:bbb",
	}) {
		t.Fatalf("image digests=%#v", baseline.Containers[0].ImageDigests)
	}
	if baseline.Files[0].EntryCount != 2 || baseline.Files[0].EntryFingerprint == "" {
		t.Fatalf("files baseline=%#v", baseline.Files[0])
	}
	if baseline.Networks[0].Labels["com.docker.compose.version"] != "2.30.0" ||
		baseline.Volumes[0].Labels["com.docker.compose.volume"] != "data" {
		t.Fatalf("resource labels network=%#v volume=%#v", baseline.Networks[0], baseline.Volumes[0])
	}
}

func TestCompareBaselines_返回稳定差异类别(t *testing.T) {
	base := Baseline{
		Containers: []ContainerBaseline{{ID: "container-1"}},
		Networks:   []NetworkBaseline{{Name: "network-1"}},
		Volumes:    []VolumeBaseline{{Name: "volume-1"}},
		Files:      []FileBaseline{{Path: "/srv/app"}},
	}
	changed := base
	changed.Containers = []ContainerBaseline{{ID: "container-2"}}
	changed.Files = []FileBaseline{{Path: "/srv/changed"}}
	if got := CompareBaselines(base, changed); !reflect.DeepEqual(got, []string{"containers", "files"}) {
		t.Fatalf("differences=%#v", got)
	}
}

func TestCompareBaselines_资源Label变化会被发现(t *testing.T) {
	base := Baseline{
		Networks: []NetworkBaseline{{Name: "network-1", Labels: map[string]string{"role": "prod"}}},
		Volumes:  []VolumeBaseline{{Name: "volume-1", Labels: map[string]string{"role": "prod"}}},
	}
	changed := base
	changed.Networks = []NetworkBaseline{{Name: "network-1", Labels: map[string]string{"role": "changed"}}}
	changed.Volumes = []VolumeBaseline{{Name: "volume-1", Labels: map[string]string{"role": "changed"}}}
	if got := CompareBaselines(base, changed); !reflect.DeepEqual(got, []string{"networks", "volumes"}) {
		t.Fatalf("differences=%#v", got)
	}
}
