package hub

import (
	"bytes"
	"strings"
	"testing"
)

func TestAdminInitCommand_从注入读取器确认密码且不回显(t *testing.T) {
	authFile := testCredentialPath(t)
	passwordText := "command password secret"
	reads := 0
	command := newAdminInitCmd(commandDependencies{
		readPassword: func() ([]byte, error) {
			reads++
			return []byte(passwordText), nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--username", "root-admin", "--auth-file", authFile})
	if err := command.Execute(); err != nil {
		t.Fatalf("admin init 失败: %v", err)
	}
	if reads != 2 {
		t.Fatalf("密码读取次数 = %d", reads)
	}
	if strings.Contains(output.String(), passwordText) {
		t.Fatal("命令输出泄漏了明文密码")
	}
	value, err := loadCredential(authFile)
	if err != nil {
		t.Fatalf("读取初始化凭证失败: %v", err)
	}
	if value.Username != "root-admin" {
		t.Fatalf("用户名 = %q", value.Username)
	}
}

func TestAdminInitCommand_两次密码不一致不创建文件(t *testing.T) {
	authFile := testCredentialPath(t)
	passwords := [][]byte{[]byte("first password value"), []byte("second password value")}
	command := newAdminInitCmd(commandDependencies{
		readPassword: func() ([]byte, error) {
			value := passwords[0]
			passwords = passwords[1:]
			return value, nil
		},
	})
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetArgs([]string{"--auth-file", authFile})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("密码不一致错误 = %v", err)
	}
	if _, err := loadCredential(authFile); err == nil {
		t.Fatal("密码不一致后不应创建凭证")
	}
}

func TestAdminResetPasswordCommand_递增Revision且不回显(t *testing.T) {
	authFile := testCredentialPath(t)
	if err := initializeCredential(authFile, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}
	passwordText := "reset command password value"
	reads := 0
	command := newAdminResetPasswordCmd(commandDependencies{
		readPassword: func() ([]byte, error) {
			reads++
			return []byte(passwordText), nil
		},
	})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--auth-file", authFile})
	if err := command.Execute(); err != nil {
		t.Fatalf("admin reset-password 失败: %v", err)
	}
	if reads != 2 || strings.Contains(output.String(), passwordText) {
		t.Fatalf("密码读取次数=%d output=%q", reads, output.String())
	}
	value, err := loadCredential(authFile)
	if err != nil {
		t.Fatalf("读取重置凭证失败: %v", err)
	}
	valid, err := verifyPassword([]byte(passwordText), value.PasswordHash)
	if err != nil || !valid || value.Revision != 2 {
		t.Fatalf("重置结果 revision=%d valid=%v err=%v", value.Revision, valid, err)
	}
}
