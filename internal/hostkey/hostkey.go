// Package hostkey 管理 ark 使用的 OpenSSH 主机密钥信任库。
//
// 本包只负责扫描公开主机密钥、生成可核对的 SHA256 指纹，以及在用户显式授权后
// 原子更新 known_hosts。它不读取身份私钥、不发起账号认证，也不替代带外身份核验。
package hostkey

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const commandTimeout = 15 * time.Second

// Fingerprint 是一条可供人工带外核对的 SSH 主机密钥指纹。
type Fingerprint struct {
	Algorithm string `json:"algorithm"`
	SHA256    string `json:"sha256"`
}

// Result 描述一次主机密钥扫描、比较与可选应用的结果。
type Result struct {
	Address        string        `json:"address"`
	KnownHostsFile string        `json:"known_hosts_file"`
	Existing       []Fingerprint `json:"existing"`
	Scanned        []Fingerprint `json:"scanned"`
	Applied        bool          `json:"applied"`
}

type commandOutput struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

type commandFunc func(context.Context, []byte, string, ...string) (commandOutput, error)

type dependencies struct {
	command       commandFunc
	rename        func(string, string) error
	remove        func(string) error
	syncDirectory func(string) error
}

// Refresh 扫描 address 当前公开的 SSH 主机密钥，并可选更新 knownHostsFile。
// @param ctx 控制外部扫描和指纹命令的取消与超时。
// @param address 清单中的 host:port 地址。
// @param knownHostsFile 要读取或更新的主机密钥库绝对路径。
// @param apply 为 true 时原子替换该 address 的记录；false 时只预览。
// @return Result 旧记录、新扫描指纹和是否已应用。
// @return error 地址、文件、外部命令或原子写入失败时的错误。
func Refresh(ctx context.Context, address, knownHostsFile string, apply bool) (Result, error) {
	return refresh(ctx, address, knownHostsFile, apply, dependencies{
		command:       runCommand,
		rename:        os.Rename,
		remove:        os.Remove,
		syncDirectory: syncDirectory,
	})
}

func refresh(
	ctx context.Context,
	address string,
	knownHostsFile string,
	apply bool,
	deps dependencies,
) (Result, error) {
	result := Result{Address: address, KnownHostsFile: knownHostsFile}
	if ctx == nil {
		return result, fmt.Errorf("刷新主机密钥失败: context 不能为空")
	}
	if deps.command == nil || deps.rename == nil || deps.remove == nil || deps.syncDirectory == nil {
		return result, fmt.Errorf("刷新主机密钥失败: 内部依赖不完整")
	}
	if !filepath.IsAbs(knownHostsFile) {
		return result, fmt.Errorf("known_hosts 路径必须是绝对路径，实际 %q", knownHostsFile)
	}

	host, port, token, err := parseAddress(address)
	if err != nil {
		return result, err
	}
	existing, exists, err := readExistingKeys(ctx, token, knownHostsFile, deps.command)
	if err != nil {
		return result, err
	}
	result.Existing, err = fingerprints(ctx, existing, deps.command)
	if err != nil {
		return result, fmt.Errorf("计算已记录主机密钥指纹失败: %w", err)
	}

	scanned, err := scanKeys(ctx, host, port, token, deps.command)
	if err != nil {
		return result, err
	}
	result.Scanned, err = fingerprints(ctx, scanned, deps.command)
	if err != nil {
		return result, fmt.Errorf("计算扫描主机密钥指纹失败: %w", err)
	}
	if !apply {
		return result, nil
	}
	if err := applyKeys(ctx, token, knownHostsFile, scanned, exists, deps); err != nil {
		return result, err
	}
	result.Applied = true
	return result, nil
}

func parseAddress(address string) (string, string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", "", fmt.Errorf("ssh 地址 %q 不是合法的 host:port: %w", address, err)
	}
	if host == "" {
		return "", "", "", fmt.Errorf("ssh 地址 %q 缺少主机名或 IP", address)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", "", fmt.Errorf("ssh 地址 %q 的端口必须是 1-65535 之间的数字", address)
	}
	token := host
	if port != "22" {
		token = "[" + host + "]:" + port
	}
	return host, port, token, nil
}

func readExistingKeys(
	ctx context.Context,
	token string,
	path string,
	command commandFunc,
) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("检查 known_hosts %q 失败: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("known_hosts %q 必须是普通文件且不能是符号链接", path)
	}

	out, err := command(ctx, nil, "ssh-keygen", "-F", token, "-f", path)
	if err != nil {
		if out.exitCode == 1 {
			return nil, true, nil
		}
		return nil, false, commandError("查询已记录主机密钥", out, err)
	}
	return keyLines(out.stdout), true, nil
}

func scanKeys(
	ctx context.Context,
	host string,
	port string,
	token string,
	command commandFunc,
) ([]byte, error) {
	out, err := command(ctx, nil, "ssh-keyscan", "-T", "10", "-p", port, "--", host)
	if err != nil {
		return nil, commandError("扫描远端主机密钥", out, err)
	}
	keys, err := normalizeScannedKeys(token, out.stdout)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func normalizeScannedKeys(token string, data []byte) ([]byte, error) {
	var normalized bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("ssh-keyscan 返回了无法识别的主机密钥行")
		}
		fmt.Fprintf(&normalized, "%s %s %s\n", token, fields[1], fields[2])
	}
	if normalized.Len() == 0 {
		return nil, fmt.Errorf("ssh-keyscan 未返回任何主机密钥")
	}
	return normalized.Bytes(), nil
}

