package backup

import (
	"context"
	"testing"

	"github.com/silentflower/ark/internal/config"
)

func TestExecuteVolume_UsesReadonlyPermissionPreservingTar(t *testing.T) {
	tests := []struct {
		name       string
		volumeName string
	}{
		{name: "普通名称", volumeName: "uploads"},
		{name: "shell 元字符保持单个参数", volumeName: "uploads;touch /tmp/no"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := config.Target{Type: config.TargetVolume, Name: tc.volumeName}
			runner := &fakeRunner{streamResponses: []streamResponse{testStreamResponse("tar")}}

			result, err := Execute(context.Background(), testHost(), target, runner)
			if err != nil {
				t.Fatalf("Execute 失败: %v", err)
			}
			assertCalls(t, runner.calls, []runnerCall{{kind: "stream", argv: []string{
				"docker", "run", "--rm", "-v", tc.volumeName + ":/src:ro",
				"alpine", "tar", "-cpf", "-", "-C", "/src", ".",
			}}})
			if want := "web-01/volume/" + tc.volumeName + ".tar"; result.StdinFilename != want {
				t.Errorf("StdinFilename = %q，期望 %q", result.StdinFilename, want)
			}
		})
	}
}
