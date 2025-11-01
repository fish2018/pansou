package huban

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"context"
	"sync"
	"sync/atomic"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	// 默认超时时间
	DefaultTimeout = 8 * time.Second
	DetailTimeout  = 6 * time.Second

	// HTTP连接池配置
	MaxIdleConns        = 200
	MaxIdleConnsPerHost = 50
	MaxConnsPerHost     = 100
	IdleConnTimeout     = 90 * time.Second

	// 并发控制
	MaxConcurrency = 20

	// 缓存TTL
	cacheTTL = 1 * time.Hour

	// 请求来源控制 - 默认开启，提高安全性
	EnableRefererCheck = false

	// 调试日志开关
	DebugLog = false
)

// 性能统计（原子操作）
var (
	searchRequests     int64 = 0
	totalSearchTime    int64 = 0 // 纳秒
	detailPageRequests int64 = 0
	totalDetailTime    int64 = 0 // 纳秒
	cacheHits          int64 = 0
	cacheMisses        int64 = 0
)

// Detail page缓存
var (
	detailCache sync.Map
	cacheMutex  sync.RWMutex
)

// 请求来源控制配置
var (
	// 允许的请求来源列表 - 参考panyq插件实现
	// 支持前缀匹配，例如 "https://example.com" 会匹配 "https://example.com/path"
	AllowedReferers = []string{
		"https://dm.xueximeng.com",
		"http://localhost:8888",
		// 可以根据需要添加更多允许的来源
	}
)

func init() {
	plugin.RegisterGlobalPlugin(NewHubanPlugin())
}

// 预编译的正则表达式
var (
	// 密码提取正则表达式
	passwordRegex = regexp.MustCompile(`\?pwd=([0-9a-zA-Z]+)`)
	password115Regex = regexp.MustCompile(`password=([0-9a-zA-Z]+)`)

	// 详情页ID提取正则表达式
	detailIDRegex = regexp.MustCompile(`/id/(\d+)`)
	
	// 常见网盘链接的正则表达式（支持16种类型）
	quarkLinkRegex     = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	ucLinkRegex        = regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-zA-Z]+(\?[^"'\s]*)?`)
	baiduLinkRegex     = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+(\?pwd=[0-9a-zA-Z]+)?`)
	aliyunLinkRegex    = regexp.MustCompile(`https?://(www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9a-zA-Z]+`)
	xunleiLinkRegex    = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+(\?pwd=[0-9a-zA-Z]+)?`)
	tianyiLinkRegex    = regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z]+`)
	link115Regex       = regexp.MustCompile(`https?://(115\.com|115cdn\.com)/s/[0-9a-zA-Z]+`)
	mobileLinkRegex    = regexp.MustCompile(`https?://caiyun\.feixin\.10086\.cn/[0-9a-zA-Z]+`)
	weiyunLinkRegex    = regexp.MustCompile(`https?://share\.weiyun\.com/[0-9a-zA-Z]+`)
	lanzouLinkRegex    = regexp.MustCompile(`https?://(www\.)?(lanzou[uixys]*|lan[zs]o[ux])\.(com|net|org)/[0-9a-zA-Z]+`)
	jianguoyunLinkRegex = regexp.MustCompile(`https?://(www\.)?jianguoyun\.com/p/[0-9a-zA-Z]+`)
	link123Regex       = regexp.MustCompile(`https?://(123pan\.com|www\.123912\.com|www\.123865\.com|www\.123684\.com)/s/[0-9a-zA-Z]+`)
	pikpakLinkRegex    = regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z]+`)
	magnetLinkRegex    = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}`)
	ed2kLinkRegex      = regexp.MustCompile(`ed2k://\|file\|.+\|\d+\|[0-9a-fA-F]{32}\|/`)
)

// HubanAsyncPlugin Huban异步插件
type HubanAsyncPlugin struct {
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

	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
	}
}

