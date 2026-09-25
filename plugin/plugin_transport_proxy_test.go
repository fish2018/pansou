package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 防回归守卫：插件里手写 &http.Transport{...} 时，Proxy 字段是零值，含义是
// "永远直连"——部署方配了代理也不生效，而 http.DefaultTransport 已由
// util.TuneDefaultTransport 接好代理解析，两者行为不一致。所有手写 Transport
// 都必须显式赋 Proxy（通常为 util.ProxyFuncForTransport()）。
//
// 这个用例直接扫描源码，新增插件忘记赋值时会在测试阶段暴露，而不是等到
// 某次"配了代理却全部插件失败"的线上排查。
func TestHandRolledTransportsSetProxy(t *testing.T) {
	files, err := filepath.Glob("*/[a-zA-Z]*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("未扫描到任何插件文件，路径假设已失效")
	}

	scanned := 0
	var violations []string

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)

		idx := 0
		for {
			i := strings.Index(src[idx:], "&http.Transport{")
			if i < 0 {
				break
			}
			brace := idx + i + len("&http.Transport")
			depth, k := 0, brace
			for k < len(src) {
				if src[k] == '{' {
					depth++
				} else if src[k] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				k++
			}
			block := src[brace+1 : k]
			line := strings.Count(src[:brace], "\n") + 1
			scanned++
			if !strings.Contains(block, "Proxy:") {
				violations = append(violations,
					filepath.ToSlash(file)+":"+itoa(line))
			}
			idx = k + 1
		}
	}

	if scanned == 0 {
		t.Fatal("未扫描到任何 &http.Transport{...}，判据可能已失效")
	}
	if len(violations) > 0 {
		t.Errorf("以下手写 Transport 缺少 Proxy 字段（零值 = 永远直连，配置代理时该插件会失效）:\n  %s",
			strings.Join(violations, "\n  "))
	}
	t.Logf("已扫描 %d 处手写 Transport，全部显式设置 Proxy", scanned)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
