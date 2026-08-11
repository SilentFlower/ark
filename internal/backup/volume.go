package backup

import (
	"context"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

func executeVolume(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
) (*Result, error) {
	argv := []string{
		"docker", "run", "--rm",
		"-v", target.Name + ":/src:ro",
		"alpine", "tar", "-cpf", "-", "-C", "/src", ".",
	}
	return startStream(ctx, host, target, ".tar", runner, argv)
}
