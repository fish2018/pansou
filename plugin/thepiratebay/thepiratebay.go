package thepiratebay

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

// 常量定义
const (
	// 搜索URL格式 - 第1页
	SearchURL = "https://thpibay.xyz/search/%s/1/99/0"

	// 分页搜索URL格式 - 其他页
	SearchPageURL = "https://thpibay.xyz/search/%s/%d/99/0"

	// 默认超时时间
	DefaultTimeout = 10 * time.Second

	// 并发数限制
	MaxConcurrency = 200

	// 最大分页数（避免无限请求）
	MaxPages = 30

	// HTTP连接池配置 - 针对高并发优化
	MaxIdleConns        = 200 // 增加全局空闲连接池大小
	MaxIdleConnsPerHost = 80  // 增加每个主机的空闲连接数，提高连接复用
	MaxConnsPerHost     = 150 // 增加每个主机的最大连接数，支持高并发
	IdleConnTimeout     = 90 * time.Second
)

// 预编译正则表达式
var (
	// 磁力链接的正则表达式
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}[^"'\s]*`)

	// 种子ID提取正则表达式
	torrentIDRegex = regexp.MustCompile(`/torrent/(\d+)/`)

	// 时间解析正则表达式 - 两种格式
	timeFormat1Regex = regexp.MustCompile(`(\d{2}-\d{2})\s+(\d{2}:\d{2})`) // MM-DD HH:MM
	timeFormat2Regex = regexp.MustCompile(`(\d{2}-\d{2})\s+(\d{4})`)       // MM-DD YYYY

	// 文件大小正则表达式
	fileSizeRegex = regexp.MustCompile(`Size\s+([0-9.]+)\s*(&nbsp;)?\s*([KMGT]?i?B)`)
)

// 缓存相关变量
var (
	// 页面缓存
	pageCache = sync.Map{}

	// 最后一次清理缓存的时间
	lastCacheCleanTime = time.Now()

	// 缓存有效期
	cacheTTL = 24 * time.Hour
)

// 缓存的页面响应
type pageResponse struct {
	Results   []model.SearchResult
	TotalPage int
	Timestamp time.Time
}

// ThePirateBayPlugin 海盗湾搜索插件
type ThePirateBayPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

// createOptimizedHTTPClient 创建优化的HTTP客户端
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		MaxConnsPerHost:     MaxConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		DisableKeepAlives:   false,
		DisableCompression:  false,
		WriteBufferSize:     16 * 1024,
		ReadBufferSize:      16 * 1024,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
	}
}

