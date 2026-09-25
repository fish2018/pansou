package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"pansou/config"
)

// EnhancedTwoLevelCache 改进的两级缓存
type EnhancedTwoLevelCache struct {
	memory     *ShardedMemoryCache
	disk       *ShardedDiskCache
	mutex      sync.RWMutex
	serializer Serializer
}

// NewEnhancedTwoLevelCache 创建新的改进两级缓存
func NewEnhancedTwoLevelCache() (*EnhancedTwoLevelCache, error) {
	// 内存缓存大小为磁盘缓存的60%
	memCacheMaxItems := 5000
	memCacheSizeMB := config.AppConfig.CacheMaxSizeMB * 3 / 5

	memCache := NewShardedMemoryCache(memCacheMaxItems, memCacheSizeMB)
	memCache.StartCleanupTask()

	// 创建优化的分片磁盘缓存，使用动态分片数量
	diskCache, err := NewOptimizedShardedDiskCache(config.AppConfig.CachePath, config.AppConfig.CacheMaxSizeMB)
	if err != nil {
		return nil, err
	}

	// 创建序列化器
	serializer := NewGobSerializer()

	// 设置内存缓存的磁盘缓存引用，用于LRU淘汰时的备份
	memCache.SetDiskCacheReference(diskCache)

	return &EnhancedTwoLevelCache{
		memory:     memCache,
		disk:       diskCache,
		serializer: serializer,
	}, nil
}

// Set 设置缓存
func (c *EnhancedTwoLevelCache) Set(key string, data []byte, ttl time.Duration) error {
	// 获取当前时间作为最后修改时间
	now := time.Now()

	// 先设置内存缓存（这是快速操作，直接在当前goroutine中执行）
	c.memory.SetWithTimestamp(key, data, ttl, now)

	// 异步设置磁盘缓存（这是IO操作，可能较慢）
	go func(k string, d []byte, t time.Duration) {
		// 使用独立的goroutine写入磁盘，避免阻塞调用者
		//
		// 磁盘写失败原先被 _ = 完全吞掉：内存里还有值，看起来一切正常，直到重启后
		// 缓存全空、且没有任何线索。异步写没法把错误回传给调用方，所以至少要让它在
		// 日志里可见——但必须限流，否则磁盘故障时每次 Set 都打一行，会把日志刷爆。
		if err := c.disk.Set(k, d, t); err != nil {
			logDiskWriteFailure(err)
		}
	}(key, data, ttl)

	return nil
}

// SetMemoryOnly 仅更新内存缓存
func (c *EnhancedTwoLevelCache) SetMemoryOnly(key string, data []byte, ttl time.Duration) error {
	now := time.Now()

	// 只更新内存缓存，不触发磁盘写入
	c.memory.SetWithTimestamp(key, data, ttl, now)

	return nil
}

// SetBothLevels 更新内存和磁盘缓存
func (c *EnhancedTwoLevelCache) SetBothLevels(key string, data []byte, ttl time.Duration) error {
	now := time.Now()

	// 同步更新内存缓存
	c.memory.SetWithTimestamp(key, data, ttl, now)

	// 同步更新磁盘缓存，确保数据立即写入
	return c.disk.Set(key, data, ttl)
}

// SetWithFinalFlag 根据结果状态选择更新策略
func (c *EnhancedTwoLevelCache) SetWithFinalFlag(key string, data []byte, ttl time.Duration, isFinal bool) error {
	if isFinal {
		return c.SetBothLevels(key, data, ttl)
	} else {
		return c.SetMemoryOnly(key, data, ttl)
	}
}

