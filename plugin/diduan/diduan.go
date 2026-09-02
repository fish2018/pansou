package diduan

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
	"github.com/Advik-B/cloudscraper/lib/stealth"
	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

var encodedURLRegex = regexp.MustCompile(`(?i)atob\(\s*['"]([A-Za-z0-9+/=_-]+)['"]\s*\)`)

func absoluteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	if strings.HasPrefix(raw, "/") {
		return BaseURL + raw
	}
	return raw
}

func linkPassword(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	query := parsed.Query()
	for _, key := range []string{"pwd", "password", "pass", "code"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

const (
	PluginName     = "diduan"
	DisplayName    = "低端影视"
	Description    = "低端影视 - 影视资源网盘链接搜索"
	BaseURL        = "https://ddys.io"
	SearchPath     = "/search?q=%s"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults     = 50
	MaxConcurrency = 20
)

// DiduanPlugin 低端影视插件
type DiduanPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
	scraper     *cloudscraper.Scraper
	scraperMu   sync.Mutex // cloudscraper 的 stealth 状态不是并发安全的
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewDiduanPlugin())
}

// NewDiduanPlugin 创建新的低端影视插件实例
func NewDiduanPlugin() *DiduanPlugin {
	debugMode := false // 生产环境关闭调试
	// API 请求已有超时与并发控制，关闭 cloudscraper 的人为延迟，避免
	// 搜索页 + 详情页请求超过 PanSou 的 4 秒快速响应窗口。
	scraper, err := cloudscraper.New(cloudscraper.WithStealth(stealth.Options{Enabled: false}))
	if err != nil {
		log.Printf("[%s] 初始化 cloudscraper 失败: %v", PluginName, err)
	}

	p := &DiduanPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 1), // 标准网盘插件，启用Service层过滤
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute, // 详情页缓存30分钟
		scraper:         scraper,
	}

	return p
}

// Name 插件名称
func (p *DiduanPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *DiduanPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *DiduanPlugin) Description() string {
	return Description
}

