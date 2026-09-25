package service

import (
	"fmt"
	"testing"
	"time"

	"pansou/model"
)

// 探针：非最终合并只写内存（SetMemoryOnly），最终写入双写。问题在于 —— 读取固定先看内存。
// 问：内存里的旧版本会不会遮住磁盘上的新版本，让"命中缓存"反而拿到更少的结果？
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
	write := func(res []model.SearchResult, ttl time.Duration) {
		data, err := c.GetSerializer().Serialize(res)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetBothLevels(key, data, ttl); err != nil {
			t.Fatal(err)
		}
	}
	read := func() int {
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

	// 场景 A：最终写入 54 条（双写），随后一次非最终合并只写内存 50 条（模拟快照更小的合并）
	write(mk(54, "完整"), time.Minute)
	memOnly, _ := c.GetSerializer().Serialize(mk(50, "合并"))
	if err := c.SetMemoryOnly(key, memOnly, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("A：磁盘 54 + 内存 50 -> 读取得到 %d 条", read())

	// 场景 B：反过来 —— 非最终合并先写内存 50，最终写入再双写 54
	write(mk(50, "合并"), time.Minute)
	write(mk(54, "完整"), time.Minute)
	if got := read(); got != 54 {
		t.Errorf("双写应当覆盖内存，期望 54，实际 %d", got)
	}
	t.Logf("A：磁盘 54 + 内存 50 -> 读取得到 50（遮蔽）；B：内存 50 后双写 54 -> 读取得到 54")

	// 记录当前行为（不是断言"正确"）：内存里的较小版本会遮住磁盘上的较大版本。
	// 若将来改了读取策略（例如让"最终写入"的版本优先），这条会失败——那时把它改成新期望。
	if got := read(); got != 50 {
		t.Errorf("内存 50 遮磁盘 54 时读取应为 50，实际 %d", got)
	}
}
