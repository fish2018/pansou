package labi

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
	// 从详情页URL中提取ID的正则表达式
	detailIDRegex = regexp.MustCompile(`/vod/detail/id/(\d+)\.html`)
	
	// 夸克网盘链接的正则表达式
	quarkLinkRegex = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	
	// 年份提取正则表达式
	yearRegex = regexp.MustCompile(`(\d{4})`)
	
	// 缓存相关
	detailCache = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL = 1 * time.Hour // 优化为更短的缓存时间
)

const (
	// 超时时间优化
	DefaultTimeout = 8 * time.Second
	DetailTimeout  = 6 * time.Second
	// 并发数优化
	MaxConcurrency = 20
	// HTTP连接池配置
	MaxIdleConns        = 200
	MaxIdleConnsPerHost = 50
	MaxConnsPerHost     = 100
	IdleConnTimeout     = 90 * time.Second
)

// 性能统计
var (
	searchRequests     int64 = 0
	detailPageRequests int64 = 0
	cacheHits          int64 = 0
	cacheMisses        int64 = 0
	totalSearchTime    int64 = 0
	totalDetailTime    int64 = 0
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewLabiPlugin())
	
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

// LabiAsyncPlugin Labi异步插件
type LabiAsyncPlugin struct {
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

// NewLabiPlugin 创建新的Labi异步插件
func NewLabiPlugin() *LabiAsyncPlugin {
	return &LabiAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("labi", 1),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *LabiAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *LabiAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *LabiAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("http://xiaocge.fun/index.php/vod/search/wd/%s.html", url.QueryEscape(keyword))
	
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
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", "http://xiaocge.fun/")
	
	// 5. 发送请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求返回状态码: %d", p.Name(), resp.StatusCode)
	}
	
	// 3. 解析搜索结果页面
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}
	
	// 4. 提取搜索结果
	var results []model.SearchResult
	
	doc.Find(".module-search-item").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchItem(s, keyword)
		if result.UniqueID != "" {
			results = append(results, result)
		}
	})
	
	// 5. 异步获取详情页信息
	enhancedResults := p.enhanceWithDetails(client, results)
	
	// 6. 关键词过滤
	return plugin.FilterResultsByKeyword(enhancedResults, keyword), nil
}

// parseSearchItem 解析单个搜索结果项
func (p *LabiAsyncPlugin) parseSearchItem(s *goquery.Selection, keyword string) model.SearchResult {
	result := model.SearchResult{}
	
	// 提取详情页链接和ID
	detailLink, exists := s.Find(".module-item-pic a").First().Attr("href")
	if !exists {
		return result
	}
	
	// 提取ID
	matches := detailIDRegex.FindStringSubmatch(detailLink)
	if len(matches) < 2 {
		return result
	}
	
	itemID := matches[1]
	result.UniqueID = fmt.Sprintf("%s-%s", p.Name(), itemID)
	
	// 提取标题
	titleElement := s.Find(".video-info-header h3 a")
	result.Title = strings.TrimSpace(titleElement.Text())
	
	// 提取资源类型/质量
	qualityElement := s.Find(".video-serial")
	quality := strings.TrimSpace(qualityElement.Text())
	
	// 提取分类信息
	var tags []string
	s.Find(".video-info-aux .tag-link a").Each(func(i int, tag *goquery.Selection) {
		tagText := strings.TrimSpace(tag.Text())
		if tagText != "" {
			tags = append(tags, tagText)
		}
	})
	result.Tags = tags
	
	// 提取导演信息
	director := ""
	s.Find(".video-info-items").Each(func(i int, item *goquery.Selection) {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		if strings.Contains(title, "导演") {
			director = strings.TrimSpace(item.Find(".video-info-actor a").Text())
		}
	})
	
	// 提取主演信息
	var actors []string
	s.Find(".video-info-items").Each(func(i int, item *goquery.Selection) {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		if strings.Contains(title, "主演") {
			item.Find(".video-info-actor a").Each(func(j int, actor *goquery.Selection) {
				actorName := strings.TrimSpace(actor.Text())
				if actorName != "" {
					actors = append(actors, actorName)
				}
			})
		}
	})
	
	// 提取剧情简介
	plotElement := s.Find(".video-info-items").FilterFunction(func(i int, item *goquery.Selection) bool {
		title := strings.TrimSpace(item.Find(".video-info-itemtitle").Text())
		return strings.Contains(title, "剧情")
	})
	plot := strings.TrimSpace(plotElement.Find(".video-info-item").Text())

	// 提取封面图片 (参考 Pan_wogg.js 的选择器)
	var images []string
	if picURL, exists := s.Find(".module-item-pic > img").Attr("data-src"); exists && picURL != "" {
		images = append(images, picURL)
	}
	result.Images = images

	// 构建内容描述
	var contentParts []string
	if quality != "" {
		contentParts = append(contentParts, "【"+quality+"】")
	}
	if director != "" {
		contentParts = append(contentParts, "导演："+director)
	}
	if len(actors) > 0 {
		actorStr := strings.Join(actors[:min(3, len(actors))], "、") // 只显示前3个演员
		if len(actors) > 3 {
			actorStr += "等"
		}
		contentParts = append(contentParts, "主演："+actorStr)
	}
	if plot != "" {
		contentParts = append(contentParts, plot)
	}

	result.Content = strings.Join(contentParts, "\n")
	result.Channel = "" // 插件搜索结果不设置频道名，只有Telegram频道结果才设置
	result.Datetime = time.Time{} // 使用零值而不是nil，参考jikepan插件标准

	return result
}

