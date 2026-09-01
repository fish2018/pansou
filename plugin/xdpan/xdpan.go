package xdpan

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	BaseURL        = "https://aipanso.com"
	UserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
	MaxConcurrency = 10 // 详情页最大并发数
	MaxRetries     = 3
	maxPageSize    = 3 << 20
)

var (
	DebugLog = false // Debug开关，默认关闭
)

// XdpanPlugin 兄弟盘插件结构
type XdpanPlugin struct {
	*plugin.BaseAsyncPlugin
	detailCache sync.Map // 详情页缓存
	cacheTTL    time.Duration
	baseURL     string
}

// NewXdpanPlugin 创建兄弟盘插件实例
func NewXdpanPlugin() *XdpanPlugin {
	return &XdpanPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("xdpan", 3), // 优先级3 = 普通质量数据源
		cacheTTL:        60 * time.Minute,
		baseURL:         BaseURL,
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *XdpanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XdpanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现搜索逻辑
func (p *XdpanPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[xdpan] 开始搜索: keyword=%s\n", keyword)
	}

	// Step 1: 获取搜索结果页面
	searchResults, err := p.fetchSearchResults(client, keyword)
	if err != nil {
		if DebugLog {
			fmt.Printf("[xdpan] 获取搜索结果失败: %v\n", err)
		}
		return nil, fmt.Errorf("[%s] 获取搜索结果失败: %w", p.Name(), err)
	}
	if DebugLog {
		fmt.Printf("[xdpan] 获取搜索结果成功: 结果数=%d\n", len(searchResults))
	}

	// Step 2: 并发获取详情页信息（获取真实的百度网盘链接）
	p.enrichWithDetailInfo(client, searchResults)
	searchResults = filterResultsWithLinks(searchResults)

	// Step 3: 关键词过滤
	filteredResults := plugin.FilterResultsByKeyword(searchResults, keyword)
	if DebugLog {
		fmt.Printf("[xdpan] 关键词过滤后: 过滤前=%d, 过滤后=%d\n", len(searchResults), len(filteredResults))
	}

	return filteredResults, nil
}

// fetchSearchResults 获取搜索结果
func (p *XdpanPlugin) fetchSearchResults(client *http.Client, keyword string) ([]model.SearchResult, error) {
	// 构建搜索URL（只获取第一页）
	searchURL := fmt.Sprintf("%s/search?page=1&k=%s&p=baidu", strings.TrimRight(p.baseURL, "/"), url.QueryEscape(keyword))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %w", err)
	}

	p.setRequestHeaders(req)

	if DebugLog {
		fmt.Printf("[xdpan] 搜索URL: %s\n", searchURL)
	}

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("GET请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求返回状态码: %d", resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxPageSize))
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	results := p.extractSearchResults(doc)
	if len(results) == 0 && doc.Find("title").First().Text() == "Just a moment..." {
		return nil, fmt.Errorf("站点触发 Cloudflare 浏览器验证")
	}
	return results, nil
}

// extractSearchResults 从搜索页面提取结果
func (p *XdpanPlugin) extractSearchResults(doc *goquery.Document) []model.SearchResult {
	var results []model.SearchResult

	// 查找所有包含详情页链接的van-row元素
	doc.Find("van-row").Each(func(i int, s *goquery.Selection) {
		// 检查是否包含详情页链接
		detailLink := s.Find("a[href^='/s/']")
		if detailLink.Length() == 0 {
			return
		}

		result := p.parseSearchResult(s)
		if result.Title != "" {
			results = append(results, result)
			if DebugLog {
				fmt.Printf("[xdpan] 解析结果[%d]: title=%s, detailUrl=%s\n", i, result.Title, result.Content)
			}
		}
	})

	if DebugLog {
		fmt.Printf("[xdpan] 提取到有效结果数: %d\n", len(results))
	}

	return results
}

