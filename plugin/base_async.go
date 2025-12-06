package plugin

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"pansou/config"
	"pansou/model"
)

// ============================================================
// BaseAsyncPlugin 基础异步插件结构和方法
// ============================================================

// BaseAsyncPlugin 基础异步插件结构
type BaseAsyncPlugin struct {
	name               string
	priority           int
	client             *http.Client  // 用于短超时的客户端
	backgroundClient   *http.Client  // 用于长超时的客户端
	cacheTTL           time.Duration // 内存缓存有效期
	mainCacheUpdater   func(string, []model.SearchResult, time.Duration, bool, string) error // 主缓存更新函数（支持IsFinal参数，接收原始数据，最后参数为关键词）
	MainCacheKey       string        // 主缓存键，导出字段
	currentKeyword     string        // 当前搜索的关键词，用于日志显示
	finalUpdateTracker map[string]bool // 追踪已更新的最终结果缓存
	finalUpdateMutex   sync.RWMutex  // 保护finalUpdateTracker的并发访问
	skipServiceFilter  bool          // 是否跳过Service层的关键词过滤
}

// NewBaseAsyncPlugin 创建基础异步插件
func NewBaseAsyncPlugin(name string, priority int) *BaseAsyncPlugin {
	// 确保异步插件已初始化
	if !initialized {
		initAsyncPlugin()
	}

	// 确定超时和缓存时间
	responseTimeout := defaultAsyncResponseTimeout
	processingTimeout := defaultPluginTimeout
	cacheTTL := defaultCacheTTL

	// 如果配置已初始化，则使用配置中的值
	if config.AppConfig != nil {
		responseTimeout = config.AppConfig.AsyncResponseTimeoutDur
		processingTimeout = config.AppConfig.PluginTimeout
		cacheTTL = time.Duration(config.AppConfig.AsyncCacheTTLHours) * time.Hour
	}

	return &BaseAsyncPlugin{
		name:     name,
		priority: priority,
		client: &http.Client{
			Timeout: responseTimeout,
		},
		backgroundClient: &http.Client{
			Timeout: processingTimeout,
		},
		cacheTTL:           cacheTTL,
		finalUpdateTracker: make(map[string]bool), // 初始化缓存更新追踪器
		skipServiceFilter:  false,                  // 默认不跳过Service层过滤
	}
}

// NewBaseAsyncPluginWithFilter 创建基础异步插件（支持设置Service层过滤参数）
func NewBaseAsyncPluginWithFilter(name string, priority int, skipServiceFilter bool) *BaseAsyncPlugin {
	// 确保异步插件已初始化
	if !initialized {
		initAsyncPlugin()
	}

	// 确定超时和缓存时间
	responseTimeout := defaultAsyncResponseTimeout
	processingTimeout := defaultPluginTimeout
	cacheTTL := defaultCacheTTL

	// 如果配置已初始化，则使用配置中的值
	if config.AppConfig != nil {
		responseTimeout = config.AppConfig.AsyncResponseTimeoutDur
		processingTimeout = config.AppConfig.PluginTimeout
		cacheTTL = time.Duration(config.AppConfig.AsyncCacheTTLHours) * time.Hour
	}

	return &BaseAsyncPlugin{
		name:     name,
		priority: priority,
		client: &http.Client{
			Timeout: responseTimeout,
		},
		backgroundClient: &http.Client{
			Timeout: processingTimeout,
		},
		cacheTTL:           cacheTTL,
		finalUpdateTracker: make(map[string]bool), // 初始化缓存更新追踪器
		skipServiceFilter:  skipServiceFilter,     // 使用传入的过滤设置
	}
}

// SetMainCacheKey 设置主缓存键
func (p *BaseAsyncPlugin) SetMainCacheKey(key string) {
	p.MainCacheKey = key
}

// SetCurrentKeyword 设置当前搜索关键词（用于日志显示）
func (p *BaseAsyncPlugin) SetCurrentKeyword(keyword string) {
	p.currentKeyword = keyword
}

// SetMainCacheUpdater 设置主缓存更新函数（修复后的签名，增加关键词参数）
func (p *BaseAsyncPlugin) SetMainCacheUpdater(updater func(string, []model.SearchResult, time.Duration, bool, string) error) {
	p.mainCacheUpdater = updater
}

// Name 返回插件名称
func (p *BaseAsyncPlugin) Name() string {
	return p.name
}

