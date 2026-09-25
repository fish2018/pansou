package aikanzy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// 预编译的正则表达式
var (
	// 夸克网盘链接
	quarkLinkRegex = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)

	// UC网盘链接
	ucLinkRegex = regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-zA-Z]+(\?[^"'\s]*)?`)

	// 百度网盘链接
	baiduLinkRegex = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_-]+`)

	// 迅雷网盘链接
	xunleiLinkRegex = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_-]+`)

	// 从URL中提取文章ID
	articleIDRegex = regexp.MustCompile(`/([a-z]+)/(\d+)\.html`)

	// 提取阅读数
	viewCountRegex = regexp.MustCompile(`(\d+)\s*阅读`)
)

// 常量定义
const (
	// 插件名称
	pluginName = "aikanzy"

	// 搜索URL模板
	searchURLTemplate = "https://www.aikanzy.com/search?word=%s&molds=article"

	// 默认优先级
	defaultPriority = 3

	// 默认超时时间（秒）
	defaultTimeout = 15

	// 详情页超时时间（秒）
	detailTimeout = 8

	// 最大重试次数
	maxRetries = 3

	// 详情页并发数
	detailConcurrency = 15

	// 指数退避基数（毫秒）
	backoffBase = 200
)

// AikanzyAsyncPlugin 是AikanZY网站的异步搜索插件实现
type AikanzyAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

// 确保AikanzyAsyncPlugin实现了AsyncSearchPlugin接口
var _ plugin.AsyncSearchPlugin = (*AikanzyAsyncPlugin)(nil)

// 在包初始化时注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewAikanzyAsyncPlugin())
}

// createOptimizedHTTPClient 创建优化的HTTP客户端
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:               util.ProxyFuncForTransport(),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   defaultTimeout * time.Second,
	}
}

// NewAikanzyAsyncPlugin 创建一个新的AikanZY异步插件实例
func NewAikanzyAsyncPlugin() *AikanzyAsyncPlugin {
	return &AikanzyAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("aikanzy", defaultPriority),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Name 返回插件名称
func (p *AikanzyAsyncPlugin) Name() string {
	return pluginName
}

// Priority 返回插件优先级
func (p *AikanzyAsyncPlugin) Priority() int {
	return defaultPriority
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *AikanzyAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *AikanzyAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 执行具体的搜索逻辑
func (p *AikanzyAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	// 对关键词进行URL编码
	encodedKeyword := url.QueryEscape(keyword)

	// 构建搜索URL
	searchURL := fmt.Sprintf(searchURLTemplate, encodedKeyword)

	// 创建一个带有超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置完整的请求头（避免反爬虫）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://www.aikanzy.com/")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")

	// 使用带重试的请求方法发送HTTP请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 请求搜索页面失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 请求搜索页面失败，状态码: %d", p.Name(), resp.StatusCode)
	}

	// 使用goquery解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析HTML失败: %w", p.Name(), err)
	}

	// 解析搜索结果列表
	articleItems := p.parseArticleList(doc)
	if len(articleItems) == 0 {
		return []model.SearchResult{}, nil
	}

	// 并发抓取详情页获取网盘链接
	results := p.fetchDetailsWithLinks(articleItems, client, keyword)

	// 使用过滤功能过滤结果
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	return filteredResults, nil
}

// ArticleItem 文章基本信息
type ArticleItem struct {
	ID          string
	Title       string
	DetailURL   string
	Category    string
	PublishDate string
	ViewCount   int
	Summary     string
	ImageURL    string
}