// enhanceWithDetails 异步获取详情页信息以获取下载链接
func (p *LabiAsyncPlugin) enhanceWithDetails(client *http.Client, results []model.SearchResult) []model.SearchResult {
	var enhancedResults []model.SearchResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	
	// 限制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	
	for _, result := range results {
		wg.Add(1)
		go func(r model.SearchResult) {
			defer wg.Done()
			
			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			
			// 从UniqueID提取ID
			parts := strings.Split(r.UniqueID, "-")
			if len(parts) < 2 {
				mu.Lock()
				enhancedResults = append(enhancedResults, r)
				mu.Unlock()
				return
			}
			
			itemID := parts[1]
			
			// 检查缓存
			if cached, ok := detailCache.Load(itemID); ok {
				if cachedResult, ok := cached.(model.SearchResult); ok {
					mu.Lock()
					enhancedResults = append(enhancedResults, cachedResult)
					mu.Unlock()
					return
				}
			}
			
			// 获取详情页链接和图片
			detailLinks, detailImages := p.fetchDetailLinksAndImages(client, itemID)
			r.Links = detailLinks

			// 合并图片：优先使用详情页的海报，如果没有则使用搜索结果的图片
			if len(detailImages) > 0 {
				r.Images = detailImages
			}

			// 缓存结果
			detailCache.Store(itemID, r)
			
			mu.Lock()
			enhancedResults = append(enhancedResults, r)
			mu.Unlock()
		}(result)
	}
	
	wg.Wait()
	return enhancedResults
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *LabiAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error
	
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}
		
		// 克隆请求
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

// fetchDetailLinksAndImages 获取详情页的下载链接和图片
func (p *LabiAsyncPlugin) fetchDetailLinksAndImages(client *http.Client, itemID string) ([]model.Link, []string) {
	detailURL := fmt.Sprintf("http://xiaocge.fun/index.php/vod/detail/id/%s.html", itemID)

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
	req.Header.Set("Referer", "http://xiaocge.fun/")

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

	// 提取详情页的海报图片 (参考 Pan_wogg.js 的选择器)
	if posterURL, exists := doc.Find(".module-item-pic > img").Attr("data-src"); exists && posterURL != "" {
		images = append(images, posterURL)
	}

	// 查找下载链接区域
	doc.Find("#download-list .module-row-one").Each(func(i int, s *goquery.Selection) {
		// 从data-clipboard-text属性提取链接
		if linkURL, exists := s.Find("[data-clipboard-text]").Attr("data-clipboard-text"); exists {
			// 过滤掉无效链接
			if p.isValidNetworkDriveURL(linkURL) && quarkLinkRegex.MatchString(linkURL) {
				link := model.Link{
					Type:     "quark",
					URL:      linkURL,
					Password: "", // 夸克网盘通常不需要密码
				}
				links = append(links, link)
			}
		}

		// 也检查直接的href属性
		s.Find("a[href]").Each(func(j int, a *goquery.Selection) {
			if linkURL, exists := a.Attr("href"); exists {
				// 过滤掉无效链接
				if p.isValidNetworkDriveURL(linkURL) && quarkLinkRegex.MatchString(linkURL) {
					// 避免重复添加
					isDuplicate := false
					for _, existingLink := range links {
						if existingLink.URL == linkURL {
							isDuplicate = true
							break
						}
					}

					if !isDuplicate {
						link := model.Link{
							Type:     "quark",
							URL:      linkURL,
							Password: "",
						}
						links = append(links, link)
					}
				}
			}
		})
	})

	return links, images
}

// fetchDetailLinks 获取详情页的下载链接（兼容性方法，仅返回链接）
func (p *LabiAsyncPlugin) fetchDetailLinks(client *http.Client, itemID string) []model.Link {
	links, _ := p.fetchDetailLinksAndImages(client, itemID)
	return links
}

// isValidNetworkDriveURL 检查URL是否为有效的网盘链接
func (p *LabiAsyncPlugin) isValidNetworkDriveURL(url string) bool {
	// 过滤掉明显无效的链接
	if strings.Contains(url, "javascript:") || 
	   strings.Contains(url, "#") ||
	   url == "" ||
	   !strings.HasPrefix(url, "http") {
		return false
	}
	
	// 对于labi插件，只检查夸克网盘格式
	return quarkLinkRegex.MatchString(url)
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}