// Priority 返回插件优先级
func (p *BaseAsyncPlugin) Priority() int {
	return p.priority
}

// SkipServiceFilter 返回是否跳过Service层的关键词过滤
func (p *BaseAsyncPlugin) SkipServiceFilter() bool {
	return p.skipServiceFilter
}

// GetClient 返回短超时客户端
func (p *BaseAsyncPlugin) GetClient() *http.Client {
	return p.client
}
// 第八部分：异步搜索核心逻辑
// ============================================================

// AsyncSearch 异步搜索基础方法
func (p *BaseAsyncPlugin) AsyncSearch(
	keyword string,
	searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error),
	mainCacheKey string,
	ext map[string]interface{},
) ([]model.SearchResult, error) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}
	
	now := time.Now()
	
	// 修改缓存键，确保包含插件名称
	pluginSpecificCacheKey := fmt.Sprintf("%s:%s", p.name, keyword)
	
	// 检查缓存
	if cachedItems, ok := apiResponseCache.Load(pluginSpecificCacheKey); ok {
		cachedResult := cachedItems.(cachedResponse)
		
		// 缓存完全有效（未过期且完整）
		if time.Since(cachedResult.Timestamp) < p.cacheTTL && cachedResult.Complete {
			recordCacheHit()
			recordCacheAccess(pluginSpecificCacheKey)
			
			// 如果缓存接近过期（已用时间超过TTL的80%），在后台刷新缓存
			if time.Since(cachedResult.Timestamp) > (p.cacheTTL * 4 / 5) {
				go p.refreshCacheInBackground(keyword, pluginSpecificCacheKey, searchFunc, cachedResult, mainCacheKey, ext)
			}
			
			return cachedResult.Results, nil
		}
		
		// 缓存已过期但有结果，启动后台刷新，同时返回旧结果
		if len(cachedResult.Results) > 0 {
			recordCacheHit()
			recordCacheAccess(pluginSpecificCacheKey)
			
			// 标记为部分过期
			if time.Since(cachedResult.Timestamp) >= p.cacheTTL {
				// 在后台刷新缓存
				go p.refreshCacheInBackground(keyword, pluginSpecificCacheKey, searchFunc, cachedResult, mainCacheKey, ext)
				
				// 日志记录
				fmt.Printf("[%s] 缓存已过期，后台刷新中: %s (已过期: %v)\n", 
					p.name, pluginSpecificCacheKey, time.Since(cachedResult.Timestamp))
			}
			
			return cachedResult.Results, nil
		}
	}
	
	recordCacheMiss()
	
	// 创建通道
	resultChan := make(chan []model.SearchResult, 1)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})
	
	// 启动后台处理
	go func() {
		// 尝试获取工作槽
		if !acquireWorkerSlot() {
			// 工作池已满，使用快速响应客户端直接处理
			results, err := searchFunc(p.client, keyword, ext)
			if err != nil {
				select {
				case errorChan <- err:
				default:
				}
				return
			}
			
			select {
			case resultChan <- results:
			default:
			}
			
			// 缓存结果
			apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
				Results:     results,
				Timestamp:   now,
				Complete:    true,
				LastAccess:  now,
				AccessCount: 1,
			})
			
			// 🔧 工作池满时短超时(默认4秒)内完成，这是完整结果
			p.updateMainCacheWithFinal(mainCacheKey, results, true)
			
			return
		}
		defer releaseWorkerSlot()
		
		// 执行搜索
		results, err := searchFunc(p.backgroundClient, keyword, ext)
		
		// 检查是否已经响应
		select {
		case <-doneChan:
			// 已经响应，只更新缓存
			if err == nil {
				// 检查是否存在旧缓存
				var accessCount int = 1
				var lastAccess time.Time = now
				
				if oldCache, ok := apiResponseCache.Load(pluginSpecificCacheKey); ok {
					oldCachedResult := oldCache.(cachedResponse)
					accessCount = oldCachedResult.AccessCount
					lastAccess = oldCachedResult.LastAccess
					
					// 合并结果（新结果优先）
					if len(oldCachedResult.Results) > 0 {
						// 创建合并结果集
						mergedResults := make([]model.SearchResult, 0, len(results) + len(oldCachedResult.Results))
						
						// 创建已有结果ID的映射
						existingIDs := make(map[string]bool)
						for _, r := range results {
							existingIDs[r.UniqueID] = true
							mergedResults = append(mergedResults, r)
						}
						
						// 添加旧结果中不存在的项
						for _, r := range oldCachedResult.Results {
							if !existingIDs[r.UniqueID] {
								mergedResults = append(mergedResults, r)
							}
						}
						
						// 使用合并结果
						results = mergedResults
					}
				}
				
				apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
					Results:     results,
					Timestamp:   now,
					Complete:    true,
					LastAccess:  lastAccess,
					AccessCount: accessCount,
				})
				recordAsyncCompletion()
				
				// 异步插件后台完成时更新主缓存（标记为最终结果）
				p.updateMainCacheWithFinal(mainCacheKey, results, true)
				
				// 异步插件本地缓存系统已移除
			}
		default:
			// 尚未响应，发送结果
			if err != nil {
				select {
				case errorChan <- err:
				default:
				}
			} else {
				// 检查是否存在旧缓存用于合并
				if oldCache, ok := apiResponseCache.Load(pluginSpecificCacheKey); ok {
					oldCachedResult := oldCache.(cachedResponse)
					if len(oldCachedResult.Results) > 0 {
						// 创建合并结果集
						mergedResults := make([]model.SearchResult, 0, len(results) + len(oldCachedResult.Results))
						
						// 创建已有结果ID的映射
						existingIDs := make(map[string]bool)
						for _, r := range results {
							existingIDs[r.UniqueID] = true
							mergedResults = append(mergedResults, r)
						}
						
						// 添加旧结果中不存在的项
						for _, r := range oldCachedResult.Results {
							if !existingIDs[r.UniqueID] {
								mergedResults = append(mergedResults, r)
							}
						}
						
						// 使用合并结果
						results = mergedResults
					}
				}
				
				select {
				case resultChan <- results:
				default:
				}
				
				// 更新缓存
				apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
					Results:     results,
					Timestamp:   now,
					Complete:    true,
					LastAccess:  now,
					AccessCount: 1,
				})
				
				// 🔧 短超时(默认4秒)内正常完成，这是完整的最终结果
				p.updateMainCacheWithFinal(mainCacheKey, results, true)
				
				// 异步插件本地缓存系统已移除
			}
		}
	}()
	
	// 获取响应超时时间
	responseTimeout := defaultAsyncResponseTimeout
	if config.AppConfig != nil {
		responseTimeout = config.AppConfig.AsyncResponseTimeoutDur
	}
	
	// 等待响应超时或结果
	select {
	case results := <-resultChan:
		close(doneChan)
		return results, nil
	case err := <-errorChan:
		close(doneChan)
		return nil, err
	case <-time.After(responseTimeout):
		// 插件响应超时，后台继续处理（优化完成，日志简化）
		
		// 响应超时，返回空结果，后台继续处理
		go func() {
			defer close(doneChan)
		}()
		
		// 检查是否有部分缓存可用
		if cachedItems, ok := apiResponseCache.Load(pluginSpecificCacheKey); ok {
			cachedResult := cachedItems.(cachedResponse)
			if len(cachedResult.Results) > 0 {
				// 有部分缓存可用，记录访问并返回
				recordCacheAccess(pluginSpecificCacheKey)
				fmt.Printf("[%s] 响应超时，返回部分缓存: %s (项目数: %d)\n", 
					p.name, pluginSpecificCacheKey, len(cachedResult.Results))
				return cachedResult.Results, nil
			}
		}
		
		// 创建空的临时缓存，以便后台处理完成后可以更新
		apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
			Results:     []model.SearchResult{},
			Timestamp:   now,
			Complete:    false, // 标记为不完整
			LastAccess:  now,
			AccessCount: 1,
		})
		
		// 🔧 修复：4秒超时时也要更新主缓存，标记为部分结果（空结果）
		p.updateMainCacheWithFinal(mainCacheKey, []model.SearchResult{}, false)
		
		// fmt.Printf("[%s] 响应超时，后台继续处理: %s\n", p.name, pluginSpecificCacheKey)
		return []model.SearchResult{}, nil
	}
}

