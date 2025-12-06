package service

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"pansou/config"
	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/cache"
	"pansou/util/pool"
)

// ============================================================
// 核心搜索服务
// ============================================================

// 全局缓存写入管理器引用（避免循环依赖）
var globalCacheWriteManager *cache.DelayedBatchWriteManager

// SetGlobalCacheWriteManager 设置全局缓存写入管理器
func SetGlobalCacheWriteManager(manager *cache.DelayedBatchWriteManager) {
	globalCacheWriteManager = manager
}

// GetGlobalCacheWriteManager 获取全局缓存写入管理器
func GetGlobalCacheWriteManager() *cache.DelayedBatchWriteManager {
	return globalCacheWriteManager
}

// GetEnhancedTwoLevelCache 获取增强版两级缓存实例
func GetEnhancedTwoLevelCache() *cache.EnhancedTwoLevelCache {
	return enhancedTwoLevelCache
}

// 全局缓存实例和缓存是否初始化标志
var (
	enhancedTwoLevelCache *cache.EnhancedTwoLevelCache
	cacheInitialized      bool
)

// 初始化缓存
func init() {
	if config.AppConfig != nil && config.AppConfig.CacheEnabled {
		var err error
		// 使用增强版缓存
		enhancedTwoLevelCache, err = cache.NewEnhancedTwoLevelCache()
		if err == nil {
			cacheInitialized = true
		}
	}
}

// injectMainCacheToAsyncPlugins 将主缓存注入到异步插件中
func injectMainCacheToAsyncPlugins(pluginManager *plugin.PluginManager, mainCache *cache.EnhancedTwoLevelCache) {
	// 如果缓存或插件管理器不可用，直接返回
	if mainCache == nil || pluginManager == nil {
		return
	}

	// 设置全局序列化器，确保异步插件与主程序使用相同的序列化格式
	serializer := mainCache.GetSerializer()
	if serializer != nil {
		plugin.SetGlobalCacheSerializer(serializer)
	}

	// 创建缓存更新函数（支持IsFinal参数）- 接收原始数据并与现有缓存合并
	cacheUpdater := func(key string, newResults []model.SearchResult, ttl time.Duration, isFinal bool, keyword string, pluginName string) error {
		// 优化：如果新结果为空，跳过缓存更新（避免无效操作）
		if len(newResults) == 0 {
			return nil
		}

		// 获取现有缓存数据进行合并
		var finalResults []model.SearchResult
		if existingData, hit, err := mainCache.Get(key); err == nil && hit {
			var existingResults []model.SearchResult
			if err := mainCache.GetSerializer().Deserialize(existingData, &existingResults); err == nil {
				// 合并新旧结果，去重保留最完整的数据
				finalResults = mergeSearchResults(existingResults, newResults)
				if config.AppConfig != nil && config.AppConfig.AsyncLogEnabled {
					if keyword != "" {
						fmt.Printf("🔄 [%s:%s] 更新缓存| 原有: %d + 新增: %d = 合并后: %d\n",
							pluginName, keyword, len(existingResults), len(newResults), len(finalResults))
					}
				}
			} else {
				// 反序列化失败，使用新结果
				finalResults = newResults
				if config.AppConfig != nil && config.AppConfig.AsyncLogEnabled {
					displayKey := key[:8] + "..."
					if keyword != "" {
						fmt.Printf("[异步插件 %s] 缓存反序列化失败，使用新结果: %s(关键词:%s) | 结果数: %d\n", pluginName, displayKey, keyword, len(newResults))
					} else {
						fmt.Printf("[异步插件 %s] 缓存反序列化失败，使用新结果: %s | 结果数: %d\n", pluginName, key, len(newResults))
					}
				}
			}
		} else {
			// 无现有缓存，直接使用新结果
			finalResults = newResults
			if config.AppConfig != nil && config.AppConfig.AsyncLogEnabled {
				displayKey := key[:8] + "..."
				if keyword != "" {
					fmt.Printf("[异步插件 %s] 初始缓存创建: %s(关键词:%s) | 结果数: %d\n", pluginName, displayKey, keyword, len(newResults))
				} else {
					fmt.Printf("[异步插件 %s] 初始缓存创建: %s | 结果数: %d\n", pluginName, key, len(newResults))
				}
			}
		}

		// 序列化合并后的结果
		data, err := mainCache.GetSerializer().Serialize(finalResults)
		if err != nil {
			fmt.Printf("[缓存更新] 序列化失败: %s | 错误: %v\n", key, err)
			return err
		}

		// 先更新内存缓存（立即可见）
		if err := mainCache.SetMemoryOnly(key, data, ttl); err != nil {
			return fmt.Errorf("内存缓存更新失败: %v", err)
		}

		// 使用新的缓存写入管理器处理磁盘写入（智能批处理）
		if cacheWriteManager := globalCacheWriteManager; cacheWriteManager != nil {
			operation := &cache.CacheOperation{
				Key:        key,
				Data:       finalResults, // 使用原始数据而不是序列化后的
				TTL:        ttl,
				IsFinal:    isFinal,
				PluginName: pluginName,
				Keyword:    keyword,
				Priority:   2,             // 中等优先级
				Timestamp:  time.Now(),
				DataSize:   len(data), // 序列化后的数据大小
			}

			// 根据是否为最终结果设置优先级
			if isFinal {
				operation.Priority = 1 // 高优先级
			}

			return cacheWriteManager.HandleCacheOperation(operation)
		}

		// 兜底：如果缓存写入管理器不可用，使用原有逻辑
		if isFinal {
			return mainCache.SetBothLevels(key, data, ttl)
		} else {
			return nil // 内存已更新，磁盘稍后批处理
		}
	}

	// 获取所有插件
	plugins := pluginManager.GetPlugins()

	// 遍历所有插件，找出异步插件
	for _, p := range plugins {
		// 检查插件是否实现了SetMainCacheUpdater方法（修复后的签名，增加关键词参数）
		if asyncPlugin, ok := p.(interface {
			SetMainCacheUpdater(func(string, []model.SearchResult, time.Duration, bool, string) error)
		}); ok {
			// 为每个插件创建专门的缓存更新函数，绑定插件名称
			pluginName := p.Name()
			pluginCacheUpdater := func(key string, newResults []model.SearchResult, ttl time.Duration, isFinal bool, keyword string) error {
				return cacheUpdater(key, newResults, ttl, isFinal, keyword, pluginName)
			}
			// 注入缓存更新函数
			asyncPlugin.SetMainCacheUpdater(pluginCacheUpdater)
		}
	}
}