// parseArticleList 解析文章列表
func (p *AikanzyAsyncPlugin) parseArticleList(doc *goquery.Document) []ArticleItem {
	var items []ArticleItem

	// 查找所有文章项
	doc.Find("article.post-list.contt.blockimg").Each(func(i int, s *goquery.Selection) {
		// 提取详情页链接
		detailLink := s.Find("a[href]").First()
		detailURL, exists := detailLink.Attr("href")
		if !exists || detailURL == "" {
			return
		}

		// 提取文章ID
		articleID := p.extractArticleID(detailURL)
		if articleID == "" {
			return
		}

		// 提取标题
		title := strings.TrimSpace(s.Find("header.entry-header span.entry-title a").Text())
		// 移除标题中的HTML标签（如<b>）
		title = p.cleanHTMLTags(title)
		if title == "" {
			return
		}

		// 提取分类
		category := strings.TrimSpace(s.Find("div.entry-meta > a").First().Text())

		// 提取发布日期
		publishDate := strings.TrimSpace(s.Find("time").First().Text())

		// 提取阅读数
		metaText := s.Find("div.entry-meta").Text()
		viewCount := p.extractViewCount(metaText)

		// 提取摘要
		summary := strings.TrimSpace(s.Find("div.entry-summary.ss p").Text())
		summary = p.cleanHTMLTags(summary)

		// 提取缩略图
		imageURL, _ := s.Find("img.block-fea").Attr("data-src")

		items = append(items, ArticleItem{
			ID:          articleID,
			Title:       title,
			DetailURL:   detailURL,
			Category:    category,
			PublishDate: publishDate,
			ViewCount:   viewCount,
			Summary:     summary,
			ImageURL:    imageURL,
		})
	})

	return items
}

// fetchDetailsWithLinks 并发抓取详情页获取网盘链接
func (p *AikanzyAsyncPlugin) fetchDetailsWithLinks(items []ArticleItem, client *http.Client, keyword string) []model.SearchResult {
	// 创建结果通道和等待组
	resultChan := make(chan model.SearchResult, len(items))
	var wg sync.WaitGroup

	// 创建信号量控制并发数
	semaphore := make(chan struct{}, detailConcurrency)

	// 并发处理每个文章项
	for _, item := range items {
		wg.Add(1)

		go func(item ArticleItem) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 抓取详情页
			links := p.fetchDetailPageLinks(item.DetailURL, client)

			// 只有包含链接的结果才添加
			if len(links) > 0 {
				// 解析发布时间
				publishTime := p.parsePublishTime(item.PublishDate)

				// 组装内容
				var contentParts []string
				if item.Summary != "" {
					contentParts = append(contentParts, item.Summary)
				}
				if item.Category != "" {
					contentParts = append(contentParts, item.Category)
				}
				if item.PublishDate != "" {
					contentParts = append(contentParts, item.PublishDate)
				}
				if item.ViewCount > 0 {
					contentParts = append(contentParts, fmt.Sprintf("%d阅读", item.ViewCount))
				}
				content := strings.Join(contentParts, " | ")

				// 组装标签
				var tags []string
				if item.Category != "" {
					tags = append(tags, item.Category)
				}

				result := model.SearchResult{
					UniqueID: fmt.Sprintf("aikanzy-%s", item.ID),
					Title:    item.Title,
					Content:  content,
					Links:    links,
					Tags:     tags,
					Channel:  "", // 插件搜索结果Channel为空
					Datetime: publishTime,
				}

				resultChan <- result
			}
		}(item)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集所有结果
	var results []model.SearchResult
	for result := range resultChan {
		results = append(results, result)
	}

	return results
}

// fetchDetailPageLinks 抓取详情页的网盘链接
func (p *AikanzyAsyncPlugin) fetchDetailPageLinks(detailURL string, client *http.Client) []model.Link {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://www.aikanzy.com/")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	// 提取网盘链接
	return p.extractNetDiskLinks(doc)
}

