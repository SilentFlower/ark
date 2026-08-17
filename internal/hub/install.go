package hub

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/store"
	arksystemd "github.com/silentflower/ark/internal/systemd"
)

type installServiceFunc func(context.Context, arksystemd.HubInstallOptions) (arksystemd.InstallResult, error)

func installHubService(ctx context.Context, options arksystemd.HubInstallOptions) (arksystemd.InstallResult, error) {
	return arksystemd.InstallHub(ctx, options)
}

func newInstallCmd(dependencies commandDependencies) *cobra.Command {
	unitDir := arksystemd.DefaultUnitDir
	listenAddress := defaultListenAddress
	stateDBPath := store.DefaultPath
	authFile := defaultAuthFile
	configPath := defaultConfigPath
	arkBinaryPath := defaultArkBinaryPath
	secureCookie := false
	command := &cobra.Command{
		Use:   "install",
		Short: "生成并安装独立的 ark-hub service",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if dependencies.executable == nil || dependencies.installService == nil {
				return fmt.Errorf("安装 ark-hub service 失败: 内部依赖不完整")
			}
			binaryPath, err := dependencies.executable()
			if err != nil {
				return fmt.Errorf("定位 ark-hub 二进制失败: %w", err)
			}
			result, err := dependencies.installService(command.Context(), arksystemd.HubInstallOptions{
				UnitDir:       unitDir,
				BinaryPath:    binaryPath,
				ListenAddress: listenAddress,
				StateDBPath:   stateDBPath,
				AuthFile:      authFile,
				ConfigPath:    configPath,
				ArkBinaryPath: arkBinaryPath,
				SecureCookie:  secureCookie,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "已安装 %d 个 systemd unit\n", len(result.Written))
			return nil
		},
	}
	command.Flags().StringVar(&unitDir, "unit-dir", arksystemd.DefaultUnitDir, "systemd unit 安装目录")
	command.Flags().StringVar(&listenAddress, "listen", defaultListenAddress, "HTTP 监听地址")
	command.Flags().StringVar(&stateDBPath, "state-db", store.DefaultPath, "ark 状态库路径")
	command.Flags().StringVar(&authFile, "auth-file", defaultAuthFile, "管理员凭证文件路径")
	command.Flags().StringVar(&configPath, "config", defaultConfigPath, "ark v2 清单绝对路径")
	command.Flags().StringVar(&arkBinaryPath, "ark-binary", defaultArkBinaryPath, "ark 可执行文件绝对路径")
	command.Flags().BoolVar(&secureCookie, "secure-cookie", false, "为浏览器 Cookie 设置 Secure")
	return command
}