// AsyncSearchWithResult 异步搜索方法，返回PluginSearchResult
func (p *BaseAsyncPlugin) AsyncSearchWithResult(
	keyword string,
	searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error),
	mainCacheKey string,
	ext map[string]interface{},
) (model.PluginSearchResult, error) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}
	
	now := time.Now()
	
	// 修改缓存键，确保包含插件名称
	pluginSpecificCacheKey := fmt.Sprintf("%s:%s", p.name, keyword)
	
	// 检查缓存
	if cachedItems, ok := apiResponseCache.Load(pluginSpecificCacheKey); ok {
		cachedResult := cachedItems.(cachedResponse)
		
		// 缓存完全有效（未过期且完整）
		if time.Since(cachedResult.Timestamp) < p.cacheTTL && cachedResult.Complete {
			recordCacheHit()
			recordCacheAccess(pluginSpecificCacheKey)
			
			// 如果缓存接近过期（已用时间超过TTL的80%），在后台刷新缓存
			if time.Since(cachedResult.Timestamp) > (p.cacheTTL * 4 / 5) {
				go p.refreshCacheInBackground(keyword, pluginSpecificCacheKey, searchFunc, cachedResult, mainCacheKey, ext)
			}
			
			return model.PluginSearchResult{
				Results:   cachedResult.Results,
				IsFinal:   cachedResult.Complete,
				Timestamp: cachedResult.Timestamp,
				Source:    p.name,
				Message:   "从缓存获取",
			}, nil
		}
		
		// 缓存已过期但有结果，启动后台刷新，同时返回旧结果
		if len(cachedResult.Results) > 0 {
			recordCacheHit()
			recordCacheAccess(pluginSpecificCacheKey)
			
			// 标记为部分过期
			if time.Since(cachedResult.Timestamp) >= p.cacheTTL {
				// 在后台刷新缓存
				go p.refreshCacheInBackground(keyword, pluginSpecificCacheKey, searchFunc, cachedResult, mainCacheKey, ext)
			}
			
			return model.PluginSearchResult{
				Results:   cachedResult.Results,
				IsFinal:   false, // 🔥 过期数据标记为非最终结果
				Timestamp: cachedResult.Timestamp,
				Source:    p.name,
				Message:   "缓存已过期，后台刷新中",
			}, nil
		}
	}
	
	recordCacheMiss()
	
	// 创建通道
	resultChan := make(chan []model.SearchResult, 1)
	errorChan := make(chan error, 1)
	doneChan := make(chan struct{})
	
	// 启动后台处理
	go func() {
		defer func() {
			select {
			case <-doneChan:
			default:
				close(doneChan)
			}
		}()
		
		// 尝试获取工作槽
		if !acquireWorkerSlot() {
			// 工作池已满，使用快速响应客户端直接处理
			results, err := searchFunc(p.client, keyword, ext)
			if err != nil {
				select {
				case errorChan <- err:
				default:
				}
				return
			}
			
			select {
			case resultChan <- results:
			default:
			}
			return
		}
		defer releaseWorkerSlot()
		
		// 使用长超时客户端进行搜索
		results, err := searchFunc(p.backgroundClient, keyword, ext)
		if err != nil {
			select {
			case errorChan <- err:
			default:
			}
		} else {
			select {
			case resultChan <- results:
			default:
			}
		}
	}()
	
	// 等待结果或超时
	responseTimeout := defaultAsyncResponseTimeout
	if config.AppConfig != nil {
		responseTimeout = config.AppConfig.AsyncResponseTimeoutDur
	}
	
	select {
	case results := <-resultChan:
		// 不直接关闭，让defer处理
		
		// 缓存结果
		apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
			Results:     results,
			Timestamp:   now,
			Complete:    true, // 🔥 及时完成，标记为完整结果
			LastAccess:  now,
			AccessCount: 1,
		})
		
		// 🔧 恢复主缓存更新：使用统一的GOB序列化
		// 传递原始数据，由主程序负责序列化
		if mainCacheKey != "" && p.mainCacheUpdater != nil {
			err := p.mainCacheUpdater(mainCacheKey, results, p.cacheTTL, true, p.currentKeyword)
			if err != nil {
				fmt.Printf("❌ [%s] 及时完成缓存更新失败: %s | 错误: %v\n", p.name, mainCacheKey, err)
			}
		}
		
		return model.PluginSearchResult{
			Results:   results,
			IsFinal:   true, // 🔥 及时完成，最终结果
			Timestamp: now,
			Source:    p.name,
			Message:   "搜索完成",
		}, nil
		
	case err := <-errorChan:
		// 不直接关闭，让defer处理
		return model.PluginSearchResult{}, err
		
	case <-time.After(responseTimeout):
		// 🔥 超时处理：返回空结果，后台继续处理
		go p.completeSearchInBackground(keyword, searchFunc, pluginSpecificCacheKey, mainCacheKey, doneChan, ext)
		
		// 存储临时缓存（标记为不完整）
		apiResponseCache.Store(pluginSpecificCacheKey, cachedResponse{
			Results:     []model.SearchResult{},
			Timestamp:   now,
			Complete:    false, // 🔥 标记为不完整
			LastAccess:  now,
			AccessCount: 1,
		})
		
		return model.PluginSearchResult{
			Results:   []model.SearchResult{},
			IsFinal:   false, // 🔥 超时返回，非最终结果
			Timestamp: now,
			Source:    p.name,
			Message:   "处理中，后台继续...",
		}, nil
	}
}