// parseSearchResult 解析单个搜索结果
func (p *XdpanPlugin) parseSearchResult(s *goquery.Selection) model.SearchResult {
	// 提取详情页链接
	detailLink := s.Find("a[href^='/s/']")
	detailPath, _ := detailLink.Attr("href")
	var detailURL string
	if detailPath != "" {
		detailURL = strings.TrimRight(p.baseURL, "/") + detailPath
	}

	// 提取资源ID
	resourceID := ""
	if detailPath != "" {
		parts := strings.Split(detailPath, "/")
		if len(parts) >= 3 {
			resourceID = parts[2]
		}
	}

	// 提取标题（从content-title div中的所有span标签）
	var titleParts []string
	s.Find("div[name='content-title'] span").Each(func(i int, span *goquery.Selection) {
		text := strings.TrimSpace(span.Text())
		if text != "" {
			titleParts = append(titleParts, text)
		}
	})
	title := strings.Join(titleParts, "")

	// 如果没有找到span标签，尝试直接获取content-title的文本
	if title == "" {
		title = strings.TrimSpace(s.Find("div[name='content-title']").Text())
	}

	// 提取时间和格式信息
	var shareTime, fileType string
	bottomText := s.Find("template").Text()
	if bottomText == "" {
		// 如果template不能直接获取文本，尝试其他方式
		bottomText = s.Find("div").FilterFunction(func(i int, sel *goquery.Selection) bool {
			return strings.Contains(sel.Text(), "时间:")
		}).Text()
	}

	// 使用正则表达式提取时间和格式
	timeRegex := regexp.MustCompile(`时间:\s*(\d{4}-\d{1,2}-\d{1,2})`)
	if matches := timeRegex.FindStringSubmatch(bottomText); len(matches) > 1 {
		shareTime = matches[1]
	}

	fileType = strings.TrimSpace(s.Find("template b").First().Text())
	if fileType == "" {
		formatRegex := regexp.MustCompile(`格式:\s*([^\s]+)`)
		if matches := formatRegex.FindStringSubmatch(bottomText); len(matches) > 1 {
			fileType = matches[1]
		}
	}

	// 解析时间
	parsedTime := p.parseTime(shareTime)

	// 构建内容描述
	content := fmt.Sprintf("类型: %s | 分享时间: %s | 详情: %s", fileType, shareTime, detailURL)

	// 如果没有找到资源ID，使用时间戳
	if resourceID == "" {
		resourceID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return model.SearchResult{
		MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
		UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
		Title:     title,
		Content:   content,
		Datetime:  parsedTime,
		Links:     []model.Link{}, // 初始为空，后续从详情页获取
		Channel:   "",             // ⭐ 重要：插件搜索结果Channel必须为空
	}
}

// enrichWithDetailInfo 并发获取详情页信息
func (p *XdpanPlugin) enrichWithDetailInfo(client *http.Client, results []model.SearchResult) {
	if len(results) == 0 {
		return
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, MaxConcurrency)

	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加延时避免请求过快
			time.Sleep(time.Duration(index%3) * 200 * time.Millisecond)

			// 从Content中提取详情页URL
			detailURL := p.extractDetailURLFromContent(results[index].Content)
			if detailURL != "" {
				links := p.fetchDetailPageLinks(client, detailURL)
				if len(links) > 0 {
					for linkIndex := range links {
						if links[linkIndex].WorkTitle == "" {
							links[linkIndex].WorkTitle = results[index].Title
						}
					}
					results[index].Links = links
					if DebugLog {
						fmt.Printf("[xdpan] 获取详情页链接成功: %s, 链接数: %d\n", detailURL, len(links))
					}
				}
			}
		}(i)
	}

	wg.Wait()
}

// fetchDetailPageLinks 获取详情页中的百度网盘链接
func (p *XdpanPlugin) fetchDetailPageLinks(client *http.Client, detailURL string) []model.Link {
	if detailURL == "" {
		return []model.Link{}
	}

	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if cacheItem, ok := cached.(cacheItem); ok {
			if time.Since(cacheItem.timestamp) < p.cacheTTL {
				return cacheItem.links
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return []model.Link{}
	}

	p.setRequestHeaders(req)

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return []model.Link{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return []model.Link{}
	}

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxPageSize))
	if err != nil {
		return []model.Link{}
	}

	links := p.extractDetailPageLinks(doc)

	// 缓存结果
	p.detailCache.Store(detailURL, cacheItem{
		links:     links,
		timestamp: time.Now(),
	})

	return links
}

