package plugin

import (
	"compress/gzip"
	"encoding/gob"
	"pansou/util/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"math/rand"

	"pansou/config"
	"pansou/model"
)

// 缓存相关变量
var (
	// API响应缓存，键为关键词，值为缓存的响应
	apiResponseCache = sync.Map{}
	
	// 最后一次清理缓存的时间
	lastCacheCleanTime = time.Now()
	
	// 最后一次保存缓存的时间
	lastCacheSaveTime = time.Now()
	
	// 工作池相关变量
	backgroundWorkerPool chan struct{}
	backgroundTasksCount int32 = 0
	
	// 统计数据 (仅用于内部监控)
	cacheHits         int64 = 0
	cacheMisses       int64 = 0
	asyncCompletions  int64 = 0
	
	// 初始化标志
	initialized       bool = false
	initLock          sync.Mutex
	
	// 缓存保存锁，防止并发保存导致的竞态条件
	saveCacheLock     sync.Mutex
	
	// 默认配置值
	defaultAsyncResponseTimeout = 4 * time.Second
	defaultPluginTimeout = 30 * time.Second
	defaultCacheTTL = 1 * time.Hour
	defaultMaxBackgroundWorkers = 20
	defaultMaxBackgroundTasks = 100
	
	// 缓存保存间隔 (2分钟)
	cacheSaveInterval = 2 * time.Minute
	
	// 缓存访问频率记录
	cacheAccessCount = sync.Map{}
)

// 缓存响应结构
type cachedResponse struct {
	Results   []model.SearchResult `json:"results"`
	Timestamp time.Time           `json:"timestamp"`
	Complete  bool                `json:"complete"`
	LastAccess time.Time          `json:"last_access"`
	AccessCount int               `json:"access_count"`
}

// 可序列化的缓存结构，用于持久化
type persistentCache struct {
	Entries map[string]cachedResponse
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
	
	// 启动缓存清理和保存goroutine
	go startCacheCleaner()
	go startCachePersistence()
	
	// 尝试从磁盘加载缓存
	loadCacheFromDisk()
	
	initialized = true
}

// InitAsyncPluginSystem 导出的初始化函数，用于确保异步插件系统初始化
func InitAsyncPluginSystem() {
	initAsyncPlugin()
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	// 每小时清理一次缓存
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		cleanCache()
	}
}

// cleanCache 清理过期缓存
func cleanCache() {
	now := time.Now()
	lastCacheCleanTime = now
	
	// 获取缓存TTL
	cacheTTL := defaultCacheTTL
	if config.AppConfig != nil && config.AppConfig.AsyncCacheTTLHours > 0 {
		cacheTTL = time.Duration(config.AppConfig.AsyncCacheTTLHours) * time.Hour
	}
	
	// 第一步：收集所有缓存项和它们的信息
	type cacheInfo struct {
		key          string
		item         cachedResponse
		age          time.Duration      // 年龄（当前时间 - 创建时间）
		idleTime     time.Duration      // 空闲时间（当前时间 - 最后访问时间）
		score        float64            // 缓存项得分（用于决定是否保留）
	}
	
	var allItems []cacheInfo
	var totalSize int = 0
	
	apiResponseCache.Range(func(k, v interface{}) bool {
		key := k.(string)
		item := v.(cachedResponse)
		age := now.Sub(item.Timestamp)
		idleTime := now.Sub(item.LastAccess)
		
		// 如果超过TTL，直接删除
		if age > cacheTTL {
			apiResponseCache.Delete(key)
			return true
		}
		
		// 计算大小（简单估算，每个结果占用1单位）
		itemSize := len(item.Results)
		totalSize += itemSize
		
		// 计算得分：访问次数 / (空闲时间的平方 * 年龄)
		// 这样：
		// - 访问频率高的得分高
		// - 最近访问的得分高
		// - 较新的缓存得分高
		score := float64(item.AccessCount) / (idleTime.Seconds() * idleTime.Seconds() * age.Seconds())
		
		allItems = append(allItems, cacheInfo{
			key:       key,
			item:      item,
			age:       age,
			idleTime:  idleTime,
			score:     score,
		})
		
		return true
	})
	
	// 获取缓存大小限制（默认10000项）
	maxCacheSize := 10000
	if config.AppConfig != nil && config.AppConfig.CacheMaxSizeMB > 0 {
		// 这里我们将MB转换为大致的项目数，假设每项平均1KB
		maxCacheSize = config.AppConfig.CacheMaxSizeMB * 1024
	}
	
	// 如果缓存不超过限制，不需要清理
	if totalSize <= maxCacheSize {
		return
	}
	
	// 按得分排序（从低到高）
	sort.Slice(allItems, func(i, j int) bool {
		return allItems[i].score < allItems[j].score
	})
	
	// 需要删除的大小
	sizeToRemove := totalSize - maxCacheSize
	
	// 从得分低的开始删除，直到满足大小要求
	removedSize := 0
	removedCount := 0
	
	for _, item := range allItems {
		if removedSize >= sizeToRemove {
			break
		}
		
		apiResponseCache.Delete(item.key)
		removedSize += len(item.item.Results)
		removedCount++
		
		// 最多删除总数的20%
		if removedCount >= len(allItems) / 5 {
			break
		}
	}
	
	fmt.Printf("缓存清理完成: 删除了%d个项目（总共%d个）\n", removedCount, len(allItems))
}

