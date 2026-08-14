package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	credentialSchemaVersion = 1
	credentialFileMode      = 0o600
	credentialDirectoryMode = 0o700
	credentialMaxBytes      = 16 * 1024
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type credential struct {
	SchemaVersion int    `json:"schema_version"`
	Username      string `json:"username"`
	PasswordHash  string `json:"password_hash"`
	Revision      uint64 `json:"revision"`
}

type credentialDependencies struct {
	openExclusive func(string) (*os.File, error)
	createTemp    func(string, string) (*os.File, error)
	rename        func(string, string) error
	remove        func(string) error
	writeAndSync  func(*os.File, []byte) error
	syncDirectory func(string) error
	lockDirectory func(string) (io.Closer, error)
}

type credentialDirectoryLock struct {
	directory *os.File
}

func initializeCredential(path, username string, password []byte) error {
	return initializeCredentialWithDependencies(path, username, password, defaultCredentialDependencies())
}

func initializeCredentialWithDependencies(
	path, username string,
	password []byte,
	dependencies credentialDependencies,
) (resultErr error) {
	if strings.TrimSpace(path) == "" {
		return errors.New("初始化管理员失败: 凭证文件路径不能为空")
	}
	if err := validateCredentialDependencies(dependencies); err != nil {
		return err
	}
	if err := validateUsername(username); err != nil {
		return err
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	value := credential{
		SchemaVersion: credentialSchemaVersion,
		Username:      username,
		PasswordHash:  passwordHash,
		Revision:      1,
	}
	if err := prepareCredentialDirectory(filepath.Dir(path), true); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	directoryLock, err := dependencies.lockDirectory(directory)
	if err != nil {
		return fmt.Errorf("初始化管理员失败: 锁定凭证目录失败: %w", err)
	}
	defer func() {
		if err := directoryLock.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	data, err := encodeCredential(value)
	if err != nil {
		return err
	}
	file, err := dependencies.openExclusive(path)
	if err != nil {
		return fmt.Errorf("初始化管理员失败: 排他创建凭证文件 %q 失败: %w", path, err)
	}
	removeOnError := true
	defer func() {
		if removeOnError {
			resultErr = errors.Join(resultErr, cleanupCredentialPath(path, directory, dependencies))
		}
	}()
	if err := dependencies.writeAndSync(file, data); err != nil {
		return fmt.Errorf("初始化管理员失败: 写入凭证文件失败: %w", err)
	}
	if err := dependencies.syncDirectory(directory); err != nil {
		return err
	}
	removeOnError = false
	return nil
}

func resetCredentialPassword(path string, password []byte) error {
	return resetCredentialPasswordWithDependencies(path, password, defaultCredentialDependencies())
}

func resetCredentialPasswordWithDependencies(
	path string,
	password []byte,
	dependencies credentialDependencies,
) (resultErr error) {
	if strings.TrimSpace(path) == "" {
		return errors.New("重置管理员密码失败: 凭证文件路径不能为空")
	}
	if err := validateCredentialDependencies(dependencies); err != nil {
		return err
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := prepareCredentialDirectory(directory, false); err != nil {
		return err
	}
	directoryLock, err := dependencies.lockDirectory(directory)
	if err != nil {
		return fmt.Errorf("重置管理员密码失败: 锁定凭证目录失败: %w", err)
	}
	defer func() {
		if err := directoryLock.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()

	// revision 的读取、递增和提交必须持有同一把跨进程锁，避免并发重置丢失递增。
	current, err := loadCredential(path)
	if err != nil {
		return err
	}
	current.PasswordHash = passwordHash
	current.Revision++
	if current.Revision == 0 {
		return errors.New("重置管理员密码失败: revision 已溢出")
	}
	data, err := encodeCredential(current)
	if err != nil {
		return err
	}

	temporary, err := dependencies.createTemp(directory, ".auth.json-")
	if err != nil {
		return fmt.Errorf("重置管理员密码失败: 创建临时凭证文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if !removeTemporary {
			return
		}
		if err := dependencies.remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("清理临时凭证文件失败: %w", err))
		}
	}()
	if err := temporary.Chmod(credentialFileMode); err != nil {
		closeErr := temporary.Close()
		return errors.Join(fmt.Errorf("设置临时凭证权限失败: %w", err), closeErr)
	}
	if err := dependencies.writeAndSync(temporary, data); err != nil {
		return fmt.Errorf("写入临时凭证文件失败: %w", err)
	}
	if err := dependencies.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("原子替换凭证文件失败: %w", err)
	}
	removeTemporary = false
	if err := dependencies.syncDirectory(directory); err != nil {
		return err
	}
	return nil
}

func loadCredential(path string) (credential, error) {
	if strings.TrimSpace(path) == "" {
		return credential{}, errors.New("读取凭证失败: 凭证文件路径不能为空")
	}
	if err := prepareCredentialDirectory(filepath.Dir(path), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credential{}, uninitializedCredentialError(path)
		}
		return credential{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credential{}, uninitializedCredentialError(path)
		}
		return credential{}, fmt.Errorf("读取凭证文件元数据失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return credential{}, fmt.Errorf("拒绝读取凭证文件 %q: 不是普通文件", path)
	}
	if info.Mode().Perm() != credentialFileMode {
		return credential{}, fmt.Errorf("拒绝读取凭证文件 %q: 权限为 %04o，必须是 0600", path, info.Mode().Perm())
	}
	if err := validateCredentialOwner(info, "凭证文件", path); err != nil {
		return credential{}, err
	}
	if info.Size() > credentialMaxBytes {
		return credential{}, fmt.Errorf("拒绝读取凭证文件 %q: 大小超过 %d 字节", path, credentialMaxBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return credential{}, fmt.Errorf("打开凭证文件失败: %w", err)
	}
	defer func() { _ = file.Close() }()
	value, err := decodeCredential(io.LimitReader(file, credentialMaxBytes+1))
	if err != nil {
		return credential{}, fmt.Errorf("解析凭证文件失败: %w", err)
	}
	return value, nil
}

func uninitializedCredentialError(path string) error {
	return fmt.Errorf("ark-hub 尚未初始化，请先在本机执行 ark-hub admin init --auth-file %q", path)
}

func decodeCredential(reader io.Reader) (credential, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var value credential
	if err := decoder.Decode(&value); err != nil {
		return credential{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return credential{}, errors.New("凭证文件包含多余 JSON 值")
		}
		return credential{}, err
	}
	if err := validateCredential(value); err != nil {
		return credential{}, err
	}
	return value, nil
}

func encodeCredential(value credential) ([]byte, error) {
	if err := validateCredential(value); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("编码凭证失败: %w", err)
	}
	return append(data, '\n'), nil
}

func validateCredential(value credential) error {
	if value.SchemaVersion != credentialSchemaVersion {
		return fmt.Errorf("schema_version: 期望 %d，实际 %d", credentialSchemaVersion, value.SchemaVersion)
	}
	if err := validateUsername(value.Username); err != nil {
		return err
	}
	if value.Revision == 0 {
		return errors.New("revision 必须大于 0")
	}
	if _, err := parsePasswordHash(value.PasswordHash); err != nil {
		return err
	}
	return nil
}

func validateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("用户名 %q 非法，只允许 1-64 个 ASCII 字母、数字、点、下划线或短横线", username)
	}
	return nil
}

