package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/config"
	arksystemd "github.com/silentflower/ark/internal/systemd"
)

type installDependencies struct {
	loadConfig func(string) (*config.Config, error)
	executable func() (string, error)
	install    func(context.Context, *config.Config, arksystemd.InstallOptions) (arksystemd.InstallResult, error)
}

// newInstallCmd 构建 systemd unit 安装命令。
func newInstallCmd(configPath *string) *cobra.Command {
	return newInstallCmdWithDependencies(configPath, installDependencies{
		loadConfig: config.LoadAndValidate,
		executable: os.Executable,
		install:    arksystemd.Install,
	})
}

func newInstallCmdWithDependencies(configPath *string, dependencies installDependencies) *cobra.Command {
	unitDir := arksystemd.DefaultUnitDir
	asJSON := false
	cmd := &cobra.Command{
		Use:   "install",
		Short: "生成并安装 systemd 备份与恢复演练任务",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dependencies.loadConfig == nil || dependencies.executable == nil || dependencies.install == nil {
				return fmt.Errorf("执行 install 失败: 内部依赖不完整")
			}
			cfg, err := dependencies.loadConfig(*configPath)
			if err != nil {
				return err
			}
			binaryPath, err := dependencies.executable()
			if err != nil {
				return fmt.Errorf("定位 ark 二进制失败: %w", err)
			}
			result, err := dependencies.install(cmd.Context(), cfg, arksystemd.InstallOptions{
				UnitDir:    unitDir,
				BinaryPath: binaryPath,
				ConfigPath: *configPath,
			})
			if err != nil {
				return err
			}
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已安装 %d 个 systemd unit", len(result.Written))
			if len(result.Removed) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "，清理 %d 个陈旧 timer", len(result.Removed))
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&unitDir, "unit-dir", arksystemd.DefaultUnitDir, "systemd unit 安装目录")
	cmd.Flags().BoolVar(&asJSON, "json", false, "以纯 JSON 输出安装结果")
	return cmd
}