func keyLines(data []byte) []byte {
	var keys bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys.WriteString(line)
		keys.WriteByte('\n')
	}
	return keys.Bytes()
}

func fingerprints(ctx context.Context, keys []byte, command commandFunc) ([]Fingerprint, error) {
	if len(bytes.TrimSpace(keys)) == 0 {
		return []Fingerprint{}, nil
	}
	out, err := command(ctx, keys, "ssh-keygen", "-lf", "-")
	if err != nil {
		return nil, commandError("生成主机密钥指纹", out, err)
	}

	seen := make(map[string]Fingerprint)
	for _, line := range strings.Split(string(out.stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasPrefix(fields[1], "SHA256:") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, fmt.Errorf("ssh-keygen 返回了无法识别的指纹行")
		}
		fingerprint := Fingerprint{
			Algorithm: strings.Trim(fields[len(fields)-1], "()"),
			SHA256:    fields[1],
		}
		seen[fingerprint.Algorithm+"\x00"+fingerprint.SHA256] = fingerprint
	}

	result := make([]Fingerprint, 0, len(seen))
	for _, fingerprint := range seen {
		result = append(result, fingerprint)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Algorithm != result[j].Algorithm {
			return result[i].Algorithm < result[j].Algorithm
		}
		return result[i].SHA256 < result[j].SHA256
	})
	return result, nil
}

func applyKeys(
	ctx context.Context,
	token string,
	path string,
	keys []byte,
	exists bool,
	deps dependencies,
) error {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("检查 known_hosts 目录 %q 失败: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("known_hosts 父路径 %q 不是目录", dir)
	}
	// 预览到应用之间文件可能被替换；写入前再次核对类型并保存可回滚快照。
	originalData, originalMode, err := snapshotKnownHosts(path, exists)
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(dir, ".ark-known-hosts-")
	if err != nil {
		return fmt.Errorf("创建 known_hosts 临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
		_ = os.Remove(temporaryPath + ".old")
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("设置 known_hosts 临时文件权限失败: %w", err), temporary.Close())
	}
	if exists {
		if _, err := temporary.Write(originalData); err != nil {
			return errors.Join(fmt.Errorf("复制 known_hosts 到临时文件失败: %w", err), temporary.Close())
		}
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("同步 known_hosts 临时文件失败: %w", err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭 known_hosts 临时文件失败: %w", err)
	}

	out, err := deps.command(ctx, nil, "ssh-keygen", "-R", token, "-f", temporaryPath)
	if err != nil {
		return commandError("移除旧主机密钥", out, err)
	}
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开 known_hosts 临时文件失败: %w", err)
	}
	if _, err := file.Write(keys); err != nil {
		return errors.Join(fmt.Errorf("写入新主机密钥失败: %w", err), file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("同步新主机密钥失败: %w", err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭新主机密钥文件失败: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return fmt.Errorf("设置新 known_hosts 权限失败: %w", err)
	}
	if err := deps.rename(temporaryPath, path); err != nil {
		return fmt.Errorf("原子替换 known_hosts %q 失败: %w", path, err)
	}
	if err := deps.syncDirectory(dir); err != nil {
		// rename 已经对进程可见，但目录项尚未确认持久化；恢复原状态才能维持失败零写入契约。
		rollbackErr := rollbackKnownHosts(path, originalData, originalMode, exists, deps)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func snapshotKnownHosts(path string, exists bool) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if !exists {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("重新检查 known_hosts %q 失败: %w", path, err)
		}
		return nil, 0, fmt.Errorf("known_hosts %q 在刷新期间被创建，请重新预览后再应用", path)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("重新检查 known_hosts %q 失败: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("known_hosts %q 必须是普通文件且不能是符号链接", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读取 known_hosts %q 失败: %w", path, err)
	}
	return data, info.Mode().Perm(), nil
}

func rollbackKnownHosts(
	path string,
	originalData []byte,
	originalMode os.FileMode,
	existed bool,
	deps dependencies,
) error {
	if existed {
		if err := replaceKnownHosts(path, originalData, originalMode, deps.rename); err != nil {
			return fmt.Errorf("目录同步失败后恢复原 known_hosts 失败: %w", err)
		}
	} else if err := deps.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("目录同步失败后移除新 known_hosts 失败: %w", err)
	}
	if err := deps.syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("恢复 known_hosts 后同步目录失败: %w", err)
	}
	return nil
}

func replaceKnownHosts(path string, data []byte, mode os.FileMode, rename func(string, string) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ark-known-hosts-rollback-")
	if err != nil {
		return fmt.Errorf("创建回滚临时文件失败: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return errors.Join(fmt.Errorf("恢复原 known_hosts 权限失败: %w", err), temporary.Close())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(fmt.Errorf("恢复原 known_hosts 内容失败: %w", err), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("同步回滚临时文件失败: %w", err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭回滚临时文件失败: %w", err)
	}
	if err := rename(temporaryPath, path); err != nil {
		return fmt.Errorf("原子恢复 known_hosts %q 失败: %w", path, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开 known_hosts 目录用于同步失败: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("同步 known_hosts 目录失败: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, stdin []byte, name string, args ...string) (commandOutput, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := commandOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	if ctxErr := commandCtx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
	}
	return result, err
}

func commandError(action string, output commandOutput, err error) error {
	if detail := strings.TrimSpace(string(output.stderr)); detail != "" {
		return fmt.Errorf("%s失败: %w: %s", action, err, detail)
	}
	return fmt.Errorf("%s失败: %w", action, err)
}