// SearchService 搜索服务
type SearchService struct {
	pluginManager *plugin.PluginManager
}

// NewSearchService 创建搜索服务实例并确保缓存可用
func NewSearchService(pluginManager *plugin.PluginManager) *SearchService {
	// 检查缓存是否已初始化，如果未初始化则尝试重新初始化
	if !cacheInitialized && config.AppConfig != nil && config.AppConfig.CacheEnabled {
		var err error
		enhancedTwoLevelCache, err = cache.NewEnhancedTwoLevelCache()
		if err == nil {
			cacheInitialized = true
		}
	}

	// 将主缓存注入到异步插件中
	injectMainCacheToAsyncPlugins(pluginManager, enhancedTwoLevelCache)

	// 确保缓存写入管理器设置了主缓存更新函数
	if globalCacheWriteManager != nil && enhancedTwoLevelCache != nil {
		globalCacheWriteManager.SetMainCacheUpdater(func(key string, data []byte, ttl time.Duration) error {
			return enhancedTwoLevelCache.SetBothLevels(key, data, ttl)
		})
	}

	return &SearchService{
		pluginManager: pluginManager,
	}
}

// Search 执行搜索
func (s *SearchService) Search(keyword string, channels []string, concurrency int, forceRefresh bool, resultType string, sourceType string, plugins []string, cloudTypes []string, ext map[string]interface{}) (model.SearchResponse, error) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}

	// 参数预处理
	if sourceType == "" {
		sourceType = "all"
	}

	// 插件参数规范化处理
	plugins = normalizePluginsParam(s, sourceType, plugins)

	// 如果未指定并发数，使用配置中的默认值
	if concurrency <= 0 {
		concurrency = config.AppConfig.DefaultConcurrency
	}

	// 并行获取TG搜索和插件搜索结果
	var tgResults []model.SearchResult
	var pluginResults []model.SearchResult

	var wg sync.WaitGroup
	var tgErr, pluginErr error

	// 如果需要搜索TG
	if sourceType == "all" || sourceType == "tg" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tgResults, tgErr = s.searchTG(keyword, channels, forceRefresh)
		}()
	}

	// 如果需要搜索插件
	if (sourceType == "all" || sourceType == "plugin") && config.AppConfig.AsyncPluginEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pluginResults, pluginErr = s.searchPlugins(keyword, plugins, forceRefresh, concurrency, ext)
		}()
	}

	// 等待所有搜索完成
	wg.Wait()

	// 检查错误
	if tgErr != nil {
		return model.SearchResponse{}, tgErr
	}
	if pluginErr != nil {
		return model.SearchResponse{}, pluginErr
	}

	// 合并结果
	allResults := mergeSearchResults(tgResults, pluginResults)

	// 排序结果
	sortResultsByTimeAndKeywords(allResults)

	// 过滤结果
	filteredForResults := filterResults(allResults)

	// 合并链接按网盘类型分组
	mergedLinks := mergeResultsByType(allResults, keyword, cloudTypes)

	// 构建响应
	total := calculateTotal(resultType, filteredForResults, mergedLinks)

	response := model.SearchResponse{
		Total:        total,
		Results:      filteredForResults,
		MergedByType: mergedLinks,
	}

	return filterResponseByType(response, resultType), nil
}

