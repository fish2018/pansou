package ahhhhfs

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
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// 预编译的正则表达式
var (
	// 从详情页URL中提取文章ID的正则表达式
	articleIDRegex = regexp.MustCompile(`/(\d+)/?$`)

	// 常见网盘链接的正则表达式
	quarkLinkRegex  = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	baiduLinkRegex  = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+`)
	aliyunLinkRegex = regexp.MustCompile(`https?://(www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9a-zA-Z]+`)
	ucLinkRegex     = regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-zA-Z]+`)
	xunleiLinkRegex = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+`)
	tianyiLinkRegex = regexp.MustCompile(`https?://cloud\.189\.cn/(t|web)/[0-9a-zA-Z]+`)
	link115Regex    = regexp.MustCompile(`https?://115\.com/s/[0-9a-zA-Z]+`)
	link123Regex    = regexp.MustCompile(`https?://123pan\.com/s/[0-9a-zA-Z]+`)
	pikpakLinkRegex = regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z]+`)

	// 提取码匹配模式
	pwdPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[：:]\s*([0-9a-zA-Z]+)`),
		regexp.MustCompile(`密码[：:]\s*([0-9a-zA-Z]+)`),
		regexp.MustCompile(`pwd[=:：]\s*([0-9a-zA-Z]+)`),
		regexp.MustCompile(`code[=:：]\s*([0-9a-zA-Z]+)`),
	}

	// 缓存相关
	detailCache     = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL        = 1 * time.Hour
)

const (
	// 插件名称
	pluginName = "ahhhhfs"

	// 优先级
	defaultPriority = 2

	// 超时时间
	DefaultTimeout = 10 * time.Second
	DetailTimeout  = 8 * time.Second

	// 并发数限制
	MaxConcurrency = 15

	// HTTP连接池配置
	MaxIdleConns        = 100
	MaxIdleConnsPerHost = 30
	MaxConnsPerHost     = 50
	IdleConnTimeout     = 90 * time.Second
)

// 性能统计
var (
	searchRequests     int64 = 0
	detailPageRequests int64 = 0
	cacheHits          int64 = 0
	cacheMisses        int64 = 0
)

// AhhhhfsAsyncPlugin ahhhhfs异步插件
type AhhhhfsAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewAhhhhfsPlugin())

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

// createOptimizedHTTPClient 创建优化的HTTP客户端
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		MaxConnsPerHost:     MaxConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		DisableKeepAlives:   false,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
	}
}

// NewAhhhhfsPlugin 创建新的ahhhhfs异步插件
func NewAhhhhfsPlugin() *AhhhhfsAsyncPlugin {
	return &AhhhhfsAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *AhhhhfsAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *AhhhhfsAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *AhhhhfsAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 性能统计
	start := time.Now()
	atomic.AddInt64(&searchRequests, 1)
	defer func() {
		fmt.Printf("[%s] 搜索耗时: %v\n", p.Name(), time.Since(start))
	}()

	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("https://www.ahhhhfs.com/?cat=&s=%s", url.QueryEscape(keyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置完整的请求头（避免反爬虫）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", "https://www.ahhhhfs.com/")

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
	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, MaxConcurrency)

	doc.Find("article.post-item.item-list").Each(func(i int, s *goquery.Selection) {
		// 解析基本信息
		titleElem := s.Find(".entry-title a")
		title := strings.TrimSpace(titleElem.Text())
		if title == "" {
			title = strings.TrimSpace(titleElem.AttrOr("title", ""))
		}

		detailURL, exists := titleElem.Attr("href")
		if !exists || detailURL == "" || title == "" {
			return
		}

		// 提取文章ID
		articleID := p.extractArticleID(detailURL)
		if articleID == "" {
			return
		}

		// 提取分类标签
		var tags []string
		s.Find(".entry-cat-dot a").Each(func(j int, tag *goquery.Selection) {
			tagText := strings.TrimSpace(tag.Text())
			if tagText != "" {
				tags = append(tags, tagText)
			}
		})

		// 提取描述
		content := strings.TrimSpace(s.Find(".entry-desc").Text())

		// 提取时间
		datetime := ""
		timeElem := s.Find(".entry-meta .meta-date time")
		if dt, exists := timeElem.Attr("datetime"); exists {
			datetime = dt
		} else {
			datetime = strings.TrimSpace(timeElem.Text())
		}

		// 解析时间
		publishTime := p.parseDateTime(datetime)

		// 异步获取详情页的网盘链接
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(title, detailURL, articleID, content string, tags []string, publishTime time.Time) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			// 获取网盘链接
			links := p.fetchDetailLinks(client, detailURL, articleID)

			if len(links) > 0 {
				result := model.SearchResult{
					UniqueID: fmt.Sprintf("%s-%s", p.Name(), articleID),
					Title:    title,
					Content:  content,
					Links:    links,
					Tags:     tags,
					Channel:  "", // 插件搜索结果 Channel 必须为空
					Datetime: publishTime,
				}

				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}
		}(title, detailURL, articleID, content, tags, publishTime)
	})

	// 等待所有详情页请求完成
	wg.Wait()

	fmt.Printf("[%s] 搜索结果: %d 条\n", p.Name(), len(results))

	// 关键词过滤
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// extractArticleID 从URL中提取文章ID
func (p *AhhhhfsAsyncPlugin) extractArticleID(detailURL string) string {
	matches := articleIDRegex.FindStringSubmatch(detailURL)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// parseDateTime 解析时间字符串
func (p *AhhhhfsAsyncPlugin) parseDateTime(datetime string) time.Time {
	datetime = strings.TrimSpace(datetime)

	// 尝试解析 ISO 格式
	if t, err := time.Parse(time.RFC3339, datetime); err == nil {
		return t
	}

	// 尝试解析标准日期格式
	layouts := []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z07:00",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, datetime); err == nil {
			return t
		}
	}

	// 处理相对时间（如"1 周前"、"2 天前"）
	now := time.Now()

	if strings.Contains(datetime, "小时前") || strings.Contains(datetime, "hours ago") {
		// 简单处理，返回当天
		return now
	}

	if strings.Contains(datetime, "天前") || strings.Contains(datetime, "days ago") {
		// 简单处理，返回近期
		return now.AddDate(0, 0, -7)
	}

	if strings.Contains(datetime, "周前") || strings.Contains(datetime, "weeks ago") {
		// 简单处理，返回一个月前
		return now.AddDate(0, -1, 0)
	}

	// 默认返回当前时间
	return now
}

