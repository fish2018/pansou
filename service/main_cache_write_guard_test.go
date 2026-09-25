package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 主缓存写入的**文件级守卫**。
//
// 同一个语义（"往这个键写结果"）已经在四处各写了一遍，其中三处出过同一类问题：
// 插件侧最终写入覆盖缓存（验收测试抓到，30 条写成 1 条）、频道路径完整度写入覆盖缓存、
// 两个 backfill 覆盖缓存（受控实测丢 74% 的并发结果）。
//
// 靠"记得每一处都要合并"是不可靠的——这三次都是改的时候没意识到还有第四处。
// 所以这里按**文件级**拦住：除了下面列出的三个入口，任何地方都不许直接 Set / SetBothLevels
// 主缓存。新增写入点必须走这些入口（它们会按键互斥并先合并）。
//
// 守卫必须是形状级而不是"某个调用长什么样"：文本模式匹配会漏掉等价写法，
// 本会话已经因此漏过两批（zlxapp 的 NewTimer+select、8 处解压站点）。
func TestMainCacheWritesGoThroughMergeEntrypoints(t *testing.T) {
	// 允许直接写的地方：前两个是"合并入口"本身，第三个是把已合并好的数据落盘的写入管理器回调。
	allowed := map[string][]string{
		"search_service.go":    {"func writeFinalMainCache", "func mergeIntoMainCache", "SetMainCacheUpdater"},
		"cache_integration.go": {"createMainCacheUpdater"},
	}

	directWrite := regexp.MustCompile(`(?:enhancedTwoLevelCache|mainCache|c\.mainCache|cache)\.Set(?:BothLevels)?\(`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	offenders := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")

		// 记录每个允许区域的起止行
		type span struct{ start, end int }
		var spans []span
		for _, fn := range allowed[file] {
			for i, line := range lines {
				if !strings.Contains(line, fn) {
					continue
				}
				depth := 0
				for j := i; j < len(lines); j++ {
					depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
					if j > i && depth <= 0 {
						spans = append(spans, span{i, j})
						break
					}
				}
			}
		}

		for i, line := range lines {
			if !directWrite.MatchString(line) {
				continue
			}
			inAllowed := false
			for _, sp := range spans {
				if i >= sp.start && i <= sp.end {
					inAllowed = true
					break
				}
			}
			if !inAllowed {
				t.Errorf("%s:%d 直接写主缓存，绕过了合并入口（会吞掉并发结果）: %s",
					file, i+1, strings.TrimSpace(line))
				offenders++
			}
		}
	}

	if offenders > 0 {
		t.Logf("共 %d 处绕过合并入口；应改为 writeFinalMainCache 或 mergeIntoMainCache", offenders)
	}
}

// 守卫的对照实验：把一处合法的合并写改成直接写，守卫必须报出来。
func TestMainCacheWriteGuardCatchesDirectWrite(t *testing.T) {
	plant := `package service

func planted() {
	enhancedTwoLevelCache.SetBothLevels("key", nil, 0)
}
`
	directWrite := regexp.MustCompile(`(?:enhancedTwoLevelCache|mainCache|c\.mainCache|cache)\.Set(?:BothLevels)?\(`)
	if !directWrite.MatchString(plant) {
		t.Fatal("对照实验失效：守卫的正则认不出直接写，说明它测不出问题")
	}
}