// startCachePersistence 启动定期保存缓存到磁盘的goroutine
func startCachePersistence() {
	// 每2分钟保存一次缓存
	ticker := time.NewTicker(cacheSaveInterval)
	defer ticker.Stop()
	
	for range ticker.C {
		// 检查是否有缓存项需要保存
		if hasCacheItems() {
			saveCacheToDisk()
		}
	}
}

// hasCacheItems 检查是否有缓存项
func hasCacheItems() bool {
	hasItems := false
	apiResponseCache.Range(func(k, v interface{}) bool {
		hasItems = true
		return false // 找到一个就停止遍历
	})
	return hasItems
}

// getCachePath 获取缓存文件路径
func getCachePath() string {
	// 默认缓存路径
	cachePath := "cache"
	
	// 如果配置已加载，则使用配置中的缓存路径
	if config.AppConfig != nil && config.AppConfig.CachePath != "" {
		cachePath = config.AppConfig.CachePath
	}
	
	// 创建缓存目录（如果不存在）
	if _, err := os.Stat(cachePath); os.IsNotExist(err) {
		os.MkdirAll(cachePath, 0755)
	}
	
	return filepath.Join(cachePath, "async_cache.gob")
}

// saveCacheToDisk 将缓存保存到磁盘
func saveCacheToDisk() {
	// 使用互斥锁确保同一时间只有一个goroutine可以执行
	saveCacheLock.Lock()
	defer saveCacheLock.Unlock()
	
	cacheFile := getCachePath()
	lastCacheSaveTime = time.Now()
	
	// 创建临时文件
	tempFile := cacheFile + ".tmp"
	file, err := os.Create(tempFile)
	if err != nil {
		fmt.Printf("创建缓存文件失败: %v\n", err)
		return
	}
	defer file.Close()
	
	// 创建gzip压缩写入器
	gzipWriter := gzip.NewWriter(file)
	defer gzipWriter.Close()
	
	// 将缓存内容转换为可序列化的结构
	persistent := persistentCache{
		Entries: make(map[string]cachedResponse),
	}
	
	// 记录缓存项数量和总结果数
	itemCount := 0
	resultCount := 0
	
	apiResponseCache.Range(func(k, v interface{}) bool {
		key := k.(string)
		value := v.(cachedResponse)
		persistent.Entries[key] = value
		itemCount++
		resultCount += len(value.Results)
		return true
	})
	
	// 使用gob编码器保存
	encoder := gob.NewEncoder(gzipWriter)
	if err := encoder.Encode(persistent); err != nil {
		fmt.Printf("编码缓存失败: %v\n", err)
		return
	}
	
	// 确保所有数据已写入
	gzipWriter.Close()
	file.Sync()
	file.Close()
	
	// 使用原子重命名（这确保了替换是原子的，避免了缓存文件损坏）
	if err := os.Rename(tempFile, cacheFile); err != nil {
		fmt.Printf("重命名缓存文件失败: %v\n", err)
		return
	}
	
	// fmt.Printf("缓存已保存到磁盘，条目数: %d，结果总数: %d\n", itemCount, resultCount)
}

