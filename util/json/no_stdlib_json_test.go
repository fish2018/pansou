package json

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 项目约定：业务代码一律使用 pansou/util/json，不要直接 import encoding/json，
// 以保证序列化行为统一由本封装的 sonic 配置决定（UseNumber/EscapeHTML 等）。
//
// 这个用例是约定的机械守卫：新增调用点若绕过封装会直接失败，而不是等到某天
// 两种实现的差异（例如同层同名 tag 的取舍、大整数精度）在线上暴露出来。
//
// 唯一允许的例外是本包的 adapter.go：它别名 stdlib 的类型（RawMessage/Number），
// 正是为了让调用方不必自己 import encoding/json。
func TestNoDirectEncodingJSONOutsideWrapper(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		filepath.Join("util", "json", "adapter.go"): true,
		// 本文件自身要在源码里写出被禁的导入路径，故排除
		filepath.Join("util", "json", "no_stdlib_json_test.go"): true,
	}

	var offenders []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if allowed[rel] {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(data), `"encoding/json"`) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("以下文件直接 import 了 encoding/json，应改用 pansou/util/json：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