// NewHubanPlugin 创建新的Huban异步插件
func NewHubanPlugin() *HubanAsyncPlugin {
	return &HubanAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("huban", 2),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 同步搜索接口
func (p *HubanAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 请求来源检查 - 参考panyq插件实现
	if EnableRefererCheck && ext != nil {
		referer := ""
		if refererVal, ok := ext["referer"].(string); ok {
			referer = refererVal
		}
		
		// 检查referer是否在允许列表中
		allowed := false
		for _, allowedReferer := range AllowedReferers {
			if strings.HasPrefix(referer, allowedReferer) {
				if DebugLog {
					fmt.Printf("[%s] 允许来自 %s 的请求\n", p.Name(), referer)
				}
				allowed = true
				break
			}
		}
		
		if !allowed {
			if DebugLog {
				fmt.Printf("[%s] 拒绝来自 %s 的请求\n", p.Name(), referer)
			}
			return nil, fmt.Errorf("[%s] 请求来源不被允许", p.Name())
		}
	}
	
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 带结果统计的搜索接口
func (p *HubanAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现 - HTML解析版本
func (p *HubanAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 性能统计
	start := time.Now()
	atomic.AddInt64(&searchRequests, 1)
	defer func() {
		duration := time.Since(start).Nanoseconds()
		atomic.AddInt64(&totalSearchTime, duration)
	}()

	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("http://103.45.162.207:20720/index.php/vod/search/wd/%s.html", url.QueryEscape(keyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "http://103.45.162.207:20720/")

	// 5. 发送请求
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

	doc.Find(".module-search-item").Each(func(i int, s *goquery.Selection) {
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
func (p *HubanAsyncPlugin) parseSearchItem(s *goquery.Selection, keyword string) model.SearchResult {
	result := model.SearchResult{}

	// 提取详情页链接和ID
	detailLink, exists := s.Find(".video-info-header h3 a").First().Attr("href")
	if !exists {
		return result
	}

	// 提取ID
	matches := detailIDRegex.FindStringSubmatch(detailLink)
	if len(matches) < 2 {
		return result
	}
	itemID := matches[1]

	// 构建唯一ID
	uniqueID := fmt.Sprintf("%s-%s", p.Name(), itemID)

	// 提取标题
	title := strings.TrimSpace(s.Find(".video-info-header h3 a").First().Text())
	if title == "" {
		return result
	}

	// 提取分类
	category := strings.TrimSpace(s.Find(".video-info-items").First().Find(".video-info-item").First().Text())

	// 提取导演
	directorElement := s.Find(".video-info-items").FilterFunction(func(i int, item *goquery.Selection) bool {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		return strings.Contains(title, "导演")
	})
	director := strings.TrimSpace(directorElement.Find(".video-info-item").Text())

	// 提取主演
	actorElement := s.Find(".video-info-items").FilterFunction(func(i int, item *goquery.Selection) bool {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		return strings.Contains(title, "主演")
	})
	actor := strings.TrimSpace(actorElement.Find(".video-info-item").Text())

	// 提取年份
	year := strings.TrimSpace(s.Find(".video-info-items").Last().Find(".video-info-item").First().Text())

	// 提取质量/状态
	quality := strings.TrimSpace(s.Find(".video-info-header .video-info-remarks").Text())

	// 提取剧情简介
	plotElement := s.Find(".video-info-items").FilterFunction(func(i int, item *goquery.Selection) bool {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		return strings.Contains(title, "剧情")
	})
	plot := strings.TrimSpace(plotElement.Find(".video-info-item").Text())

	// 提取封面图片
	coverImage, _ := s.Find(".module-item-pic > img").Attr("data-src")

	// 构建内容描述
	var contentParts []string
	if category != "" {
		contentParts = append(contentParts, fmt.Sprintf("分类: %s", category))
	}
	if director != "" {
		contentParts = append(contentParts, fmt.Sprintf("导演: %s", director))
	}
	if actor != "" {
		contentParts = append(contentParts, fmt.Sprintf("主演: %s", actor))
	}
	if quality != "" {
		contentParts = append(contentParts, fmt.Sprintf("质量: %s", quality))
	}
	if plot != "" {
		contentParts = append(contentParts, fmt.Sprintf("剧情: %s", plot))
	}

	// 构建标签
	var tags []string
	if year != "" {
		tags = append(tags, year)
	}

	// 构建图片数组
	var images []string
	if coverImage != "" {
		images = append(images, coverImage)
	}

	return model.SearchResult{
		UniqueID: uniqueID,
		Title:    title,
		Content:  strings.Join(contentParts, " | "),
		Images:   images,
		Tags:     tags,
		Channel:  "",
		Datetime: time.Time{},
	}
}

// enhanceWithDetails 异步获取详情页信息
func (p *HubanAsyncPlugin) enhanceWithDetails(client *http.Client, results []model.SearchResult) []model.SearchResult {
	var enhancedResults []model.SearchResult
	var wg sync.WaitGroup
	var mu sync.Mutex

	// 创建信号量限制并发数
	semaphore := make(chan struct{}, MaxConcurrency)

	for _, result := range results {
		wg.Add(1)
		go func(result model.SearchResult) {
			defer wg.Done()
			semaphore <- struct{}{}        // 获取信号量
			defer func() { <-semaphore }() // 释放信号量

			// 从UniqueID中提取itemID
			parts := strings.Split(result.UniqueID, "-")
			if len(parts) < 2 {
				mu.Lock()
				enhancedResults = append(enhancedResults, result)
				mu.Unlock()
				return
			}
			itemID := parts[1]

			// 检查缓存
			if cached, ok := detailCache.Load(itemID); ok {
				atomic.AddInt64(&cacheHits, 1)
				r := cached.(model.SearchResult)
				mu.Lock()
				enhancedResults = append(enhancedResults, r)
				mu.Unlock()
				return
			}

			atomic.AddInt64(&cacheMisses, 1)

			// 获取详情页链接和图片
			detailLinks, detailImages := p.fetchDetailLinksAndImages(client, itemID)
			result.Links = detailLinks

			// 合并图片：优先使用详情页的海报，如果没有则使用搜索结果的图片
			if len(detailImages) > 0 {
				result.Images = detailImages
			}

			// 缓存结果
			detailCache.Store(itemID, result)

			mu.Lock()
			enhancedResults = append(enhancedResults, result)
			mu.Unlock()
		}(result)
	}

	wg.Wait()
	return enhancedResults
}

// fetchDetailLinksAndImages 获取详情页的下载链接和图片
func (p *HubanAsyncPlugin) fetchDetailLinksAndImages(client *http.Client, itemID string) ([]model.Link, []string) {
	// 性能统计
	start := time.Now()
	atomic.AddInt64(&detailPageRequests, 1)
	defer func() {
		duration := time.Since(start).Nanoseconds()
		atomic.AddInt64(&totalDetailTime, duration)
	}()

	detailURL := fmt.Sprintf("http://103.45.162.207:20720/index.php/vod/detail/id/%s.html", itemID)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DetailTimeout)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, nil
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "http://103.45.162.207:20720/")

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, nil
	}

	var links []model.Link
	var images []string

	// 提取详情页的海报图片
	if posterURL, exists := doc.Find(".mobile-play .lazyload").Attr("data-src"); exists && posterURL != "" {
		images = append(images, posterURL)
	}

	// 查找下载链接区域
	doc.Find("#download-list .module-row-one").Each(func(i int, s *goquery.Selection) {
		// 从data-clipboard-text属性提取链接
		if linkURL, exists := s.Find("[data-clipboard-text]").Attr("data-clipboard-text"); exists {
			// 过滤掉无效链接
			if p.isValidNetworkDriveURL(linkURL) {
				if linkType := p.determineLinkType(linkURL); linkType != "" {
					link := model.Link{
						Type:     linkType,
						URL:      linkURL,
						Password: "", // 大部分网盘不需要密码
					}
					links = append(links, link)
				}
			}
		}
	})

	return links, images
}



// isValidNetworkDriveURL 检查URL是否为有效的网盘链接
func (p *HubanAsyncPlugin) isValidNetworkDriveURL(url string) bool {
	// 过滤掉明显无效的链接
	if strings.Contains(url, "javascript:") || 
	   url == "" ||
	   (!strings.HasPrefix(url, "http") && !strings.HasPrefix(url, "magnet:") && !strings.HasPrefix(url, "ed2k:")) {
		return false
	}
	
	// 检查是否匹配任何支持的网盘格式（16种）
	return quarkLinkRegex.MatchString(url) ||
		   ucLinkRegex.MatchString(url) ||
		   baiduLinkRegex.MatchString(url) ||
		   aliyunLinkRegex.MatchString(url) ||
		   xunleiLinkRegex.MatchString(url) ||
		   tianyiLinkRegex.MatchString(url) ||
		   link115Regex.MatchString(url) ||
		   mobileLinkRegex.MatchString(url) ||
		   weiyunLinkRegex.MatchString(url) ||
		   lanzouLinkRegex.MatchString(url) ||
		   jianguoyunLinkRegex.MatchString(url) ||
		   link123Regex.MatchString(url) ||
		   pikpakLinkRegex.MatchString(url) ||
		   magnetLinkRegex.MatchString(url) ||
		   ed2kLinkRegex.MatchString(url)
}

// determineLinkType 根据URL确定链接类型（支持16种类型）
func (p *HubanAsyncPlugin) determineLinkType(url string) string {
	switch {
	case quarkLinkRegex.MatchString(url):
		return "quark"
	case ucLinkRegex.MatchString(url):
		return "uc"
	case baiduLinkRegex.MatchString(url):
		return "baidu"
	case aliyunLinkRegex.MatchString(url):
		return "aliyun"
	case xunleiLinkRegex.MatchString(url):
		return "xunlei"
	case tianyiLinkRegex.MatchString(url):
		return "tianyi"
	case link115Regex.MatchString(url):
		return "115"
	case mobileLinkRegex.MatchString(url):
		return "mobile"
	case weiyunLinkRegex.MatchString(url):
		return "weiyun"
	case lanzouLinkRegex.MatchString(url):
		return "lanzou"
	case jianguoyunLinkRegex.MatchString(url):
		return "jianguoyun"
	case link123Regex.MatchString(url):
		return "123"
	case pikpakLinkRegex.MatchString(url):
		return "pikpak"
	case magnetLinkRegex.MatchString(url):
		return "magnet"
	case ed2kLinkRegex.MatchString(url):
		return "ed2k"
	default:
		return "" // 不支持的类型返回空字符串
	}
}

// extractPassword 从URL中提取密码
func (p *HubanAsyncPlugin) extractPassword(url string) string {
	// 百度网盘密码
	if matches := passwordRegex.FindStringSubmatch(url); len(matches) > 1 {
		return matches[1]
	}
	
	// 115网盘密码
	if matches := password115Regex.FindStringSubmatch(url); len(matches) > 1 {
		return matches[1]
	}
	
	return ""
}

// doRequestWithRetry 带重试的HTTP请求
func (p *HubanAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 2
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp, nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP状态码: %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		// 快速重试：只等待很短时间
		if i < maxRetries-1 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	return nil, fmt.Errorf("[%s] 请求失败，重试%d次后仍失败: %w", p.Name(), maxRetries, lastErr)
}

// GetPerformanceStats 获取性能统计信息
func (p *HubanAsyncPlugin) GetPerformanceStats() map[string]interface{} {
	totalRequests := atomic.LoadInt64(&searchRequests)
	totalTime := atomic.LoadInt64(&totalSearchTime)
	detailRequests := atomic.LoadInt64(&detailPageRequests)
	detailTime := atomic.LoadInt64(&totalDetailTime)
	hits := atomic.LoadInt64(&cacheHits)
	misses := atomic.LoadInt64(&cacheMisses)

	var avgTime float64
	if totalRequests > 0 {
		avgTime = float64(totalTime) / float64(totalRequests) / 1e6 // 转换为毫秒
	}

	var avgDetailTime float64
	if detailRequests > 0 {
		avgDetailTime = float64(detailTime) / float64(detailRequests) / 1e6 // 转换为毫秒
	}

	return map[string]interface{}{
		"search_requests":      totalRequests,
		"avg_search_time_ms":   avgTime,
		"total_search_time_ns": totalTime,
		"detail_page_requests": detailRequests,
		"avg_detail_time_ms":   avgDetailTime,
		"total_detail_time_ns": detailTime,
		"cache_hits":           hits,
		"cache_misses":         misses,
	}
}

// AddAllowedReferer 添加允许的请求来源
func AddAllowedReferer(referer string) {
	for _, existing := range AllowedReferers {
		if existing == referer {
			return // 已存在，不重复添加
		}
	}
	AllowedReferers = append(AllowedReferers, referer)
}

// RemoveAllowedReferer 移除允许的请求来源
func RemoveAllowedReferer(referer string) {
	for i, existing := range AllowedReferers {
		if existing == referer {
			AllowedReferers = append(AllowedReferers[:i], AllowedReferers[i+1:]...)
			return
		}
	}
}

// GetAllowedReferers 获取当前允许的请求来源列表
func GetAllowedReferers() []string {
	result := make([]string, len(AllowedReferers))
	copy(result, AllowedReferers)
	return result
}

// IsRefererAllowed 检查指定的referer是否被允许
func IsRefererAllowed(referer string) bool {
	for _, allowedReferer := range AllowedReferers {
		if strings.HasPrefix(referer, allowedReferer) {
			return true
		}
	}
	return false
}