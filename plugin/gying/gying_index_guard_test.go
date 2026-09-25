package gying

import (
	"testing"

	"pansou/model"
)

// 上游的 l.title / l.d / l.i 是三个互相独立的 JSON 数组，长度并不保证一致。
// 旧守卫只校验了 l.title，而搜索循环的边界只由 l.i 决定，于是 l.d 短一截时
// searchData.L.D[index] 越界 panic——代码跑在无 recover 的 goroutine 里，
// 后果是进程退出。这个用例锁住"三个必需数组必须都覆盖 index"的约束。
func TestSearchDataHasAlignedIndex(t *testing.T) {
	var sd SearchData
	sd.L.Title = []string{"t0", "t1"}
	sd.L.D = []string{"mv"} // 比 I 短
	sd.L.I = []string{"i0", "i1"}

	cases := []struct {
		index int
		want  bool
	}{
		{0, true},
		{1, false}, // Title 有、I 有、但 D 没有 → 旧代码正是在这里越界
		{2, false},
		{-1, false},
	}
	for _, c := range cases {
		if got := sd.hasAlignedIndex(c.index); got != c.want {
			t.Errorf("hasAlignedIndex(%d) = %v, 期望 %v", c.index, got, c.want)
		}
	}
}

// 数组缺失时同样必须被挡住，不能只靠长度。
func TestSearchDataHasAlignedIndexMissingArrays(t *testing.T) {
	var sd SearchData
	sd.L.Title = []string{"t0"}
	if sd.hasAlignedIndex(0) {
		t.Error("D 与 I 都为空时不应认为索引可用")
	}
	sd.L.D = []string{"mv"}
	if sd.hasAlignedIndex(0) {
		t.Error("I 为空时不应认为索引可用")
	}
	sd.L.I = []string{"i0"}
	if !sd.hasAlignedIndex(0) {
		t.Error("三个数组齐备且长度足够时应认为索引可用")
	}
}

// 直接回归崩溃点：D 比 Title 短时 buildResult 曾越界 panic。
func TestBuildResultDoesNotPanicOnMismatchedArrays(t *testing.T) {
	p := &GyingPlugin{}
	var sd SearchData
	sd.L.Title = []string{"仙逆"}
	sd.L.D = []string{} // 上游少给 / 缺失
	sd.L.I = []string{"res-1"}

	got := p.buildResult(&DetailData{}, &sd, 0)
	if got.Title != "" {
		t.Errorf("数组不齐时应返回空结果，实际 Title=%q", got.Title)
	}
}

var _ = model.SearchResult{}