// normalizePluginsParam 规范化插件参数
func normalizePluginsParam(s *SearchService, sourceType string, plugins []string) []string {
	if sourceType == "tg" {
		return nil
	}

	if sourceType == "all" || sourceType == "plugin" {
		if plugins == nil || len(plugins) == 0 {
			return nil
		}

		// 检查是否有非空元素
		hasNonEmpty := false
		for _, p := range plugins {
			if p != "" {
				hasNonEmpty = true
				break
			}
		}

		if !hasNonEmpty {
			return nil
		}

		// 检查是否包含所有插件
		if includesAllPlugins(s, plugins) {
			return nil
		}
	}

	return plugins
}

// includesAllPlugins 检查是否包含所有插件
func includesAllPlugins(s *SearchService, plugins []string) bool {
	allPlugins := s.pluginManager.GetPlugins()
	allPluginNames := make([]string, 0, len(allPlugins))
	for _, p := range allPlugins {
		allPluginNames = append(allPluginNames, strings.ToLower(p.Name()))
	}

	requestedPlugins := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if p != "" {
			requestedPlugins = append(requestedPlugins, strings.ToLower(p))
		}
	}

	if len(requestedPlugins) != len(allPluginNames) {
		return false
	}

	pluginMap := make(map[string]bool)
	for _, p := range requestedPlugins {
		pluginMap[p] = true
	}

	for _, name := range allPluginNames {
		if !pluginMap[name] {
			return false
		}
	}

	return true
}

// filterResults 过滤结果
func filterResults(allResults []model.SearchResult) []model.SearchResult {
	filteredForResults := make([]model.SearchResult, 0, len(allResults))
	for _, result := range allResults {
		source := getResultSource(result)
		pluginLevel := getPluginLevelBySource(source)

		if !result.Datetime.IsZero() || getKeywordPriority(result.Title) > 0 || pluginLevel <= 2 {
			filteredForResults = append(filteredForResults, result)
		}
	}
	return filteredForResults
}

// calculateTotal 计算总数
func calculateTotal(resultType string, filteredResults []model.SearchResult, mergedLinks model.MergedLinks) int {
	if resultType == "merged_by_type" {
		total := 0
		for _, links := range mergedLinks {
			total += len(links)
		}
		return total
	}
	return len(filteredResults)
}

// searchTG 搜索TG频道
func (s *SearchService) searchTG(keyword string, channels []string, forceRefresh bool) ([]model.SearchResult, error) {
	cacheKey := cache.GenerateTGCacheKey(keyword, channels)

	// 尝试从缓存获取
	if !forceRefresh && cacheInitialized && config.AppConfig.CacheEnabled {
		if enhancedTwoLevelCache != nil {
			data, hit, err := enhancedTwoLevelCache.Get(cacheKey)

			if err == nil && hit {
				var results []model.SearchResult
				if err := enhancedTwoLevelCache.GetSerializer().Deserialize(data, &results); err == nil {
					return results, nil
				}
			}
		}
	}

	// 执行实际搜索
	var results []model.SearchResult

	tasks := make([]pool.Task, 0, len(channels))

	for _, channel := range channels {
		ch := channel
		tasks = append(tasks, func() interface{} {
			results, err := s.searchChannel(keyword, ch)
			if err != nil {
				return nil
			}
			return results
		})
	}

	taskResults := pool.ExecuteBatchWithTimeout(tasks, len(channels), config.AppConfig.PluginTimeout)

	for _, result := range taskResults {
		if result != nil {
			channelResults := result.([]model.SearchResult)
			results = append(results, channelResults...)
		}
	}

	// 异步缓存结果
	if cacheInitialized && config.AppConfig.CacheEnabled {
		go func(res []model.SearchResult) {
			ttl := time.Duration(config.AppConfig.CacheTTLMinutes) * time.Minute

			if enhancedTwoLevelCache != nil {
				data, err := enhancedTwoLevelCache.GetSerializer().Serialize(res)
				if err != nil {
					return
				}
				enhancedTwoLevelCache.Set(cacheKey, data, ttl)
			}
		}(results)
	}

	return results, nil
}

