package hdmoli

import (
	"context"
	"fmt"
	"io"
	"log"
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
	PluginName     = "hdmoli"
	DisplayName    = "HDmoli"
	Description    = "HDmoli - 影视资源网盘下载链接搜索"
	BaseURL        = "https://www.hdmoli.com"
	SearchPath     = "/search.php?searchkey=%s&submit="
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults     = 50
	MaxConcurrency = 20
)

// HdmoliPlugin HDmoli插件
type HdmoliPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewHdmoliPlugin())
}

// NewHdmoliPlugin 创建新的HDmoli插件实例
func NewHdmoliPlugin() *HdmoliPlugin {
	debugMode := false // 生产环境关闭调试

	p := &HdmoliPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 2), // 标准网盘插件，启用Service层过滤
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute, // 详情页缓存30分钟
	}

	return p
}

// Name 插件名称
func (p *HdmoliPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *HdmoliPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *HdmoliPlugin) Description() string {
	return Description
}

// Search 搜索接口
func (p *HdmoliPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	return p.searchImpl(&http.Client{Timeout: 30 * time.Second}, keyword, ext)
}

// searchImpl 搜索实现
func (p *HdmoliPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[HDMOLI] 开始搜索: %s", keyword)
	}

	// 第一步：执行搜索获取结果列表
	searchResults, err := p.executeSearch(client, keyword)
	if err != nil {
		return nil, fmt.Errorf("[%s] 执行搜索失败: %w", p.Name(), err)
	}

	if p.debugMode {
		log.Printf("[HDMOLI] 搜索获取到 %d 个结果", len(searchResults))
	}

	// 第二步：并发获取详情页链接
	finalResults := p.fetchDetailLinks(client, searchResults, keyword)

	if p.debugMode {
		log.Printf("[HDMOLI] 最终获取到 %d 个有效结果", len(finalResults))
	}

	// 第三步：关键词过滤（标准网盘插件需要过滤）
	filteredResults := plugin.FilterResultsByKeyword(finalResults, keyword)

	if p.debugMode {
		log.Printf("[HDMOLI] 关键词过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// executeSearch 执行搜索请求
func (p *HdmoliPlugin) executeSearch(client *http.Client, keyword string) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("%s%s", BaseURL, fmt.Sprintf(SearchPath, url.QueryEscape(keyword)))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", BaseURL+"/") // HDmoli需要设置referer

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求HTTP状态错误: %d", p.Name(), resp.StatusCode)
	}

	// 解析HTML提取搜索结果
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果HTML失败: %w", p.Name(), err)
	}

	return p.parseSearchResults(doc)
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *HdmoliPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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

	return nil, fmt.Errorf("[%s] 重试 %d 次后仍然失败: %w", p.Name(), maxRetries, lastErr)
}

// parseSearchResults 解析搜索结果HTML
func (p *HdmoliPlugin) parseSearchResults(doc *goquery.Document) ([]model.SearchResult, error) {
	var results []model.SearchResult

	// 查找搜索结果项: #searchList > li.active.clearfix
	doc.Find("#searchList > li.active.clearfix").Each(func(i int, s *goquery.Selection) {
		if len(results) >= MaxResults {
			return
		}

		result := p.parseResultItem(s, i+1)
		if result != nil {
			results = append(results, *result)
		}
	})

	if p.debugMode {
		log.Printf("[HDMOLI] 解析到 %d 个原始结果", len(results))
	}

	return results, nil
}

