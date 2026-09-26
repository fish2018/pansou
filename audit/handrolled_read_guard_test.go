package audit

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 上轮把 77 处上游响应体读取统一封顶 16 MiB，但守卫是按 io.ReadAll( 文本匹配的，
// 于是 quarktv 里手写的 ioReadAll 会完全逃过检查——它自己 make 一个缓冲、在 for {} 里
// 无限 append，同样没有上限。这个守卫补上那一类。
var handRolledReadPattern = regexp.MustCompile(`(?s)for\s*\{[^}]*?\.Read\([^)]*\)[^}]*?append\(`)

// handRolledReadOffenders 返回 src 中"手写无界读取循环"的可疑行号与摘录。
// 判定标准：for {} 循环体内出现 .Read(...) 且紧跟 append(...) 累积，且整段没提到上限常量。
func handRolledReadOffenders(path, src string) []string {
	var out []string
	for _, loc := range handRolledReadPattern.FindAllStringIndex(src, -1) {
		snippet := src[loc[0]:loc[1]]
		// 提到上限常量即认为有界（例如按 MaxUpstreamResponseBytes 截断）
		if strings.Contains(snippet, "MaxUpstreamResponseBytes") ||
			strings.Contains(snippet, "maxReadBytes") ||
			strings.Contains(snippet, "LimitReader") {
			continue
		}
		line := 1 + strings.Count(src[:loc[0]], "\n")
		out = append(out, path+":"+strconv.Itoa(line)+" 手写读取循环未见上限")
	}
	return out
}

// 对照实验：守卫必须能抓到已修复的那段代码（quarktv 原先的 ioReadAll 形态）。
func TestHandRolledReadGuardCatchesPlant(t *testing.T) {
	planted := `package main

import "io"

func ioReadAll(body io.Reader) ([]byte, error) {
	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return buf, nil
		}
	}
}
`
	if got := handRolledReadOffenders("planted.go", planted); len(got) == 0 {
		t.Error("守卫未能抓到已修复的那种手写无界读取循环（对照实验失败）")
	}

	// 有界写法不该被误报
	bounded := `package main

func readIt(body io.Reader) ([]byte, error) {
	return util.ReadAllLimited(body, util.MaxUpstreamResponseBytes)
}
`
	if got := handRolledReadOffenders("bounded.go", bounded); len(got) != 0 {
		t.Errorf("有界写法被误报: %v", got)
	}
}

// 全仓扫描：插件与服务代码里不允许出现手写无界读取。
func TestNoHandRolledUnboundedBodyRead(t *testing.T) {
	roots := []string{"plugin", "util", "service", "api"}
	var offenders []string

	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			src := string(data)
			// 先剥行尾注释：本守卫的说明文字里就写着 for {} 与 append(
			var stripped []string
			for _, l := range strings.Split(src, "\n") {
				if i := strings.Index(l, "//"); i >= 0 {
					l = l[:i]
				}
				stripped = append(stripped, l)
			}
			for _, o := range handRolledReadOffenders(path, strings.Join(stripped, "\n")) {
				offenders = append(offenders, o)
			}
			return nil
		})
	}

	if len(offenders) > 0 {
		t.Errorf("发现手写无界读取（应改用 util.ReadAllLimited）:\n  %s", strings.Join(offenders, "\n  "))
	}
}
