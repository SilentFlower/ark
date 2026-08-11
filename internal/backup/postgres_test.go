package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

func TestExecutePostgres_BuildsExactArgvAndMetadata(t *testing.T) {
	tests := []struct {
		name   string
		target config.Target
		want   []string
	}{
		{
			name: "包含用户",
			target: config.Target{
				Type: config.TargetPostgres, Service: "postgres", Database: "app db", User: "backup'user",
			},
			want: []string{
				"docker", "compose", "-f", "/srv/app/compose.yaml",
				"-p", "production", "--env-file", "/srv/app/.env",
				"exec", "-T", "postgres", "pg_dump", "-U", "backup'user", "-d", "app db",
				"--no-owner", "--no-acl", "--clean", "--if-exists",
			},
		},
		{
			name: "省略用户",
			target: config.Target{
				Type: config.TargetPostgres, Service: "postgres", Database: "app",
			},
			want: []string{
				"docker", "compose", "-f", "/srv/app/compose.yaml",
				"-p", "production", "--env-file", "/srv/app/.env",
				"exec", "-T", "postgres", "pg_dump", "-d", "app",
				"--no-owner", "--no-acl", "--clean", "--if-exists",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{streamResponses: []streamResponse{testStreamResponse("sql")}}
			result, err := Execute(context.Background(), testHost(), tc.target, runner)
			if err != nil {
				t.Fatalf("Execute 失败: %v", err)
			}
			assertCalls(t, runner.calls, []runnerCall{{kind: "stream", argv: tc.want}})
			if result.Host != "web-01" || result.TargetID != tc.target.ID() || result.TargetType != config.TargetPostgres {
				t.Fatalf("结果元数据 = %#v", result)
			}
			if want := "web-01/" + tc.target.ID() + ".sql"; result.StdinFilename != want {
				t.Errorf("StdinFilename = %q，期望 %q", result.StdinFilename, want)
			}
			data, err := io.ReadAll(result.Reader)
			if err != nil || string(data) != "sql" {
				t.Fatalf("读取数据 = %q, %v", data, err)
			}
			if err := result.Wait(); err != nil {
				t.Fatalf("Wait 失败: %v", err)
			}
		})
	}
}

func TestStreamResult_WaitAndCloseOnlyRunOnce(t *testing.T) {
	waitErr := errors.New("wait failed")
	closeErr := errors.New("close failed")
	waitCalls := 0
	reader := &trackedReadCloser{reader: strings.NewReader("partial"), closeErr: closeErr}
	runner := &fakeRunner{streamResponses: []streamResponse{{
		reader: reader,
		wait: func() error {
			waitCalls++
			return waitErr
		},
	}}}
	target := config.Target{Type: config.TargetPostgres, Service: "postgres", Database: "app"}

	result, err := Execute(context.Background(), testHost(), target, runner)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := result.Wait(); !errors.Is(err, waitErr) {
			t.Errorf("第 %d 次 Wait 错误 = %v", i+1, err)
		}
		if err := result.Reader.Close(); !errors.Is(err, closeErr) {
			t.Errorf("第 %d 次 Close 错误 = %v", i+1, err)
		}
	}
	if waitCalls != 1 || reader.closeCalls != 1 {
		t.Fatalf("Wait 调用 %d 次，Close 调用 %d 次，期望均为 1", waitCalls, reader.closeCalls)
	}
}

func TestExecutePostgres_PreservesStreamError(t *testing.T) {
	wantErr := errors.New("stream failed")
	runner := &fakeRunner{streamResponses: []streamResponse{{err: wantErr}}}
	target := config.Target{Type: config.TargetPostgres, Service: "postgres", Database: "app"}

	_, err := Execute(context.Background(), testHost(), target, runner)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), target.ID()) {
		t.Fatalf("Execute 错误 = %v", err)
	}
}

func TestExecutePostgres_RejectsAndCleansIncompleteStream(t *testing.T) {
	waitErr := errors.New("wait failed")
	closeErr := errors.New("close failed")
	waitCalls := 0
	reader := &trackedReadCloser{reader: strings.NewReader(""), closeErr: closeErr}
	tests := []struct {
		name     string
		response streamResponse
		wantErr  error
	}{
		{
			name: "Reader 为空",
			response: streamResponse{wait: func() error {
				waitCalls++
				return waitErr
			}},
			wantErr: waitErr,
		},
		{
			name:     "Wait 为空",
			response: streamResponse{reader: reader},
			wantErr:  closeErr,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{streamResponses: []streamResponse{tc.response}}
			target := config.Target{Type: config.TargetPostgres, Service: "postgres", Database: "app"}
			_, err := Execute(context.Background(), testHost(), target, runner)
			if err == nil || !strings.Contains(err.Error(), "不完整") || !errors.Is(err, tc.wantErr) {
				t.Fatalf("Execute 错误 = %v", err)
			}
		})
	}
	if waitCalls != 1 || reader.closeCalls != 1 {
		t.Fatalf("半初始化清理 Wait=%d, Close=%d，期望均为 1", waitCalls, reader.closeCalls)
	}
}
