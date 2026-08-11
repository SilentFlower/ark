package envfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse_严格解析且错误不泄漏内容(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repo.env")
	content := "# comment\nexport TOKEN='literal $(id)'\nDUP=first\nDUP=second\nEMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入环境文件失败: %v", err)
	}

	values, err := Parse(path)
	if err != nil {
		t.Fatalf("Parse 返回错误: %v", err)
	}
	want := map[string]string{"TOKEN": "literal $(id)", "DUP": "second", "EMPTY": ""}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("Parse = %#v，期望 %#v", values, want)
	}

	secret := "SHOULD_NOT_APPEAR"
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatalf("覆盖环境文件失败: %v", err)
	}
	_, err = Parse(path)
	if err == nil {
		t.Fatal("无等号的环境行应返回错误")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误泄漏了环境文件内容: %v", err)
	}
}

func TestMerge_覆盖并消除重复Key(t *testing.T) {
	got := Merge([]string{"B=old", "A=first", "B=new", "INVALID"}, map[string]string{
		"B": "override",
		"C": "added",
	})
	want := []string{"B=override", "A=first", "C=added"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %#v，期望 %#v", got, want)
	}
}
