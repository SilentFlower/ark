package backup

import (
	"context"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/sshexec"
)

func executePostgres(
	ctx context.Context,
	host config.Host,
	target config.Target,
	runner sshexec.Runner,
) (*Result, error) {
	argv := append(composeArgv(host.Project), "exec", "-T", target.Service, "pg_dump")
	if target.User != "" {
		argv = append(argv, "-U", target.User)
	}
	argv = append(argv,
		"-d", target.Database,
		"--no-owner",
		"--no-acl",
		"--clean",
		"--if-exists",
	)
	return startStream(ctx, host, target, ".sql", runner, argv)
}
