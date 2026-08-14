package hub

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newAdminCmd(dependencies commandDependencies) *cobra.Command {
	command := &cobra.Command{
		Use:   "admin",
		Short: "初始化或重置本地管理员",
	}
	command.AddCommand(
		newAdminInitCmd(dependencies),
		newAdminResetPasswordCmd(dependencies),
	)
	return command
}

func newAdminInitCmd(dependencies commandDependencies) *cobra.Command {
	username := "admin"
	authFile := defaultAuthFile
	command := &cobra.Command{
		Use:   "init",
		Short: "排他创建本地管理员凭证",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if dependencies.readPassword == nil {
				return fmt.Errorf("初始化管理员失败: 内部依赖不完整")
			}
			password, err := readConfirmedPassword(command, dependencies.readPassword)
			if err != nil {
				return err
			}
			defer clearBytes(password)
			if err := initializeCredential(authFile, username, password); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "管理员 %q 已初始化，凭证写入 %s\n", username, authFile)
			return nil
		},
	}
	command.Flags().StringVar(&username, "username", "admin", "本地管理员用户名")
	command.Flags().StringVar(&authFile, "auth-file", defaultAuthFile, "管理员凭证文件路径")
	return command
}

func newAdminResetPasswordCmd(dependencies commandDependencies) *cobra.Command {
	authFile := defaultAuthFile
	command := &cobra.Command{
		Use:   "reset-password",
		Short: "原子更新本地管理员密码",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if dependencies.readPassword == nil {
				return fmt.Errorf("重置管理员密码失败: 内部依赖不完整")
			}
			password, err := readConfirmedPassword(command, dependencies.readPassword)
			if err != nil {
				return err
			}
			defer clearBytes(password)
			if err := resetCredentialPassword(authFile, password); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "管理员密码已重置，现有登录会话将在下次请求时失效\n")
			return nil
		},
	}
	command.Flags().StringVar(&authFile, "auth-file", defaultAuthFile, "管理员凭证文件路径")
	return command
}

func readConfirmedPassword(command *cobra.Command, reader func() ([]byte, error)) ([]byte, error) {
	fmt.Fprint(command.OutOrStdout(), "请输入管理员密码: ")
	password, err := reader()
	fmt.Fprintln(command.OutOrStdout())
	if err != nil {
		return nil, fmt.Errorf("读取管理员密码失败: %w", err)
	}
	if err := validatePassword(password); err != nil {
		clearBytes(password)
		return nil, err
	}

	fmt.Fprint(command.OutOrStdout(), "请再次输入管理员密码: ")
	confirmation, err := reader()
	fmt.Fprintln(command.OutOrStdout())
	if err != nil {
		clearBytes(password)
		return nil, fmt.Errorf("读取确认密码失败: %w", err)
	}
	defer clearBytes(confirmation)
	if !passwordMatches(password, confirmation) {
		clearBytes(password)
		return nil, fmt.Errorf("两次输入的管理员密码不一致")
	}
	return password, nil
}

func readTerminalPassword() ([]byte, error) {
	fileDescriptor := int(os.Stdin.Fd())
	if !term.IsTerminal(fileDescriptor) {
		return nil, fmt.Errorf("标准输入不是终端，密码必须在本机 TTY 中无回显输入")
	}
	return term.ReadPassword(fileDescriptor)
}
