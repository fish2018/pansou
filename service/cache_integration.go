package service

import (
	"fmt"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/cache"
)

// CacheWriteIntegration 缓存写入集成层
type CacheWriteIntegration struct {
	batchManager *cache.DelayedBatchWriteManager
	mainCache    *cache.EnhancedTwoLevelCache
	strategy     cache.CacheWriteStrategy
	initialized  bool
}

// NewCacheWriteIntegration 创建缓存写入集成
func NewCacheWriteIntegration(mainCache *cache.EnhancedTwoLevelCache) (*CacheWriteIntegration, error) {
	// 创建延迟批量写入管理器
	batchManager, err := cache.NewDelayedBatchWriteManager()
	if err != nil {
		return nil, fmt.Errorf("创建批量写入管理器失败: %v", err)
	}

	integration := &CacheWriteIntegration{
		batchManager: batchManager,
		mainCache:    mainCache,
	}

	// 设置主缓存更新函数
	batchManager.SetMainCacheUpdater(integration.createMainCacheUpdater())

	// 初始化管理器
	if err := batchManager.Initialize(); err != nil {
		return nil, fmt.Errorf("初始化批量写入管理器失败: %v", err)
	}

	integration.initialized = true

	fmt.Printf("[缓存写入集成] 初始化完成\n")
	return integration, nil
}

// createMainCacheUpdater 创建主缓存更新函数
func (c *CacheWriteIntegration) createMainCacheUpdater() func(string, []byte, time.Duration) error {
	return func(key string, data []byte, ttl time.Duration) error {
		// 调用现有的缓存系统进行实际写入
		return c.mainCache.SetBothLevels(key, data, ttl)
	}
}

// HandleCacheWrite 处理缓存写入请求
func (c *CacheWriteIntegration) HandleCacheWrite(key string, results []model.SearchResult, ttl time.Duration, isFinal bool, keyword string, pluginName string) error {
	if !c.initialized {
		return fmt.Errorf("缓存写入集成未初始化")
	}

	// 计算插件优先级
	priority := c.getPluginPriority(pluginName)

	// 计算数据大小（估算）
	dataSize := c.estimateDataSize(results)

	// 创建缓存操作
	operation := &cache.CacheOperation{
		Key:        key,
		Data:       results,
		TTL:        ttl,
		PluginName: pluginName,
		Keyword:    keyword,
		Timestamp:  time.Now(),
		Priority:   priority,
		DataSize:   dataSize,
		IsFinal:    isFinal,
	}

	// 调用批量写入管理器处理
	return c.batchManager.HandleCacheOperation(operation)
}

// getPluginPriority 获取插件优先级
func (c *CacheWriteIntegration) getPluginPriority(pluginName string) int {
	// 从插件管理器动态获取真实的优先级
	if pluginInstance, exists := plugin.GetPluginByName(pluginName); exists {
		return pluginInstance.Priority()
	}

	// 如果插件不存在，返回默认等级4（最低优先级）
	return 4
}

// estimateDataSize 估算数据大小
func (c *CacheWriteIntegration) estimateDataSize(results []model.SearchResult) int {
	// 简化估算：每个结果约500字节
	return len(results) * 500
}

// Shutdown 优雅关闭
func (c *CacheWriteIntegration) Shutdown(timeout time.Duration) error {
	if !c.initialized {
		return nil
	}

	return c.batchManager.Shutdown(timeout)
}

// GetStats 获取统计信息
func (c *CacheWriteIntegration) GetStats() interface{} {
	if !c.initialized {
		return nil
	}

	return c.batchManager.GetStats()
}

// SetStrategy 设置写入策略
func (c *CacheWriteIntegration) SetStrategy(strategy cache.CacheWriteStrategy) {
	c.strategy = strategy
}

// GetStrategy 获取当前策略
func (c *CacheWriteIntegration) GetStrategy() cache.CacheWriteStrategy {
	return c.strategy
}
