package backup

import (
	"context"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

func executeFiles(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
) (*Result, error) {
	argv := []string{"tar", "-cpf", "-", "--"}
	argv = append(argv, target.Paths...)
	return startStream(ctx, host, target, ".tar", runner, argv)
}
