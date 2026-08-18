// Package secretfile 安全打开只应由当前进程用户读取的凭证文件。
package secretfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Open 使用 NOFOLLOW 打开凭证文件，并校验类型、所有者与权限。
// @param path 凭证文件的绝对路径。
// @param field 配置字段名，用于生成可定位且不含秘密的错误。
// @return *os.File 已通过安全校验且位于文件起始位置的只读文件。
// @return error 路径、打开、类型、所有者或权限不满足要求时的错误。
func Open(path string, field string) (*os.File, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s 不能为空", field)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%s 必须是绝对路径", field)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("安全打开 %s %s 失败: %w", field, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("安全打开 %s %s 失败", field, path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("读取 %s %s 元数据失败: %w", field, path, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("拒绝读取 %s %s: 不是普通文件", field, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("拒绝读取 %s %s: 权限为 %04o，应不超过 0600", field, path, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		_ = file.Close()
		return nil, fmt.Errorf("拒绝读取 %s %s: 无法确认所有者", field, path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		return nil, fmt.Errorf("拒绝读取 %s %s: 所有者 UID 为 %d，必须是当前进程 UID %d", field, path, stat.Uid, os.Geteuid())
	}
	return file, nil
}

// IsSymlinkError 判断错误是否由 NOFOLLOW 拒绝符号链接导致。
// @param err Open 返回的错误。
// @return bool 底层错误为 ELOOP 时返回 true。
func IsSymlinkError(err error) bool {
	return errors.Is(err, unix.ELOOP)
}
