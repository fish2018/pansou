package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试是"源码守卫"：用正则扫描整个仓库的源码，检查是否出现已知的危险写法
// （重试循环体内 defer 关闭响应体、解压不封顶、裸读上游响应体、手写无上限读取等）。
//
// 它们原先作为 package main 的测试放在仓库根目录，因为 Go 要求同包测试必须同目录。
// 但那会让根目录堆一排 main_xxx_test.go，很乱。这里独立成 audit 包，
// 并统一在 TestMain 里把工作目录切到仓库根，让各个扫描器里 os.Getwd() 与
// filepath.Walk(".") 的相对路径语义保持原样、一行都不用改。
func TestMain(m *testing.M) {
	if err := os.Chdir(".."); err != nil {
		panic("切换到仓库根目录失败: " + err.Error())
	}
	// 防止将来目录结构调整后，扫描范围悄悄变成错的目录而测试照样"通过"
	for _, must := range []string{"main.go", "go.mod", "plugin", "service", "util"} {
		if _, err := os.Stat(must); err != nil {
			panic("工作目录不是仓库根，缺少 " + must + ": " + err.Error())
		}
	}
	// 再确认扫描确实扫到了东西：filepath.Walk 的守卫在空目录上会"全部通过"，
	// 只看 TestMain 里几个目录是否存在挡不住"扫描范围悄悄变成空的"。
	goFiles := 0
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".go") {
			goFiles++
		}
		return nil
	})
	if goFiles < 200 {
		panic(fmt.Sprintf("扫描到的 .go 文件仅 %d 个，明显不是完整仓库，守卫会假通过", goFiles))
	}
	os.Exit(m.Run())
}