// extractDetailPageLinks 从详情页提取百度网盘链接
func (p *XdpanPlugin) extractDetailPageLinks(doc *goquery.Document) []model.Link {
	var links []model.Link

	// 提取密码
	password := ""
	doc.Find("van-cell").Each(func(i int, s *goquery.Selection) {
		title, _ := s.Attr("title")
		if title == "密码" {
			password = strings.TrimSpace(s.Find("b").Text())
		}
	})

	// 从JavaScript代码中提取百度网盘链接
	doc.Find("script").Each(func(i int, s *goquery.Selection) {
		scriptContent := s.Text()

		// 兼容 window.open 和 location.href 两种详情页跳转写法。
		patterns := []*regexp.Regexp{
			regexp.MustCompile(`window\.open\(\s*["']([^"']*pan\.baidu\.com[^"']*)["']`),
			regexp.MustCompile(`(?:window\.)?location\.href\s*=\s*["']([^"']*pan\.baidu\.com[^"']*)["']`),
		}
		for _, pattern := range patterns {
			matches := pattern.FindStringSubmatch(scriptContent)
			if len(matches) <= 1 {
				continue
			}
			baiduURL := matches[1]

			// 如果链接中没有密码参数，但我们从页面中提取到了密码，则添加密码参数
			if !strings.Contains(baiduURL, "pwd=") && password != "" {
				separator := "?"
				if strings.Contains(baiduURL, "?") {
					separator = "&"
				}
				baiduURL = fmt.Sprintf("%s%spwd=%s", baiduURL, separator, password)
			}

			links = append(links, model.Link{
				URL:      baiduURL,
				Type:     "baidu",
				Password: password,
			})

			if DebugLog {
				fmt.Printf("[xdpan] 提取到百度网盘链接: %s, 密码: %s\n", baiduURL, password)
			}
			break
		}
	})

	return links
}

// extractDetailURLFromContent 从Content中提取详情页URL
func (p *XdpanPlugin) extractDetailURLFromContent(content string) string {
	// 查找详情URL模式
	re := regexp.MustCompile(`详情:\s*(https?://[^\s]+)`)
	matches := re.FindStringSubmatch(content)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// parseTime 解析时间字符串
func (p *XdpanPlugin) parseTime(timeStr string) time.Time {
	timeStr = strings.TrimSpace(timeStr)
	if timeStr == "" {
		return time.Now()
	}

	formats := []string{
		"2006-1-2",
		"2006-01-02",
		"2006-1-2 15:04",
		"2006-01-02 15:04",
		"2006-1-2 15:04:05",
		"2006-01-02 15:04:05",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t
		}
	}

	// 如果解析失败，返回当前时间
	return time.Now()
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *XdpanPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
			if DebugLog {
				fmt.Printf("[xdpan] 重试第%d次，等待%v\n", i, backoff)
			}
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") {
			resp.Body.Close()
			return nil, fmt.Errorf("站点 %s 已迁移并启用 Cloudflare 浏览器验证，当前服务端请求无法通过", p.baseURL)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		resp.Body.Close()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未知请求错误")
	}
	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", MaxRetries, lastErr)
}

// setRequestHeaders 设置请求头
func (p *XdpanPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", strings.TrimRight(p.baseURL, "/")+"/?p=baidu")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "max-age=0")
}

// cacheItem 缓存项结构
type cacheItem struct {
	links     []model.Link
	timestamp time.Time
}

func filterResultsWithLinks(results []model.SearchResult) []model.SearchResult {
	filtered := make([]model.SearchResult, 0, len(results))
	for _, result := range results {
		if len(result.Links) > 0 {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func init() {
	p := NewXdpanPlugin()
	plugin.RegisterGlobalPlugin(p)
}
