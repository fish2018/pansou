package pansearch

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
	"sync/atomic"
)

// 预编译正则表达式
var (
	// 从HTML中提取buildId的正则表达式
	buildIdRegex = regexp.MustCompile(`"buildId":"([^"]+)"`)

	// 从__NEXT_DATA__脚本中提取数据的正则表达式
	nextDataRegex = regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
	
	// 缓存相关变量
	searchResultCache = sync.Map{}
	lastCacheCleanTime = time.Now()
	cacheTTL = 1 * time.Hour
)

// 在init函数中注册插件
func init() {
	// 使用全局超时时间创建插件实例并注册
	plugin.RegisterGlobalPlugin(NewPanSearchPlugin())
	
	// 启动缓存清理goroutine
	go startCacheCleaner()
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	// 每小时清理一次缓存
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		// 清空所有缓存
		searchResultCache = sync.Map{}
		lastCacheCleanTime = time.Now()
	}
}

// 缓存响应结构
type cachedResponse struct {
	results   []model.SearchResult
	timestamp time.Time
}

const (
	// 网站基础URL
	WebsiteURL = "https://www.pansearch.me/search"

	// API基础URL模板 - 需要替换buildId
	BaseURLTemplate = "https://www.pansearch.me/_next/data/%s/search.json"

	// 默认参数
	DefaultTimeout = 6 * time.Second // 减少默认超时时间
	PageSize       = 10
	MaxResults     = 1000
	MaxConcurrent  = 200 // 增加最大并发数
	MaxRetries     = 2
	MaxAPIPages    = 100 // API最大页数限制

	// HTTP 客户端配置
	MaxIdleConns          = 500 // 增加最大空闲连接数
	MaxIdleConnsPerHost   = 200 // 增加每个主机的最大空闲连接数
	MaxConnsPerHost       = 400 // 增加每个主机的最大连接数
	IdleConnTimeout       = 120 * time.Second
	TLSHandshakeTimeout   = 10 * time.Second
	ExpectContinueTimeout = 1 * time.Second
	WriteBufferSize       = 16 * 1024
	ReadBufferSize        = 16 * 1024

	// buildId缓存有效期（分钟）- 减少缓存时间以确保更及时更新
	BuildIdCacheDuration = 30
)

// 缓存buildId和过期时间
var (
	buildIdCache     string
	buildIdCacheTime time.Time
	buildIdMutex     sync.RWMutex
)

// PanSearchPlugin 盘搜插件
type PanSearchPlugin struct {
	client        *http.Client
	timeout       time.Duration
	maxResults    int
	maxConcurrent int
	retries       int
	workerPool    *WorkerPool // 添加工作池
}

// WorkerPool 工作池结构
type WorkerPool struct {
	tasks   chan Task
	results chan TaskResult
	errors  chan error
	wg      sync.WaitGroup
	closed  atomic.Bool // 添加原子标志来标记工作池是否已关闭
	mu      sync.Mutex  // 添加互斥锁保护提交操作
}

// Task 工作任务
type Task struct {
	keyword string
	offset  int
	baseURL string
}

// TaskResult 任务结果
type TaskResult struct {
	offset  int
	results []PanSearchItem
}

// NewWorkerPool 创建新的工作池
func NewWorkerPool(size int) *WorkerPool {
	return &WorkerPool{
		tasks:   make(chan Task, size*3),       // 增加任务通道容量
		results: make(chan TaskResult, size*3), // 增加结果通道容量
		errors:  make(chan error, size*3),      // 增加错误通道容量
	}
}

