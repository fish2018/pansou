package service

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"pansou/config"
	"pansou/model"
	"pansou/util/cache"
)

// withMainCacheConfig 注入一份可用的缓存配置与一个全新的两级缓存。
func withMainCacheConfig(t *testing.T) *cache.EnhancedTwoLevelCache {
	t.Helper()
	saved := config.AppConfig
	config.AppConfig = &config.Config{
		CachePath:      t.TempDir(),
		CacheMaxSizeMB: 10,
		CacheEnabled:   true,
	}
	t.Cleanup(func() { config.AppConfig = saved })

	c, err := cache.NewEnhancedTwoLevelCache()
	if err != nil {
		t.Fatalf("创建两级缓存失败: %v", err)
	}
	return c
}

// readMergedResults 读回主缓存条目并反序列化，返回条目数。
func readMergedResults(t *testing.T, c *cache.EnhancedTwoLevelCache, key string) []model.SearchResult {
	t.Helper()
	data, hit, err := c.Get(key)
	if err != nil {
		t.Fatalf("读取主缓存失败: %v", err)
	}
	if !hit {
		return nil
	}
	var got []model.SearchResult
	if err := c.GetSerializer().Deserialize(data, &got); err != nil {
		t.Fatalf("反序列化主缓存失败: %v", err)
	}
	return got
}

// 主缓存并发合并的端到端验证。
//
// 场景就是生产里的常态：同一个关键词下多个异步插件**并发**完成，各自把结果并进同一条
// 主缓存。整段"读现有 → 合并 → 写回"若不做按键互斥，两个调用会读到同一份旧值、各自只
// 并进自己那部分再写回，后写覆盖先写——先完成那个插件的结果**静默消失**。
//
// 这里不依赖插件框架：直接并发调用 mergeIntoMainCache（上一轮从闭包里抽出来的接缝），
// 跑完再读回，断言每个插件的结果都在。
func TestMergeIntoMainCacheConcurrentKeepsEveryPluginResult(t *testing.T) {
	c := withMainCacheConfig(t)

	const key = "并发合并测试键"
	const pluginCount = 16

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < pluginCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量让所有 goroutine 同时起跑，放大竞争窗口
			res := []model.SearchResult{{
				UniqueID: fmt.Sprintf("插件-%d-唯一ID", i),
				Title:    fmt.Sprintf("插件 %d 的结果", i),
			}}
			if err := mergeIntoMainCache(c, key, res, time.Minute, true, "关键词", fmt.Sprintf("插件%d", i)); err != nil {
				t.Errorf("插件 %d 合并失败: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	got := readMergedResults(t, c, key)
	if len(got) != pluginCount {
		seen := map[string]bool{}
		for _, r := range got {
			seen[r.UniqueID] = true
		}
		var missing []string
		for i := 0; i < pluginCount; i++ {
			id := fmt.Sprintf("插件-%d-唯一ID", i)
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		t.Fatalf("并发合并丢结果：期望 %d 条、实际 %d 条，缺失=%v", pluginCount, len(got), missing)
	}
}

// 对照实验：把同一段逻辑**去掉互斥**再跑一遍（即修复前的行为），确认这个探针确实能测出
// 丢结果。否则上面那条用例通过说明不了任何问题——探针本身可能是瞎的。
//
// 在"读"与"写"之间插入固定延迟以放大窗口；这与生产里的竞争是同一个机制，只是更容易复现。
func TestUnlockedMergeLosesResultsControl(t *testing.T) {
	c := withMainCacheConfig(t)

	const key = "对照实验键"
	const pluginCount = 16

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < pluginCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// ↓↓↓ 修复前的行为：读-改-写不加锁
			var existing []model.SearchResult
			if data, hit, err := c.Get(key); err == nil && hit {
				_ = c.GetSerializer().Deserialize(data, &existing)
			}
			time.Sleep(2 * time.Millisecond) // 放大窗口
			merged := append(existing, model.SearchResult{
				UniqueID: fmt.Sprintf("插件-%d-唯一ID", i),
				Title:    fmt.Sprintf("插件 %d 的结果", i),
			})
			data, err := c.GetSerializer().Serialize(merged)
			if err != nil {
				t.Errorf("序列化失败: %v", err)
				return
			}
			if err := c.SetMemoryOnly(key, data, time.Minute); err != nil {
				t.Errorf("写入失败: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	got := readMergedResults(t, c, key)
	if len(got) >= pluginCount {
		t.Fatalf("对照实验失效：不加锁竟然也没丢结果（%d 条），说明这个探针测不出竞争，"+
			"那么 TestMergeIntoMainCacheConcurrentKeepsEveryPluginResult 的通过没有意义", len(got))
	}
	t.Logf("对照实验符合预期：不加锁时 %d 个插件只留下 %d 条结果", pluginCount, len(got))
}