// extractNetDiskLinks 从详情页提取网盘链接
func (p *AikanzyAsyncPlugin) extractNetDiskLinks(doc *goquery.Document) []model.Link {
	var links []model.Link
	foundURLs := make(map[string]bool) // 用于去重

	// 方法1: 从<a>标签的href属性提取
	doc.Find("a[href*='pan.quark.cn'], a[href*='drive.uc.cn'], a[href*='pan.baidu.com'], a[href*='pan.xunlei.com']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 去重
		if foundURLs[href] {
			return
		}
		foundURLs[href] = true

		// 确定链接类型
		linkType := p.determineLinkType(href)
		if linkType == "" {
			return
		}

		links = append(links, model.Link{
			Type:     linkType,
			URL:      href,
			Password: p.extractPassword(href),
		})
	})

	// 方法2: 从页面HTML文本中提取（正则表达式）
	if len(links) == 0 {
		html, _ := doc.Html()

		// 提取夸克网盘链接
		quarkLinks := quarkLinkRegex.FindAllString(html, -1)
		for _, link := range quarkLinks {
			if !foundURLs[link] {
				foundURLs[link] = true
				links = append(links, model.Link{
					Type:     "quark",
					URL:      link,
					Password: p.extractPassword(link),
				})
			}
		}

		// 提取UC网盘链接
		ucLinks := ucLinkRegex.FindAllString(html, -1)
		for _, link := range ucLinks {
			if !foundURLs[link] {
				foundURLs[link] = true
				links = append(links, model.Link{
					Type:     "uc",
					URL:      link,
					Password: p.extractPassword(link),
				})
			}
		}

		// 提取百度网盘链接
		baiduLinks := baiduLinkRegex.FindAllString(html, -1)
		for _, link := range baiduLinks {
			if !foundURLs[link] {
				foundURLs[link] = true
				links = append(links, model.Link{
					Type:     "baidu",
					URL:      link,
					Password: p.extractPassword(link),
				})
			}
		}

		// 提取迅雷网盘链接
		xunleiLinks := xunleiLinkRegex.FindAllString(html, -1)
		for _, link := range xunleiLinks {
			if !foundURLs[link] {
				foundURLs[link] = true
				links = append(links, model.Link{
					Type:     "xunlei",
					URL:      link,
					Password: p.extractPassword(link),
				})
			}
		}
	}

	return links
}

// determineLinkType 根据URL确定链接类型
func (p *AikanzyAsyncPlugin) determineLinkType(urlStr string) string {
	lowerURL := strings.ToLower(urlStr)

	switch {
	case strings.Contains(lowerURL, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lowerURL, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		return "xunlei"
	default:
		return ""
	}
}

// extractArticleID 从URL中提取文章ID
func (p *AikanzyAsyncPlugin) extractArticleID(urlStr string) string {
	matches := articleIDRegex.FindStringSubmatch(urlStr)
	if len(matches) >= 3 {
		return matches[2] // 返回数字ID
	}
	return ""
}

// extractViewCount 提取阅读数
func (p *AikanzyAsyncPlugin) extractViewCount(text string) int {
	matches := viewCountRegex.FindStringSubmatch(text)
	if len(matches) >= 2 {
		var count int
		fmt.Sscanf(matches[1], "%d", &count)
		return count
	}
	return 0
}

// cleanHTMLTags 清除HTML标签
func (p *AikanzyAsyncPlugin) cleanHTMLTags(text string) string {
	// 移除<b>标签
	text = aikanzyRe1.ReplaceAllString(text, "")
	text = aikanzyRe2.ReplaceAllString(text, "")

	// 移除其他常见HTML标签
	text = aikanzyRe3.ReplaceAllString(text, "")

	return strings.TrimSpace(text)
}

// parsePublishTime 解析发布时间
func (p *AikanzyAsyncPlugin) parsePublishTime(dateStr string) time.Time {
	dateStr = strings.TrimSpace(dateStr)
	if dateStr == "" {
		return time.Time{}
	}

	// 尝试多种日期格式
	formats := []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05+08:00",
		"2006-01-02T15:04:05-07:00",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}

	// 如果以上格式都不匹配，尝试使用time.RFC3339格式（处理<time>标签的datetime属性）
	if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
		return t
	}

	return time.Time{}
}

// extractPassword 从网盘链接中提取密码
func (p *AikanzyAsyncPlugin) extractPassword(urlStr string) string {
	// 从URL中提取pwd=后面的四位密码(不包含#)
	pwdRegex := aikanzyRe4
	matches := pwdRegex.FindStringSubmatch(urlStr)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// doRequestWithRetry 发送HTTP请求，带重试机制
func (p *AikanzyAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var resp *http.Response
	var err error

	for retry := 0; retry <= maxRetries; retry++ {
		if retry > 0 {
			// 指数退避
			backoffTime := time.Duration(1<<uint(retry-1)) * backoffBase * time.Millisecond
			time.Sleep(backoffTime)

			// 克隆请求
			req = req.Clone(req.Context())
		}

		resp, err = client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
		}
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, err)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	aikanzyRe1 = regexp.MustCompile(`<b[^>]*>`)
	aikanzyRe2 = regexp.MustCompile(`</b>`)
	aikanzyRe3 = regexp.MustCompile(`<[^>]+>`)
	aikanzyRe4 = regexp.MustCompile(`pwd=([^#&]{4})`)
)
