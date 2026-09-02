package djgou

import (
	"context"
	"fmt"
	"io"
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
	// 夸克网盘链接正则表达式（网站只有夸克网盘）
	// 注意：夸克链接可能包含字母、数字、下划线、连字符等字符
	quarkLinkRegex = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z_\-]+`)
	btwafURLRegex  = regexp.MustCompile(`window\.location\.href\s*=\s*["']([^"']*btwaf=[^"']+)["']`)

	// 提取码正则表达式
	pwdRegex = regexp.MustCompile(`提取码[:：]\s*([a-zA-Z0-9]{4})`)

	// 缓存相关
	detailCache     = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL        = 1 * time.Hour
)

const (
	// 超时时间
	DefaultTimeout = 8 * time.Second
	DetailTimeout  = 6 * time.Second

	// 并发数（精简后的代码使用较低的并发即可）
	MaxConcurrency = 15

	// HTTP连接池配置
	MaxIdleConns        = 50
	MaxIdleConnsPerHost = 20
	MaxConnsPerHost     = 30
	IdleConnTimeout     = 90 * time.Second

	// 网站URL
	SiteURL = "https://duanjugou.top"
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewDjgouPlugin())

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

// DjgouPlugin 短剧狗插件
type DjgouPlugin struct {
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

// NewDjgouPlugin 创建新的短剧狗插件
func NewDjgouPlugin() *DjgouPlugin {
	return &DjgouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("djgou", 2), // 优先级2：质量良好的数据源
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *DjgouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *DjgouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *DjgouPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("%s/search.php?q=%s&page=1", SiteURL, url.QueryEscape(keyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置完整的请求头（避免反爬虫）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", SiteURL)

	// 5. 发送请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("[%s] 搜索请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 6. 读取并解析搜索结果页面。部分节点先返回 BTWAF JS 跳转页。
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索页面失败: %w", p.Name(), err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}
	if doc.Find("article.post-item-row").Length() == 0 {
		if match := btwafURLRegex.FindStringSubmatch(string(body)); len(match) > 1 {
			challengeURL := match[1]
			if strings.HasPrefix(challengeURL, "/") {
				challengeURL = SiteURL + challengeURL
			}
			challengeReq, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, challengeURL, nil)
			if reqErr == nil {
				challengeReq.Header = req.Header.Clone()
				challengeResp, doErr := p.doRequestWithRetry(challengeReq, client)
				if doErr == nil {
					challengeBody, readErr := io.ReadAll(challengeResp.Body)
					challengeResp.Body.Close()
					if readErr == nil {
						doc, _ = goquery.NewDocumentFromReader(strings.NewReader(string(challengeBody)))
					}
				}
			}
		}
	}

	// 7. 解析每个搜索结果项
	var results []model.SearchResult
	doc.Find("article.post-item-row").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchItem(s, keyword)
		if result.UniqueID != "" {
			results = append(results, result)
		}
	})

	// 8. 异步获取详情页信息
	enhancedResults := p.enhanceWithDetails(client, results)

	// 9. 关键词过滤
	return plugin.FilterResultsByKeyword(enhancedResults, keyword), nil
}

// parseSearchItem 解析单个搜索结果项
func (p *DjgouPlugin) parseSearchItem(s *goquery.Selection, keyword string) model.SearchResult {
	result := model.SearchResult{}

	// 新版站点使用 Z-Blog 的 post-item-row 模板。
	linkElem := s.Find("h2.post-title a").First()
	if linkElem.Length() == 0 {
		return result
	}

	title := strings.TrimSpace(linkElem.Text())
	link, exists := linkElem.Attr("href")
	if !exists || link == "" {
		return result
	}

	// 处理相对路径
	if !strings.HasPrefix(link, "http") {
		if strings.HasPrefix(link, "/") {
			link = SiteURL + link
		} else {
			link = SiteURL + "/" + link
		}
	}

	timeText := strings.TrimSpace(s.Find(".post-date").First().Text())

	// 生成唯一ID（使用链接的路径部分）
	itemID := strings.TrimPrefix(link, SiteURL)
	itemID = strings.Trim(itemID, "/")
	result.UniqueID = fmt.Sprintf("%s-%s", p.Name(), url.QueryEscape(itemID))
	result.MessageID = result.UniqueID

	result.Title = title
	result.Datetime = p.parseTime(timeText)
	result.Tags = []string{"短剧"}
	result.Channel = "" // 插件搜索结果必须为空字符串

	// 将详情页链接存储在Content中，后续获取详情
	result.Content = link

	return result
}

