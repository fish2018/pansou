package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 上游响应体不能裸读：量级完全由对方决定，而插件是并发跑的，
// 几十个插件同时撞上一个异常大的响应就足以把进程内存打满。
// 统一走 util.ReadAllLimited，上限与理由见 util/http_body.go。
func TestNoUnboundedUpstreamBodyRead(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// 匹配对任意 <x>.Body 的裸读取；排除已经收口的 util.ReadAllLimited
	pat := regexp.MustCompile(`(?m)^.*\bio\.ReadAll\([A-Za-z_][A-Za-z0-9_.]*\.Body\)`)
	var offenders []string

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			// 先剥注释：文档里引用这个写法会被误判成违规
			code := stripLineComment(line)
			if pat.MatchString(code) {
				offenders = append(offenders, rel+":"+itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("以下位置裸读上游响应体，请改用 util.ReadAllLimited：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// stripLineComment 去掉行尾注释，避免文档/说明里提到的写法被当成违规代码。
func stripLineComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}
