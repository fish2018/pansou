package service

import (
	"fmt"
	"testing"
	"time"

	"pansou/model"
)

// 测量：补写用"请求开始时的快照"覆盖缓存时，会丢掉多少并发请求已经并进来的结果。
//
// 完全照抄两个 backfill 的实际写回（search_service.go:1466 的 Set / :1741 的 SetBothLevels）：
// merged = 本请求 collected + 补齐回来的，然后整块写，不读缓存里当前已有的。
func TestBackfillOverwriteLosesConcurrentResults(t *testing.T) {
	c := withMainCacheConfig(t)

	const key = "补齐覆盖丢结果"

	// 场景：某个请求（请求 A）在 30 秒窗口内只拿到 8 条，随后进入后台补齐。
	// 它自己在内存里持有的"快照"就是这 8 条。
	snapshot := make([]model.SearchResult, 0, 8)
	for i := 0; i < 8; i++ {
		snapshot = append(snapshot, model.SearchResult{UniqueID: fmt.Sprintf("A-%d", i), Title: fmt.Sprintf("A 的第 %d 条", i)})
	}

	// 与此同时，其它请求（B/C/D…）把各自的结果并进了同一个缓存键——这正是异步插件路径的常态：
	// 插件边走边并，任何一次重复搜索都会往里加。
	concurrentTotal := 50
	concurrent := make([]model.SearchResult, 0, concurrentTotal)
	for i := 0; i < concurrentTotal; i++ {
		concurrent = append(concurrent, model.SearchResult{UniqueID: fmt.Sprintf("B-%d", i), Title: fmt.Sprintf("并发请求的第 %d 条", i)})
	}
	if err := mergeIntoMainCache(c, key, concurrent, time.Minute, true, "关键词", "并发请求"); err != nil {
		t.Fatal(err)
	}
	before := len(readMergedResults(t, c, key))

	// 补齐回来 5 条，按两个 backfill 的写法合并成 merged 再整块写
	merged := append(append([]model.SearchResult{}, snapshot...), model.SearchResult{UniqueID: "补齐-0"}, model.SearchResult{UniqueID: "补齐-1"}, model.SearchResult{UniqueID: "补齐-2"}, model.SearchResult{UniqueID: "补齐-3"}, model.SearchResult{UniqueID: "补齐-4"})
	data, err := c.GetSerializer().Serialize(merged)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetBothLevels(key, data, time.Minute); err != nil { // ← 现在两个 backfill 的写法
		t.Fatal(err)
	}
	afterOverwrite := len(readMergedResults(t, c, key))

	// 改成合并写（第一步要做的），看能保住多少
	if err := mergeIntoMainCache(c, key, concurrent, time.Minute, true, "关键词", "并发"); err != nil {
		t.Fatal(err)
	}
	writeFinalMainCache(c, key, merged, time.Minute)
	afterMerge := len(readMergedResults(t, c, key))

	t.Logf("补齐前缓存: %d 条；按现状覆盖写后: %d 条（丢 %d 条）；改成合并写后: %d 条",
		before, afterOverwrite, before-afterOverwrite, afterMerge)

	// 现状：覆盖写把并发请求并进来的结果吞掉，只留本请求快照 + 补齐回来的
	if afterOverwrite != 13 {
		t.Errorf("现状跑覆盖写，实测只剩 13 条，实际 %d", afterOverwrite)
	}
	if before-afterOverwrite == 0 {
		t.Fatal("探针失效：覆盖写竟然没丢结果，说明这个场景测不出问题")
	}
	// 改成合并写之后，并发请求的结果被保住
	if afterMerge < before {
		t.Errorf("合并写不该少于覆盖前：%d < %d", afterMerge, before)
	}
}
