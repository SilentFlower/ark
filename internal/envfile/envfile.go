// Package envfile 解析凭证环境文件并构造隔离的子进程环境。
//
// 本包只接受受限的 KEY=VALUE 语法，不执行 shell 展开、命令替换或变量插值。
// 环境合并会消除重复 key，避免同一凭证因出现多次而产生不确定的覆盖语义。
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse 读取受限语法的环境文件。
// @param path 要读取的环境文件路径。
// @return map[string]string 解析后的环境变量；重复 key 以后值覆盖前值。
// @return error 文件访问、扫描或语法错误；错误不会包含变量值。
func Parse(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开凭证文件 %s 失败: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	values := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("解析凭证文件 %s 第 %d 行失败: 环境变量格式无效", path, lineNumber)
		}
		value = strings.TrimSpace(value)
		if value != "" && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return nil, fmt.Errorf("解析凭证文件 %s 第 %d 行失败: 引号未闭合", path, lineNumber)
			}
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取凭证文件 %s 失败: %w", path, err)
	}
	return values, nil
}

// Merge 合并基础环境与覆盖值，并保证每个 key 只出现一次。
// @param base 基础环境，格式为 KEY=VALUE。
// @param overrides 要覆盖或追加的环境变量。
// @return []string 合并后的环境；基础 key 保持原顺序，新 key 按名称排序。
func Merge(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	order := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}

	var newKeys []string
	for key, value := range overrides {
		if _, exists := values[key]; !exists {
			newKeys = append(newKeys, key)
		}
		values[key] = value
	}
	sort.Strings(newKeys)
	order = append(order, newKeys...)

	merged := make([]string, 0, len(order))
	for _, key := range order {
		merged = append(merged, key+"="+values[key])
	}
	return merged
}
