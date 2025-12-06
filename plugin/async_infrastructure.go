package plugin

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"pansou/config"
	"pansou/model"
)

// ============================================================
// 异步插件基础设施（初始化、工作池、缓存管理）
// ============================================================

// 工作池和统计相关变量
var (
	// API响应缓存，键为关键词，值为缓存的响应（仅内存，不持久化）
	apiResponseCache = sync.Map{}

	// 工作池相关变量
	backgroundWorkerPool chan struct{}
	backgroundTasksCount int32 = 0

	// 统计数据 (仅用于内部监控)
	cacheHits        int64 = 0
	cacheMisses      int64 = 0
	asyncCompletions int64 = 0

	// 初始化标志
	initialized bool = false
	initLock    sync.Mutex

	// 默认配置值
	defaultAsyncResponseTimeout = 4 * time.Second
	defaultPluginTimeout        = 30 * time.Second
	defaultCacheTTL             = 1 * time.Hour // 恢复但仅用于内存缓存
	defaultMaxBackgroundWorkers = 20
	defaultMaxBackgroundTasks   = 100

	// 缓存访问频率记录
	cacheAccessCount = sync.Map{}

	// 缓存清理相关变量
	lastCleanupTime = time.Now()
	cleanupMutex    sync.Mutex
)

// 全局序列化器引用（由主程序设置）
var globalCacheSerializer interface {
	Serialize(interface{}) ([]byte, error)
	Deserialize([]byte, interface{}) error
}

// 缓存响应结构（仅内存，不持久化到磁盘）
type cachedResponse struct {
	Results     []model.SearchResult `json:"results"`
	Timestamp   time.Time            `json:"timestamp"`
	Complete    bool                 `json:"complete"`
	LastAccess  time.Time            `json:"last_access"`
	AccessCount int                  `json:"access_count"`
}

// cleanupExpiredApiCache 清理过期API缓存的函数
func cleanupExpiredApiCache() {
	cleanupMutex.Lock()
	defer cleanupMutex.Unlock()

	now := time.Now()
	// 只有距离上次清理超过30分钟才执行
	if now.Sub(lastCleanupTime) < 30*time.Minute {
		return
	}

	cleanedCount := 0
	totalCount := 0
	deletedKeys := make([]string, 0)

	// 清理已过期的缓存（基于实际TTL + 合理的宽限期）
	apiResponseCache.Range(func(key, value interface{}) bool {
		totalCount++
		if cached, ok := value.(cachedResponse); ok {
			// 使用默认TTL + 30分钟宽限期，避免过于激进的清理
			expireThreshold := defaultCacheTTL + 30*time.Minute
			if now.Sub(cached.Timestamp) > expireThreshold {
				keyStr := key.(string)
				apiResponseCache.Delete(key)
				deletedKeys = append(deletedKeys, keyStr)
				cleanedCount++
			}
		}
		return true
	})

	// 清理访问计数缓存中对应的项
	for _, key := range deletedKeys {
		cacheAccessCount.Delete(key)
	}

	lastCleanupTime = now

	// 记录清理日志（仅在有清理时输出）
	if cleanedCount > 0 {
		fmt.Printf("[Cache] 清理过期缓存: 删除 %d/%d 项，释放内存\n", cleanedCount, totalCount)
	}
}

// initAsyncPlugin 初始化异步插件配置
func initAsyncPlugin() {
	initLock.Lock()
	defer initLock.Unlock()

	if initialized {
		return
	}

	// 如果配置已加载，则从配置读取工作池大小
	maxWorkers := defaultMaxBackgroundWorkers
	if config.AppConfig != nil {
		maxWorkers = config.AppConfig.AsyncMaxBackgroundWorkers
	}

	backgroundWorkerPool = make(chan struct{}, maxWorkers)

	// 异步插件本地缓存系统已移除，现在只依赖主缓存系统

	initialized = true
}

// InitAsyncPluginSystem 导出的初始化函数，用于确保异步插件系统初始化
func InitAsyncPluginSystem() {
	initAsyncPlugin()
}

// acquireWorkerSlot 尝试获取工作槽
func acquireWorkerSlot() bool {
	// 获取最大任务数
	maxTasks := int32(defaultMaxBackgroundTasks)
	if config.AppConfig != nil {
		maxTasks = int32(config.AppConfig.AsyncMaxBackgroundTasks)
	}

	// 检查总任务数
	if atomic.LoadInt32(&backgroundTasksCount) >= maxTasks {
		return false
	}

	// 尝试获取工作槽
	select {
	case backgroundWorkerPool <- struct{}{}:
		atomic.AddInt32(&backgroundTasksCount, 1)
		return true
	default:
		return false
	}
}

// releaseWorkerSlot 释放工作槽
func releaseWorkerSlot() {
	<-backgroundWorkerPool
	atomic.AddInt32(&backgroundTasksCount, -1)
}

// recordCacheHit 记录缓存命中 (内部使用)
func recordCacheHit() {
	atomic.AddInt64(&cacheHits, 1)
}

// recordCacheMiss 记录缓存未命中 (内部使用)
func recordCacheMiss() {
	atomic.AddInt64(&cacheMisses, 1)
}

// recordAsyncCompletion 记录异步完成 (内部使用)
func recordAsyncCompletion() {
	atomic.AddInt64(&asyncCompletions, 1)
}

// recordCacheAccess 记录缓存访问次数，用于智能缓存策略（仅内存）
func recordCacheAccess(key string) {
	// 更新缓存项的访问时间和计数
	if cached, ok := apiResponseCache.Load(key); ok {
		cachedItem := cached.(cachedResponse)
		cachedItem.LastAccess = time.Now()
		cachedItem.AccessCount++
		apiResponseCache.Store(key, cachedItem)
	}

	// 更新全局访问计数
	if count, ok := cacheAccessCount.Load(key); ok {
		cacheAccessCount.Store(key, count.(int)+1)
	} else {
		cacheAccessCount.Store(key, 1)
	}

	// 触发定期清理（异步执行，不阻塞当前操作）
	go cleanupExpiredApiCache()
}

// SetGlobalCacheSerializer 设置全局缓存序列化器（由主程序调用）
func SetGlobalCacheSerializer(serializer interface {
	Serialize(interface{}) ([]byte, error)
	Deserialize([]byte, interface{}) error
}) {
	globalCacheSerializer = serializer
}

// getEnhancedCacheSerializer 获取增强缓存的序列化器
func getEnhancedCacheSerializer() interface {
	Serialize(interface{}) ([]byte, error)
	Deserialize([]byte, interface{}) error
} {
	return globalCacheSerializer
}