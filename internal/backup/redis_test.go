package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/silentflower/ark/internal/config"
)

func TestExecuteRedis_WaitsForLastSaveChange(t *testing.T) {
	runner := &fakeRunner{
		runResponses: []runResponse{{
			out: "AUTH failed: ERR AUTH called without any password configured\nBackground saving started\n",
		}},
		streamResponses: []streamResponse{
			testStreamResponse("100\n"),
			testStreamResponse("100\n"),
			testStreamResponse("101\n"),
			testStreamResponse("rdb"),
		},
	}
	target := config.Target{Type: config.TargetRedis, Service: "redis;id"}

	result, err := executeRedis(context.Background(), testHost(), target, runner, time.Millisecond)
	if err != nil {
		t.Fatalf("executeRedis 失败: %v", err)
	}
	compose := []string{
		"docker", "compose", "-f", "/srv/app/compose.yaml",
		"-p", "production", "--env-file", "/srv/app/.env",
	}
	lastSave := append(append([]string(nil), compose...),
		"exec", "-T", "redis;id", "redis-cli", "LASTSAVE")
	bgsave := append(append([]string(nil), compose...),
		"exec", "-T", "redis;id", "redis-cli", "BGSAVE")
	stream := append(append([]string(nil), compose...),
		"exec", "-T", "redis;id", "cat", "/data/dump.rdb")
	assertCalls(t, runner.calls, []runnerCall{
		{kind: "stream", argv: lastSave},
		{kind: "run", argv: bgsave},
		{kind: "stream", argv: lastSave},
		{kind: "stream", argv: lastSave},
		{kind: "stream", argv: stream},
	})
	if result.StdinFilename != "web-01/redis/redis;id.rdb" {
		t.Errorf("StdinFilename = %q", result.StdinFilename)
	}
	data, err := io.ReadAll(result.Reader)
	if err != nil || string(data) != "rdb" {
		t.Fatalf("读取 RDB = %q, %v", data, err)
	}
}

func TestExecuteRedis_PreservesStageErrors(t *testing.T) {
	wantErr := errors.New("stage failed")
	tests := []struct {
		name            string
		runResponses    []runResponse
		streamResponses []streamResponse
		wantSub         string
	}{
		{
			name: "基线失败",
			streamResponses: []streamResponse{{
				err: wantErr,
			}},
			wantSub: "LASTSAVE",
		},
		{
			name: "基线格式错误",
			streamResponses: []streamResponse{
				testStreamResponse("not-a-time"),
			},
			wantSub: "非负时间戳",
		},
		{
			name:         "触发失败",
			runResponses: []runResponse{{err: wantErr}},
			streamResponses: []streamResponse{
				testStreamResponse("100"),
			},
			wantSub: "BGSAVE",
		},
		{
			name:         "轮询失败",
			runResponses: []runResponse{{out: "ok"}},
			streamResponses: []streamResponse{
				testStreamResponse("100"),
				{err: wantErr},
			},
			wantSub: "LASTSAVE",
		},
		{
			name:         "流启动失败",
			runResponses: []runResponse{{out: "ok"}},
			streamResponses: []streamResponse{
				testStreamResponse("100"),
				testStreamResponse("101"),
				{err: wantErr},
			},
			wantSub: "数据流",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{
				runResponses:    tc.runResponses,
				streamResponses: tc.streamResponses,
			}
			target := config.Target{Type: config.TargetRedis, Service: "redis"}
			_, err := executeRedis(context.Background(), testHost(), target, runner, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("executeRedis 错误 = %v，期望包含 %q", err, tc.wantSub)
			}
			if tc.name != "基线格式错误" && !errors.Is(err, wantErr) {
				t.Fatalf("executeRedis 未保留错误链: %v", err)
			}
		})
	}
}

func TestExecuteRedis_ContextCancellationStopsPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{
		runResponses: []runResponse{{out: "ok"}},
		streamResponses: []streamResponse{
			testStreamResponse("100"),
			testStreamResponse("100"),
		},
	}
	target := config.Target{Type: config.TargetRedis, Service: "redis"}
	cancel()

	_, err := executeRedis(ctx, testHost(), target, runner, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeRedis 错误 = %v，期望 context.Canceled", err)
	}
}