// Start 启动工作池
func (wp *WorkerPool) Start(ctx context.Context, handler func(ctx context.Context, task Task) (TaskResult, error)) {
	for i := 0; i < cap(wp.tasks); i++ {
		wp.wg.Add(1)
		go func() {
			defer wp.wg.Done()
			for {
				select {
				case task, ok := <-wp.tasks:
					if !ok {
						return
					}

					result, err := handler(ctx, task)
					if err != nil {
						select {
						case wp.errors <- err:
							// 成功发送错误
						default:
							// 通道可能已关闭，忽略错误
							fmt.Printf("无法发送错误: %v\n", err)
						}
					} else {
						select {
						case wp.results <- result:
							// 成功发送结果
						default:
							// 通道可能已关闭，忽略结果
							fmt.Printf("无法发送结果\n")
						}
					}

				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

// Submit 提交任务到工作池
func (wp *WorkerPool) Submit(task Task) bool {
	wp.mu.Lock()
	defer wp.mu.Unlock()

	// 检查工作池是否已关闭
	if wp.closed.Load() {
		return false
	}

	select {
	case wp.tasks <- task:
		return true
	default:
		// 如果通道已满，返回失败
		return false
	}
}

// Close 关闭工作池
func (wp *WorkerPool) Close() {
	wp.mu.Lock()
	if !wp.closed.Load() {
		wp.closed.Store(true)
		close(wp.tasks)
	}
	wp.mu.Unlock()

	wp.wg.Wait()

	// 安全关闭结果和错误通道
	wp.mu.Lock()
	defer wp.mu.Unlock()

	select {
	case _, ok := <-wp.results:
		if ok {
			close(wp.results)
		}
	default:
		close(wp.results)
	}

	select {
	case _, ok := <-wp.errors:
		if ok {
			close(wp.errors)
		}
	default:
		close(wp.errors)
	}
}

// NewPanSearchPlugin 创建新的盘搜插件
func NewPanSearchPlugin() *PanSearchPlugin {
	timeout := DefaultTimeout

	// 创建自定义 Transport 以优化连接池
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 60 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          MaxIdleConns,
		MaxIdleConnsPerHost:   MaxIdleConnsPerHost,
		MaxConnsPerHost:       MaxConnsPerHost,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ExpectContinueTimeout: ExpectContinueTimeout,
		WriteBufferSize:       WriteBufferSize,
		ReadBufferSize:        ReadBufferSize,
		DisableKeepAlives:     false,
	}

	maxConcurrent := MaxConcurrent

	p := &PanSearchPlugin{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		timeout:       timeout,
		maxResults:    MaxResults,
		maxConcurrent: maxConcurrent,
		retries:       MaxRetries,
		workerPool:    NewWorkerPool(maxConcurrent), // 初始化工作池
	}

	// 初始化时预热获取 buildId
	go func() {
		_, err := p.getBuildId()
		if err != nil {
			fmt.Printf("预热获取 buildId 失败: %v\n", err)
		}
	}()

	// 启动后台 buildId 更新器
	go p.startBuildIdUpdater()

	return p
}

// startBuildIdUpdater 启动一个定期更新 buildId 的后台协程
func (p *PanSearchPlugin) startBuildIdUpdater() {
	// 每10分钟更新一次 buildId
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		p.updateBuildId()
	}
}

// updateBuildId 更新 buildId 缓存
func (p *PanSearchPlugin) updateBuildId() {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// 发送请求获取页面
	req, err := http.NewRequestWithContext(ctx, "GET", WebsiteURL, nil)
	if err != nil {
		fmt.Printf("创建请求失败: %v\n", err)
		return
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")

	resp, err := p.client.Do(req)
	if err != nil {
		fmt.Printf("请求失败: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("获取buildId时服务器返回非200状态码: %d\n", resp.StatusCode)
		return
	}

	// 使用更高效的方式读取响应体
	var bodyBuilder strings.Builder
	_, err = io.Copy(&bodyBuilder, resp.Body)
	if err != nil {
		fmt.Printf("读取响应失败: %v\n", err)
		return
	}
	body := bodyBuilder.String()

	// 尝试提取 buildId
	newBuildId := extractBuildId(body)
	if newBuildId == "" {
		fmt.Println("未能从响应中提取 buildId")
		return
	}

	// 更新缓存
	buildIdMutex.Lock()
	defer buildIdMutex.Unlock()

	// 只有当新的 buildId 不为空且与当前缓存不同时才更新
	if newBuildId != "" && newBuildId != buildIdCache {
		buildIdCache = newBuildId
		buildIdCacheTime = time.Now()
		fmt.Printf("成功更新 buildId: %s\n", newBuildId)
	}
}

// extractBuildId 从 HTML 内容中提取 buildId
func extractBuildId(body string) string {
	// 使用预编译的正则表达式提取buildId
	matches := buildIdRegex.FindStringSubmatch(body)

	if len(matches) >= 2 {
		return matches[1]
	}

	// 尝试从NEXT_DATA中提取
	scriptMatches := nextDataRegex.FindStringSubmatch(body)

	if len(scriptMatches) >= 2 {
		var nextData map[string]interface{}
		if err := json.Unmarshal([]byte(scriptMatches[1]), &nextData); err == nil {
			if buildId, ok := nextData["buildId"].(string); ok && buildId != "" {
				return buildId
			}
		}
	}

	return ""
}

// Name 返回插件名称
func (p *PanSearchPlugin) Name() string {
	return "pansearch"
}

// Priority 返回插件优先级
func (p *PanSearchPlugin) Priority() int {
	return 3 // 中等优先级
}

// getBuildId 获取buildId，优先使用缓存
func (p *PanSearchPlugin) getBuildId() (string, error) {
	// 检查缓存是否有效
	buildIdMutex.RLock()
	if buildIdCache != "" && time.Since(buildIdCacheTime) < BuildIdCacheDuration*time.Minute {
		defer buildIdMutex.RUnlock()
		return buildIdCache, nil
	}
	buildIdMutex.RUnlock()

	// 缓存无效，需要重新获取
	buildIdMutex.Lock()
	defer buildIdMutex.Unlock()

	// 双重检查
	if buildIdCache != "" && time.Since(buildIdCacheTime) < BuildIdCacheDuration*time.Minute {
		return buildIdCache, nil
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// 发送请求获取页面
	req, err := http.NewRequestWithContext(ctx, "GET", WebsiteURL, nil)
	if err != nil {
		// 如果创建请求失败但有旧的缓存，使用旧的缓存（优雅降级）
		if buildIdCache != "" {
			fmt.Printf("创建请求失败，使用旧的buildId: %v\n", err)
			return buildIdCache, nil
		}
		return "", fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")

	// 使用重试机制发送请求
	var resp *http.Response
	var respErr error

	for retry := 0; retry <= p.retries; retry++ {
		if retry > 0 {
			// 指数退避重试
			backoffTime := time.Duration(1<<uint(retry-1)) * 100 * time.Millisecond
			time.Sleep(backoffTime)
		}

		resp, respErr = p.client.Do(req)
		if respErr == nil && resp.StatusCode == 200 {
			break
		}

		if resp != nil {
			resp.Body.Close()
		}
	}

	// 如果所有重试都失败，但有旧的缓存，使用旧的缓存（优雅降级）
	if respErr != nil || resp == nil {
		if buildIdCache != "" {
			fmt.Printf("请求失败，使用旧的buildId: %v\n", respErr)
			return buildIdCache, nil
		}
		return "", fmt.Errorf("请求失败: %w", respErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// 如果状态码不是200，但有旧的缓存，使用旧的缓存（优雅降级）
		if buildIdCache != "" {
			fmt.Printf("获取buildId时服务器返回非200状态码: %d，使用旧的buildId\n", resp.StatusCode)
			return buildIdCache, nil
		}
		return "", fmt.Errorf("获取buildId时服务器返回非200状态码: %d", resp.StatusCode)
	}

	// 使用更高效的方式读取响应体
	var bodyBuilder strings.Builder
	_, err = io.Copy(&bodyBuilder, resp.Body)
	if err != nil {
		// 如果读取响应失败，但有旧的缓存，使用旧的缓存（优雅降级）
		if buildIdCache != "" {
			fmt.Printf("读取响应失败，使用旧的buildId: %v\n", err)
			return buildIdCache, nil
		}
		return "", fmt.Errorf("读取响应失败: %w", err)
	}
	body := bodyBuilder.String()

	// 使用提取函数获取 buildId
	buildId := extractBuildId(body)

	// 如果提取失败，但有旧的缓存，使用旧的缓存（优雅降级）
	if buildId == "" {
		if buildIdCache != "" {
			fmt.Println("未找到buildId，使用旧的buildId")
			return buildIdCache, nil
		}
		return "", fmt.Errorf("未找到buildId")
	}

	// 更新缓存
	buildIdCache = buildId
	buildIdCacheTime = time.Now()

	return buildId, nil
}

// getBaseURL 获取完整的API基础URL
func (p *PanSearchPlugin) getBaseURL() (string, error) {
	buildId, err := p.getBuildId()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(BaseURLTemplate, buildId), nil
}

// Search 执行搜索并返回结果
func (p *PanSearchPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 生成缓存键
	cacheKey := keyword
	
	// 检查缓存中是否已有结果
	if cachedItems, ok := searchResultCache.Load(cacheKey); ok {
		// 检查缓存是否过期
		cachedResult := cachedItems.(cachedResponse)
		if time.Since(cachedResult.timestamp) < cacheTTL {
			return cachedResult.results, nil
		}
	}
	
	// 获取API基础URL
	baseURL, err := p.getBaseURL()
	if err != nil {
		return nil, fmt.Errorf("获取API基础URL失败: %w", err)
	}

	// 1. 发起首次请求获取total和第一页数据
	firstPageResults, total, err := p.fetchFirstPage(keyword, baseURL)
	if err != nil {
		// 如果返回404错误，可能是buildId过期，尝试强制刷新buildId
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
			fmt.Println("检测到404错误，buildId可能已过期，尝试强制刷新")

			// 强制刷新buildId
			buildIdMutex.Lock()
			buildIdCache = ""              // 清空缓存
			buildIdCacheTime = time.Time{} // 重置缓存时间
			buildIdMutex.Unlock()

			// 重新获取buildId
			baseURL, err = p.getBaseURL()
			if err != nil {
				return nil, fmt.Errorf("刷新buildId失败: %w", err)
			}

			// 重试请求
			firstPageResults, total, err = p.fetchFirstPage(keyword, baseURL)
			if err != nil {
				return nil, fmt.Errorf("刷新buildId后获取首页仍然失败: %w", err)
			}

			// 成功刷新后，触发后台更新以保持最新状态
			go p.updateBuildId()
		} else {
			return nil, fmt.Errorf("获取首页失败: %w", err)
		}
	}

	allResults := firstPageResults

	// 2. 计算需要的页数，但限制在最大结果数内和API最大页数内
	remainingResults := min(total-PageSize, p.maxResults-PageSize)
	if remainingResults <= 0 {
		results := p.convertResults(allResults, keyword)
		
		// 缓存结果
		searchResultCache.Store(cacheKey, cachedResponse{
			results:   results,
			timestamp: time.Now(),
		})
		
		return results, nil
	}

	// 计算需要的页数，考虑API的100页限制
	neededPages := min((remainingResults+PageSize-1)/PageSize, MaxAPIPages-1) // 向上取整，减1是因为第一页已经获取

	// 如果只需要获取少量页面，直接返回
	if neededPages <= 0 {
		results := p.convertResults(allResults, keyword)
		
		// 缓存结果
		searchResultCache.Store(cacheKey, cachedResponse{
			results:   results,
			timestamp: time.Now(),
		})
		
		return results, nil
	}

	// 根据实际页数确定并发数，但不超过最大并发数
	actualConcurrent := min(neededPages, p.maxConcurrent)

	// 创建适合实际并发数的工作池
	p.workerPool = NewWorkerPool(actualConcurrent)

	// 创建上下文用于管理所有请求
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout*2)
	defer cancel()

	// 创建一个标志，用于标记是否需要刷新buildId
	needRefreshBuildId := &atomic.Bool{}

	// 启动工作池
	p.workerPool.Start(ctx, func(ctx context.Context, task Task) (TaskResult, error) {
		var pageResults []PanSearchItem
		var err error

		for retry := 0; retry <= p.retries; retry++ {
			// 如果有其他协程发现buildId过期，等待刷新完成
			if needRefreshBuildId.Load() {
				time.Sleep(100 * time.Millisecond)
				continue
			}

			pageResults, err = p.fetchPage(task.keyword, task.offset, task.baseURL)
			if err == nil {
				break
			}

			// 如果返回404错误，可能是buildId过期
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
				// 标记需要刷新buildId
				if !needRefreshBuildId.Load() {
					needRefreshBuildId.Store(true)
					// 在一个新的协程中刷新buildId
					go func() {
						buildIdMutex.Lock()
						buildIdCache = ""              // 清空缓存
						buildIdCacheTime = time.Time{} // 重置缓存时间
						buildIdMutex.Unlock()

						// 重新获取buildId
						newBuildId, err := p.getBuildId()
						if err == nil && newBuildId != "" {
							// 更新baseURL
							task.baseURL = fmt.Sprintf(BaseURLTemplate, newBuildId)
							fmt.Printf("成功刷新buildId: %s\n", newBuildId)
						}

						// 重置标志
						needRefreshBuildId.Store(false)
					}()
				}

				// 等待刷新完成
				for i := 0; i < 10 && needRefreshBuildId.Load(); i++ {
					time.Sleep(100 * time.Millisecond)
				}

				// 如果还在刷新，报告错误
				if needRefreshBuildId.Load() {
					return TaskResult{}, fmt.Errorf("404错误，buildId可能已过期: %w", err)
				}

				// 刷新完成后重试
				continue
			}

			if retry < p.retries {
				// 指数退避重试
				select {
				case <-time.After(time.Duration(1<<retry) * 100 * time.Millisecond):
					// 继续重试
				case <-ctx.Done():
					return TaskResult{}, ctx.Err()
				}
			}
		}

		if err != nil {
			return TaskResult{}, fmt.Errorf("获取偏移量 %d 的结果失败: %w", task.offset, err)
		}

		return TaskResult{offset: task.offset, results: pageResults}, nil
	})

	// 提交任务计数器
	submittedTasks := 0

	// 简化批次处理逻辑，考虑到最多只有99页需要获取（第一页已经获取）
	// 使用两个批次：每批次最多50页，避免一次性提交过多任务
	batchSize := (neededPages + 1) / 2 // 将总页数分成两批
	if batchSize < 1 {
		batchSize = neededPages // 如果页数很少，就一次性提交所有任务
	}

	// 提交所有任务
	for i := 0; i < neededPages; i += batchSize {
		// 检查上下文是否已取消
		select {
		case <-ctx.Done():
			// 上下文已取消，停止提交任务
			goto CollectResults
		default:
			// 继续执行
		}

		end := i + batchSize
		if end > neededPages {
			end = neededPages
		}

		// 提交一批任务
		for j := i; j < end; j++ {
			offset := PageSize + j*PageSize
			if offset < p.maxResults {
				task := Task{
					keyword: keyword,
					offset:  offset,
					baseURL: baseURL,
				}

				// 尝试提交任务，如果失败则跳出循环
				if !p.workerPool.Submit(task) {
					fmt.Printf("无法提交任务，工作池可能已关闭\n")
					goto CollectResults
				}

				submittedTasks++
			}
		}

		// 只有在有多个批次且不是最后一批时才等待
		if batchSize < neededPages && end < neededPages {
			select {
			case <-time.After(50 * time.Millisecond):
				// 继续执行
			case <-ctx.Done():
				// 上下文已取消，停止提交任务
				goto CollectResults
			}
		}
	}

CollectResults:
	// 关闭任务提交通道
	go p.workerPool.Close()

	// 收集结果
	resultCount := 0
	errorCount := 0
	var lastError error

	// 使用select非阻塞地收集结果和错误
	for resultCount+errorCount < submittedTasks {
		select {
		case result, ok := <-p.workerPool.results:
			if !ok {
				// 结果通道已关闭
				goto ProcessResults
			}
			allResults = append(allResults, result.results...)
			resultCount++

		case err, ok := <-p.workerPool.errors:
			if !ok {
				// 错误通道已关闭
				goto ProcessResults
			}
			errorCount++
			lastError = err

		case <-ctx.Done():
			// 上下文超时，返回已收集的结果
			results := p.convertResults(allResults, keyword)
			
			// 缓存结果（即使超时也缓存已获取的结果）
			searchResultCache.Store(cacheKey, cachedResponse{
				results:   results,
				timestamp: time.Now(),
			})
			
			return results, fmt.Errorf("搜索超时: %w", ctx.Err())
		}
	}

ProcessResults:
	// 如果所有请求都失败且没有获得首页以外的结果，则返回错误
	if submittedTasks > 0 && errorCount == submittedTasks && len(allResults) == len(firstPageResults) {
		results := p.convertResults(allResults, keyword)
		
		// 缓存结果（即使有错误也缓存已获取的结果）
		searchResultCache.Store(cacheKey, cachedResponse{
			results:   results,
			timestamp: time.Now(),
		})
		
		return results, fmt.Errorf("所有后续页面请求失败: %v", lastError)
	}

	// 4. 去重和格式化结果
	uniqueResults := p.deduplicateItems(allResults)
	results := p.convertResults(uniqueResults, keyword)
	
	// 缓存结果
	searchResultCache.Store(cacheKey, cachedResponse{
		results:   results,
		timestamp: time.Now(),
	})

	return results, nil
}

// fetchFirstPage 获取第一页结果和总数
func (p *PanSearchPlugin) fetchFirstPage(keyword string, baseURL string) ([]PanSearchItem, int, error) {
	// 构建请求URL
	reqURL := fmt.Sprintf("%s?keyword=%s&offset=0", baseURL, url.QueryEscape(keyword))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// 发送请求
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Referer", "https://www.pansearch.me/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	// 发送请求
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode == 404 {
		return nil, 0, fmt.Errorf("404 Not Found，buildId可能已过期")
	}

	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("服务器返回非200状态码: %d", resp.StatusCode)
	}

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("读取响应失败: %w", err)
	}

	// 解析响应
	var apiResp PanSearchResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, 0, fmt.Errorf("解析响应失败: %w", err)
	}

	// 获取total和结果
	total := apiResp.PageProps.Data.Total
	items := apiResp.PageProps.Data.Data

	return items, total, nil
}

// fetchPage 获取指定偏移量的页面
func (p *PanSearchPlugin) fetchPage(keyword string, offset int, baseURL string) ([]PanSearchItem, error) {
	// 构建请求URL
	reqURL := fmt.Sprintf("%s?keyword=%s&offset=%d", baseURL, url.QueryEscape(keyword), offset)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// 发送请求
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Referer", "https://www.pansearch.me/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	// 发送请求
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("404 Not Found，buildId可能已过期")
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("服务器返回非200状态码: %d", resp.StatusCode)
	}

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	// 解析响应
	var apiResp PanSearchResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return apiResp.PageProps.Data.Data, nil
}

// deduplicateItems 去重处理
func (p *PanSearchPlugin) deduplicateItems(items []PanSearchItem) []PanSearchItem {
	// 使用map进行去重，键为资源ID
	uniqueMap := make(map[int]PanSearchItem)

	for _, item := range items {
		uniqueMap[item.ID] = item
	}

	// 将map转回切片
	result := make([]PanSearchItem, 0, len(uniqueMap))
	for _, item := range uniqueMap {
		result = append(result, item)
	}

	return result
}

// convertResults 将API响应转换为标准SearchResult格式
func (p *PanSearchPlugin) convertResults(items []PanSearchItem, keyword string) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for _, item := range items {
		// 提取链接和密码
		linkInfo := extractLinkAndPassword(item.Content)

		// 获取链接类型，确保映射到系统支持的类型
		linkType := item.Pan
		// 将aliyundrive映射到aliyun
		if linkType == "aliyundrive" {
			linkType = "aliyun"
		}

		// 创建链接
		link := model.Link{
			URL:      linkInfo.URL,
			Type:     linkType,
			Password: linkInfo.Password,
		}

		// 创建唯一ID
		uniqueID := fmt.Sprintf("pansearch-%d", item.ID)

		// 解析时间
		var datetime time.Time
		if item.Time != "" {
			// 尝试解析时间，格式：2025-07-07T13:54:43+08:00
			parsedTime, err := time.Parse(time.RFC3339, item.Time)
			if err == nil {
				datetime = parsedTime
			}
		}

		// 如果时间解析失败，使用零值
		if datetime.IsZero() {
			datetime = time.Time{}
		}

		// 创建搜索结果
		result := model.SearchResult{
			UniqueID: uniqueID,
			Title:    extractTitle(item.Content, keyword),
			Content:  item.Content,
			Datetime: datetime,
			Links:    []model.Link{link},
		}

		results = append(results, result)
	}

	return results
}

