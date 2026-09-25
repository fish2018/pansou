package xinjuc

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// 预编译的正则表达式
var (
	// 百度网盘链接正则表达式（网站只有百度盘）
	// 要求s/后面至少10个字符，避免匹配到不完整的链接
	baiduLinkRegex = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]{10,}(?:\?pwd=[0-9a-zA-Z]+)?`)

	// 提取码正则表达式
	pwdRegex = regexp.MustCompile(`提取码[:：]\s*([a-zA-Z0-9]{4})`)

	// 从URL参数提取密码
	pwdURLRegex = regexp.MustCompile(`\?pwd=([0-9a-zA-Z]+)`)

	// 从详情链接提取ID
	detailIDRegex = regexp.MustCompile(`/(\d+)\.html`)

	// 缓存相关
	detailCache     = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL        = 1 * time.Hour
)

const (
	// 超时时间
	DefaultTimeout = 10 * time.Second
	DetailTimeout  = 8 * time.Second

	// 并发数
	MaxConcurrency = 15

	// HTTP连接池配置
	MaxIdleConns        = 50
	MaxIdleConnsPerHost = 20
	MaxConnsPerHost     = 30
	IdleConnTimeout     = 90 * time.Second

	// 网站URL
	SiteURL = "https://www.xinjuc.com"
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewXinjucPlugin())

	// 启动缓存清理goroutine
	go startCacheCleaner()
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		// 清空所有缓存
		detailCache = sync.Map{}
		lastCleanupTime = time.Now()
	}
}

// XinjucPlugin 新剧坊插件
type XinjucPlugin struct {
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
	}
	return &http.Client{Transport: transport, Timeout: DefaultTimeout}
}

// NewXinjucPlugin 创建新的新剧坊插件
func NewXinjucPlugin() *XinjucPlugin {
	return &XinjucPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("xinjuc", 2), // 优先级2：质量良好的数据源
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *XinjucPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XinjucPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *XinjucPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("%s/?s=%s", SiteURL, url.QueryEscape(keyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置完整的请求头（避免反爬虫）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", SiteURL)

	// 5. 发送请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 6. 解析搜索结果页面
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}

	// 7. 提取搜索结果
	var results []model.SearchResult

	// 查找搜索结果列表
	postList := doc.Find("div.row-xs.post-list article.post-item")
	if postList.Length() == 0 {
		return []model.SearchResult{}, nil // 没有搜索结果
	}

	// 8. 解析每个搜索结果项
	postList.Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchItem(s, keyword)
		if result.UniqueID != "" {
			results = append(results, result)
		}
	})

	// 9. 异步获取详情页信息
	enhancedResults := p.enhanceWithDetails(client, results)

	// 10. 关键词过滤
	return plugin.FilterResultsByKeyword(enhancedResults, keyword), nil
}

// parseSearchItem 解析单个搜索结果项
func (p *XinjucPlugin) parseSearchItem(s *goquery.Selection, keyword string) model.SearchResult {
	result := model.SearchResult{}

	// 提取详情页链接
	linkElem := s.Find("div.post-image a")
	if linkElem.Length() == 0 {
		return result
	}

	detailLink, exists := linkElem.Attr("href")
	if !exists || detailLink == "" {
		return result
	}

	// 处理相对路径
	if !strings.HasPrefix(detailLink, "http") {
		if strings.HasPrefix(detailLink, "/") {
			detailLink = SiteURL + detailLink
		} else {
			detailLink = SiteURL + "/" + detailLink
		}
	}

	// 提取ID
	matches := detailIDRegex.FindStringSubmatch(detailLink)
	if len(matches) < 2 {
		return result
	}
	itemID := matches[1]
	result.UniqueID = fmt.Sprintf("%s-%s", p.Name(), itemID)

	// 提取标题
	titleElem := s.Find("h5.post-title a")
	if titleElem.Length() > 0 {
		result.Title = strings.TrimSpace(titleElem.Text())
	}

	// 提取标记（如"更至163"、"1080P"）
	markElem := s.Find("div.mark span")
	if markElem.Length() > 0 {
		mark := strings.TrimSpace(markElem.Text())
		if mark != "" {
			result.Tags = []string{mark}
		}
	}

	// 提取更新时间
	timeElem := s.Find("div.post-footer span.time")
	if timeElem.Length() > 0 {
		timeStr := strings.TrimSpace(timeElem.Text())
		result.Datetime = p.parseTime(timeStr)
	} else {
		result.Datetime = time.Now()
	}

	result.Channel = "" // 插件搜索结果必须为空字符串

	// 将详情页链接存储在Content中，后续获取详情
	result.Content = detailLink

	return result
}

// parseTime 解析时间字符串
func (p *XinjucPlugin) parseTime(timeStr string) time.Time {
	// 时间格式示例: "2025-04-21 更新", "04-21"
	timeStr = strings.Replace(timeStr, " 更新", "", -1)
	timeStr = strings.TrimSpace(timeStr)

	// 尝试多种时间格式
	formats := []string{
		"2006-01-02",
		"01-02",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			// 如果只有月-日，补充当前年份
			if format == "01-02" {
				now := time.Now()
				t = time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
			}
			return t
		}
	}

	return time.Now()
}

// enhanceWithDetails 异步获取详情页信息
func (p *XinjucPlugin) enhanceWithDetails(client *http.Client, results []model.SearchResult) []model.SearchResult {
	var wg sync.WaitGroup
	var mu sync.Mutex

	// 使用信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)

	enhancedResults := make([]model.SearchResult, 0, len(results))

	for _, result := range results {
		wg.Add(1)
		go func(r model.SearchResult) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 从缓存或详情页获取链接
			links, content := p.getDetailInfo(client, r.Content)

			// 更新结果
			r.Links = links
			r.Content = content

			// 只添加有链接的结果
			if len(links) > 0 {
				mu.Lock()
				enhancedResults = append(enhancedResults, r)
				mu.Unlock()
			}
		}(result)
	}

	wg.Wait()
	return enhancedResults
}

// getDetailInfo 获取详情页信息（带缓存）
func (p *XinjucPlugin) getDetailInfo(client *http.Client, detailURL string) ([]model.Link, string) {
	// 检查缓存
	if cached, ok := detailCache.Load(detailURL); ok {
		cachedData := cached.(DetailCacheData)
		if time.Since(cachedData.Timestamp) < cacheTTL {
			return cachedData.Links, cachedData.Content
		}
	}

	// 获取详情页
	links, content := p.fetchDetailPage(client, detailURL)

	// 存入缓存
	if len(links) > 0 {
		detailCache.Store(detailURL, DetailCacheData{
			Links:     links,
			Content:   content,
			Timestamp: time.Now(),
		})
	}

	return links, content
}

// DetailCacheData 详情页缓存数据
type DetailCacheData struct {
	Links     []model.Link
	Content   string
	Timestamp time.Time
}

// fetchDetailPage 获取详情页信息
func (p *XinjucPlugin) fetchDetailPage(client *http.Client, detailURL string) ([]model.Link, string) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DetailTimeout)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, ""
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", SiteURL)

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, ""
	}

	// 解析页面
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, ""
	}

	// 查找文章内容区域
	articleContent := doc.Find("div.article-content")
	if articleContent.Length() == 0 {
		return nil, ""
	}

	// 提取百度盘链接（从整个文档中提取）
	links := p.extractLinksFromDoc(doc)

	// 提取简介（从文章内容中提取）
	content := p.extractContent(articleContent)

	return links, content
}

// extractLinksFromDoc 从整个文档中提取百度盘链接
func (p *XinjucPlugin) extractLinksFromDoc(doc *goquery.Document) []model.Link {
	var links []model.Link
	linkMap := make(map[string]bool) // 去重（使用trim后的URL）

	// 获取整个页面的HTML内容
	htmlContent, _ := doc.Html()

	// 提取提取码（多种方式）
	password := ""

	// 方式1: 从文本中提取提取码
	if match := pwdRegex.FindStringSubmatch(htmlContent); len(match) > 1 {
		password = match[1]
	}

	// 方式2: 使用正则表达式提取所有百度盘链接
	baiduLinks := baiduLinkRegex.FindAllString(htmlContent, -1)
	for _, baiduURL := range baiduLinks {
		// 清理链接（去除首尾空格）
		baiduURL = strings.TrimSpace(baiduURL)

		// 验证链接有效性
		if !p.isValidBaiduLink(baiduURL) {
			continue
		}

		// 去重
		if !linkMap[baiduURL] {
			linkMap[baiduURL] = true

			// 从URL中提取密码（如果有）
			urlPassword := password
			if match := pwdURLRegex.FindStringSubmatch(baiduURL); len(match) > 1 {
				urlPassword = match[1]
			}

			links = append(links, model.Link{
				Type:     "baidu",
				URL:      baiduURL,
				Password: urlPassword,
			})
		}
	}

	// 方式3: 从<a>标签中查找百度盘链接（作为补充）
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 清理链接
		href = strings.TrimSpace(href)

		// 必须是纯百度盘域名开头，避免匹配到分享链接中的百度盘URL
		if !strings.HasPrefix(href, "http://pan.baidu.com") && !strings.HasPrefix(href, "https://pan.baidu.com") {
			return
		}

		// 验证链接有效性
		if !p.isValidBaiduLink(href) {
			return
		}

		// 去重
		if !linkMap[href] {
			linkMap[href] = true

			// 从URL中提取密码（如果有）
			urlPassword := password
			if match := pwdURLRegex.FindStringSubmatch(href); len(match) > 1 {
				urlPassword = match[1]
			}

			links = append(links, model.Link{
				Type:     "baidu",
				URL:      href,
				Password: urlPassword,
			})
		}
	})

	return links
}

// isValidBaiduLink 验证百度盘链接的有效性
func (p *XinjucPlugin) isValidBaiduLink(link string) bool {
	// 必须是百度盘域名
	if !strings.HasPrefix(link, "http://pan.baidu.com") && !strings.HasPrefix(link, "https://pan.baidu.com") {
		return false
	}

	// 必须包含 /s/ 路径
	if !strings.Contains(link, "/s/") {
		return false
	}

	// 使用正则验证格式
	if !baiduLinkRegex.MatchString(link) {
		return false
	}

	return true
}

// extractContent 提取简介
func (p *XinjucPlugin) extractContent(articleContent *goquery.Selection) string {
	// 提取文本内容
	content := strings.TrimSpace(articleContent.Text())

	// 清理空白字符
	content = xinjucRe1.ReplaceAllString(content, " ")

	// 移除百度盘相关的文本
	content = xinjucRe2.ReplaceAllString(content, "")
	content = xinjucRe3.ReplaceAllString(content, "")
	content = xinjucRe4.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)

	// 限制长度
	if len(content) > 300 {
		content = content[:300] + "..."
	}

	return content
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *XinjucPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
			status := resp.StatusCode
			resp.Body.Close()
			if err == nil {
				// Do 成功但状态码非 200。此前这里只执行 lastErr = err，
				// err 为 nil 时会把 lastErr 清空，三次失败后仅报出
				// "%!w(<nil>)"，真实状态码被丢掉、无法定位失败原因。
				err = fmt.Errorf("HTTP 状态码 %d", status)
			}
		}
		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	xinjucRe1 = regexp.MustCompile(`\s+`)
	xinjucRe2 = regexp.MustCompile(`百度云网盘资源下载地址[:：]?\s*`)
	xinjucRe3 = regexp.MustCompile(`链接[:：]?\s*https?://pan\.baidu\.com/[^\s]+`)
	xinjucRe4 = regexp.MustCompile(`提取码[:：]?\s*[a-zA-Z0-9]{4}`)
)