// parseResultItem 解析单个搜索结果项
func (p *HdmoliPlugin) parseResultItem(s *goquery.Selection, index int) *model.SearchResult {
	// 提取标题和链接
	titleEl := s.Find(".detail h4.title a")
	if titleEl.Length() == 0 {
		if p.debugMode {
			log.Printf("[HDMOLI] 跳过无标题链接的结果")
		}
		return nil
	}

	// 提取标题
	title := strings.TrimSpace(titleEl.Text())
	if title == "" {
		return nil
	}

	// 提取详情页链接
	detailURL, _ := titleEl.Attr("href")
	if detailURL == "" {
		// 尝试从缩略图获取链接
		thumbEl := s.Find(".thumb a")
		if thumbEl.Length() > 0 {
			detailURL, _ = thumbEl.Attr("href")
		}
	}

	if detailURL == "" {
		if p.debugMode {
			log.Printf("[HDMOLI] 跳过无链接的结果: %s", title)
		}
		return nil
	}

	// 处理相对路径
	if strings.HasPrefix(detailURL, "/") {
		detailURL = BaseURL + detailURL
	}

	// 提取评分
	rating := p.extractRating(s)

	// 提取更新状态
	updateStatus := p.extractUpdateStatus(s)

	// 提取导演
	director := p.extractDirector(s)

	// 提取主演
	actors := p.extractActors(s)

	// 提取分类信息
	category, region, year := p.extractCategoryInfo(s)

	// 提取简介
	description := p.extractDescription(s)

	// 构建内容
	var contentParts []string
	if rating != "" {
		contentParts = append(contentParts, fmt.Sprintf("评分：%s", rating))
	}
	if updateStatus != "" {
		contentParts = append(contentParts, fmt.Sprintf("状态：%s", updateStatus))
	}
	if director != "" {
		contentParts = append(contentParts, fmt.Sprintf("导演：%s", director))
	}
	if len(actors) > 0 {
		actorStr := strings.Join(actors, " ")
		if len(actorStr) > 100 {
			actorStr = actorStr[:100] + "..."
		}
		contentParts = append(contentParts, fmt.Sprintf("主演：%s", actorStr))
	}
	if category != "" {
		contentParts = append(contentParts, fmt.Sprintf("分类：%s", category))
	}
	if region != "" {
		contentParts = append(contentParts, fmt.Sprintf("地区：%s", region))
	}
	if year != "" {
		contentParts = append(contentParts, fmt.Sprintf("年份：%s", year))
	}
	if description != "" {
		contentParts = append(contentParts, fmt.Sprintf("简介：%s", description))
	}

	content := strings.Join(contentParts, "\n")

	// 构建标签
	var tags []string
	if category != "" {
		tags = append(tags, category)
	}
	if region != "" {
		tags = append(tags, region)
	}
	if year != "" {
		tags = append(tags, year)
	}

	// 构建初始结果对象（详情页链接稍后获取）
	itemID := detailURL
	if parsedURL, err := url.Parse(detailURL); err == nil {
		itemID = parsedURL.Path
	}
	stableID := fmt.Sprintf("%s-%s", p.Name(), url.QueryEscape(strings.Trim(itemID, "/")))
	result := model.SearchResult{
		Title:     title,
		Content:   content,
		Channel:   "", // 插件搜索结果必须为空字符串（按开发指南要求）
		MessageID: stableID,
		UniqueID:  stableID,
		Datetime:  time.Now(),     // 搜索结果页没有明确时间，使用当前时间
		Links:     []model.Link{}, // 先为空，详情页处理后添加
		Tags:      tags,
	}

	// 添加详情页URL到临时字段（用于后续处理）
	result.Content += fmt.Sprintf("\n详情页URL: %s", detailURL)

	if p.debugMode {
		log.Printf("[HDMOLI] 解析结果: %s (%s)", title, category)
	}

	return &result
}

// extractRating 提取评分
func (p *HdmoliPlugin) extractRating(s *goquery.Selection) string {
	ratingEl := s.Find(".pic-tag")
	if ratingEl.Length() > 0 {
		rating := strings.TrimSpace(ratingEl.Text())
		return rating
	}
	return ""
}

// extractUpdateStatus 提取更新状态
func (p *HdmoliPlugin) extractUpdateStatus(s *goquery.Selection) string {
	statusEl := s.Find(".pic-text")
	if statusEl.Length() > 0 {
		status := strings.TrimSpace(statusEl.Text())
		return status
	}
	return ""
}

// extractDirector 提取导演
func (p *HdmoliPlugin) extractDirector(s *goquery.Selection) string {
	var director string
	s.Find("p").Each(func(i int, p *goquery.Selection) {
		if director != "" {
			return // 已找到，跳过
		}
		text := p.Text()
		if strings.Contains(text, "导演：") {
			// 提取导演名称
			parts := strings.Split(text, "导演：")
			if len(parts) > 1 {
				director = strings.TrimSpace(parts[1])
			}
		}
	})
	return director
}

// extractActors 提取主演
func (p *HdmoliPlugin) extractActors(s *goquery.Selection) []string {
	var actors []string
	s.Find("p").Each(func(i int, p *goquery.Selection) {
		text := p.Text()
		if strings.Contains(text, "主演：") {
			// 在这个p标签中查找所有链接
			p.Find("a").Each(func(j int, a *goquery.Selection) {
				actor := strings.TrimSpace(a.Text())
				if actor != "" {
					actors = append(actors, actor)
				}
			})
		}
	})
	return actors
}

