package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decompressUnboundedOffenders 返回"做了解压但没有任何封顶手段"的文件。
//
// 为什么需要这条守卫：上游响应体封顶那一轮只扫了 `io.ReadAll(resp.Body)` 这一种写法，
// 而解压路径把 reader **换成了变量**（`reader = gzReader` 之后再 `io.ReadAll(reader)` 或
// `goquery.NewDocumentFromReader(reader)`），于是整批解压站点全被漏掉——压缩数据可以很小、
// 解压后极大（解压炸弹），而解压后的内容通常还要整体物化进内存再解析。
//
// 这就是"用单一文本模式给代码分类会漏掉等价写法"的同一个病根，所以守卫按**文件**判断：
// 只要文件里出现了 gzip/zlib/flate 解压，就必须同时出现封顶手段。
func decompressUnboundedOffenders(path string, src string) bool {
	if !strings.Contains(src, "gzip.NewReader(") &&
		!strings.Contains(src, "zlib.NewReader(") &&
		!strings.Contains(src, "flate.NewReader(") {
		return false
	}
	if strings.Contains(src, "NewCappedReader(") || strings.Contains(src, "ReadAllDecompressed(") {
		return false
	}
	_ = path
	return true
}

// 对照实验：给一个"解压且不封顶"的文件，守卫必须抓到。
func TestDecompressGuardCatchesPlant(t *testing.T) {
	plant := `package fake

import "compress/gzip"

func bad(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	reader = gz
	return io.ReadAll(reader)
}
`
	if !decompressUnboundedOffenders("plugin/fake/fake.go", plant) {
		t.Fatal("对照实验失败：守卫抓不到不封顶的解压文件，这条守卫没有意义")
	}

	// 封顶后就该放过
	fixed := strings.Replace(plant, "reader = gz", "reader = util.NewCappedReader(gz, util.MaxDecompressedBytes)", 1)
	if decompressUnboundedOffenders("plugin/fake/fake.go", fixed) {
		t.Error("已封顶的文件不该被判为有问题")
	}
}

// 真实扫描：全仓不允许存在"做了解压但没封顶"的文件。
func TestNoUnboundedDecompression(t *testing.T) {
	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if decompressUnboundedOffenders(path, string(data)) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("以下文件做了解压但没有任何封顶手段（解压炸弹风险）:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