// NewThePirateBayPlugin 创建新的海盗湾搜索异步插件
func NewThePirateBayPlugin() *ThePirateBayPlugin {
	return &ThePirateBayPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("thepiratebay", 3, true), // 跳过Service层过滤
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// 初始化插件
func init() {
	plugin.RegisterGlobalPlugin(NewThePirateBayPlugin())

	// 启动缓存清理
	go startCacheCleaner()
}

// startCacheCleaner 定期清理缓存
func startCacheCleaner() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		// 清空页面缓存
		pageCache = sync.Map{}
		lastCacheCleanTime = time.Now()
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *ThePirateBayPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *ThePirateBayPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑（支持分页）
func (p *ThePirateBayPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	// 检查是否提供了英文标题参数 - 对英文搜索更友好
	searchKeyword := keyword
	if ext != nil {
		if titleEn, exists := ext["title_en"]; exists {
			if titleEnStr, ok := titleEn.(string); ok && titleEnStr != "" {
				searchKeyword = titleEnStr
			}
		}
	}

	encodedKeyword := url.PathEscape(searchKeyword)
	allResults := make([]model.SearchResult, 0)

	// 1. 搜索第一页，获取总页数
	firstPageResults, totalPages, err := p.searchPage(client, encodedKeyword, 1)
	if err != nil {
		return nil, err
	}
	allResults = append(allResults, firstPageResults...)

	// 2. 如果有多页，并发搜索其他页面（限制最大页数）
	maxPagesToSearch := totalPages
	if maxPagesToSearch > MaxPages {
		maxPagesToSearch = MaxPages
	}

	if totalPages > 1 && maxPagesToSearch > 1 {
		// 并发搜索其他页面 - 参考fox4k的并发策略
		var wg sync.WaitGroup
		var mu sync.Mutex

		// 使用信号量控制并发数
		semaphore := make(chan struct{}, MaxConcurrency)

		// 存储每页结果
		pageResults := make(map[int][]model.SearchResult)

		for page := 2; page <= maxPagesToSearch; page++ {
			wg.Add(1)
			go func(pageNum int) {
				defer wg.Done()

				// 获取信号量
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				currentPageResults, _, err := p.searchPage(client, encodedKeyword, pageNum)
				if err == nil && len(currentPageResults) > 0 {
					mu.Lock()
					pageResults[pageNum] = currentPageResults
					mu.Unlock()
				}
			}(page)
		}

		wg.Wait()

		// 按页码顺序合并所有页面的结果
		for page := 2; page <= maxPagesToSearch; page++ {
			if results, exists := pageResults[page]; exists {
				allResults = append(allResults, results...)
			}
		}
	}

	// 3. 过滤关键词匹配的结果 - 使用处理后的搜索关键词进行过滤
	// 注意：标题中的'.'已经被替换为空格，提高匹配准确度
	results := plugin.FilterResultsByKeyword(allResults, searchKeyword)

	return results, nil
}

// searchPage 搜索指定页面
func (p *ThePirateBayPlugin) searchPage(client *http.Client, encodedKeyword string, page int) ([]model.SearchResult, int, error) {
	// 1. 构建搜索URL
	var searchURL string
	if page == 1 {
		searchURL = fmt.Sprintf(SearchURL, encodedKeyword)
	} else {
		searchURL = fmt.Sprintf(SearchPageURL, encodedKeyword, page)
	}

	// 2. 检查缓存
	cacheKey := fmt.Sprintf("%s-page-%d", encodedKeyword, page)
	if cached, ok := pageCache.Load(cacheKey); ok {
		if cachedResp, ok := cached.(*pageResponse); ok {
			if time.Since(cachedResp.Timestamp) < cacheTTL {
				return cachedResp.Results, cachedResp.TotalPage, nil
			}
		}
	}

	// 3. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 4. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 5. 设置完整的请求头 - 参考插件开发指南的最佳实践
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", "https://thpibay.xyz/")

	// 6. 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 第%d页搜索请求失败: %w", p.Name(), page, err)
	}
	defer resp.Body.Close()

	// 7. 检查状态码
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("[%s] 第%d页请求返回状态码: %d", p.Name(), page, resp.StatusCode)
	}

	// 8. 解析HTML响应
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 第%d页HTML解析失败: %w", p.Name(), page, err)
	}

	// 9. 解析分页信息（只在第一页解析）
	totalPages := 1
	if page == 1 {
		totalPages = p.parseTotalPages(doc)
	}

	// 10. 提取搜索结果
	results := make([]model.SearchResult, 0)
	doc.Find("table#searchResult tr").Each(func(i int, s *goquery.Selection) {
		// 跳过表头
		if s.HasClass("header") {
			return
		}

		result := p.parseSearchResultItem(s)
		if result != nil {
			results = append(results, *result)
		}
	})

	// 11. 缓存结果
	cachedResp := &pageResponse{
		Results:   results,
		TotalPage: totalPages,
		Timestamp: time.Now(),
	}
	pageCache.Store(cacheKey, cachedResp)

	return results, totalPages, nil
}

// parseTotalPages 解析总页数
func (p *ThePirateBayPlugin) parseTotalPages(doc *goquery.Document) int {
	// 查找分页信息，ThePirateBay的分页在底部
	// 格式: <b>1</b> <a href="/search/...">2</a> <a href="/search/...">3</a> ...

	maxPage := 1
	doc.Find("table#searchResult").Next().Find("a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}

		// 从URL中提取页码: /search/keyword/PAGE/99/0
		parts := strings.Split(href, "/")
		if len(parts) >= 4 {
			if pageStr := parts[3]; pageStr != "" {
				if pageNum, err := strconv.Atoi(pageStr); err == nil && pageNum > maxPage {
					maxPage = pageNum
				}
			}
		}
	})

	// 也检查分页导航区域
	doc.Find("td[colspan='9'] a").Each(func(i int, s *goquery.Selection) {
		pageText := strings.TrimSpace(s.Text())
		if pageNum, err := strconv.Atoi(pageText); err == nil && pageNum > maxPage {
			maxPage = pageNum
		}
	})

	// 限制最大页数，避免过度请求
	if maxPage > MaxPages {
		maxPage = MaxPages
	}

	return maxPage
}

