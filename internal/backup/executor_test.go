package backup

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

type runnerCall struct {
	kind string
	argv []string
}

type runResponse struct {
	out string
	err error
}

type streamResponse struct {
	reader io.ReadCloser
	wait   func() error
	err    error
}

type fakeRunner struct {
	calls           []runnerCall
	runResponses    []runResponse
	streamResponses []streamResponse
}

func (r *fakeRunner) Run(_ context.Context, argv ...string) (string, error) {
	r.calls = append(r.calls, runnerCall{kind: "run", argv: append([]string(nil), argv...)})
	if len(r.runResponses) == 0 {
		return "", errors.New("未配置 Run 响应")
	}
	response := r.runResponses[0]
	r.runResponses = r.runResponses[1:]
	return response.out, response.err
}

func (r *fakeRunner) Stream(_ context.Context, argv ...string) (io.ReadCloser, func() error, error) {
	r.calls = append(r.calls, runnerCall{kind: "stream", argv: append([]string(nil), argv...)})
	if len(r.streamResponses) == 0 {
		return nil, nil, errors.New("未配置 Stream 响应")
	}
	response := r.streamResponses[0]
	r.streamResponses = r.streamResponses[1:]
	return response.reader, response.wait, response.err
}

func (r *fakeRunner) Feed(_ context.Context, _ io.Reader, argv ...string) error {
	r.calls = append(r.calls, runnerCall{kind: "feed", argv: append([]string(nil), argv...)})
	return errors.New("测试不应调用 Feed")
}

type trackedReadCloser struct {
	reader     io.Reader
	closeCalls int
	closeErr   error
}

func (r *trackedReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *trackedReadCloser) Close() error {
	r.closeCalls++
	return r.closeErr
}

func testHost() config.Host {
	return config.Host{
		Host: "web-01",
		Project: config.Project{
			ComposeFile: "/srv/app/compose.yaml",
			ProjectName: "production",
			EnvFile:     "/srv/app/.env",
		},
	}
}

func testStreamResponse(payload string) streamResponse {
	return streamResponse{
		reader: io.NopCloser(strings.NewReader(payload)),
		wait:   func() error { return nil },
	}
}

func assertCalls(t *testing.T, got, want []runnerCall) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Runner 调用 = %#v，期望 %#v", got, want)
	}
}

func TestExecute_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		host    config.Host
		target  config.Target
		runner  *fakeRunner
		wantSub string
	}{
		{
			name:    "context 为空",
			host:    testHost(),
			target:  config.Target{Type: config.TargetFiles, Name: "etc", Paths: []string{"/etc/hosts"}},
			runner:  &fakeRunner{},
			wantSub: "context",
		},
		{
			name:    "runner 为空",
			ctx:     context.Background(),
			host:    testHost(),
			target:  config.Target{Type: config.TargetFiles, Name: "etc", Paths: []string{"/etc/hosts"}},
			wantSub: "runner",
		},
		{
			name:    "host 为空",
			ctx:     context.Background(),
			host:    config.Host{Project: config.Project{ComposeFile: "/compose.yaml"}},
			target:  config.Target{Type: config.TargetFiles, Name: "etc", Paths: []string{"/etc/hosts"}},
			runner:  &fakeRunner{},
			wantSub: "host",
		},
		{
			name:    "compose file 为空",
			ctx:     context.Background(),
			host:    config.Host{Host: "web-01"},
			target:  config.Target{Type: config.TargetFiles, Name: "etc", Paths: []string{"/etc/hosts"}},
			runner:  &fakeRunner{},
			wantSub: "compose_file",
		},
		{
			name:    "未知类型",
			ctx:     context.Background(),
			host:    testHost(),
			target:  config.Target{Type: config.TargetType("unknown")},
			runner:  &fakeRunner{},
			wantSub: "不支持类型",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var runner = tc.runner
			var runnerArg sshexec.Runner
			if runner != nil {
				runnerArg = runner
			}
			_, err := Execute(tc.ctx, tc.host, tc.target, runnerArg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Execute 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
		})
	}
}
