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

// errBackupFailed 表示 backup 已经输出完整结果，但本轮存在失败。
// Execute 只转换退出码，不再次打印相同错误。
var errBackupFailed = errors.New("备份未完全成功")

// errRestoreFailed 表示 restore 已经输出结构化最终结果，但本轮恢复失败。
// Execute 只转换退出码，不再次打印可能包含底层命令细节的错误链。
var errRestoreFailed = errors.New("恢复未完成")

// errVerifyFailed 表示 verify 已经输出完整最终结果，但至少一台 host 演练失败。
// Execute 只转换退出码，不重复打印底层错误链。
var errVerifyFailed = errors.New("恢复演练未完成")

// Execute 运行根命令并返回进程退出码。
func Execute() int {
	err := newRootCmd().Execute()
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errChecksFailed):
		// 检查报告已经打印过，这里不再重复输出错误信息。
		return 2
	case errors.Is(err, errBackupFailed), errors.Is(err, errRestoreFailed), errors.Is(err, errVerifyFailed):
		// backup/restore/verify 的人类摘要或 JSON 已包含失败事实，这里只保留非零退出码。
		return 1
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
		newHostKeyCmd(&configPath),
		newBackupCmd(&configPath),
		newRestoreCmd(&configPath),
		newVerifyCmd(&configPath),
		newInstallCmd(&configPath),
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
	return newDoctorCmdWithChecks(configPath, doctor.RunLocal, doctor.RunHost, doctor.RunDNSMgr)
}

type runLocalFunc func(context.Context, *config.Config) *doctor.Report
type runHostFunc func(context.Context, *config.Config, *config.Host) *doctor.Report
type runDNSMgrFunc func(context.Context, *config.Config) *doctor.Report

// newDoctorCmdWithRunners 允许测试替换实际环境探测，同时保留完整的 CLI 编排行为。
func newDoctorCmdWithRunners(
	configPath *string,
	runLocal runLocalFunc,
	runHost runHostFunc,
) *cobra.Command {
	return newDoctorCmdWithChecks(configPath, runLocal, runHost, doctor.RunDNSMgr)
}

func newDoctorCmdWithChecks(
	configPath *string,
	runLocal runLocalFunc,
	runHost runHostFunc,
	runDNSMgr runDNSMgrFunc,
) *cobra.Command {
	var asJSON bool
	var hostName string
	var all bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "检查运行环境是否具备执行备份的条件",
		Args:  cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if hostName != "" && all {
				return errors.New("--host 与 --all 不能同时使用")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadAndValidate(*configPath)
			if err != nil {
				return err
			}

			report, err := runDoctorWithDNSMgr(cmd.Context(), cfg, hostName, all, runLocal, runHost, runDNSMgr)
			if err != nil {
				return err
			}

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
	cmd.Flags().StringVar(&hostName, "host", "", "只检查指定 host")
	cmd.Flags().BoolVar(&all, "all", false, "检查 hub 和清单中的全部 host")
	cmd.MarkFlagsMutuallyExclusive("host", "all")
	return cmd
}

func runDoctorWithDNSMgr(
	ctx context.Context,
	cfg *config.Config,
	hostName string,
	all bool,
	runLocal runLocalFunc,
	runHost runHostFunc,
	runDNSMgr runDNSMgrFunc,
) (*doctor.Report, error) {
	report, err := runDoctor(ctx, cfg, hostName, all, runLocal, runHost)
	if err != nil || hostName != "" || cfg == nil || cfg.DNSMgr == nil {
		return report, err
	}
	if runDNSMgr == nil {
		return nil, fmt.Errorf("执行 dnsmgr doctor 失败: 内部依赖不完整")
	}
	if next := runDNSMgr(ctx, cfg); next != nil {
		report.Checks = append(report.Checks, next.Checks...)
	}
	return report, nil
}

// runDoctor 选择检查范围并按调用顺序合并报告。
func runDoctor(
	ctx context.Context,
	cfg *config.Config,
	hostName string,
	all bool,
	runLocal runLocalFunc,
	runHost runHostFunc,
) (*doctor.Report, error) {
	report := &doctor.Report{}
	appendReport := func(next *doctor.Report) {
		if next != nil {
			report.Checks = append(report.Checks, next.Checks...)
		}
	}

	switch {
	case hostName != "":
		for i := range cfg.Hosts {
			if cfg.Hosts[i].Host == hostName {
				appendReport(runHost(ctx, cfg, &cfg.Hosts[i]))
				return report, nil
			}
		}
		return nil, fmt.Errorf("清单中不存在 host %q", hostName)
	case all:
		appendReport(runLocal(ctx, cfg))
		for i := range cfg.Hosts {
			appendReport(runHost(ctx, cfg, &cfg.Hosts[i]))
		}
	default:
		appendReport(runLocal(ctx, cfg))
	}
	return report, nil
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