// Get 获取缓存
func (c *EnhancedTwoLevelCache) Get(key string) ([]byte, bool, error) {

	// 检查内存缓存
	data, _, memHit := c.memory.GetWithTimestamp(key)
	if memHit {
		return data, true, nil
	}

	// 尝试从磁盘读取数据
	diskData, diskHit, diskErr := c.disk.Get(key)
	if diskErr == nil && diskHit {
		// 磁盘缓存命中，更新内存缓存。
		//
		// 这里必须按磁盘条目的**剩余**寿命回填，不能一律用 CacheTTLMinutes：
		// 内存缓存的 SetWithTimestamp 是 expiry = now + ttl，重新计时会让
		// 磁盘上按短 TTL（CachePartialTTLMinutes，默认 3 分钟）落盘的部分结果
		// 在内存里活满完整 TTL（默认 60 分钟），service 侧读缓存又不校验新鲜度
		// （search_service.go 注释明写"不检查新鲜度"），短 TTL 分流因此被整个绕过。
		ttl := time.Duration(config.AppConfig.CacheTTLMinutes) * time.Minute
		if expiry, ok := c.disk.GetExpiry(key); ok {
			remaining := time.Until(expiry)
			if remaining <= 0 {
				// 磁盘条目其实已过期（可能尚未被清理任务删掉），不能复活它
				return nil, false, nil
			}
			ttl = remaining
		}
		diskLastModified, _ := c.disk.GetLastModified(key)
		c.memory.SetWithTimestamp(key, diskData, ttl, diskLastModified)
		return diskData, true, nil
	}

	return nil, false, nil
}

// Delete 删除缓存
func (c *EnhancedTwoLevelCache) Delete(key string) error {
	// 从内存缓存删除
	c.memory.Delete(key)

	// 从磁盘缓存删除
	return c.disk.Delete(key)
}

// Clear 清空所有缓存
func (c *EnhancedTwoLevelCache) Clear() error {
	// 清空内存缓存
	c.memory.Clear()

	// 清空磁盘缓存
	return c.disk.Clear()
}

// 设置序列化器
func (c *EnhancedTwoLevelCache) SetSerializer(serializer Serializer) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.serializer = serializer
}

// 获取序列化器
func (c *EnhancedTwoLevelCache) GetSerializer() Serializer {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	return c.serializer
}

// FlushMemoryToDisk 将内存缓存中的所有数据刷新到磁盘
func (c *EnhancedTwoLevelCache) FlushMemoryToDisk() error {
	// 获取内存缓存中的所有键值对
	allItems := c.memory.GetAllItems()

	var lastErr error

	for key, item := range allItems {
		// 同步写入到磁盘缓存
		if err := c.disk.Set(key, item.Data, item.TTL); err != nil {
			fmt.Printf("[内存同步] 同步失败: %s -> %v\n", key, err)
			lastErr = err
			continue
		}
	}

	return lastErr
}

// diskWriteFailureLogInterval 是磁盘写失败日志的最小间隔。
// 磁盘故障（例如写满）会让每次 Set 都失败，不限流会把日志刷爆、反而看不到别的问题。
const diskWriteFailureLogInterval = 30 * time.Second

var (
	diskWriteFailureLastLog int64 // 上一次打印的时间戳（UnixNano，atomic）
	diskWriteFailureSkipped int64 // 被限流跳过的次数（atomic）
)

// logDiskWriteFailure 记录磁盘写失败，按 diskWriteFailureLogInterval 限流。
// 限流期间累积的失败次数会在下一次真正打印时一并报出，保证"数量"这个信息不丢。
func logDiskWriteFailure(err error) {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&diskWriteFailureLastLog)
	if last != 0 && now-last < int64(diskWriteFailureLogInterval) {
		atomic.AddInt64(&diskWriteFailureSkipped, 1)
		return
	}
	if !atomic.CompareAndSwapInt64(&diskWriteFailureLastLog, last, now) {
		// 另一个 goroutine 刚打印过，这次让给它
		atomic.AddInt64(&diskWriteFailureSkipped, 1)
		return
	}
	skipped := atomic.SwapInt64(&diskWriteFailureSkipped, 0)
	if skipped > 0 {
		fmt.Printf("[CACHE] 磁盘缓存写入失败（另有 %d 次同类失败被限流跳过）: %v\n", skipped, err)
		return
	}
	fmt.Printf("[CACHE] 磁盘缓存写入失败: %v\n", err)
}