// Search 搜索接口
func (p *DiduanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 使用 BaseAsyncPlugin 的缓存和后台刷新能力。
func (p *DiduanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现
func (p *DiduanPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.scraper == nil {
		return nil, fmt.Errorf("[%s] Cloudflare 请求客户端未初始化", p.Name())
	}
	if p.debugMode {
		log.Printf("[DIDUAN] 开始搜索: %s", keyword)
	}

	// 第一步：执行搜索获取结果列表
	searchResults, err := p.executeSearch(keyword)
	if err != nil {
		return nil, fmt.Errorf("[%s] 执行搜索失败: %w", p.Name(), err)
	}

	if p.debugMode {
		log.Printf("[DIDUAN] 搜索获取到 %d 个结果", len(searchResults))
	}

	// 第二步：并发获取详情页链接
	finalResults := p.fetchDetailLinks(searchResults, keyword)

	if p.debugMode {
		log.Printf("[DIDUAN] 最终获取到 %d 个有效结果", len(finalResults))
	}

	// 第三步：关键词过滤（标准网盘插件需要过滤）
	filteredResults := plugin.FilterResultsByKeyword(finalResults, keyword)

	if p.debugMode {
		log.Printf("[DIDUAN] 关键词过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// executeSearch 执行搜索请求
func (p *DiduanPlugin) executeSearch(keyword string) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("%s%s", BaseURL, fmt.Sprintf(SearchPath, url.QueryEscape(keyword)))

	resp, err := p.getPage(searchURL)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, p.httpStatusError("搜索", resp)
	}

	// 解析HTML提取搜索结果
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果HTML失败: %w", p.Name(), err)
	}

	return p.parseSearchResults(doc)
}

// getPage 串行化 cloudscraper 调用，避免其 stealth 计数器并发竞争。
func (p *DiduanPlugin) getPage(rawURL string) (*http.Response, error) {
	p.scraperMu.Lock()
	defer p.scraperMu.Unlock()
	return p.scraper.Get(rawURL)
}

func (p *DiduanPlugin) httpStatusError(action string, resp *http.Response) error {
	if strings.EqualFold(resp.Header.Get("cf-mitigated"), "challenge") {
		return fmt.Errorf("[%s] %s触发 Cloudflare Managed Challenge (HTTP %d)", p.Name(), action, resp.StatusCode)
	}
	return fmt.Errorf("[%s] %sHTTP状态错误: %d", p.Name(), action, resp.StatusCode)
}

// parseSearchResults 解析搜索结果HTML
func (p *DiduanPlugin) parseSearchResults(doc *goquery.Document) ([]model.SearchResult, error) {
	var results []model.SearchResult

	// ddys.io 当前页面使用 movie-card；影视搜索区的第一个 h2 下才是搜索结果，
	// 后面的 movie-card 是推荐内容，不能一并请求详情页。
	var cards *goquery.Selection
	doc.Find("h2").EachWithBreak(func(_ int, heading *goquery.Selection) bool {
		if strings.HasPrefix(strings.TrimSpace(heading.Text()), "影视") {
			cards = heading.Parent().Parent().Find(".movie-card")
			return false
		}
		return true
	})
	if cards == nil || cards.Length() == 0 {
		// 兼容旧模板：仅扫描文章列表。
		cards = doc.Find("article[class*='post-']")
	}
	cards.EachWithBreak(func(i int, s *goquery.Selection) bool {
		if len(results) >= MaxResults {
			return false
		}
		if result := p.parseResultItem(s, i+1); result != nil {
			results = append(results, *result)
		}
		return true
	})

	if p.debugMode {
		log.Printf("[DIDUAN] 解析到 %d 个原始结果", len(results))
	}

	return results, nil
}

// parseResultItem 解析单个搜索结果项
func (p *DiduanPlugin) parseResultItem(s *goquery.Selection, index int) *model.SearchResult {
	// 新版 ddys 卡片：/movie/<slug>，标题位于 h3 a，海报位于 img。
	linkEl := s.Find("h3 a[href^='/movie/']").First()
	if linkEl.Length() == 0 {
		linkEl = s.Find("a[href^='/movie/']").First()
	}
	if linkEl.Length() > 0 {
		detailURL, exists := linkEl.Attr("href")
		if !exists || detailURL == "" {
			return nil
		}
		detailURL = absoluteURL(detailURL)
		title := strings.TrimSpace(linkEl.Text())
		if title == "" {
			return nil
		}
		slug := strings.Trim(strings.TrimPrefix(detailURL, BaseURL+"/movie/"), "/")
		if slug == "" {
			slug = fmt.Sprintf("item-%d", index)
		}
		content := strings.TrimSpace(s.Find("h3").Parent().Text())
		result := &model.SearchResult{
			Title:     title,
			Content:   fmt.Sprintf("%s\n详情页: %s", content, detailURL),
			Channel:   "",
			MessageID: fmt.Sprintf("%s-%s", p.Name(), slug),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), slug),
			Datetime:  time.Now(),
			Links:     []model.Link{},
		}
		if poster, ok := s.Find("img").First().Attr("src"); ok && poster != "" {
			result.Images = []string{poster}
		}
		return result
	}

	// 旧 WordPress 模板兼容路径。
	// 提取文章ID
	articleClass, _ := s.Attr("class")
	postID := p.extractPostID(articleClass)
	if postID == "" {
		postID = fmt.Sprintf("unknown-%d", index)
	}

	// 提取标题和链接
	linkEl = s.Find(".post-title a")
	if linkEl.Length() == 0 {
		if p.debugMode {
			log.Printf("[DIDUAN] 跳过无标题链接的结果")
		}
		return nil
	}

	// 提取标题
	title := strings.TrimSpace(linkEl.Text())
	if title == "" {
		return nil
	}

	// 提取详情页链接
	detailURL, _ := linkEl.Attr("href")
	if detailURL == "" {
		if p.debugMode {
			log.Printf("[DIDUAN] 跳过无链接的结果: %s", title)
		}
		return nil
	}

	// 提取发布时间
	publishTime := p.extractPublishTime(s)

	// 提取分类
	category := p.extractCategory(s)

	// 提取简介
	content := p.extractContent(s)

	// 构建初始结果对象（详情页链接稍后获取）
	result := model.SearchResult{
		Title:     title,
		Content:   fmt.Sprintf("分类：%s\n%s", category, content),
		Channel:   "", // 插件搜索结果必须为空字符串（按开发指南要求）
		MessageID: fmt.Sprintf("%s-%s-%d", p.Name(), postID, index),
		UniqueID:  fmt.Sprintf("%s-%s-%d", p.Name(), postID, index),
		Datetime:  publishTime,
		Links:     []model.Link{}, // 先为空，详情页处理后添加
		Tags:      []string{category},
	}

	// 添加详情页URL到临时字段（用于后续处理）
	result.Content += fmt.Sprintf("\n详情页: %s", detailURL)

	if p.debugMode {
		log.Printf("[DIDUAN] 解析结果: %s (%s)", title, category)
	}

	return &result
}

