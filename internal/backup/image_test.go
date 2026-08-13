package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

const composeMetadataHelperEnv = "ARK_COMPOSE_METADATA_HELPER"

func TestComposeMetadataHelper(t *testing.T) {
	if os.Getenv(composeMetadataHelperEnv) != "1" {
		return
	}
	_, _ = io.WriteString(os.Stdout, `{"services":{"api":{}}}`)
	_, _ = io.WriteString(os.Stderr, "compose warning should stay on stderr")
	os.Exit(0)
}

func TestExecuteImageDigest_UsesRunningImageIDsAndStableJSON(t *testing.T) {
	runner := &fakeRunner{
		streamResponses: []streamResponse{testStreamResponse(`{"services":{"api":{"ports":[{"target":8080,"published":"8080","host_ip":"127.0.0.1","protocol":"tcp","app_protocol":"http","mode":"ingress"}]},"worker":{}}}`)},
		runResponses: []runResponse{
			{out: strings.Join([]string{
				`{"ID":"worker-1","Service":"worker","State":"running"}`,
				`{"ID":"api-1","Service":"api","State":"running"}`,
				`{"ID":"api-old","Service":"api","State":"exited"}`,
			}, "\n")},
			{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
			{out: `["ghcr.io/acme/app@sha256:111"]`},
			{out: `{"image_id":"sha256:worker","image_ref":"redis:7"}`},
			{out: `["docker.io/library/redis@sha256:222"]`},
		}}
	target := config.Target{Type: config.TargetImageDigest, Services: []string{"worker", "api"}}

	result, err := Execute(context.Background(), testHost(), target, runner)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	assertCalls(t, runner.calls, []runnerCall{
		{kind: "stream", argv: []string{
			"docker", "compose", "-f", "/srv/app/compose.yaml",
			"-p", "production", "--env-file", "/srv/app/.env",
			"config", "--format", "json", "--no-env-resolution",
		}},
		{kind: "run", argv: []string{
			"docker", "compose", "-f", "/srv/app/compose.yaml",
			"-p", "production", "--env-file", "/srv/app/.env",
			"ps", "--format", "json",
		}},
		{kind: "run", argv: []string{
			"docker", "container", "inspect", "--format", containerInspectFormat, "api-1",
		}},
		{kind: "run", argv: []string{
			"docker", "image", "inspect", "--format", imageInspectFormat, "sha256:api",
		}},
		{kind: "run", argv: []string{
			"docker", "container", "inspect", "--format", containerInspectFormat, "worker-1",
		}},
		{kind: "run", argv: []string{
			"docker", "image", "inspect", "--format", imageInspectFormat, "sha256:worker",
		}},
	})
	wantDigests := map[string]string{
		"api":    "ghcr.io/acme/app@sha256:111",
		"worker": "docker.io/library/redis@sha256:222",
	}
	if len(result.ImageDigests) != len(wantDigests) {
		t.Fatalf("ImageDigests = %#v", result.ImageDigests)
	}
	for service, want := range wantDigests {
		if result.ImageDigests[service] != want {
			t.Errorf("ImageDigests[%q] = %q，期望 %q", service, result.ImageDigests[service], want)
		}
	}
	if result.ComposeMetadata == nil || len(result.ComposeMetadata.PublishedPorts) != 1 {
		t.Fatalf("ComposeMetadata = %#v", result.ComposeMetadata)
	}
	port := result.ComposeMetadata.PublishedPorts[0]
	if port.Service != "api" || port.HostIP != "127.0.0.1" || port.Published != "8080" ||
		port.Target != 8080 || port.Protocol != "tcp" || port.AppProtocol != "http" || port.Mode != "ingress" {
		t.Fatalf("published port = %#v", port)
	}
	data, err := io.ReadAll(result.Reader)
	if err != nil {
		t.Fatalf("读取 JSON 失败: %v", err)
	}
	wantJSON := "{\"api\":\"ghcr.io/acme/app@sha256:111\",\"worker\":\"docker.io/library/redis@sha256:222\"}\n"
	if string(data) != wantJSON {
		t.Errorf("JSON = %q，期望 %q", data, wantJSON)
	}
	if result.StdinFilename != "web-01/image_digest.json" {
		t.Errorf("StdinFilename = %q", result.StdinFilename)
	}
	if err := result.Wait(); err != nil {
		t.Fatalf("内存流 Wait 失败: %v", err)
	}
}

func TestExecuteImageDigest_RejectsAmbiguousOrMissingDigest(t *testing.T) {
	runnerErr := errors.New("inspect failed")
	tests := []struct {
		name      string
		responses []runResponse
		services  []string
		wantSub   string
		wantErr   error
	}{
		{
			name:      "compose JSON 无效",
			responses: []runResponse{{out: `{`}},
			services:  []string{"api"},
			wantSub:   "compose ps JSON 无效",
		},
		{
			name:      "没有运行容器",
			responses: []runResponse{{out: `{"ID":"api-1","Service":"api","State":"exited"}`}},
			services:  []string{"api"},
			wantSub:   "没有运行中的容器",
		},
		{
			name: "RepoDigests 为空",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `[]`},
			},
			services: []string{"api"},
			wantSub:  "RepoDigests 为空",
		},
		{
			name: "容器 inspect 失败",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{err: runnerErr},
			},
			services: []string{"api"},
			wantSub:  "运行镜像失败",
			wantErr:  runnerErr,
		},
		{
			name: "容器 inspect JSON 无效",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{`},
			},
			services: []string{"api"},
			wantSub:  "解析 target",
		},
		{
			name: "镜像 inspect 失败",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
				{err: runnerErr},
			},
			services: []string{"api"},
			wantSub:  "RepoDigests 失败",
			wantErr:  runnerErr,
		},
		{
			name: "镜像 inspect JSON 无效",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `{`},
			},
			services: []string{"api"},
			wantSub:  "解析 target",
		},
		{
			name: "仓库不匹配",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `["ghcr.io/other/app@sha256:111"]`},
			},
			services: []string{"api"},
			wantSub:  "无法对应",
		},
		{
			name: "多个仓库候选",
			responses: []runResponse{
				{out: `{"ID":"api-1","Service":"api","State":"running"}`},
				{out: `{"image_id":"sha256:api","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `["ghcr.io/acme/app@sha256:111","ghcr.io/acme/app@sha256:222"]`},
			},
			services: []string{"api"},
			wantSub:  "多个 RepoDigest",
		},
		{
			name: "同服务多个运行 digest",
			responses: []runResponse{
				{out: strings.Join([]string{
					`{"ID":"api-1","Service":"api","State":"running"}`,
					`{"ID":"api-2","Service":"api","State":"running"}`,
				}, "\n")},
				{out: `{"image_id":"sha256:api1","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `["ghcr.io/acme/app@sha256:111"]`},
				{out: `{"image_id":"sha256:api2","image_ref":"ghcr.io/acme/app:latest"}`},
				{out: `["ghcr.io/acme/app@sha256:222"]`},
			},
			services: []string{"api"},
			wantSub:  "多个 digest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{
				streamResponses: []streamResponse{testStreamResponse(`{"services":{"api":{}}}`)},
				runResponses:    append([]runResponse(nil), tc.responses...),
			}
			target := config.Target{Type: config.TargetImageDigest, Services: tc.services}
			_, err := Execute(context.Background(), testHost(), target, runner)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Execute 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Execute 未保留错误链: %v", err)
			}
		})
	}
}

func TestExecuteImageDigest_PreservesRunnerError(t *testing.T) {
	wantErr := errors.New("docker failed")
	runner := &fakeRunner{
		streamResponses: []streamResponse{testStreamResponse(`{"services":{"api":{}}}`)},
		runResponses:    []runResponse{{err: wantErr}},
	}
	target := config.Target{Type: config.TargetImageDigest, Services: []string{"api"}}

	_, err := Execute(context.Background(), testHost(), target, runner)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Execute 错误 = %v，期望保留原始错误", err)
	}
}

func TestExecuteImageDigest_ComposeMetadata命令失败保留错误链(t *testing.T) {
	wantErr := errors.New("SECRET_TOKEN=compose-config-failed")
	runner := &fakeRunner{streamResponses: []streamResponse{{err: wantErr}}}
	target := config.Target{Type: config.TargetImageDigest, Services: []string{"api"}}

	_, err := Execute(context.Background(), testHost(), target, runner)
	if !errors.Is(err, wantErr) || strings.Contains(err.Error(), "SECRET_TOKEN") {
		t.Fatalf("Execute 错误 = %v", err)
	}
}

func TestReadComposeMetadataCanonical_忽略Stderr警告(t *testing.T) {
	t.Setenv(composeMetadataHelperEnv, "1")
	payload, err := readComposeMetadataCanonical(context.Background(), sshexec.NewLocal(), []string{
		os.Args[0], "-test.run=^TestComposeMetadataHelper$",
	})
	if err != nil {
		t.Fatalf("readComposeMetadataCanonical 失败: %v", err)
	}
	metadata, err := parseComposeMetadata(payload)
	if err != nil || metadata == nil {
		t.Fatalf("parseComposeMetadata metadata=%#v err=%v payload=%q", metadata, err, payload)
	}
}

func TestParseComposeMetadata_忠实记录隔离暂不支持的端口模式(t *testing.T) {
	canonical := `{"services":{"api":{"ports":[{"target":8080,"protocol":"sctp"},{"target":8080,"protocol":"sctp"}]}}}`
	metadata, err := parseComposeMetadata([]byte(canonical))
	if err != nil {
		t.Fatalf("parseComposeMetadata 失败: %v", err)
	}
	if len(metadata.PublishedPorts) != 2 || metadata.PublishedPorts[0].Protocol != "sctp" {
		t.Fatalf("ComposeMetadata = %#v", metadata)
	}
}

func TestSelectRepoDigest_HandlesTagsPortsAndDockerHubAliases(t *testing.T) {
	tests := []struct {
		name       string
		imageRef   string
		digests    []string
		want       string
		wantErrSub string
	}{
		{
			name:     "registry 端口与 tag",
			imageRef: "registry.example:5000/team/app:v1",
			digests:  []string{"registry.example:5000/team/app@sha256:111"},
			want:     "registry.example:5000/team/app@sha256:111",
		},
		{
			name:     "Docker Hub 官方镜像别名",
			imageRef: "redis:7",
			digests:  []string{"docker.io/library/redis@sha256:222"},
			want:     "docker.io/library/redis@sha256:222",
		},
		{
			name:       "只有 image ID",
			imageRef:   "sha256:abc",
			digests:    []string{"repo/app@sha256:333"},
			wantErrSub: "不包含可确定的仓库",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectRepoDigest(tc.imageRef, tc.digests)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("selectRepoDigest 错误 = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("selectRepoDigest = %q, %v，期望 %q", got, err, tc.want)
			}
		})
	}
}