// LinkInfo 链接信息
type LinkInfo struct {
	URL      string
	Password string
}

// extractLinkAndPassword 从内容中提取链接和密码
func extractLinkAndPassword(content string) LinkInfo {
	// 实现从内容中提取链接和密码的逻辑
	// 这里需要解析HTML内容，提取<a>标签中的链接和密码
	// 简单实现，实际可能需要使用正则表达式或HTML解析库

	// 示例实现
	linkInfo := LinkInfo{}

	// 提取链接
	linkStartIndex := strings.Index(content, "href=\"")
	if linkStartIndex != -1 {
		linkStartIndex += 6 // "href="的长度
		linkEndIndex := strings.Index(content[linkStartIndex:], "\"")
		if linkEndIndex != -1 {
			linkInfo.URL = content[linkStartIndex : linkStartIndex+linkEndIndex]
		}
	}

	// 提取密码
	pwdIndex := strings.Index(content, "?pwd=")
	if pwdIndex != -1 {
		pwdStartIndex := pwdIndex + 5 // "?pwd="的长度
		pwdEndIndex := strings.Index(content[pwdStartIndex:], "\"")
		if pwdEndIndex != -1 {
			linkInfo.Password = content[pwdStartIndex : pwdStartIndex+pwdEndIndex]
		} else {
			// 可能是百度网盘链接结尾形式
			pwdEndIndex = strings.Index(content[pwdStartIndex:], "#")
			if pwdEndIndex != -1 {
				linkInfo.Password = content[pwdStartIndex : pwdStartIndex+pwdEndIndex]
			} else {
				// 取到结尾
				linkInfo.Password = content[pwdStartIndex:]
			}
		}
	}

	return linkInfo
}