// SaveCacheToDisk 导出的缓存保存函数，用于程序退出时调用
func SaveCacheToDisk() {
	if initialized {
		// fmt.Println("程序退出，正在保存异步插件缓存...")
		saveCacheToDisk()
		// fmt.Println("异步插件缓存保存完成")
	}
}

// loadCacheFromDisk 从磁盘加载缓存
func loadCacheFromDisk() {
	cacheFile := getCachePath()
	
	// 检查缓存文件是否存在
	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		// fmt.Println("缓存文件不存在，跳过加载")
		return
	}
	
	// 打开缓存文件
	file, err := os.Open(cacheFile)
	if err != nil {
		// fmt.Printf("打开缓存文件失败: %v\n", err)
		return
	}
	defer file.Close()
	
	// 创建gzip读取器
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		// 尝试作为非压缩文件读取（向后兼容）
		file.Seek(0, 0) // 重置文件指针
		decoder := gob.NewDecoder(file)
		var persistent persistentCache
		if err := decoder.Decode(&persistent); err != nil {
			fmt.Printf("解码缓存失败: %v\n", err)
			return
		}
		loadCacheEntries(persistent)
		return
	}
	defer gzipReader.Close()
	
	// 使用gob解码器加载
	var persistent persistentCache
	decoder := gob.NewDecoder(gzipReader)
	if err := decoder.Decode(&persistent); err != nil {
		fmt.Printf("解码缓存失败: %v\n", err)
		return
	}
	
	loadCacheEntries(persistent)
}

// loadCacheEntries 加载缓存条目到内存
func loadCacheEntries(persistent persistentCache) {
	// 获取缓存TTL，用于过滤过期项
	cacheTTL := defaultCacheTTL
	if config.AppConfig != nil && config.AppConfig.AsyncCacheTTLHours > 0 {
		cacheTTL = time.Duration(config.AppConfig.AsyncCacheTTLHours) * time.Hour
	}
	
	now := time.Now()
	loadedCount := 0
	totalResultCount := 0
	
	// 将解码后的缓存加载到内存
	for key, value := range persistent.Entries {
		// 只加载未过期的缓存
		if now.Sub(value.Timestamp) <= cacheTTL {
			apiResponseCache.Store(key, value)
			loadedCount++
			totalResultCount += len(value.Results)
		}
	}
	
	// fmt.Printf("从磁盘加载了%d条缓存（过滤后），包含%d个搜索结果\n", loadedCount, totalResultCount)
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

// recordCacheAccess 记录缓存访问次数，用于智能缓存策略
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
		cacheAccessCount.Store(key, count.(int) + 1)
	} else {
		cacheAccessCount.Store(key, 1)
	}
}

// BaseAsyncPlugin 基础异步插件结构
type BaseAsyncPlugin struct {
	name              string
	priority          int
	client            *http.Client  // 用于短超时的客户端
	backgroundClient  *http.Client  // 用于长超时的客户端
	cacheTTL          time.Duration // 缓存有效期
	mainCacheUpdater  func(string, []byte, time.Duration) error // 主缓存更新函数
	MainCacheKey      string        // 主缓存键，导出字段
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
		cacheTTL: cacheTTL,
	}
}

// SetMainCacheKey 设置主缓存键
func (p *BaseAsyncPlugin) SetMainCacheKey(key string) {
	p.MainCacheKey = key
}

