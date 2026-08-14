package hub

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var testPassword = []byte("correct horse battery staple")

func TestPasswordHash_验证正确密码并拒绝损坏参数(t *testing.T) {
	encoded, err := hashPassword(testPassword)
	if err != nil {
		t.Fatalf("hashPassword 失败: %v", err)
	}
	valid, err := verifyPassword(testPassword, encoded)
	if err != nil || !valid {
		t.Fatalf("正确密码校验结果 valid=%v err=%v", valid, err)
	}
	valid, err = verifyPassword([]byte("wrong password value"), encoded)
	if err != nil || valid {
		t.Fatalf("错误密码校验结果 valid=%v err=%v", valid, err)
	}
	expensive := strings.Replace(encoded, "m=19456", "m=1048576", 1)
	if _, err := verifyPassword(testPassword, expensive); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("越界 memory 错误 = %v", err)
	}
}

func TestPasswordValidation_保留空格且限制字节长度(t *testing.T) {
	if err := validatePassword([]byte("  有效密码 123456  ")); err != nil {
		t.Fatalf("含空格 Unicode 密码被拒绝: %v", err)
	}
	if err := validatePassword(bytes.Repeat([]byte{'a'}, passwordMinBytes-1)); err == nil {
		t.Fatal("过短密码应被拒绝")
	}
	if err := validatePassword(bytes.Repeat([]byte{'a'}, passwordMaxBytes+1)); err == nil {
		t.Fatal("过长密码应被拒绝")
	}
}

func TestCredential_初始化重置并递增Revision(t *testing.T) {
	path := testCredentialPath(t)
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("initializeCredential 失败: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("读取凭证元数据失败: %v", err)
	}
	if info.Mode().Perm() != credentialFileMode {
		t.Fatalf("凭证权限 = %04o", info.Mode().Perm())
	}
	initial, err := loadCredential(path)
	if err != nil {
		t.Fatalf("loadCredential 失败: %v", err)
	}
	if initial.Username != "admin" || initial.Revision != 1 {
		t.Fatalf("初始凭证 = %#v", initial)
	}
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err == nil {
		t.Fatal("重复初始化应失败")
	}

	newPassword := []byte("another correct battery staple")
	if err := resetCredentialPassword(path, append([]byte(nil), newPassword...)); err != nil {
		t.Fatalf("resetCredentialPassword 失败: %v", err)
	}
	updated, err := loadCredential(path)
	if err != nil {
		t.Fatalf("读取重置凭证失败: %v", err)
	}
	if updated.Revision != 2 || updated.Username != initial.Username {
		t.Fatalf("重置凭证 = %#v", updated)
	}
	oldValid, err := verifyPassword(testPassword, updated.PasswordHash)
	if err != nil || oldValid {
		t.Fatalf("旧密码仍有效: valid=%v err=%v", oldValid, err)
	}
	newValid, err := verifyPassword(newPassword, updated.PasswordHash)
	if err != nil || !newValid {
		t.Fatalf("新密码无效: valid=%v err=%v", newValid, err)
	}
}