// extractCategoryInfo 提取分类信息（分类、地区、年份）
func (p *HdmoliPlugin) extractCategoryInfo(s *goquery.Selection) (category, region, year string) {
	s.Find("p").Each(func(i int, p *goquery.Selection) {
		text := p.Text()
		if strings.Contains(text, "分类：") {
			// 解析分类信息行
			parts := strings.Split(text, "：")
			for i, part := range parts {
				part = strings.TrimSpace(part)
				if strings.HasSuffix(parts[i], "分类") && i+1 < len(parts) {
					// 提取分类，可能包含地区和年份信息
					info := strings.TrimSpace(parts[i+1])
					// 按分隔符分割
					infoParts := regexp.MustCompile(`[，,\s]+`).Split(info, -1)
					if len(infoParts) > 0 && infoParts[0] != "" {
						category = infoParts[0]
					}
				} else if strings.HasSuffix(parts[i], "地区") && i+1 < len(parts) {
					regionPart := strings.TrimSpace(parts[i+1])
					regionParts := regexp.MustCompile(`[，,\s]+`).Split(regionPart, -1)
					if len(regionParts) > 0 && regionParts[0] != "" {
						region = regionParts[0]
					}
				} else if strings.HasSuffix(parts[i], "年份") && i+1 < len(parts) {
					yearPart := strings.TrimSpace(parts[i+1])
					yearParts := regexp.MustCompile(`[，,\s]+`).Split(yearPart, -1)
					if len(yearParts) > 0 && yearParts[0] != "" {
						year = yearParts[0]
					}
				}
			}
		}
	})
	return category, region, year
}

// extractDescription 提取简介
func (p *HdmoliPlugin) extractDescription(s *goquery.Selection) string {
	var description string
	descEl := s.Find("p.hidden-xs")
	descEl.Each(func(i int, p *goquery.Selection) {
		if description != "" {
			return // 已找到，跳过
		}
		text := p.Text()
		if strings.Contains(text, "简介：") {
			parts := strings.Split(text, "简介：")
			if len(parts) > 1 {
				desc := strings.TrimSpace(parts[1])
				// 限制长度
				if len(desc) > 200 {
					desc = desc[:200] + "..."
				}
				description = desc
			}
		}
	})
	return description
}

// fetchDetailLinks 并发获取详情页链接
func (p *HdmoliPlugin) fetchDetailLinks(client *http.Client, searchResults []model.SearchResult, keyword string) []model.SearchResult {
	if len(searchResults) == 0 {
		return []model.SearchResult{}
	}

	// 使用通道控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	resultsChan := make(chan model.SearchResult, len(searchResults))

	for _, result := range searchResults {
		wg.Add(1)
		go func(r model.SearchResult) {
			defer wg.Done()
			semaphore <- struct{}{}        // 获取信号量
			defer func() { <-semaphore }() // 释放信号量

			// 从Content中提取详情页URL
			detailURL := p.extractDetailURLFromContent(r.Content)
			if detailURL == "" {
				if p.debugMode {
					log.Printf("[HDMOLI] 跳过无详情页URL的结果: %s", r.Title)
				}
				return
			}

			// 获取详情页链接
			links := p.fetchDetailPageLinks(client, detailURL)
			if len(links) > 0 {
				r.Links = links
				// 清理Content中的详情页URL
				r.Content = p.cleanContent(r.Content)
				resultsChan <- r
			} else if p.debugMode {
				log.Printf("[HDMOLI] 详情页无有效链接: %s", r.Title)
			}
		}(result)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// 收集结果
	var finalResults []model.SearchResult
	for result := range resultsChan {
		finalResults = append(finalResults, result)
	}

	return finalResults
}

// extractDetailURLFromContent 从Content中提取详情页URL
func (p *HdmoliPlugin) extractDetailURLFromContent(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "详情页URL: ") {
			return strings.TrimPrefix(line, "详情页URL: ")
		}
	}
	return ""
}

// cleanContent 清理Content，移除详情页URL行
func (p *HdmoliPlugin) cleanContent(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "详情页URL: ") {
			cleanedLines = append(cleanedLines, line)
		}
	}
	return strings.Join(cleanedLines, "\n")
}

// fetchDetailPageLinks 获取详情页的网盘链接
func (p *HdmoliPlugin) fetchDetailPageLinks(client *http.Client, detailURL string) []model.Link {
	// 检查缓存
	if cached, found := p.detailCache.Load(detailURL); found {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[HDMOLI] 使用缓存的详情页链接: %s", detailURL)
			}
			return links
		}
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		if p.debugMode {
			log.Printf("[HDMOLI] 创建详情页请求失败: %v", err)
		}
		return []model.Link{}
	}

	// 设置请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", BaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[HDMOLI] 详情页请求失败: %v", err)
		}
		return []model.Link{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[HDMOLI] 详情页HTTP状态错误: %d", resp.StatusCode)
		}
		return []model.Link{}
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[HDMOLI] 读取详情页响应失败: %v", err)
		}
		return []model.Link{}
	}

	// 解析网盘链接
	links := p.parseNetworkDiskLinks(string(body))

	// 缓存结果
	if len(links) > 0 {
		p.detailCache.Store(detailURL, links)
	}

	if p.debugMode {
		log.Printf("[HDMOLI] 从详情页提取到 %d 个链接: %s", len(links), detailURL)
	}

	return links
}

