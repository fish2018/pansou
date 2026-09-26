package cpu

import (
	"runtime"
	"testing"
)

func TestSchedulableCountFollowsGOMAXPROCS(t *testing.T) {
	// 关键性质：跟随 GOMAXPROCS（容器里由 cgroup 配额决定），而不是宿主核数。
	prev := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(prev)

	for _, n := range []int{2, 3, 1} {
		runtime.GOMAXPROCS(n)
		if got := SchedulableCount(); got != n {
			t.Fatalf("GOMAXPROCS=%d 时应返回 %d，实际 %d", n, n, got)
		}
	}
}

func TestSchedulableCountIsNoopByDefault(t *testing.T) {
	// 未显式设置 GOMAXPROCS 时取值必须与 NumCPU 一致，否则此前所有实测都要重跑。
	if runtime.GOMAXPROCS(0) == runtime.NumCPU() {
		if got := SchedulableCount(); got != runtime.NumCPU() {
			t.Fatalf("默认环境应等于 NumCPU=%d，实际 %d", runtime.NumCPU(), got)
		}
	}
	if got := SchedulableCount(); got < 1 {
		t.Fatalf("返回值必须至少为 1，实际 %d", got)
	}
}
