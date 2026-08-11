package backup

import (
	"context"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

func TestExecuteFiles_PreservesPathsAsSeparateArguments(t *testing.T) {
	tests := []struct {
		name       string
		archive    string
		paths      []string
		wantSuffix []string
	}{
		{
			name:       "单个路径",
			archive:    "hosts",
			paths:      []string{"/etc/hosts"},
			wantSuffix: []string{"/etc/hosts"},
		},
		{
			name:       "空格引号和命令替换保持独立参数",
			archive:    "config",
			paths:      []string{"/srv/app/with space", "/etc/x'y", "/tmp/$(id)"},
			wantSuffix: []string{"/srv/app/with space", "/etc/x'y", "/tmp/$(id)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := config.Target{Type: config.TargetFiles, Name: tc.archive, Paths: tc.paths}
			runner := &fakeRunner{streamResponses: []streamResponse{testStreamResponse("tar")}}

			result, err := Execute(context.Background(), testHost(), target, runner)
			if err != nil {
				t.Fatalf("Execute 失败: %v", err)
			}
			wantArgv := []string{"tar", "-cpf", "-", "--"}
			wantArgv = append(wantArgv, tc.wantSuffix...)
			assertCalls(t, runner.calls, []runnerCall{{kind: "stream", argv: wantArgv}})
			if want := "web-01/files/" + tc.archive + ".tar"; result.StdinFilename != want {
				t.Errorf("StdinFilename = %q，期望 %q", result.StdinFilename, want)
			}
		})
	}
}