func prepareCredentialDirectory(directory string, create bool) error {
	if strings.TrimSpace(directory) == "" {
		return errors.New("凭证目录不能为空")
	}
	if create {
		if err := os.MkdirAll(directory, credentialDirectoryMode); err != nil {
			return fmt.Errorf("创建凭证目录失败: %w", err)
		}
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("读取凭证目录元数据失败: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("凭证目录 %q 不是目录", directory)
	}
	if info.Mode().Perm() != credentialDirectoryMode {
		return fmt.Errorf("凭证目录 %q 权限为 %04o，必须是 0700", directory, info.Mode().Perm())
	}
	if err := validateCredentialOwner(info, "凭证目录", directory); err != nil {
		return err
	}
	return nil
}

func defaultCredentialDependencies() credentialDependencies {
	return credentialDependencies{
		openExclusive: func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, credentialFileMode)
		},
		createTemp:    os.CreateTemp,
		rename:        os.Rename,
		remove:        os.Remove,
		writeAndSync:  writeAndSync,
		syncDirectory: syncCredentialDirectory,
		lockDirectory: lockCredentialDirectory,
	}
}

func validateCredentialDependencies(dependencies credentialDependencies) error {
	if dependencies.openExclusive == nil || dependencies.createTemp == nil || dependencies.rename == nil ||
		dependencies.remove == nil || dependencies.writeAndSync == nil || dependencies.syncDirectory == nil ||
		dependencies.lockDirectory == nil {
		return errors.New("凭证文件操作失败: 内部依赖不完整")
	}
	return nil
}

func validateCredentialOwner(info os.FileInfo, label, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("拒绝使用%s %q: 无法确认所有者", label, path)
	}
	expectedUID := uint32(os.Geteuid())
	if stat.Uid != expectedUID {
		return fmt.Errorf("拒绝使用%s %q: 所有者 UID 为 %d，必须是当前进程 UID %d", label, path, stat.Uid, expectedUID)
	}
	return nil
}

func lockCredentialDirectory(path string) (io.Closer, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// 锁定目录而不是 auth 文件，确保原子 rename 后所有进程仍竞争同一稳定锁对象。
	if err := unix.Flock(int(directory.Fd()), unix.LOCK_EX); err != nil {
		return nil, errors.Join(err, directory.Close())
	}
	return &credentialDirectoryLock{directory: directory}, nil
}

func (lock *credentialDirectoryLock) Close() error {
	unlockErr := unix.Flock(int(lock.directory.Fd()), unix.LOCK_UN)
	closeErr := lock.directory.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return fmt.Errorf("释放凭证目录锁失败: %w", err)
	}
	return nil
}

func cleanupCredentialPath(path, directory string, dependencies credentialDependencies) error {
	removeErr := dependencies.remove(path)
	if errors.Is(removeErr, os.ErrNotExist) {
		return nil
	}
	if removeErr != nil {
		return fmt.Errorf("清理未完成的凭证文件失败: %w", removeErr)
	}
	// 删除失败提交的文件也属于目录元数据变化，必须同步后才能把清理报告为成功。
	if err := dependencies.syncDirectory(directory); err != nil {
		return fmt.Errorf("同步凭证清理结果失败: %w", err)
	}
	return nil
}

func writeAndSync(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		closeErr := file.Close()
		return errors.Join(err, closeErr)
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		return errors.Join(err, closeErr)
	}
	return file.Close()
}

func syncCredentialDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开凭证目录用于同步失败: %w", err)
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return fmt.Errorf("同步凭证目录失败: %w", err)
	}
	return nil
}
