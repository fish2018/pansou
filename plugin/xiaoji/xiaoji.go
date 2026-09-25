package xiaoji

import (
	"context"
	"encoding/base64"
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
	// 从详情页URL中提取ID的正则表达式
	detailIDRegex = regexp.MustCompile(`/(\d+)\.html`)

	// go.html链接的正则表达式，用于提取base64编码部分
	goLinkRegex = regexp.MustCompile(`/go\.html\?url=([A-Za-z0-9+/]+=*)`)

	// 年份提取正则表达式
	yearRegex = regexp.MustCompile(`(\d{4})`)

	// 缓存相关
	detailCache     = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL        = 1 * time.Hour
)

const (
	// 基础配置
	pluginName = "xiaoji"
	baseURL    = "https://www.xiaojitv.com"

	// 超时时间配置
	DefaultTimeout = 10 * time.Second
	DetailTimeout  = 8 * time.Second

	// 并发数配置
	MaxConcurrency = 15

	// HTTP连接池配置
	MaxIdleConns        = 100
	MaxIdleConnsPerHost = 30
	MaxConnsPerHost     = 50
	IdleConnTimeout     = 90 * time.Second
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewXiaojiPlugin())

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

// XiaojiAsyncPlugin 小鸡影视异步插件
type XiaojiAsyncPlugin struct {
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
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: transport, Timeout: DefaultTimeout}
}

// NewXiaojiPlugin 创建新的小鸡影视异步插件
func NewXiaojiPlugin() *XiaojiAsyncPlugin {
	return &XiaojiAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 兼容性方法，实际调用SearchWithResult
func (p *XiaojiAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XiaojiAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 具体的搜索实现
func (p *XiaojiAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 构建搜索URL
	encodedKeyword := url.QueryEscape(keyword)
	searchURL := fmt.Sprintf("%s/?s=%s", baseURL, encodedKeyword)

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", pluginName, err)
	}

	// 4. 设置请求头
	p.setRequestHeaders(req)

	// 5. 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", pluginName, err)
	}
	defer resp.Body.Close()

	// 6. 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", pluginName, resp.StatusCode)
	}

	// 7. 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", pluginName, err)
	}

	// 8. 解析搜索结果
	results := p.parseSearchResults(doc, keyword)

	// 9. 关键词过滤
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// setRequestHeaders 设置HTTP请求头，模拟真实浏览器
func (p *XiaojiAsyncPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *XiaojiAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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

// parseSearchResults 解析搜索结果
func (p *XiaojiAsyncPlugin) parseSearchResults(doc *goquery.Document, keyword string) []model.SearchResult {
	results := make([]model.SearchResult, 0)

	// 查找所有搜索结果项
	doc.Find("article.poster-item").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchResultItem(s, keyword)
		if result != nil {
			results = append(results, *result)
		}
	})

	return results
}

// parseSearchResultItem 解析单个搜索结果项
func (p *XiaojiAsyncPlugin) parseSearchResultItem(s *goquery.Selection, keyword string) *model.SearchResult {
	// 1. 提取详情页链接
	detailLink, exists := s.Find(".poster-link").Attr("href")
	if !exists || detailLink == "" {
		return nil
	}

	// 2. 确保链接是绝对路径
	if strings.HasPrefix(detailLink, "/") {
		detailLink = baseURL + detailLink
	}

	// 3. 提取资源ID
	matches := detailIDRegex.FindStringSubmatch(detailLink)
	if len(matches) < 2 {
		return nil
	}
	resourceID := matches[1]

	// 4. 提取标题
	title := strings.TrimSpace(s.Find(".poster-title a").Text())
	if title == "" {
		return nil
	}

	// 5. 提取评分
	rating := strings.TrimSpace(s.Find(".rating-score").Text())

	// 6. 提取分类
	category := strings.TrimSpace(s.Find(".poster-category a").Text())

	// 7. 提取标签
	var tags []string
	s.Find(".poster-tags a").Each(func(i int, tagSel *goquery.Selection) {
		tag := strings.TrimSpace(tagSel.Text())
		if tag != "" {
			tags = append(tags, tag)
		}
	})

	// 8. 提取封面图片
	coverImg, _ := s.Find(".poster-image img").Attr("src")

	// 9. 构建基础信息
	content := fmt.Sprintf("分类: %s", category)
	if rating != "" {
		content += fmt.Sprintf(" | 评分: %s", rating)
	}
	if len(tags) > 0 {
		content += fmt.Sprintf(" | 标签: %s", strings.Join(tags, ", "))
	}

	// 10. 获取详情页的下载链接
	links := p.fetchDetailPageLinks(detailLink)

	// 11. 创建搜索结果
	result := &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", pluginName, resourceID),
		Title:    title,
		Content:  content,
		Datetime: time.Now(),
		Tags:     tags,
		Links:    links,
		Channel:  "", // 插件搜索结果必须为空字符串
	}

	// 12. 如果有封面图片，可以添加到额外信息中
	if coverImg != "" {
		// 这里可以扩展添加图片信息，当前版本暂不处理
	}

	return result
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *XiaojiAsyncPlugin) fetchDetailPageLinks(detailURL string) []model.Link {
	// 1. 检查缓存
	if cached, ok := detailCache.Load(detailURL); ok {
		if links, ok := cached.([]model.Link); ok {
			return links
		}
	}

	// 2. 创建请求
	ctx, cancel := context.WithTimeout(context.Background(), DetailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil
	}

	// 3. 设置请求头
	p.setRequestHeaders(req)

	// 4. 发送请求
	resp, err := p.doRequestWithRetry(req, p.optimizedClient)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	// 5. 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	// 6. 提取下载链接
	links := p.parseDetailPageLinks(doc)

	// 7. 缓存结果
	if len(links) > 0 {
		detailCache.Store(detailURL, links)
	}

	return links
}