func TestCredential_拒绝未知字段权限过宽与符号链接(t *testing.T) {
	path := testCredentialPath(t)
	directory := filepath.Dir(path)
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取凭证失败: %v", err)
	}
	unknown := bytes.Replace(data, []byte("\n}"), []byte(",\n  \"totp\": true\n}"), 1)
	if err := os.WriteFile(path, unknown, credentialFileMode); err != nil {
		t.Fatalf("写入未知字段失败: %v", err)
	}
	if _, err := loadCredential(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("未知字段错误 = %v", err)
	}
	unknownSchema := bytes.Replace(data, []byte(`"schema_version": 1`), []byte(`"schema_version": 2`), 1)
	if err := os.WriteFile(path, unknownSchema, credentialFileMode); err != nil {
		t.Fatalf("写入未知 schema 失败: %v", err)
	}
	if _, err := loadCredential(path); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("未知 schema 错误 = %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, credentialMaxBytes+1), credentialFileMode); err != nil {
		t.Fatalf("写入超大凭证失败: %v", err)
	}
	if _, err := loadCredential(path); err == nil || !strings.Contains(err.Error(), "大小超过") {
		t.Fatalf("超大凭证错误 = %v", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatalf("恢复凭证失败: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("修改凭证权限失败: %v", err)
	}
	if _, err := loadCredential(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("权限过宽错误 = %v", err)
	}

	link := filepath.Join(directory, "auth-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	if _, err := loadCredential(link); err == nil || !strings.Contains(err.Error(), "不是普通文件") {
		t.Fatalf("符号链接错误 = %v", err)
	}
}

func TestCredential_未初始化提示本机命令(t *testing.T) {
	path := testCredentialPath(t)
	_, err := loadCredential(path)
	if err == nil || !strings.Contains(err.Error(), "ark-hub admin init") {
		t.Fatalf("未初始化错误 = %v", err)
	}
}

func TestCredential_拒绝非当前进程所有者(t *testing.T) {
	path := testCredentialPath(t)
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("读取凭证元数据失败: %v", err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid++
	if err := validateCredentialOwner(fileInfoWithStat{FileInfo: info, stat: &stat}, "凭证文件", path); err == nil ||
		!strings.Contains(err.Error(), "所有者 UID") {
		t.Fatalf("所有者错误 = %v", err)
	}

	if os.Geteuid() != 0 {
		return
	}
	if err := os.Chown(path, 65534, -1); err != nil {
		t.Fatalf("修改凭证所有者失败: %v", err)
	}
	if _, err := loadCredential(path); err == nil || !strings.Contains(err.Error(), "所有者 UID") {
		t.Fatalf("非 root 凭证文件错误 = %v", err)
	}
}

func TestCredential_并发重置不会丢失Revision(t *testing.T) {
	path := testCredentialPath(t)
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}

	const resetCount = 6
	start := make(chan struct{})
	errorsChannel := make(chan error, resetCount)
	var waitGroup sync.WaitGroup
	for index := 0; index < resetCount; index++ {
		password := []byte(fmt.Sprintf("concurrent password value %02d", index))
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			errorsChannel <- resetCredentialPassword(path, password)
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("并发重置失败: %v", err)
		}
	}

	value, err := loadCredential(path)
	if err != nil {
		t.Fatalf("读取并发重置结果失败: %v", err)
	}
	if value.Revision != 1+resetCount {
		t.Fatalf("并发重置 revision=%d，期望 %d", value.Revision, 1+resetCount)
	}
}

func TestCredential_初始化同步和清理失败均返回(t *testing.T) {
	path := testCredentialPath(t)
	syncFailure := errors.New("sync failure")
	removeFailure := errors.New("remove failure")
	dependencies := defaultCredentialDependencies()
	dependencies.syncDirectory = func(string) error { return syncFailure }
	dependencies.remove = func(string) error { return removeFailure }

	err := initializeCredentialWithDependencies(
		path,
		"admin",
		append([]byte(nil), testPassword...),
		dependencies,
	)
	if !errors.Is(err, syncFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("聚合错误 = %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("注入的清理失败后凭证文件应仍存在: %v", statErr)
	}
}

func TestCredential_重置Rename失败保留旧凭证(t *testing.T) {
	path := testCredentialPath(t)
	if err := initializeCredential(path, "admin", append([]byte(nil), testPassword...)); err != nil {
		t.Fatalf("初始化凭证失败: %v", err)
	}
	renameFailure := errors.New("rename failure")
	dependencies := defaultCredentialDependencies()
	dependencies.rename = func(string, string) error { return renameFailure }
	if err := resetCredentialPasswordWithDependencies(
		path,
		[]byte("replacement password value"),
		dependencies,
	); !errors.Is(err, renameFailure) {
		t.Fatalf("rename 错误 = %v", err)
	}

	value, err := loadCredential(path)
	if err != nil {
		t.Fatalf("读取旧凭证失败: %v", err)
	}
	valid, err := verifyPassword(testPassword, value.PasswordHash)
	if err != nil || !valid || value.Revision != 1 {
		t.Fatalf("旧凭证被修改: revision=%d valid=%v err=%v", value.Revision, valid, err)
	}
}

type fileInfoWithStat struct {
	os.FileInfo
	stat *syscall.Stat_t
}

func (info fileInfoWithStat) Sys() any {
	return info.stat
}

func testCredentialPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "auth", "auth.json")
}