// SetMainCacheUpdater 设置主缓存更新函数
func (p *BaseAsyncPlugin) SetMainCacheUpdater(updater func(string, []byte, time.Duration) error) {
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
			
			// 更新主缓存系统
			p.updateMainCache(mainCacheKey, results)
			
			return
		}
		defer releaseWorkerSlot()
		
		// 执行搜索
		results, err := searchFunc(p.backgroundClient, keyword, ext)
		
		// 检查是否已经响应
		select {
		case <-doneChan:
			// 已经响应，只更新缓存
			if err == nil && len(results) > 0 {
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
				
				// 更新主缓存系统
				p.updateMainCache(mainCacheKey, results)
				
				// 更新缓存后立即触发保存
				go saveCacheToDisk()
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
				
				// 更新主缓存系统
				p.updateMainCache(mainCacheKey, results)
				
				// 更新缓存后立即触发保存
				go saveCacheToDisk()
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
		
		// fmt.Printf("[%s] 响应超时，后台继续处理: %s\n", p.name, pluginSpecificCacheKey)
		return []model.SearchResult{}, nil
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
	
	// 更新主缓存系统
	// 使用传入的originalCacheKey，直接传递给updateMainCache
	p.updateMainCache(originalCacheKey, mergedResults)
	
	// 记录刷新时间
	refreshTime := time.Since(refreshStart)
	fmt.Printf("[%s] 后台刷新完成: %s (耗时: %v, 新项目: %d, 合并项目: %d)\n", 
		p.name, cacheKey, refreshTime, len(results), len(mergedResults))
	
	// 添加随机延迟，避免多个goroutine同时调用saveCacheToDisk
	time.Sleep(time.Duration(100+rand.Intn(500)) * time.Millisecond)
	
	// 更新缓存后立即触发保存
	go saveCacheToDisk()
} 

// updateMainCache 更新主缓存系统
func (p *BaseAsyncPlugin) updateMainCache(cacheKey string, results []model.SearchResult) {
	// 如果主缓存更新函数为空或缓存键为空，直接返回
	if p.mainCacheUpdater == nil || cacheKey == "" {
		return
	}
	
	// 序列化结果
	data, err := json.Marshal(results)
	if err != nil {
		fmt.Printf("[%s] 序列化结果失败: %v\n", p.name, err)
		return
	}
	
	// 调用主缓存更新函数
	if err := p.mainCacheUpdater(cacheKey, data, p.cacheTTL); err != nil {
		fmt.Printf("[%s] 更新主缓存失败: %v\n", p.name, err)
	} else {
		fmt.Printf("[%s] 成功更新主缓存: %s\n", p.name, cacheKey)
	}
} 

// FilterResultsByKeyword 根据关键词过滤搜索结果
func (p *BaseAsyncPlugin) FilterResultsByKeyword(results []model.SearchResult, keyword string) []model.SearchResult {
	if keyword == "" {
		return results
	}
	
	// 预估过滤后会保留80%的结果
	filteredResults := make([]model.SearchResult, 0, len(results)*8/10)

	// 将关键词转为小写，用于不区分大小写的比较
	lowerKeyword := strings.ToLower(keyword)

	// 将关键词按空格分割，用于支持多关键词搜索
	keywords := strings.Fields(lowerKeyword)

	for _, result := range results {
		// 将标题和内容转为小写
		lowerTitle := strings.ToLower(result.Title)
		lowerContent := strings.ToLower(result.Content)

		// 检查每个关键词是否在标题或内容中
		matched := true
		for _, kw := range keywords {
			// 对于所有关键词，检查是否在标题或内容中
			if !strings.Contains(lowerTitle, kw) && !strings.Contains(lowerContent, kw) {
				matched = false
				break
			}
		}

		if matched {
			filteredResults = append(filteredResults, result)
		}
	}

	return filteredResults
} 

// GetClient 返回短超时客户端
func (p *BaseAsyncPlugin) GetClient() *http.Client {
	return p.client
} 