// parseDetailPageLinks 解析详情页的下载链接
func (p *XiaojiAsyncPlugin) parseDetailPageLinks(doc *goquery.Document) []model.Link {
	links := make([]model.Link, 0)
	seenLinks := make(map[string]bool) // 用于去重

	// 查找相关资源区域的链接
	doc.Find(".resource-compact-link a").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists {
			return
		}

		var realURL string

		// 检查是否为go.html格式的链接（需要base64解码）
		if strings.Contains(href, "/go.html?url=") {
			// 提取并解码真实链接
			realURL = p.decodeGoLink(href)
		} else if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "magnet:") || strings.HasPrefix(href, "ed2k://") {
			// 直接链接（包括磁力链接、网盘链接等）
			realURL = href
		}

		// 处理有效链接
		if p.isValidURL(realURL) && !seenLinks[realURL] {
			// 确定网盘类型
			linkType := p.determineCloudType(realURL)

			// 创建链接对象
			link := model.Link{
				Type:     linkType,
				URL:      realURL,
				Password: "", // xiaoji网站通常无密码
			}

			links = append(links, link)
			seenLinks[realURL] = true
		}
	})

	return links
}

// decodeGoLink 解码go.html链接，提取真实的网盘链接
func (p *XiaojiAsyncPlugin) decodeGoLink(goLink string) string {
	// 1. 提取base64编码部分
	matches := goLinkRegex.FindStringSubmatch(goLink)
	if len(matches) < 2 {
		return ""
	}

	encoded := matches[1]

	// 2. 清理编码字符串
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return ""
	}

	// 3. Base64解码
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// 尝试处理可能的URL编码问题
		encoded = strings.ReplaceAll(encoded, " ", "+")
		// 尝试修复padding问题
		switch len(encoded) % 4 {
		case 2:
			encoded += "=="
		case 3:
			encoded += "="
		}
		decoded, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return ""
		}
	}

	realURL := strings.TrimSpace(string(decoded))

	// 4. 验证解码结果是否为有效URL
	if p.isValidURL(realURL) {
		return realURL
	}

	return ""
}

// isValidURL 验证URL是否有效
func (p *XiaojiAsyncPlugin) isValidURL(urlStr string) bool {
	if urlStr == "" {
		return false
	}

	// 检查基本的URL格式
	if strings.HasPrefix(urlStr, "http://") || strings.HasPrefix(urlStr, "https://") {
		// HTTP/HTTPS链接需要有域名
		if len(urlStr) <= 8 || urlStr == "http://" || urlStr == "https://" {
			return false
		}
		// 简单检查是否包含域名
		return strings.Contains(urlStr[8:], ".")
	}

	// 磁力链接
	if strings.HasPrefix(urlStr, "magnet:") {
		return len(urlStr) > 7 && strings.Contains(urlStr, "xt=")
	}

	// ED2K链接
	if strings.HasPrefix(urlStr, "ed2k://") {
		return len(urlStr) > 7
	}

	return false
}

// determineCloudType 确定网盘类型
func (p *XiaojiAsyncPlugin) determineCloudType(url string) string {
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
	case strings.Contains(url, "115.com") || strings.Contains(url, "115cdn.com"):
		return "115"
	case strings.Contains(url, "123pan.com"):
		return "123"
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	case strings.Contains(url, "magnet:"):
		return "magnet"
	case strings.Contains(url, "ed2k://"):
		return "ed2k"
	default:
		// ctfile.com 和其他未知网盘都归类到 others
		return "others"
	}
}