// completeSearchInBackground 后台完成搜索
func (p *BaseAsyncPlugin) completeSearchInBackground(
	keyword string,
	searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error),
	pluginCacheKey string,
	mainCacheKey string,
	doneChan chan struct{},
	ext map[string]interface{},
) {
	defer func() {
		select {
		case <-doneChan:
		default:
			close(doneChan)
		}
	}()
	
	// 执行完整搜索
	results, err := searchFunc(p.backgroundClient, keyword, ext)
	if err != nil {
		return
	}
	
	// 更新插件缓存
	now := time.Now()
	apiResponseCache.Store(pluginCacheKey, cachedResponse{
		Results:     results,
		Timestamp:   now,
		Complete:    true, // 🔥 标记为完整结果
		LastAccess:  now,
		AccessCount: 1,
	})
	
	// 🔧 恢复主缓存更新：使用统一的GOB序列化
	// 传递原始数据，由主程序负责序列化
	if mainCacheKey != "" && p.mainCacheUpdater != nil {
		err := p.mainCacheUpdater(mainCacheKey, results, p.cacheTTL, true, p.currentKeyword)
		if err != nil {
			fmt.Printf("❌ [%s] 后台完成缓存更新失败: %s | 错误: %v\n", p.name, mainCacheKey, err)
		}
	}
}