// extractPostID 从文章class中提取文章ID
func (p *DiduanPlugin) extractPostID(articleClass string) string {
	// 匹配 post-{数字} 格式
	re := regexp.MustCompile(`post-(\d+)`)
	matches := re.FindStringSubmatch(articleClass)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// extractPublishTime 提取发布时间
func (p *DiduanPlugin) extractPublishTime(s *goquery.Selection) time.Time {
	timeEl := s.Find(".meta_date time.entry-date")
	if timeEl.Length() == 0 {
		return time.Now()
	}

	datetime, exists := timeEl.Attr("datetime")
	if !exists {
		return time.Now()
	}

	// 解析ISO 8601格式时间
	if t, err := time.Parse(time.RFC3339, datetime); err == nil {
		return t
	}

	return time.Now()
}

// extractCategory 提取分类
func (p *DiduanPlugin) extractCategory(s *goquery.Selection) string {
	categoryEl := s.Find(".meta_categories .cat-links a")
	if categoryEl.Length() > 0 {
		return strings.TrimSpace(categoryEl.Text())
	}
	return "未分类"
}

// extractContent 提取内容简介
func (p *DiduanPlugin) extractContent(s *goquery.Selection) string {
	contentEl := s.Find(".entry-content")
	if contentEl.Length() > 0 {
		content := strings.TrimSpace(contentEl.Text())
		// 限制长度
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		return content
	}
	return ""
}

// fetchDetailLinks 并发获取详情页链接
func (p *DiduanPlugin) fetchDetailLinks(searchResults []model.SearchResult, keyword string) []model.SearchResult {
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
					log.Printf("[DIDUAN] 跳过无详情页URL的结果: %s", r.Title)
				}
				return
			}

			// 获取详情页链接
			links := p.fetchDetailPageLinks(detailURL)
			if len(links) > 0 {
				r.Links = links
				// 清理Content中的详情页URL
				r.Content = p.cleanContent(r.Content)
				resultsChan <- r
			} else if p.debugMode {
				log.Printf("[DIDUAN] 详情页无有效链接: %s", r.Title)
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
func (p *DiduanPlugin) extractDetailURLFromContent(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "详情页: ") {
			return strings.TrimPrefix(line, "详情页: ")
		}
	}
	return ""
}

// cleanContent 清理Content，移除详情页URL行
func (p *DiduanPlugin) cleanContent(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "详情页: ") {
			cleanedLines = append(cleanedLines, line)
		}
	}
	return strings.Join(cleanedLines, "\n")
}

// fetchDetailPageLinks 获取详情页的网盘链接
func (p *DiduanPlugin) fetchDetailPageLinks(detailURL string) []model.Link {
	// 检查缓存
	if cached, found := p.detailCache.Load(detailURL); found {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[DIDUAN] 使用缓存的详情页链接: %s", detailURL)
			}
			return links
		}
	}

	resp, err := p.getPage(detailURL)
	if err != nil {
		if p.debugMode {
			log.Printf("[DIDUAN] 详情页请求失败: %v", err)
		}
		return []model.Link{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[DIDUAN] 详情页请求失败: %v", p.httpStatusError("详情页", resp))
		}
		return []model.Link{}
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[DIDUAN] 读取详情页响应失败: %v", err)
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
		log.Printf("[DIDUAN] 从详情页提取到 %d 个链接: %s", len(links), detailURL)
	}

	return links
}

