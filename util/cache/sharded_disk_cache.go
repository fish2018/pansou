package cache

import (
	"fmt"
	"hash/fnv"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// ShardedDiskCache 分片磁盘缓存
type ShardedDiskCache struct {
	baseDir    string
	shardCount int
	shardMask  uint32 // 用于快速取模的掩码
	shards     []*DiskCache
	maxSizeMB  int
	mutex      sync.RWMutex
}

// NewShardedDiskCache 创建新的分片磁盘缓存（兼容现有接口）
func NewShardedDiskCache(baseDir string, shardCount, maxSizeMB int) (*ShardedDiskCache, error) {
	return newShardedDiskCacheWithCount(baseDir, shardCount, maxSizeMB)
}

// NewOptimizedShardedDiskCache 创建优化的分片磁盘缓存（动态分片数）
func NewOptimizedShardedDiskCache(baseDir string, maxSizeMB int) (*ShardedDiskCache, error) {
	// 动态确定分片数量：与内存缓存保持一致的策略
	shardCount := runtime.NumCPU() * 2
	if shardCount < 4 {
		shardCount = 4
	}
	if shardCount > 32 { // 磁盘缓存分片数适当限制，避免过多文件夹
		shardCount = 32
	}

	// 确保分片数是2的幂，便于使用掩码进行快速取模
	shardCount = nextPowerOfTwoDisk(shardCount)

	return newShardedDiskCacheWithCount(baseDir, shardCount, maxSizeMB)
}

// 获取下一个2的幂（磁盘缓存版本）
func nextPowerOfTwoDisk(n int) int {
	if n <= 1 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	return n + 1
}

// 内部构造函数
func newShardedDiskCacheWithCount(baseDir string, shardCount, maxSizeMB int) (*ShardedDiskCache, error) {
	// 确保每个分片的大小合理
	shardSize := maxSizeMB / shardCount
	if shardSize < 1 {
		shardSize = 1
	}

	cache := &ShardedDiskCache{
		baseDir:    baseDir,
		shardCount: shardCount,
		shardMask:  uint32(shardCount - 1), // 用于快速取模
		shards:     make([]*DiskCache, shardCount),
		maxSizeMB:  maxSizeMB,
	}

	// 初始化每个分片
	for i := 0; i < shardCount; i++ {
		shardPath := filepath.Join(baseDir, fmt.Sprintf("shard_%d", i))
		diskCache, err := NewDiskCache(shardPath, shardSize)
		if err != nil {
			return nil, err
		}
		cache.shards[i] = diskCache
	}

	return cache, nil
}

// 获取键对应的分片
func (c *ShardedDiskCache) getShard(key string) *DiskCache {
	// 计算哈希值决定分片
	h := fnv.New32a()
	h.Write([]byte(key))
	shardIndex := h.Sum32() & c.shardMask // 使用掩码进行快速取模
	return c.shards[shardIndex]
}

// Set 设置缓存
func (c *ShardedDiskCache) Set(key string, data []byte, ttl time.Duration) error {
	shard := c.getShard(key)
	return shard.Set(key, data, ttl)
}

// Get 获取缓存
func (c *ShardedDiskCache) Get(key string) ([]byte, bool, error) {
	shard := c.getShard(key)
	return shard.Get(key)
}

// Delete 删除缓存
func (c *ShardedDiskCache) Delete(key string) error {
	shard := c.getShard(key)
	return shard.Delete(key)
}

// Has 检查缓存是否存在
func (c *ShardedDiskCache) Has(key string) bool {
	shard := c.getShard(key)
	return shard.Has(key)
}

// Clear 清空所有缓存
func (c *ShardedDiskCache) Clear() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	var lastErr error
	for _, shard := range c.shards {
		if err := shard.Clear(); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// GetLastModified 获取缓存项的最后修改时间
func (c *ShardedDiskCache) GetLastModified(key string) (time.Time, bool) {
	shard := c.getShard(key)
	return shard.GetLastModified(key)
}

// cleanExpired 清理所有分片中的过期项
func (c *ShardedDiskCache) cleanExpired() {
	// 并行清理所有分片中的过期项
	for _, shard := range c.shards {
		go func(s *DiskCache) {
			s.cleanExpired()
		}(shard)
	}
}

// CleanExpired 公开的清理方法，符合cleanupTarget接口
func (c *ShardedDiskCache) CleanExpired() {
	c.cleanExpired()
}

// StartCleanupTask 启动定期清理任务（修改为使用单例模式）
func (c *ShardedDiskCache) StartCleanupTask() {
	// 使用与内存缓存相同的全局清理系统
	registerForCleanup(c)
	startGlobalCleanupTask()
}

// GetShards 获取所有分片（用于测试和调试）
func (c *ShardedDiskCache) GetShards() []*DiskCache {
	return c.shards
}

// GetShardIndex 获取指定键对应的分片索引（用于测试和调试）
func (c *ShardedDiskCache) GetShardIndex(key string) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	if c.shardMask > 0 {
		return int(h.Sum32() & c.shardMask)
	} else {
		// 兼容老版本的模运算
		return int(h.Sum32()) % c.shardCount
	}
}