// parseSearchResultItem 解析单个搜索结果项
func (p *ThePirateBayPlugin) parseSearchResultItem(s *goquery.Selection) *model.SearchResult {
	// 获取详情页链接和标题
	titleElement := s.Find(".detName a.detLink").First()
	if titleElement.Length() == 0 {
		return nil
	}

	title := strings.TrimSpace(titleElement.Text())
	if title == "" {
		return nil
	}

	// 优化标题格式：将'.'替换为空格，便于关键词匹配
	title = strings.ReplaceAll(title, ".", " ")

	detailURL, exists := titleElement.Attr("href")
	if !exists || detailURL == "" {
		return nil
	}

	// 补全URL
	if strings.HasPrefix(detailURL, "/") {
		detailURL = "https://thpibay.xyz" + detailURL
	}

	// 提取种子ID
	matches := torrentIDRegex.FindStringSubmatch(detailURL)
	if len(matches) < 2 {
		return nil
	}
	torrentID := matches[1]

	// 获取磁力链接
	magnetElement := s.Find("a[href^='magnet:']").First()
	magnetURL, exists := magnetElement.Attr("href")
	if !exists || magnetURL == "" {
		return nil // ThePirateBay只提供磁力链接，没有磁力链接就跳过
	}

	// 验证磁力链接格式
	if !magnetLinkRegex.MatchString(magnetURL) {
		return nil
	}

	// 获取分类信息
	var tags []string
	s.Find(".vertTh a").Each(func(i int, elem *goquery.Selection) {
		tag := strings.TrimSpace(elem.Text())
		if tag != "" {
			tags = append(tags, tag)
		}
	})

	// 获取种子元数据（文件大小、上传时间、上传者等）
	detDesc := s.Find(".detDesc").Text()

	// 解析上传时间
	datetime := p.parseUploadTime(detDesc)

	// 提取文件大小信息
	var content string
	if sizeMatch := fileSizeRegex.FindStringSubmatch(detDesc); len(sizeMatch) > 0 {
		content = fmt.Sprintf("文件大小: %s%s", sizeMatch[1], sizeMatch[3])
	}

	// 添加其他元数据信息
	if content != "" {
		content += ", "
	}
	content += fmt.Sprintf("上传信息: %s", strings.TrimSpace(detDesc))

	// 获取Seeders和Leechers数量
	seeders := strings.TrimSpace(s.Find("td").Eq(2).Text())
	leechers := strings.TrimSpace(s.Find("td").Eq(3).Text())

	if seeders != "" && leechers != "" {
		content += fmt.Sprintf(", Seeders: %s, Leechers: %s", seeders, leechers)
	}

	// 创建磁力链接
	magnetLink := model.Link{
		Type:     "magnet",
		URL:      magnetURL,
		Password: "", // 磁力链接不需要密码
	}

	return &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), torrentID),
		Title:    title,
		Content:  content,
		Datetime: datetime,
		Tags:     tags,
		Links:    []model.Link{magnetLink},
		Channel:  "", // 插件搜索结果，Channel必须为空
	}
}

// parseUploadTime 解析上传时间的两种格式
func (p *ThePirateBayPlugin) parseUploadTime(timeStr string) time.Time {
	// 去除&nbsp;
	timeStr = strings.ReplaceAll(timeStr, "&nbsp;", " ")

	// 格式1: "07-28 05:35" (当年)
	if matches := timeFormat1Regex.FindStringSubmatch(timeStr); len(matches) >= 3 {
		currentYear := time.Now().Year()
		fullTimeStr := fmt.Sprintf("%d-%s %s", currentYear, matches[1], matches[2])
		if t, err := time.Parse("2006-01-02 15:04", fullTimeStr); err == nil {
			return t
		}
	}

	// 格式2: "10-30 2023" (历史)
	if matches := timeFormat2Regex.FindStringSubmatch(timeStr); len(matches) >= 3 {
		dateStr := fmt.Sprintf("%s-%s", matches[2], matches[1]) // YYYY-MM-DD
		if t, err := time.Parse("2006-01-02", dateStr); err == nil {
			return t
		}
	}

	// 默认返回当前时间
	return time.Now()
}

// doRequestWithRetry 带重试机制的HTTP请求 - 参考插件开发指南的最佳实践
func (p *ThePirateBayPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}
