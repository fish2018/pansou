package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 循环体里的 defer Body.Close() 会把每次迭代的响应体一直压到函数返回才关。
//
// 反例（真实存在过）：重试循环里 `defer resp.Body.Close()` + `continue`，
// 重试 N 次就同时压着 N 个未关闭的响应体（连同连接不归还连接池）。
//
// 这个检测器只认"循环体内、且该循环体内存在 continue"这一种形状，因为另外两种
// 看着相似的写法是**正确**的，不能误伤：
//   - defer 在分支里，分支内立即 return（defer 当场触发）
//   - 循环体是每轮即时调用的闭包 func(...){...}(args)（defer 作用域是那一轮）
func TestNoDeferBodyCloseInsideRetryLoop(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

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
		offenders = append(offenders, scanDeferInRetryLoop(rel, string(src))...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("以下位置在重试循环体内 defer 关闭响应体，请改为读完立即 Close：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func repoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd, nil
}

var (
	reDeferBodyClose = regexp.MustCompile(`^\s*defer\s+\w+\.Body\.Close\(\)`)
	reForStart       = regexp.MustCompile(`^\s*for\b`)
	reContinue       = regexp.MustCompile(`^\s*continue\b`)
	reLineComment    = regexp.MustCompile(`//.*`)
)

// scanDeferInRetryLoop 逐文件扫描，返回"循环体内 defer 且该循环体内有 continue"的位置。
func scanDeferInRetryLoop(rel, src string) []string {
	lines := strings.Split(src, "\n")
	var out []string
	depth := 0
	// 每个进行中的循环：记录其循环头所在深度与该循环体内是否出现 continue
	type loopInfo struct {
		headDepth int
		hasCont   bool
		deferLine int
	}
	var loops []*loopInfo

	for i, raw := range lines {
		code := reLineComment.ReplaceAllString(raw, "")
		stripped := strings.TrimSpace(code)

		if reForStart.MatchString(code) && strings.Contains(code, "{") {
			loops = append(loops, &loopInfo{headDepth: depth})
		}
		if len(loops) > 0 && reContinue.MatchString(code) {
			loops[len(loops)-1].hasCont = true
		}
		if len(loops) > 0 && reDeferBodyClose.MatchString(code) {
			loops[len(loops)-1].deferLine = i + 1
		}

		depth += strings.Count(code, "{") - strings.Count(code, "}")
		// 循环体结束：结算
		for len(loops) > 0 && depth <= loops[len(loops)-1].headDepth {
			l := loops[len(loops)-1]
			loops = loops[:len(loops)-1]
			if l.hasCont && l.deferLine > 0 {
				out = append(out, fmt.Sprintf("%s:%d", rel, l.deferLine))
			}
		}
		_ = stripped
	}
	return out
}