// extractTitle 从内容中提取标题
func extractTitle(content string, keyword string) string {
	// 实现从内容中提取标题的逻辑
	// 标题通常在"名称："之后
	titlePrefix := "名称："
	titleStartIndex := strings.Index(content, titlePrefix)
	if titleStartIndex == -1 {
		return keyword // 使用搜索关键词作为默认标题
	}

	titleStartIndex += len(titlePrefix)
	titleEndIndex := strings.Index(content[titleStartIndex:], "\n")
	if titleEndIndex == -1 {
		return cleanHTML(content[titleStartIndex:])
	}

	return cleanHTML(content[titleStartIndex : titleStartIndex+titleEndIndex])
}

// cleanHTML 清理HTML标签
func cleanHTML(html string) string {
	// 实现清理HTML标签的逻辑
	// 这里简单实现，实际可能需要使用HTML解析库

	// 替换常见HTML标签
	replacements := map[string]string{
		"<span class='highlight-keyword'>": "",
		"</span>":                          "",
		"<a class=\"resource-link\" target=\"_blank\" href=\"": "",
		"</a>": "",
		"<br>": "\n",
		"<p>":  "",
		"</p>": "\n",
	}

	result := html
	for tag, replacement := range replacements {
		result = strings.Replace(result, tag, replacement, -1)
	}

	// 清理其他HTML标签
	for {
		startIndex := strings.Index(result, "<")
		if startIndex == -1 {
			break
		}

		endIndex := strings.Index(result[startIndex:], ">")
		if endIndex == -1 {
			break
		}

		result = result[:startIndex] + result[startIndex+endIndex+1:]
	}

	return strings.TrimSpace(result)
}

// min 返回两个int中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// PanSearchResponse API响应结构
type PanSearchResponse struct {
	PageProps struct {
		Data struct {
			Total int             `json:"total"`
			Data  []PanSearchItem `json:"data"`
			Time  int             `json:"time"`
		} `json:"data"`
		Limit    int  `json:"limit"`
		IsMobile bool `json:"isMobile"`
	} `json:"pageProps"`
	NSSP bool `json:"__N_SSP"`
}

// PanSearchItem API响应中的单个结果项
type PanSearchItem struct {
	ID      int    `json:"id"`
	Content string `json:"content"`
	Pan     string `json:"pan"`
	Image   string `json:"image"`
	Time    string `json:"time"`
}