// searchChannel 搜索单个频道
func (s *SearchService) searchChannel(keyword string, channel string) ([]model.SearchResult, error) {
	url := util.BuildSearchURL(channel, keyword, "")
	client := util.GetHTTPClient()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	results, _, err := util.ParseSearchResults(string(body), channel)
	if err != nil {
		return nil, err
	}

	return results, nil
}

// searchPlugins 搜索插件
func (s *SearchService) searchPlugins(keyword string, plugins []string, forceRefresh bool, concurrency int, ext map[string]interface{}) ([]model.SearchResult, error) {
	if ext == nil {
		ext = make(map[string]interface{})
	}

	if forceRefresh {
		ext["refresh"] = true
	}

	cacheKey := cache.GeneratePluginCacheKey(keyword, plugins)

	// 尝试从缓存获取
	if !forceRefresh && cacheInitialized && config.AppConfig.CacheEnabled {
		if enhancedTwoLevelCache != nil {
			data, hit, err := enhancedTwoLevelCache.Get(cacheKey)

			if err == nil && hit {
				var results []model.SearchResult
				if err := enhancedTwoLevelCache.GetSerializer().Deserialize(data, &results); err == nil {
					fmt.Printf("✅ [%s] 命中缓存 结果数: %d\n", keyword, len(results))
					return results, nil
				}
			}
		}
	}

	// 执行实际搜索
	availablePlugins := selectAvailablePlugins(s, plugins)

	if concurrency <= 0 {
		concurrency = config.AppConfig.DefaultConcurrency
	}

	tasks := make([]pool.Task, 0, len(availablePlugins))
	for _, p := range availablePlugins {
		plugin := p
		tasks = append(tasks, func() interface{} {
			plugin.SetMainCacheKey(cacheKey)
			plugin.SetCurrentKeyword(keyword)

			results, err := plugin.AsyncSearch(keyword, func(client *http.Client, kw string, extParams map[string]interface{}) ([]model.SearchResult, error) {
				return plugin.Search(kw, extParams)
			}, cacheKey, ext)

			if err != nil {
				return nil
			}
			return results
		})
	}

	results := pool.ExecuteBatchWithTimeout(tasks, concurrency, config.AppConfig.PluginTimeout)

	var allResults []model.SearchResult
	for _, result := range results {
		if result != nil {
			pluginResults := result.([]model.SearchResult)
			for _, pluginResult := range pluginResults {
				if len(pluginResult.Links) > 0 {
					allResults = append(allResults, pluginResult)
				}
			}
		}
	}

	// 缓存结果
	if cacheInitialized && config.AppConfig.CacheEnabled {
		go func(res []model.SearchResult, kw string, key string) {
			ttl := time.Duration(config.AppConfig.CacheTTLMinutes) * time.Minute

			if enhancedTwoLevelCache != nil {
				data, err := enhancedTwoLevelCache.GetSerializer().Serialize(res)
				if err != nil {
					return
				}

				enhancedTwoLevelCache.SetBothLevels(key, data, ttl)
			}
		}(allResults, keyword, cacheKey)
	}

	return allResults, nil
}

// selectAvailablePlugins 选择可用插件
func selectAvailablePlugins(s *SearchService, plugins []string) []plugin.AsyncSearchPlugin {
	var availablePlugins []plugin.AsyncSearchPlugin

	if s.pluginManager != nil {
		allPlugins := s.pluginManager.GetPlugins()

		hasPlugins := plugins != nil && len(plugins) > 0
		hasNonEmptyPlugin := false

		if hasPlugins {
			for _, p := range plugins {
				if p != "" {
					hasNonEmptyPlugin = true
					break
				}
			}
		}

		if hasPlugins && hasNonEmptyPlugin {
			pluginMap := make(map[string]bool)
			for _, p := range plugins {
				if p != "" {
					pluginMap[strings.ToLower(p)] = true
				}
			}

			for _, p := range allPlugins {
				if pluginMap[strings.ToLower(p.Name())] {
					availablePlugins = append(availablePlugins, p)
				}
			}
		} else {
			availablePlugins = allPlugins
		}
	}

	return availablePlugins
}

// GetPluginManager 获取插件管理器
func (s *SearchService) GetPluginManager() *plugin.PluginManager {
	return s.pluginManager
}