// parseTime 解析时间字符串
func (p *DjgouPlugin) parseTime(timeStr string) time.Time {
	if timeStr == "" {
		return time.Now()
	}
	timeStr = strings.TrimSpace(timeStr)
	if parsed, err := time.Parse("2006-01-02", timeStr); err == nil {
		return parsed
	}
	if parsed, err := time.ParseInLocation("01-02", timeStr, time.Local); err == nil {
		return parsed.AddDate(time.Now().Year()-parsed.Year(), 0, 0)
	}

	// 尝试多种时间格式
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02 15:04",
		"2006/01/02",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t
		}
	}

	return time.Now()
}

// enhanceWithDetails 异步获取详情页信息
func (p *DjgouPlugin) enhanceWithDetails(client *http.Client, results []model.SearchResult) []model.SearchResult {
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
func (p *DjgouPlugin) getDetailInfo(client *http.Client, detailURL string) ([]model.Link, string) {
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
func (p *DjgouPlugin) fetchDetailPage(client *http.Client, detailURL string) ([]model.Link, string) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DetailTimeout)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, ""
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
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

	// 提取网盘链接（从整个页面HTML中提取，不仅仅是mainContent）
	links := p.extractLinksFromDoc(doc)
	if len(links) == 0 {
		return nil, ""
	}

	// 新模板使用 post-content；旧模板仍兼容 erx-wrap。
	mainContent := doc.Find("div.post-content").First()
	if mainContent.Length() == 0 {
		mainContent = doc.Find("div.erx-wrap").First()
	}
	content := p.extractContent(mainContent)

	return links, content
}

// extractLinksFromDoc 从整个文档中提取夸克网盘链接（重要：从整个页面HTML中提取，不限于某个div）
func (p *DjgouPlugin) extractLinksFromDoc(doc *goquery.Document) []model.Link {
	var links []model.Link
	linkMap := make(map[string]bool) // 去重

	// 获取整个页面的HTML内容（这是关键！）
	htmlContent, _ := doc.Html()

	// 提取提取码
	password := ""
	if match := pwdRegex.FindStringSubmatch(htmlContent); len(match) > 1 {
		password = match[1]
	}

	// 方法1：使用专用正则表达式提取夸克网盘链接
	quarkLinks := quarkLinkRegex.FindAllString(htmlContent, -1)
	for _, quarkURL := range quarkLinks {
		// 去重
		if !linkMap[quarkURL] {
			linkMap[quarkURL] = true
			links = append(links, model.Link{
				Type:     "quark",
				URL:      quarkURL,
				Password: password,
			})
		}
	}

	// 方法2：从所有<a>标签中查找夸克链接（作为补充）
	doc.Find("a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 检查是否是夸克网盘链接
		if strings.Contains(href, "pan.quark.cn") {
			// 去重
			if !linkMap[href] {
				linkMap[href] = true
				links = append(links, model.Link{
					Type:     "quark",
					URL:      href,
					Password: password,
				})
			}
		}
	})

	return links
}

// extractContent 提取简介
func (p *DjgouPlugin) extractContent(mainContent *goquery.Selection) string {
	content := strings.TrimSpace(mainContent.Text())

	// 清理空白字符
	content = regexp.MustCompile(`\s+`).ReplaceAllString(content, " ")

	// 限制长度
	if len(content) > 300 {
		content = content[:300] + "..."
	}

	return content
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *DjgouPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
