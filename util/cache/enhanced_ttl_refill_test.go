package cache

import (
	"testing"
	"time"

	"pansou/config"
)

// 磁盘→内存回填必须按条目的**剩余**寿命计时。
//
// 原始缺陷：回填时一律用 config.AppConfig.CacheTTLMinutes（默认 60 分钟）重新计时，
// 而磁盘上按短 TTL（CachePartialTTLMinutes，默认 3 分钟）落盘的部分结果因此获得
// 完整寿命；service 侧读缓存又不校验新鲜度，短 TTL 分流被整个绕过。
//
// 用例走真实链路：短 TTL 写两级 → 把内存项删掉制造磁盘命中 → Get 触发回填 →
// 等过短 TTL → 内存里必须已经失效。修复前它会继续命中 60 分钟。
func TestDiskRefillUsesRemainingTTL(t *testing.T) {
	withCacheConfig(t, 60, t.TempDir())

	c, err := NewEnhancedTwoLevelCache()
	if err != nil {
		t.Fatal(err)
	}
	key := "partial-result"
	data := []byte(`{"results":[1]}`)
	const shortTTL = 150 * time.Millisecond

	if err := c.SetBothLevels(key, data, shortTTL); err != nil {
		t.Fatal(err)
	}
	if _, expiryOK := c.disk.GetExpiry(key); !expiryOK {
		t.Fatal("磁盘未记录过期时间，无法按剩余寿命回填")
	}

	// 制造磁盘命中：内存项先移除，Get 会从磁盘回填
	c.memory.Delete(key)
	if got, hit, err := c.Get(key); err != nil || !hit || string(got) != string(data) {
		t.Fatalf("磁盘回填失败: hit=%v err=%v", hit, err)
	}

	// 回填后内存项的剩余寿命应约等于 shortTTL 的剩余量，而不是 60 分钟
	time.Sleep(250 * time.Millisecond)
	if _, hit, _ := c.Get(key); hit {
		t.Error("短 TTL 的结果在内存里活过了自己的寿命，说明回填用了完整 TTL 重新计时")
	}
}

func withCacheConfig(t *testing.T, ttlMinutes int, path string) {
	t.Helper()
	saved := config.AppConfig
	config.AppConfig = &config.Config{
		CacheTTLMinutes:        ttlMinutes,
		CachePath:              path,
		CacheMaxSizeMB:         10,
		CacheEnabled:           true,
		CachePartialTTLMinutes: 0,
	}
	t.Cleanup(func() { config.AppConfig = saved })
}
