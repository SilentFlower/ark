// Package hub 提供 ark-hub 的命令、密码鉴权与 HTTP 服务边界。
//
// 本包只持有 hub 本机的凭证文件和内存会话，并通过 store.Store 打开现有状态库；
// 它不承载备份调度，也不直接访问 SQLite 或执行备份、恢复与演练业务。
package hub

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

const (
	defaultListenAddress = "127.0.0.1:8080"
	defaultAuthFile      = "/var/lib/ark-hub/auth.json"
)

type commandDependencies struct {
	run            func(context.Context, ServeOptions) error
	readPassword   func() ([]byte, error)
	executable     func() (string, error)
	installService installServiceFunc
}

// Execute 运行 ark-hub 根命令并返回进程退出码。
// @return int 0 表示成功，1 表示命令或运行时错误。
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := newRootCmd(defaultCommandDependencies()).ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, "错误:", err)
	return 1
}

func defaultCommandDependencies() commandDependencies {
	return commandDependencies{
		run:            Run,
		readPassword:   readTerminalPassword,
		executable:     os.Executable,
		installService: installHubService,
	}
}

func newRootCmd(dependencies commandDependencies) *cobra.Command {
	root := &cobra.Command{
		Use:           "ark-hub",
		Short:         "ark 的本地管理服务",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newServeCmd(dependencies),
		newAdminCmd(dependencies),
		newInstallCmd(dependencies),
	)
	return root
}
