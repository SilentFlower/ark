package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/hostkey"
)

type hostKeyDependencies struct {
	loadConfig func(string) (*config.Config, error)
	refresh    func(context.Context, string, string, bool) (hostkey.Result, error)
}

// newHostKeyCmd 构建 SSH 主机密钥管理命令组。
func newHostKeyCmd(configPath *string) *cobra.Command {
	return newHostKeyCmdWithDependencies(configPath, hostKeyDependencies{
		loadConfig: config.LoadAndValidate,
		refresh:    hostkey.Refresh,
	})
}

func newHostKeyCmdWithDependencies(configPath *string, dependencies hostKeyDependencies) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host-key",
		Short: "查看和更新 SSH 主机密钥信任记录",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newHostKeyRefreshCmd(configPath, dependencies))
	return cmd
}

func newHostKeyRefreshCmd(configPath *string, dependencies hostKeyDependencies) *cobra.Command {
	var hostName string
	var apply bool
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "预览或显式更新一台远程主机的 SSH 密钥",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if hostName == "" {
				return fmt.Errorf("--host 不能为空")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dependencies.loadConfig == nil || dependencies.refresh == nil {
				return fmt.Errorf("刷新主机密钥失败: 内部依赖不完整")
			}
			cfg, err := dependencies.loadConfig(*configPath)
			if err != nil {
				return err
			}
			host, err := remoteHostByName(cfg, hostName)
			if err != nil {
				return err
			}
			result, err := dependencies.refresh(
				cmd.Context(), host.SSH.Address, host.SSH.KnownHostsFile, apply,
			)
			if err != nil {
				return fmt.Errorf("刷新 host %q 的主机密钥失败: %w", hostName, err)
			}
			if asJSON {
				return printHostKeyJSON(cmd, hostName, result)
			}
			printHostKeyResult(cmd, hostName, result)
			return nil
		},
	}
	cmd.Flags().StringVar(&hostName, "host", "", "清单中的远程 host 名称")
	cmd.Flags().BoolVar(&apply, "apply", false, "确认已带外核对指纹并原子更新 known_hosts")
	cmd.Flags().BoolVar(&asJSON, "json", false, "以纯 JSON 输出扫描和应用结果")
	return cmd
}

func remoteHostByName(cfg *config.Config, hostName string) (*config.Host, error) {
	if cfg == nil {
		return nil, fmt.Errorf("清单为空")
	}
	for i := range cfg.Hosts {
		host := &cfg.Hosts[i]
		if host.Host != hostName {
			continue
		}
		if host.Local || host.SSH == nil {
			return nil, fmt.Errorf("host %q 是本机，不使用 SSH 主机密钥", hostName)
		}
		return host, nil
	}
	return nil, fmt.Errorf("清单中不存在 host %q", hostName)
}

type hostKeyJSONResult struct {
	Host           string                `json:"host"`
	Address        string                `json:"address"`
	KnownHostsFile string                `json:"known_hosts_file"`
	Existing       []hostkey.Fingerprint `json:"existing"`
	Scanned        []hostkey.Fingerprint `json:"scanned"`
	Applied        bool                  `json:"applied"`
}

func printHostKeyJSON(cmd *cobra.Command, hostName string, result hostkey.Result) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(hostKeyJSONResult{
		Host:           hostName,
		Address:        result.Address,
		KnownHostsFile: result.KnownHostsFile,
		Existing:       result.Existing,
		Scanned:        result.Scanned,
		Applied:        result.Applied,
	})
}

func printHostKeyResult(cmd *cobra.Command, hostName string, result hostkey.Result) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "主机: %s\n地址: %s\nknown_hosts: %s\n", hostName, result.Address, result.KnownHostsFile)
	printFingerprints(out, "已记录指纹", result.Existing)
	printFingerprints(out, "扫描到的指纹", result.Scanned)
	if result.Applied {
		fmt.Fprintln(out, "\n已更新 known_hosts。后续 SSH 连接仍会拒绝未经确认的再次变更。")
		return
	}
	fmt.Fprintln(out, "\n尚未修改 known_hosts。请通过云控制台或服务器本地核对指纹后，重新运行并增加 --apply。")
}

func printFingerprints(out io.Writer, title string, values []hostkey.Fingerprint) {
	fmt.Fprintf(out, "\n%s:\n", title)
	if len(values) == 0 {
		fmt.Fprintln(out, "  （无）")
		return
	}
	for _, value := range values {
		fmt.Fprintf(out, "  - %s %s\n", value.Algorithm, value.SHA256)
	}
}