// parseNetworkDiskLinks 解析网盘链接
func (p *HdmoliPlugin) parseNetworkDiskLinks(htmlContent string) []model.Link {
	var links []model.Link

	// 解析HTML文档以便更精确的提取
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		if p.debugMode {
			log.Printf("[HDMOLI] 解析详情页HTML失败: %v", err)
		}
		// 如果解析失败，使用正则表达式作为备选
		return p.parseNetworkDiskLinksWithRegex(htmlContent)
	}

	// 在"视频下载"区域查找网盘链接
	doc.Find(".downlist").Each(func(i int, s *goquery.Selection) {
		s.Find("p").Each(func(j int, pEl *goquery.Selection) {
			text := pEl.Text()

			// 查找夸克网盘
			if strings.Contains(text, "夸 克：") || strings.Contains(text, "夸克：") {
				pEl.Find("a").Each(func(k int, a *goquery.Selection) {
					href, exists := a.Attr("href")
					if exists && strings.Contains(href, "pan.quark.cn") {
						link := model.Link{
							Type:     "quark",
							URL:      href,
							Password: p.extractPasswordFromQuarkURL(href),
						}
						links = append(links, link)
						if p.debugMode {
							log.Printf("[HDMOLI] 找到夸克链接: %s", href)
						}
					}
				})
			}

			// 查找百度网盘
			if strings.Contains(text, "百 度：") || strings.Contains(text, "百度：") {
				pEl.Find("a").Each(func(k int, a *goquery.Selection) {
					href, exists := a.Attr("href")
					if exists && strings.Contains(href, "pan.baidu.com") {
						password := p.extractPasswordFromBaiduURL(href)
						link := model.Link{
							Type:     "baidu",
							URL:      href,
							Password: password,
						}
						links = append(links, link)
						if p.debugMode {
							log.Printf("[HDMOLI] 找到百度链接: %s (密码: %s)", href, password)
						}
					}
				})
			}
		})
	})

	return links
}

// parseNetworkDiskLinksWithRegex 使用正则表达式解析网盘链接（备选方案）
func (p *HdmoliPlugin) parseNetworkDiskLinksWithRegex(htmlContent string) []model.Link {
	var links []model.Link

	// 夸克网盘链接模式
	quarkPattern := regexp.MustCompile(`<b>夸\s*克：</b><a[^>]*href\s*=\s*["']([^"']*pan\.quark\.cn[^"']*)["'][^>]*>`)
	quarkMatches := quarkPattern.FindAllStringSubmatch(htmlContent, -1)
	for _, match := range quarkMatches {
		if len(match) > 1 {
			link := model.Link{
				Type:     "quark",
				URL:      match[1],
				Password: "",
			}
			links = append(links, link)
		}
	}

	// 百度网盘链接模式
	baiduPattern := regexp.MustCompile(`<b>百\s*度：</b><a[^>]*href\s*=\s*["']([^"']*pan\.baidu\.com[^"']*)["'][^>]*>`)
	baiduMatches := baiduPattern.FindAllStringSubmatch(htmlContent, -1)
	for _, match := range baiduMatches {
		if len(match) > 1 {
			password := p.extractPasswordFromBaiduURL(match[1])
			link := model.Link{
				Type:     "baidu",
				URL:      match[1],
				Password: password,
			}
			links = append(links, link)
		}
	}

	return links
}

// extractPasswordFromQuarkURL 从夸克网盘URL提取提取码
func (p *HdmoliPlugin) extractPasswordFromQuarkURL(panURL string) string {
	// 夸克网盘一般不需要提取码，直接返回空
	return ""
}

// extractPasswordFromBaiduURL 从百度网盘URL提取提取码
func (p *HdmoliPlugin) extractPasswordFromBaiduURL(panURL string) string {
	// 检查URL中是否包含pwd参数
	if strings.Contains(panURL, "?pwd=") {
		parts := strings.Split(panURL, "?pwd=")
		if len(parts) > 1 {
			return parts[1]
		}
	}
	if strings.Contains(panURL, "&pwd=") {
		parts := strings.Split(panURL, "&pwd=")
		if len(parts) > 1 {
			return parts[1]
		}
	}
	return ""
}
