package pansearch

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/config"
	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

// 预编译正则表达式
var (
	// 从HTML中提取buildId的正则表达式
	buildIdRegex = regexp.MustCompile(`"buildId":"([^"]+)"`)

	// 从__NEXT_DATA__脚本中提取数据的正则表达式
	nextDataRegex = regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
	passwordRegex = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|code)\s*[:=：]?\s*([0-9a-z]{4,8})`)

	// 缓存相关变量
	searchResultCache  = sync.Map{}
	lastCacheCleanTime = time.Now()
	cacheTTL           = 1 * time.Hour
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
	DefaultTimeout = 15 * time.Second
	PageSize       = 10
	MaxResults     = 50
	MaxConcurrent  = 4
	MaxRetries     = 2
	MaxAPIPages    = 5

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

// PanSearchAsyncPlugin 盘搜异步插件
type PanSearchAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	timeout       time.Duration
	maxResults    int
	maxConcurrent int
	retries       int
}

// WorkerPool 工作池结构
type WorkerPool struct {
	tasks       chan Task
	results     chan TaskResult
	errors      chan error
	workerCount int
	wg          sync.WaitGroup
	closed      atomic.Bool
	mu          sync.Mutex
	closeOnce   sync.Once
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
func NewWorkerPool(workerCount, bufferSize int) *WorkerPool {
	if workerCount < 1 {
		workerCount = 1
	}
	if bufferSize < workerCount {
		bufferSize = workerCount
	}

	return &WorkerPool{
		tasks:       make(chan Task, bufferSize),
		results:     make(chan TaskResult, bufferSize),
		errors:      make(chan error, bufferSize),
		workerCount: workerCount,
	}
}

// Start 启动工作池
func (wp *WorkerPool) Start(ctx context.Context, handler func(ctx context.Context, task Task) (TaskResult, error)) {
	for i := 0; i < wp.workerCount; i++ {
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
						case <-ctx.Done():
							return
						}
					} else {
						select {
						case wp.results <- result:
						case <-ctx.Done():
							return
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

	if wp.closed.Load() {
		return false
	}

	wp.tasks <- task
	return true
}

// Close 关闭工作池
func (wp *WorkerPool) Close() {
	wp.closeOnce.Do(func() {
		wp.mu.Lock()
		wp.closed.Store(true)
		close(wp.tasks)
		wp.mu.Unlock()

		wp.wg.Wait()
		close(wp.results)
		close(wp.errors)
	})
}

// NewPanSearchPlugin 创建新的盘搜异步插件
func NewPanSearchPlugin() *PanSearchAsyncPlugin {
	timeout := DefaultTimeout
	maxConcurrent := MaxConcurrent

	p := &PanSearchAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("pansearch", 3),
		timeout:         timeout,
		maxResults:      MaxResults,
		maxConcurrent:   maxConcurrent,
		retries:         MaxRetries,
	}

	return p
}

// startBuildIdUpdater 启动一个定期更新 buildId 的后台协程
func (p *PanSearchAsyncPlugin) startBuildIdUpdater() {
	// 每10分钟更新一次 buildId
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		p.updateBuildId()
	}
}

// updateBuildId 更新 buildId 缓存
func (p *PanSearchAsyncPlugin) updateBuildId() {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	// 发送请求获取页面
	req, err := http.NewRequestWithContext(ctx, "GET", WebsiteURL, nil)
	if err != nil {
		// fmt.Printf("创建请求失败: %v\n", err)
		return
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")

	resp, err := p.GetClient().Do(req)
	if err != nil {
		// fmt.Printf("请求失败: %v\n", err)
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
		// fmt.Printf("读取响应失败: %v\n", err)
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
func (p *PanSearchAsyncPlugin) Name() string {
	return "pansearch"
}

// Priority 返回插件优先级
func (p *PanSearchAsyncPlugin) Priority() int {
	return 3 // 中等优先级
}

// getBuildId 获取buildId，优先使用缓存
func (p *PanSearchAsyncPlugin) getBuildId(client *http.Client) (string, error) {
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
			// fmt.Printf("创建请求失败，使用旧的buildId: %v\n", err)
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

	client = p.requestClient(client)

	// 使用重试机制发送请求
	var resp *http.Response
	var respErr error

	for retry := 0; retry <= p.retries; retry++ {
		if retry > 0 {
			// 指数退避重试
			backoffTime := time.Duration(1<<uint(retry-1)) * 100 * time.Millisecond
			time.Sleep(backoffTime)
		}

		resp, respErr = p.GetClient().Do(req)
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
			// fmt.Printf("请求失败，使用旧的buildId: %v\n", respErr)
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
			// fmt.Printf("读取响应失败，使用旧的buildId: %v\n", err)
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
			// fmt.Println("未找到buildId，使用旧的buildId")
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
func (p *PanSearchAsyncPlugin) getBaseURL(client *http.Client) (string, error) {
	buildId, err := p.getBuildId(client)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(BaseURLTemplate, buildId), nil
}

func (p *PanSearchAsyncPlugin) requestClient(fallback *http.Client) *http.Client {
	proxyURL := ""
	if config.AppConfig != nil {
		proxyURL = panSearchFirstNonEmpty(config.AppConfig.ProxyURL, config.AppConfig.HTTPSProxyURL, config.AppConfig.HTTPProxyURL)
	}
	if proxyURL != "" {
		client, err := util.NewHTTPClient(proxyURL)
		if err == nil {
			client.Timeout = p.timeout
			return client
		}
	}
	if fallback != nil {
		return fallback
	}
	return &http.Client{Timeout: p.timeout}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *PanSearchAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *PanSearchAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 执行具体的搜索逻辑
func (p *PanSearchAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, fmt.Errorf("[%s] 关键词不能为空", p.Name())
	}
	client = p.requestClient(client)

	baseURL, err := p.getBaseURL(client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取API基础URL失败: %w", p.Name(), err)
	}

	firstPageResults, total, err := p.fetchFirstPage(keyword, baseURL, client)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not Found") {
			buildIdMutex.Lock()
			buildIdCache = ""
			buildIdCacheTime = time.Time{}
			buildIdMutex.Unlock()
			baseURL, err = p.getBaseURL(client)
			if err != nil {
				return nil, fmt.Errorf("[%s] 刷新 buildId 失败: %w", p.Name(), err)
			}
			firstPageResults, total, err = p.fetchFirstPage(keyword, baseURL, client)
			if err != nil {
				return nil, fmt.Errorf("[%s] 刷新 buildId 后获取首页失败: %w", p.Name(), err)
			}
		} else {
			return nil, fmt.Errorf("[%s] 获取首页失败: %w", p.Name(), err)
		}
	}

	allResults := append([]PanSearchItem(nil), firstPageResults...)
	pageCount := min((min(total, p.maxResults)+PageSize-1)/PageSize, MaxAPIPages)
	if pageCount > 1 {
		type pageResult struct {
			offset int
			items  []PanSearchItem
		}
		resultCh := make(chan pageResult, pageCount-1)
		sem := make(chan struct{}, min(p.maxConcurrent, pageCount-1))
		var wg sync.WaitGroup

		for page := 1; page < pageCount; page++ {
			offset := page * PageSize
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				items, pageErr := p.fetchPage(keyword, offset, baseURL, client)
				if pageErr == nil {
					resultCh <- pageResult{offset: offset, items: items}
				}
			}()
		}
		wg.Wait()
		close(resultCh)

		pages := make([]pageResult, 0, pageCount-1)
		for page := range resultCh {
			pages = append(pages, page)
		}
		sort.Slice(pages, func(i, j int) bool { return pages[i].offset < pages[j].offset })
		for _, page := range pages {
			allResults = append(allResults, page.items...)
		}
	}

	uniqueResults := p.deduplicateItems(allResults)
	results := p.convertResults(uniqueResults, keyword)
	searchResultCache.Store(keyword, cachedResponse{
		results:   results,
		timestamp: time.Now(),
	})
	return results, nil
}

// fetchFirstPage 获取第一页结果和总数
func (p *PanSearchAsyncPlugin) fetchFirstPage(keyword string, baseURL string, client *http.Client) ([]PanSearchItem, int, error) {
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
	resp, err := client.Do(req)
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
func (p *PanSearchAsyncPlugin) fetchPage(keyword string, offset int, baseURL string, client *http.Client) ([]PanSearchItem, error) {
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
	resp, err := client.Do(req)
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
func (p *PanSearchAsyncPlugin) deduplicateItems(items []PanSearchItem) []PanSearchItem {
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
func (p *PanSearchAsyncPlugin) convertResults(items []PanSearchItem, keyword string) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for _, item := range items {
		linkInfo := extractLinkAndPassword(item.Content)
		if linkInfo.URL == "" {
			continue
		}

		linkType := normalizePanSearchType(item.Pan, linkInfo.URL)
		title := extractTitle(item.Content, keyword)
		link := model.Link{
			Type:      linkType,
			URL:       linkInfo.URL,
			Password:  linkInfo.Password,
			WorkTitle: title,
		}

		var datetime time.Time
		if item.Time != "" {
			datetime, _ = time.Parse(time.RFC3339, item.Time)
		}

		result := model.SearchResult{
			MessageID: fmt.Sprintf("%d", item.ID),
			UniqueID:  fmt.Sprintf("%s-%d", p.Name(), item.ID),
			Channel:   "",
			Title:     title,
			Content:   cleanHTML(item.Content),
			Datetime:  datetime,
			Links:     []model.Link{link},
			Tags:      []string{linkType, p.Name()},
		}
		if imageURL := strings.TrimSpace(item.Image); strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
			result.Images = []string{imageURL}
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
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + content + "</div>"))
	if err != nil {
		return LinkInfo{}
	}
	href, ok := doc.Find("a.resource-link, a[href]").First().Attr("href")
	if !ok {
		return LinkInfo{}
	}
	href = html.UnescapeString(strings.TrimSpace(href))
	if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
		return LinkInfo{}
	}

	password := ""
	if parsed, parseErr := url.Parse(href); parseErr == nil {
		for _, key := range []string{"pwd", "password", "passcode", "code"} {
			if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
				password = value
				break
			}
		}
	}
	if password == "" {
		if match := passwordRegex.FindStringSubmatch(doc.Text()); len(match) > 1 {
			password = match[1]
		}
	}
	return LinkInfo{URL: strings.TrimRight(href, "#"), Password: password}
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
func cleanHTML(value string) string {
	value = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n").Replace(value)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + value + "</div>"))
	if err != nil {
		return strings.TrimSpace(html.UnescapeString(value))
	}
	lines := strings.Split(html.UnescapeString(doc.Text()), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, "\n")
}

func normalizePanSearchType(rawType string, rawURL string) string {
	normalized := strings.ToLower(strings.TrimSpace(rawType))
	switch normalized {
	case "ali", "alipan", "aliyun", "aliyundrive":
		return "aliyun"
	case "quark", "uc", "baidu", "xunlei", "tianyi", "115", "123", "mobile", "pikpak":
		return normalized
	}

	lowerURL := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lowerURL, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lowerURL, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lowerURL, "alipan.com"), strings.Contains(lowerURL, "aliyundrive.com"):
		return "aliyun"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lowerURL, "115.com"):
		return "115"
	case strings.Contains(lowerURL, "123pan"), strings.Contains(lowerURL, "123865.com"), strings.Contains(lowerURL, "123684.com"):
		return "123"
	case strings.Contains(lowerURL, "139.com"), strings.Contains(lowerURL, "10086.cn"):
		return "mobile"
	case strings.Contains(lowerURL, "pikpak"):
		return "pikpak"
	default:
		return "others"
	}
}

func panSearchFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
