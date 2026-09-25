package service

import (
	"hash/fnv"
	"sync"
)

// 主缓存的"读-改-写"必须按缓存键串行化。
//
// injectMainCacheToAsyncPlugins 注入的 cacheUpdater 干的是：
//
//	Get(key) → 反序列化 → mergeSearchResults(现有, 新增) → 序列化 → Set(key)
//
// 同一关键词下多个插件并发完成时（这正是异步插件的常态），两个调用会读到同一份旧值，
// 各自只把自己那部分结果并进去再写回，后写的覆盖先写的——先完成那个插件的结果凭空消失。
// 结果数越多、插件越慢，越容易出现"明明搜到了却只剩一个源"。
//
// 用按键分条的锁而不是单把大锁：只有同一个缓存键的更新才需要互斥，不同键之间不该排队。
const mainCacheLockStripes = 64

var mainCacheKeyLocks [mainCacheLockStripes]sync.Mutex

// lockMainCacheKey 锁住该缓存键并返回解锁函数，用法：
//
//	unlock := lockMainCacheKey(key)
//	defer unlock()
func lockMainCacheKey(key string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	mu := &mainCacheKeyLocks[h.Sum32()%mainCacheLockStripes]
	mu.Lock()
	return mu.Unlock
}