// parseNetworkDiskLinks 解析网盘链接
func (p *DiduanPlugin) parseNetworkDiskLinks(htmlContent string) []model.Link {
	var links []model.Link
	seen := make(map[string]bool)
	appendLink := func(rawURL, password string) {
		rawURL = absoluteURL(rawURL)
		if rawURL == "" || seen[rawURL] {
			return
		}
		if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
			return
		}
		if password == "" {
			password = linkPassword(rawURL)
		}
		seen[rawURL] = true
		links = append(links, model.Link{Type: p.determineCloudType(rawURL), URL: rawURL, Password: password})
	}

	// 当前 ddys 详情页通过 JavaScript atob() 懒加载网盘地址。
	for _, match := range encodedURLRegex.FindAllStringSubmatch(htmlContent, -1) {
		if len(match) < 2 {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(match[1], "-", "+"))
		if err != nil {
			continue
		}
		decodedURL := strings.TrimSpace(string(decoded))
		if strings.Contains(decodedURL, "://") {
			appendLink(decodedURL, "")
		}
	}

	// 定义网盘链接匹配模式
	patterns := []struct {
		name    string
		pattern string
		urlType string
	}{
		{"夸克网盘", `\(夸克[^)]*\)[：:]\s*<a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`, "quark"},
		{"百度网盘", `\(百度[^)]*\)[：:]\s*<a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`, "baidu"},
		{"阿里云盘", `\(阿里[^)]*\)[：:]\s*<a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`, "aliyun"},
		{"天翼云盘", `\(天翼[^)]*\)[：:]\s*<a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`, "tianyi"},
		{"迅雷网盘", `\(迅雷[^)]*\)[：:]\s*<a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>([^<]+)</a>`, "xunlei"},
		// 通用模式
		{"通用网盘", `<a[^>]*href\s*=\s*["'](https?://[^"']*(?:pan|drive|cloud)[^"']*)["'][^>]*>([^<]+)</a>`, "others"},
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern.pattern)
		matches := re.FindAllStringSubmatch(htmlContent, -1)

		for _, match := range matches {
			if len(match) >= 3 {
				url := match[1]

				// 确定网盘类型
				urlType := p.determineCloudType(url)
				if urlType == "others" {
					urlType = pattern.urlType
				}

				// 提取可能的提取码
				password := p.extractPassword(htmlContent, url)
				if password == "" {
					password = linkPassword(url)
				}

				if !seen[url] {
					seen[url] = true
					links = append(links, model.Link{Type: urlType, URL: url, Password: password})
				}

				if p.debugMode {
					log.Printf("[DIDUAN] 找到链接: %s (%s)", url, urlType)
				}
			}
		}
	}

	return links
}

// extractPassword 提取网盘提取码
func (p *DiduanPlugin) extractPassword(content string, panURL string) string {
	// 常见提取码模式
	patterns := []string{
		`提取[码密][：:]?\s*([A-Za-z0-9]{4,8})`,
		`密码[：:]?\s*([A-Za-z0-9]{4,8})`,
		`[码密][：:]?\s*([A-Za-z0-9]{4,8})`,
		`([A-Za-z0-9]{4,8})\s*[是为]?提取[码密]`,
	}

	// 在网盘链接附近搜索提取码
	urlIndex := strings.Index(content, panURL)
	if urlIndex == -1 {
		return ""
	}

	// 搜索范围：链接前后200个字符
	start := urlIndex - 200
	if start < 0 {
		start = 0
	}
	end := urlIndex + len(panURL) + 200
	if end > len(content) {
		end = len(content)
	}

	searchArea := content[start:end]

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(searchArea)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// determineCloudType 根据URL自动识别网盘类型（按开发指南完整列表）
func (p *DiduanPlugin) determineCloudType(url string) string {
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
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "115.com"):
		return "115"
	case strings.Contains(url, "123pan.com"):
		return "123"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	case strings.Contains(url, "lanzou"):
		return "lanzou"
	default:
		return "others"
	}
}
