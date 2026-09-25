package cache

import (
	"sync"
	"time"
)

// 简单的内存缓存项
type memoryCacheItem struct {
	data         []byte
	expiry       time.Time
	lastUsed     time.Time
	lastModified time.Time // 添加最后修改时间
	size         int
}

// 内存缓存
type MemoryCache struct {
	items    map[string]*memoryCacheItem
	mutex    sync.RWMutex
	maxItems int
	maxSize  int64
	currSize int64
}

// 创建新的内存缓存
func NewMemoryCache(maxItems int, maxSizeMB int) *MemoryCache {
	return &MemoryCache{
		items:    make(map[string]*memoryCacheItem),
		maxItems: maxItems,
		maxSize:  int64(maxSizeMB) * 1024 * 1024,
	}
}

// 设置缓存
func (c *MemoryCache) Set(key string, data []byte, ttl time.Duration) {
	c.SetWithTimestamp(key, data, ttl, time.Now())
}

// SetWithTimestamp 设置缓存，并指定最后修改时间
func (c *MemoryCache) SetWithTimestamp(key string, data []byte, ttl time.Duration, lastModified time.Time) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// 如果已存在，先减去旧项的大小
	if item, exists := c.items[key]; exists {
		c.currSize -= int64(item.size)
	}

	// 创建新的缓存项
	now := time.Now()
	item := &memoryCacheItem{
		data:         data,
		expiry:       now.Add(ttl),
		lastUsed:     now,
		lastModified: lastModified,
		size:         len(data),
	}

	// 检查是否需要清理空间
	if len(c.items) >= c.maxItems || c.currSize+int64(len(data)) > c.maxSize {
		c.evict()
	}

	// 存储新项
	c.items[key] = item
	c.currSize += int64(len(data))
}

// 获取缓存
func (c *MemoryCache) Get(key string) ([]byte, bool) {
	c.mutex.RLock()
	item, exists := c.items[key]
	c.mutex.RUnlock()

	if !exists {
		return nil, false
	}

	// 检查是否过期
	if time.Now().After(item.expiry) {
		c.mutex.Lock()
		delete(c.items, key)
		c.currSize -= int64(item.size)
		c.mutex.Unlock()
		return nil, false
	}

	// 更新最后使用时间
	c.mutex.Lock()
	item.lastUsed = time.Now()
	c.mutex.Unlock()

	return item.data, true
}

// GetWithTimestamp 获取缓存及其最后修改时间
func (c *MemoryCache) GetWithTimestamp(key string) ([]byte, time.Time, bool) {
	c.mutex.RLock()
	item, exists := c.items[key]
	c.mutex.RUnlock()

	if !exists {
		return nil, time.Time{}, false
	}

	// 检查是否过期
	if time.Now().After(item.expiry) {
		c.mutex.Lock()
		delete(c.items, key)
		c.currSize -= int64(item.size)
		c.mutex.Unlock()
		return nil, time.Time{}, false
	}

	// 更新最后使用时间
	c.mutex.Lock()
	item.lastUsed = time.Now()
	c.mutex.Unlock()

	return item.data, item.lastModified, true
}

// GetLastModified 获取缓存项的最后修改时间
func (c *MemoryCache) GetLastModified(key string) (time.Time, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	item, exists := c.items[key]
	if !exists {
		return time.Time{}, false
	}

	// 检查是否过期
	if time.Now().After(item.expiry) {
		return time.Time{}, false
	}

	return item.lastModified, true
}

// 驱逐策略 - LRU
func (c *MemoryCache) evict() {
	// 找出最久未使用的项
	var oldestKey string
	var oldestTime time.Time

	// 初始化为当前时间
	oldestTime = time.Now()

	for k, v := range c.items {
		if v.lastUsed.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.lastUsed
		}
	}

	// 如果找到了最久未使用的项，删除它
	if oldestKey != "" {
		item := c.items[oldestKey]
		c.currSize -= int64(item.size)
		delete(c.items, oldestKey)
	}
}

// 清理过期项
func (c *MemoryCache) CleanExpired() {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	now := time.Now()
	for k, v := range c.items {
		if now.After(v.expiry) {
			c.currSize -= int64(v.size)
			delete(c.items, k)
		}
	}
}

// 启动定期清理
func (c *MemoryCache) StartCleanupTask() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for range ticker.C {
			c.CleanExpired()
		}
	}()
}
