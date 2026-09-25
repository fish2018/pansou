package service

import (
	"fmt"
	"testing"
	"time"

	"pansou/model"
)

// 探针：记录"内存版本遮蔽磁盘版本"这一当前行为（不是断言它正确）。
//
// 背景：非最终合并只写内存（SetMemoryOnly），最终写入双写（SetBothLevels），
// 而 Get 固定先看内存、命中即返回。于是内存里一个较小的版本会持续遮住磁盘上较大的版本，
// 表现为"命中缓存反而比缓存实际持有的少"——全量验收里观测到一次：命中 50 条、磁盘 54 条。
//
// 若将来改了读取策略（例如让最终写入的版本优先），下面的断言会失败，届时改成新期望。
func TestProbeLevelShadowing(t *testing.T) {
	c := withMainCacheConfig(t)
	const key = "两级视图探针"

	mk := func(n int, prefix string) []model.SearchResult {
		out := make([]model.SearchResult, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, model.SearchResult{UniqueID: fmt.Sprintf("%s-%d", prefix, i), Title: prefix})
		}
		return out
	}
	writeBoth := func(res []model.SearchResult) {
		t.Helper()
		data, err := c.GetSerializer().Serialize(res)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetBothLevels(key, data, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	read := func() int {
		t.Helper()
		data, hit, err := c.Get(key)
		if err != nil || !hit {
			t.Fatalf("读取失败 hit=%v err=%v", hit, err)
		}
		var res []model.SearchResult
		if err := c.GetSerializer().Deserialize(data, &res); err != nil {
			t.Fatal(err)
		}
		return len(res)
	}

	// 场景 A：最终写入 54 条（双写），随后一次非最终合并只写内存 50 条
	writeBoth(mk(54, "完整"))
	memOnly, err := c.GetSerializer().Serialize(mk(50, "合并"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetMemoryOnly(key, memOnly, time.Minute); err != nil {
		t.Fatal(err)
	}
	afterA := read()

	// 场景 B：非最终合并先写内存 50 条，随后最终写入双写 54 条
	writeBoth(mk(50, "合并"))
	writeBoth(mk(54, "完整"))
	afterB := read()

	t.Logf("A：磁盘 54 + 内存 50 -> 读取 %d 条（内存遮蔽磁盘）；B：内存 50 后双写 54 -> 读取 %d 条", afterA, afterB)

	if afterA != 50 {
		t.Errorf("内存 50 遮磁盘 54 时当前行为应为 50，实际 %d", afterA)
	}
	if afterB != 54 {
		t.Errorf("双写应覆盖内存，期望 54，实际 %d", afterB)
	}
}
