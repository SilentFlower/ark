// Package cli 组装 ark 的命令行界面。
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/silentflower/ark/internal/config"
	"github.com/silentflower/ark/internal/doctor"
)

// 版本信息由构建时通过 -ldflags -X 注入，见 Makefile。
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// defaultConfigPath 是清单的默认位置。
// 放在 /etc 下而不是随二进制走，是因为清单描述的是「这套部署里有哪些机器」，
// 与二进制版本无关。它只存在于 hub 上。
const defaultConfigPath = "/etc/ark/ark.yaml"

// errChecksFailed 表示 doctor 发现了必须修复的问题。
// Execute 会把它转换成退出码 2，让 systemd 和监控脚本能区分
// 「工具本身出错」（退出码 1）和「检查未通过」（退出码 2）。
var errChecksFailed = errors.New("环境检查未通过")

// Execute 运行根命令并返回进程退出码。
func Execute() int {
	err := newRootCmd().Execute()
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errChecksFailed):
		// 检查报告已经打印过，这里不再重复输出错误信息。
		return 2
	default:
		fmt.Fprintln(os.Stderr, "错误:", err)
		return 1
	}
}

// newRootCmd 构建根命令及其子命令。
func newRootCmd() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:   "ark",
		Short: "Docker Compose 整机备份与重建",
		Long: "ark 把一台 docker compose 机器的数据库、数据卷、配置和镜像身份\n" +
			"打包成可在任意机器上重建的快照。",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVarP(&configPath, "config", "c", defaultConfigPath, "备份清单路径")

	root.AddCommand(
		newVersionCmd(),
		newValidateCmd(&configPath),
		newDoctorCmd(&configPath),
	)
	return root
}

// newVersionCmd 输出版本信息。
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "输出版本信息",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "ark %s (commit %s, built %s)\n", Version, Commit, Date)
		},
	}
}

// newValidateCmd 只做清单的静态校验，不访问 docker 也不访问网络。
// 因此在任何机器上都能跑，不需要能连上清单里的那些目标机。
func newValidateCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "校验备份清单的语法和语义",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadAndValidate(*configPath)
			if err != nil {
				return err
			}
			printManifestSummary(cmd, cfg)
			return nil
		},
	}
}

// printManifestSummary 逐台打印清单摘要。
//
// 不做列对齐：host 名和项目名长短不一，而「本机」这类中文的终端显示宽度
// 是字符数的两倍，按 %-10s 补空格反而会错位。
func printManifestSummary(cmd *cobra.Command, cfg *config.Config) {
	out := cmd.OutOrStdout()

	total := 0
	for _, h := range cfg.Hosts {
		total += len(h.Targets)
	}
	fmt.Fprintf(out, "清单校验通过: %s\n  %d 台机器 / %d 个备份目标\n\n",
		cfg.Path(), len(cfg.Hosts), total)

	for i := range cfg.Hosts {
		h := &cfg.Hosts[i]
		conn := "本机"
		if h.SSH != nil {
			conn = "ssh " + h.SSH.Address
		}
		fmt.Fprintf(out, "  %s（%s）项目 %s，%d 个目标，%s\n",
			h.Host, conn, h.Project.Name, len(h.Targets), cfg.ScheduleFor(h).OnCalendar)
	}
}

// newDoctorCmd 校验运行环境。
func newDoctorCmd(configPath *string) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "检查运行环境是否具备执行备份的条件",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadAndValidate(*configPath)
			if err != nil {
				return err
			}

			report := doctor.Run(context.Background(), cfg)

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
			} else {
				printReport(cmd, report)
			}

			if report.Failed() {
				return errChecksFailed
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出，便于被监控采集")
	return cmd
}

// printReport 以人类可读的形式打印检查报告。
func printReport(cmd *cobra.Command, report *doctor.Report) {
	out := cmd.OutOrStdout()
	for _, c := range report.Checks {
		// 检查项名带机器前缀（如 "web-01 / ssh.identity_file"），列宽相应留宽。
		fmt.Fprintf(out, "%s %-38s %s\n", statusSymbol(c.Status), c.Name, c.Detail)
	}
	ok, warn, fail := report.Counts()
	fmt.Fprintf(out, "\n通过 %d / 警告 %d / 失败 %d\n", ok, warn, fail)
}

// statusSymbol 返回状态对应的前缀符号。
func statusSymbol(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "✓"
	case doctor.StatusWarn:
		return "!"
	default:
		return "✗"
	}
}