// fetchDetailLinks 获取详情页的网盘链接
func (p *AhhhhfsAsyncPlugin) fetchDetailLinks(client *http.Client, detailURL, articleID string) []model.Link {
	atomic.AddInt64(&detailPageRequests, 1)

	// 检查缓存
	if cached, ok := detailCache.Load(articleID); ok {
		atomic.AddInt64(&cacheHits, 1)
		return cached.([]model.Link)
	}

	atomic.AddInt64(&cacheMisses, 1)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DetailTimeout)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		fmt.Printf("[%s] 创建详情页请求失败: %v\n", p.Name(), err)
		return nil
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.ahhhhfs.com/")

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[%s] 详情页请求失败: %v\n", p.Name(), err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("[%s] 详情页返回状态码: %d\n", p.Name(), resp.StatusCode)
		return nil
	}

	// 解析详情页
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Printf("[%s] 解析详情页失败: %v\n", p.Name(), err)
		return nil
	}

	// 提取网盘链接
	links := p.extractNetDiskLinks(doc)

	// 缓存结果
	if len(links) > 0 {
		detailCache.Store(articleID, links)
	}

	return links
}

// extractNetDiskLinks 从详情页提取网盘链接
func (p *AhhhhfsAsyncPlugin) extractNetDiskLinks(doc *goquery.Document) []model.Link {
	var links []model.Link
	linkMap := make(map[string]model.Link) // 用于去重

	// 在文章内容中查找所有链接
	doc.Find(".post-content a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 判断是否为网盘链接
		cloudType := p.determineCloudType(href)
		if cloudType == "others" {
			return
		}

		// 提取提取码
		password := p.extractPassword(s, href)

		// 添加到结果（去重）
		if _, exists := linkMap[href]; !exists {
			link := model.Link{
				Type:     cloudType,
				URL:      href,
				Password: password,
			}
			linkMap[href] = link
			links = append(links, link)
		}
	})

	return links
}

// determineCloudType 判断链接类型
func (p *AhhhhfsAsyncPlugin) determineCloudType(url string) string {
	switch {
	case strings.Contains(url, "pan.quark.cn"):
		return "quark"
	case strings.Contains(url, "drive.uc.cn"):
		return "uc"
	case strings.Contains(url, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(url, "aliyundrive.com") || strings.Contains(url, "alipan.com"):
		return "aliyun"
	case strings.Contains(url, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(url, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(url, "115.com"):
		return "115"
	case strings.Contains(url, "123pan.com"):
		return "123"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	default:
		return "others"
	}
}

// extractPassword 提取提取码
func (p *AhhhhfsAsyncPlugin) extractPassword(linkElem *goquery.Selection, url string) string {
	// 1. 从链接的 title 属性中提取
	if title, exists := linkElem.Attr("title"); exists {
		for _, pattern := range pwdPatterns {
			if matches := pattern.FindStringSubmatch(title); len(matches) >= 2 {
				return matches[1]
			}
		}
	}

	// 2. 从链接文本中提取
	linkText := linkElem.Text()
	for _, pattern := range pwdPatterns {
		if matches := pattern.FindStringSubmatch(linkText); len(matches) >= 2 {
			return matches[1]
		}
	}

	// 3. 从链接后面的兄弟节点或父节点的文本中提取
	parent := linkElem.Parent()
	parentText := parent.Text()

	// 获取链接在父元素文本中的位置
	linkIndex := strings.Index(parentText, linkText)
	if linkIndex >= 0 {
		// 获取链接后面的文本
		afterText := parentText[linkIndex+len(linkText):]
		for _, pattern := range pwdPatterns {
			if matches := pattern.FindStringSubmatch(afterText); len(matches) >= 2 {
				return matches[1]
			}
		}
	}

	// 4. 从 URL 参数中提取
	if strings.Contains(url, "pwd=") {
		parts := strings.Split(url, "pwd=")
		if len(parts) >= 2 {
			pwd := parts[1]
			// 只取密码部分（去除其他参数）
			if idx := strings.IndexAny(pwd, "&?#"); idx >= 0 {
				pwd = pwd[:idx]
			}
			return pwd
		}
	}

	return ""
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *AhhhhfsAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