// refreshCacheInBackground 在后台刷新缓存
func (p *BaseAsyncPlugin) refreshCacheInBackground(
	keyword string,
	cacheKey string,
	searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error),
	oldCache cachedResponse,
	originalCacheKey string,
	ext map[string]interface{},
) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}
	
	// 注意：这里的cacheKey已经是插件特定的了，因为是从AsyncSearch传入的
	
	// 检查是否有足够的工作槽
	if !acquireWorkerSlot() {
		return
	}
	defer releaseWorkerSlot()
	
	// 记录刷新开始时间
	refreshStart := time.Now()
	
	// 执行搜索
	results, err := searchFunc(p.backgroundClient, keyword, ext)
	if err != nil || len(results) == 0 {
		return
	}
	
	// 创建合并结果集
	mergedResults := make([]model.SearchResult, 0, len(results) + len(oldCache.Results))
	
	// 创建已有结果ID的映射
	existingIDs := make(map[string]bool)
	for _, r := range results {
		existingIDs[r.UniqueID] = true
		mergedResults = append(mergedResults, r)
	}
	
	// 添加旧结果中不存在的项
	for _, r := range oldCache.Results {
		if !existingIDs[r.UniqueID] {
			mergedResults = append(mergedResults, r)
		}
	}
	
	// 更新缓存
	apiResponseCache.Store(cacheKey, cachedResponse{
		Results:     mergedResults,
		Timestamp:   time.Now(),
		Complete:    true,
		LastAccess:  oldCache.LastAccess,
		AccessCount: oldCache.AccessCount,
	})
	
	// 🔥 异步插件后台刷新完成时更新主缓存（标记为最终结果）
	p.updateMainCacheWithFinal(originalCacheKey, mergedResults, true)
	
	// 记录刷新时间
	refreshTime := time.Since(refreshStart)
	fmt.Printf("[%s] 后台刷新完成: %s (耗时: %v, 新项目: %d, 合并项目: %d)\n", 
		p.name, cacheKey, refreshTime, len(results), len(mergedResults))
	
	// 异步插件本地缓存系统已移除
} 

// ============================================================
// 第九部分：缓存管理
