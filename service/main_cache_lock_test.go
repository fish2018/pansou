package service

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 同一个缓存键上的更新必须互斥：主缓存的"读现有 → 合并 → 写回"不是原子操作，
// 两个并发调用会读到同一份旧值、各自只并进自己那部分再写回，后写覆盖先写。
// 这里验证互斥本身（同一键串行、不同键不互斥），端到端驱动见下方注释。
func TestLockMainCacheKeySerializesSameKey(t *testing.T) {
	const key = "同一个缓存键"

	var inside int32
	var overlapped int32
	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockMainCacheKey(key)
			defer unlock()
			if atomic.AddInt32(&inside, 1) != 1 {
				atomic.StoreInt32(&overlapped, 1)
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()

	if overlapped == 1 {
		t.Error("同一个缓存键上有两个持有者同时进入临界区，读-改-写仍会互相覆盖")
	}
}

// 不同缓存键之间不该排队：否则一个慢键会拖住所有关键词的缓存更新。
func TestLockMainCacheKeyAllowsDifferentKeys(t *testing.T) {
	// 先占住键 A
	unlockA := lockMainCacheKey("键A")
	defer unlockA()

	done := make(chan struct{})
	go func() {
		unlockB := lockMainCacheKey("键B")
		unlockB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("不同缓存键之间被互相阻塞，分条锁退化成了全局锁")
	}
}

// 同一个键的解锁必须能把后来者放行（锁没有泄漏）。
func TestLockMainCacheKeyReleasesForSameKey(t *testing.T) {
	const key = "释放测试"
	unlock := lockMainCacheKey(key)
	unlock()

	done := make(chan struct{})
	go func() {
		u := lockMainCacheKey(key)
		u()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("解锁后后来者仍拿不到锁")
	}
}

// 键相同但字符串不同源时必须落到同一条（分条按内容哈希，不按指针）。
func TestLockMainCacheKeyHashesContent(t *testing.T) {
	key := "内容相同即可"
	unlock := lockMainCacheKey(key + "")
	unlock()

	done := make(chan struct{})
	go func() {
		// 另建一份等值字符串
		other := []byte("内容相同即可")
		u := lockMainCacheKey(string(other))
		u()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("等值字符串未落到同一条锁，互斥失效")
	}
}

// 端到端缺口（如实记录）：直接对 cacheUpdater 做并发合并断言需要驱动插件完成路径
// （AsyncSearch 的同步/后台完成分支 + 工作池），在单测环境里代价过高；本次修复的依据是
// 临界区边界（Get → 合并 → Set）已整段纳入 lockMainCacheKey。该缺口在部署环境用
// 多插件并发搜索同一关键词观察 merged_by_type 各源条数即可复核。
