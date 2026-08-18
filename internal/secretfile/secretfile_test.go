package secretfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpen_校验安全文件边界(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "secret.env")
	if err := os.WriteFile(valid, []byte("KEY=value\n"), 0o600); err != nil {
		t.Fatalf("写入凭证文件失败: %v", err)
	}
	file, err := Open(valid, "test.env_file")
	if err != nil {
		t.Fatalf("安全文件应通过: %v", err)
	}
	_ = file.Close()

	wide := filepath.Join(directory, "wide.env")
	if err := os.WriteFile(wide, nil, 0o640); err != nil {
		t.Fatalf("写入宽权限文件失败: %v", err)
	}
	if _, err := Open(wide, "test.env_file"); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("宽权限错误 = %v", err)
	}

	link := filepath.Join(directory, "link.env")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatalf("创建符号链接失败: %v", err)
	}
	if _, err := Open(link, "test.env_file"); err == nil || !IsSymlinkError(err) {
		t.Fatalf("符号链接错误 = %v", err)
	}
}

func TestOpen_拒绝空路径与相对路径(t *testing.T) {
	for _, path := range []string{"", "secret.env"} {
		if _, err := Open(path, "test.env_file"); err == nil || !strings.Contains(err.Error(), "test.env_file") {
			t.Fatalf("路径 %q 错误 = %v", path, err)
		}
	}
}
