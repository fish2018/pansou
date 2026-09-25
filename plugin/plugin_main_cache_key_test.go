package plugin

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"pansou/model"
)

// service 层此前逐个请求往共享插件实例上写 SetMainCacheKey/SetCurrentKeyword，
// 两个并发请求（或请求与后台补齐）会互相覆盖，A 关键词的结果可能被写进 B 的
// 缓存槽。修复后主缓存键随 ext 传递，框架以 ext 为准、实例字段只作兜底。
func TestResolveMainCacheKeyPrefersExt(t *testing.T) {
	// 没有 ext 时沿用调用方传入的值（106 处插件调用点传的是 p.MainCacheKey）
	if got := resolveMainCacheKey("from-instance", nil); got != "from-instance" {
		t.Errorf("无 ext 时应兜底用参数: %q", got)
	}
	if got := resolveMainCacheKey("from-instance", map[string]interface{}{}); got != "from-instance" {
		t.Errorf("空 ext 时应兜底用参数: %q", got)
	}

	// ext 里有值时必须以它为准
	ext := map[string]interface{}{ExtMainCacheKey: "per-request"}
	if got := resolveMainCacheKey("from-instance", ext); got != "per-request" {
		t.Errorf("应优先取 ext: %q", got)
	}

	// 类型不符或空串时退回参数，不能被坏值带偏
	for _, bad := range []interface{}{123, "", nil} {
		if got := resolveMainCacheKey("from-instance", map[string]interface{}{ExtMainCacheKey: bad}); got != "from-instance" {
			t.Errorf("ext 值 %#v 无效时应退回参数，实际 %q", bad, got)
		}
	}
}

// 并发下每个请求必须拿到自己的主缓存键。
//
// 这个用例直接模拟修复前的病灶：实例字段被反复改写，但 ext 里的值始终属于本次
// 调用自身，所以 updater 收到的永远是自己的键。
func TestConcurrentSearchesKeepTheirOwnMainCacheKey(t *testing.T) {
	p := NewBaseAsyncPlugin("mc-concurrency", 1)

	var mu sync.Mutex
	seen := map[string]string{} // keyword -> 收到的缓存键

	p.SetMainCacheUpdater(func(cacheKey string, results []model.SearchResult, ttl time.Duration, isFinal bool, keyword string) error {
		mu.Lock()
		seen[keyword] = cacheKey
		mu.Unlock()
		return nil
	})

	keywords := []string{"仙逆", "遮天", "凡人修仙传", "兰香如故", "完美世界", "斗破苍穹"}
	var wg sync.WaitGroup
	for _, kw := range keywords {
		wg.Add(1)
		go func(keyword string) {
			defer wg.Done()
			// 每个请求一份自己的 ext，键与关键词一一对应
			ext := map[string]interface{}{
				ExtMainCacheKey: "key-for-" + keyword,
				"refresh":       true, // 跳过缓存，确保走到 updater
			}
			// 同时故意用共享实例字段写入"别人的"键，模拟修复前 service 的行为
			p.SetMainCacheKey("key-for-WRONG")
			p.SetCurrentKeyword("WRONG")

			_, _ = p.AsyncSearch(keyword, func(_ *http.Client, _ string, _ map[string]interface{}) ([]model.SearchResult, error) {
				return []model.SearchResult{{UniqueID: "u-" + keyword, Title: keyword}}, nil
			}, "key-for-WRONG", ext)
		}(kw)
	}
	wg.Wait()

	// AsyncSearch 在响应超时内完成时走同步 updater，超时则交给后台完成路径，
	// 后者是异步的。等所有关键词都到齐（带上限），避免用例因调度抖动而漏判。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= len(keywords) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, kw := range keywords {
		got, ok := seen[kw]
		if !ok {
			t.Errorf("关键词 %q 在等待上限内没有触发 updater，用例未覆盖到该路径", kw)
			continue
		}
		if want := "key-for-" + kw; got != want {
			t.Errorf("关键词 %q 收到了别人的缓存键: 得到 %q, 期望 %q", kw, got, want)
		}
	}
}
