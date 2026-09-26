package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "limit")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配额文件失败: %v", err)
	}
	return p
}

func TestApplySetsNinetyPercent(t *testing.T) {
	prev := debug.SetMemoryLimit(-1) // -1 只读取当前值，不修改
	defer debug.SetMemoryLimit(prev)

	p := writeTemp(t, "1073741824\n") // 1GiB
	got, reason := apply([]string{p}, "")
	want := int64(1073741824) * 9 / 10
	if got != want {
		t.Fatalf("应设为 1GiB 的 90%%=%d，实际 %d（%s）", want, got, reason)
	}
	if debug.SetMemoryLimit(-1) != want {
		t.Fatalf("debug.SetMemoryLimit 未生效")
	}
}

func TestApplyDoesNotOverrideExplicitGOMEMLIMIT(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	p := writeTemp(t, "1073741824")
	got, reason := apply([]string{p}, "512MiB")
	if got != 0 {
		t.Fatalf("已有 GOMEMLIMIT 时不应改动，实际 %d", got)
	}
	if debug.SetMemoryLimit(-1) != prev {
		t.Fatalf("堆软上限被意外修改")
	}
	if reason == "" {
		t.Fatalf("应给出不覆盖的原因")
	}
}

func TestApplySkipsUnlimitedAndBrokenValues(t *testing.T) {
	for name, content := range map[string]string{
		"cgroup v2 无限": "max",
		"cgroup v1 无限": "9223372036854771712",
		"旧内核 -1":       "-1",
		"非数字":          "not-a-number",
		"空文件":          "",
		"配额过小":         "8388608", // 8MiB，视为配置错误
	} {
		t.Run(name, func(t *testing.T) {
			got, reason := apply([]string{writeTemp(t, content)}, "")
			if got != 0 {
				t.Fatalf("%s 时不应设置上限，实际 %d", name, got)
			}
			if reason == "" {
				t.Fatalf("应给出跳过原因")
			}
		})
	}
}

func TestApplyFallsBackToSecondPath(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	missing := filepath.Join(t.TempDir(), "nope")
	v1 := writeTemp(t, "268435456") // 256MiB
	got, _ := apply([]string{missing, v1}, "")
	want := int64(268435456) * 9 / 10
	if got != want {
		t.Fatalf("应回退到第二个路径并按 256MiB 计算 %d，实际 %d", want, got)
	}
}

func TestApplyNoCgroupFilesIsNoop(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(prev)

	got, reason := apply([]string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}, "")
	if got != 0 || reason == "" {
		t.Fatalf("容器外应为 no-op，实际 limit=%d reason=%q", got, reason)
	}
	if debug.SetMemoryLimit(-1) != prev {
		t.Fatalf("容器外不应修改堆软上限")
	}
}
