package xb6v

import (
	"compress/gzip"
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
	BaseURL        = "https://www.66ss.org"  // 主域名
	BackupURL      = "https://www.xb6v.com"  // 备用域名
	SearchPath     = "/e/search/11index.php" // 搜索端点（当前站点表单入口）
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxConcurrency = 50 // 详情页最大并发数
	MaxResults     = 50 // 最大搜索结果数
)

// Xb6vPlugin 6v电影插件
type Xb6vPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
	currentBase string // 当前使用的域名
}

// DetailPageInfo 详情页信息
type DetailPageInfo struct {
	URL      string    // 详情页URL
	DateTime time.Time // 发布日期
}

// NewXb6vPlugin 创建新的6v电影插件实例
func NewXb6vPlugin() *Xb6vPlugin {
	// 检查调试模式
	debugMode := false // 启用调试

	p := &Xb6vPlugin{
		// 磁力搜索插件：优先级4，跳过Service层过滤
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("xb6v", 3, true),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,
		currentBase:     BaseURL,
	}

	// 设置主缓存键
	p.BaseAsyncPlugin.SetMainCacheKey(p.Name())

	return p
}

// Name 返回插件名称
func (p *Xb6vPlugin) Name() string {
	return "xb6v"
}

// DisplayName 返回插件显示名称
func (p *Xb6vPlugin) DisplayName() string {
	return "6v电影"
}

// Description 返回插件描述
func (p *Xb6vPlugin) Description() string {
	return "6v电影 - 磁力链接资源站"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *Xb6vPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *Xb6vPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// setRequestHeaders 设置请求头
func (p *Xb6vPlugin) setRequestHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// doRequest 发送HTTP请求
func (p *Xb6vPlugin) doRequest(client *http.Client, method, url, postData string, referer string) (*http.Response, error) {
	var req *http.Request
	var err error

	if method == "POST" && postData != "" {
		req, err = http.NewRequest("POST", url, strings.NewReader(postData))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
	}

	p.setRequestHeaders(req, referer)

	if p.debugMode {
		log.Printf("[Xb6v] 发送 %s 请求: %s", method, url)
	}

	resp, err := client.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xb6v] 请求失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[Xb6v] 响应状态: %d", resp.StatusCode)
	}

	return resp, nil
}

// searchImpl 实际的搜索实现
func (p *Xb6vPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 先进行URL解码，处理%20等编码
	decodedKeyword, err := url.QueryUnescape(keyword)
	if err != nil {
		// 解码失败，使用原始关键词
		decodedKeyword = keyword
	}

	// 优化关键词：如果包含空格，只使用空格前的部分
	originalKeyword := decodedKeyword
	if spaceIndex := strings.Index(decodedKeyword, " "); spaceIndex > 0 {
		decodedKeyword = decodedKeyword[:spaceIndex]
		if p.debugMode {
			log.Printf("[Xb6v] 关键词优化: '%s' -> '%s'", originalKeyword, decodedKeyword)
		}
	}

	// 使用处理后的关键词
	keyword = decodedKeyword

	if p.debugMode {
		log.Printf("[Xb6v] 开始搜索: %s (原始: %s)", keyword, originalKeyword)
	}

	// 第一步：POST搜索请求
	searchURL := p.currentBase + SearchPath
	postData := fmt.Sprintf("show=title&tempid=1&tbname=article&mid=1&dopost=search&submit=&keyboard=%s", url.QueryEscape(keyword))

	// 创建不自动重定向的客户端
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := p.doRequest(noRedirectClient, "POST", searchURL, postData, p.currentBase)
	if err != nil {
		return nil, fmt.Errorf("搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if p.debugMode {
		log.Printf("[Xb6v] POST响应状态码: %d", resp.StatusCode)
	}

	// 获取重定向的location
	location := resp.Header.Get("Location")
	if p.debugMode {
		log.Printf("[Xb6v] Location头: '%s'", location)
	}

	// 如果没有Location头，可能需要从响应体中解析
	if location == "" {
		if p.debugMode {
			log.Printf("[Xb6v] 未找到Location头，尝试解析响应体")
		}

		// 读取响应体看看是否包含重定向信息
		bodyReader, err := p.getResponseReader(resp)
		if err != nil {
			return nil, fmt.Errorf("读取响应体失败: %w", err)
		}

		bodyBytes, err := io.ReadAll(bodyReader)
		if err != nil {
			return nil, fmt.Errorf("读取响应体失败: %w", err)
		}

		bodyStr := string(bodyBytes)
		if p.debugMode {
			log.Printf("[Xb6v] 响应体长度: %d", len(bodyStr))
			// 只打印前500个字符避免日志过长
			if len(bodyStr) > 500 {
				log.Printf("[Xb6v] 响应体前500字符: %s", bodyStr[:500])
			} else {
				log.Printf("[Xb6v] 响应体内容: %s", bodyStr)
			}
		}

		// 尝试从响应体中提取重定向URL
		// 可能是JavaScript重定向或meta refresh
		if strings.Contains(bodyStr, "location.href") || strings.Contains(bodyStr, "window.location") {
			// JavaScript重定向
			re := regexp.MustCompile(`location\.href\s*=\s*["']([^"']+)["']`)
			matches := re.FindStringSubmatch(bodyStr)
			if len(matches) > 1 {
				location = matches[1]
				if p.debugMode {
					log.Printf("[Xb6v] 从JavaScript中提取到Location: %s", location)
				}
			}
		}

		// 尝试查找其他形式的重定向
		if location == "" {
			// 查找可能的URL模式，比如包含searchid的链接
			re := regexp.MustCompile(`(?:href|url)\s*[=:]\s*["']?([^"'\s]*searchid=[^"'\s&]+)`)
			matches := re.FindAllStringSubmatch(bodyStr, -1)
			for _, match := range matches {
				if len(match) > 1 {
					location = match[1]
					if p.debugMode {
						log.Printf("[Xb6v] 从URL模式中提取到Location: %s", location)
					}
					break
				}
			}
		}

		// 如果还是没找到，尝试查找简单的result/?searchid=格式
		if location == "" {
			re := regexp.MustCompile(`result/\?searchid=\d+`)
			match := re.FindString(bodyStr)
			if match != "" {
				location = match
				if p.debugMode {
					log.Printf("[Xb6v] 从正则匹配中提取到Location: %s", location)
				}
			}
		}

		if location == "" {
			return nil, fmt.Errorf("未找到搜索结果页面重定向信息")
		}
	}

	// 构建完整的搜索结果URL
	// Location通常是类似 "result/?searchid=39616" 的格式，需要加上 /e/search/ 前缀
	var resultURL string
	if strings.HasPrefix(location, "result/") {
		resultURL = p.currentBase + "/e/search/" + location
	} else {
		resultURL = p.currentBase + "/" + strings.TrimPrefix(location, "/")
	}

	if p.debugMode {
		log.Printf("[Xb6v] 搜索结果页面: %s", resultURL)
	}

	// 第二步：获取搜索结果页面
	resp2, err := p.doRequest(client, "GET", resultURL, "", p.currentBase)
	if err != nil {
		return nil, fmt.Errorf("获取搜索结果失败: %w", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索结果响应状态码异常: %d", resp2.StatusCode)
	}

	// 解析搜索结果页面
	reader, err := p.getResponseReader(resp2)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("解析搜索结果HTML失败: %w", err)
	}

	// 提取搜索结果（详情页链接和日期）
	detailPages := p.extractDetailURLs(doc)

	if p.debugMode {
		log.Printf("[Xb6v] 找到 %d 个详情页链接", len(detailPages))
	}

	if len(detailPages) == 0 {
		return nil, fmt.Errorf("未找到搜索结果")
	}

	// 限制结果数量
	if len(detailPages) > MaxResults {
		detailPages = detailPages[:MaxResults]
	}

	// 并发获取详情页的磁力链接
	results := p.fetchMagnetLinksFromDetails(client, detailPages, keyword)

	// 过滤空结果
	validResults := p.filterValidResults(results)

	if p.debugMode {
		log.Printf("[Xb6v] 去除无链接结果后剩余 %d 个结果", len(validResults))
	}

	// 插件层关键词过滤（必须执行，因为跳过了Service层过滤）
	keywordFilteredResults := plugin.FilterResultsByKeyword(validResults, keyword)

	if p.debugMode {
		log.Printf("[Xb6v] 关键词过滤后最终返回 %d 个结果", len(keywordFilteredResults))
	}

	return keywordFilteredResults, nil
}

// getResponseReader 获取响应读取器（处理gzip压缩）
func (p *Xb6vPlugin) getResponseReader(resp *http.Response) (io.Reader, error) {
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if p.debugMode {
		log.Printf("[Xb6v] Content-Encoding: %s", contentEncoding)
	}

	// 如果是gzip压缩，手动解压
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip reader失败: %w", err)
		}
		reader = gzReader
	}

	return reader, nil
}

// extractDetailURLs 从搜索结果页面提取详情页链接和日期
func (p *Xb6vPlugin) extractDetailURLs(doc *goquery.Document) []DetailPageInfo {
	var detailPages []DetailPageInfo
	urlMap := make(map[string]bool) // 去重

	// 只从搜索结果区域提取链接，搜索结果在 ul#post_container 中
	doc.Find("ul#post_container li.post").Each(func(i int, li *goquery.Selection) {
		// 提取详情页链接
		linkEl := li.Find("a[href*='.html']")
		if linkEl.Length() == 0 {
			return
		}

		href, exists := linkEl.Attr("href")
		if !exists || href == "" {
			return
		}

		if p.debugMode {
			log.Printf("[Xb6v] 找到搜索结果链接: %s", href)
		}

		// 检查链接是否符合内容页面格式（分类/子分类/数字.html）
		if !p.isValidContentURL(href) {
			if p.debugMode {
				log.Printf("[Xb6v] 链接格式无效，跳过: %s", href)
			}
			return
		}

		// 构建完整URL
		var fullURL string
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			fullURL = href
		} else {
			fullURL = p.currentBase + "/" + strings.TrimPrefix(href, "/")
		}

		// 去重检查
		if urlMap[fullURL] {
			return
		}

		// 提取发布日期
		dateText := strings.TrimSpace(li.Find(".info .info_date").Text())
		var publishDate time.Time

		if dateText != "" {
			// 解析日期，格式通常是 "2025-08-17"
			if parsedDate, err := time.Parse("2006-01-02", dateText); err == nil {
				publishDate = parsedDate
			} else {
				if p.debugMode {
					log.Printf("[Xb6v] 日期解析失败: %s, 使用当前时间", dateText)
				}
				publishDate = time.Now()
			}
		} else {
			if p.debugMode {
				log.Printf("[Xb6v] 未找到日期信息，使用当前时间")
			}
			publishDate = time.Now()
		}

		urlMap[fullURL] = true
		detailPages = append(detailPages, DetailPageInfo{
			URL:      fullURL,
			DateTime: publishDate,
		})

		if p.debugMode {
			log.Printf("[Xb6v] 添加有效链接: %s, 日期: %s", fullURL, publishDate.Format("2006-01-02"))
		}
	})

	if p.debugMode {
		log.Printf("[Xb6v] 提取到 %d 个有效详情页链接", len(detailPages))
	}

	return detailPages
}

// isInSidebar 检查元素是否在侧边栏或不相关区域
func (p *Xb6vPlugin) isInSidebar(s *goquery.Selection) bool {
	// 检查父元素是否包含侧边栏相关的class
	parent := s.Parent()
	for i := 0; i < 5 && parent.Length() > 0; i++ { // 向上查找5层
		class, _ := parent.Attr("class")
		if strings.Contains(class, "widget") ||
			strings.Contains(class, "sidebar") ||
			strings.Contains(class, "box row") ||
			strings.Contains(class, "related") ||
			strings.Contains(class, "tagcloud") {
			return true
		}
		parent = parent.Parent()
	}
	return false
}

// isValidContentURL 检查是否是有效的内容页面URL
func (p *Xb6vPlugin) isValidContentURL(href string) bool {
	// 内容页面URL格式通常是：/分类/子分类/数字.html
	// 例如：/donghuapian/26525.html 或 /dianshiju/guoju/26608.html
	parts := strings.Split(strings.Trim(href, "/"), "/")
	if len(parts) < 2 {
		return false
	}

	// 最后一部分应该是数字.html格式
	lastPart := parts[len(parts)-1]
	if !strings.HasSuffix(lastPart, ".html") {
		return false
	}

	// 提取数字部分
	nameWithoutExt := strings.TrimSuffix(lastPart, ".html")
	if len(nameWithoutExt) == 0 {
		return false
	}

	// 检查是否包含数字（内容ID）
	hasNumber := regexp.MustCompile(`\d+`).MatchString(nameWithoutExt)
	return hasNumber
}

// cleanTitle 清理标题，移除网站名称等不需要的部分
func (p *Xb6vPlugin) cleanTitle(title string) string {
	// 移除常见的网站名称前缀/后缀
	cleaners := []string{
		"6v电影-新版",
		"6v电影",
		"新版6v",
		"新版6V",
		"6V电影",
	}

	cleaned := title
	for _, cleaner := range cleaners {
		// 移除前缀（包括可能的空格）
		if strings.HasPrefix(cleaned, cleaner) {
			cleaned = strings.TrimLeft(cleaned[len(cleaner):], " \t　") // 包括中文空格
		}

		// 移除后缀（包括可能的空格）
		if strings.HasSuffix(cleaned, cleaner) {
			cleaned = strings.TrimRight(cleaned[:len(cleaned)-len(cleaner)], " \t　") // 包括中文空格
		}

		// 移除中间的网站名称（用分隔符分隔）
		parts := strings.Split(cleaned, cleaner)
		if len(parts) > 1 {
			var validParts []string
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part != "" {
					validParts = append(validParts, part)
				}
			}
			if len(validParts) > 0 {
				cleaned = strings.Join(validParts, " ")
			}
		}
	}

	// 清理多余的空格和特殊字符
	cleaned = strings.TrimSpace(cleaned)
	// 移除多个连续空格
	re := regexp.MustCompile(`\s+`)
	cleaned = re.ReplaceAllString(cleaned, " ")

	if cleaned == "" {
		return "未知标题"
	}

	return cleaned
}

// fetchMagnetLinksFromDetails 并发从详情页获取磁力链接
func (p *Xb6vPlugin) fetchMagnetLinksFromDetails(client *http.Client, detailPages []DetailPageInfo, keyword string) []model.SearchResult {
	var results []model.SearchResult
	var mu sync.Mutex
	var wg sync.WaitGroup

	// 使用信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)

	for i, detailPage := range detailPages {
		wg.Add(1)
		go func(idx int, pageInfo DetailPageInfo) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加延迟避免请求过频
			time.Sleep(time.Duration(idx*100) * time.Millisecond)

			pageResults := p.fetchDetailPageMagnetLinks(client, pageInfo.URL, pageInfo.DateTime)
			if len(pageResults) > 0 {
				mu.Lock()
				results = append(results, pageResults...)
				mu.Unlock()
			}

			if p.debugMode {
				log.Printf("[Xb6v] 详情页 %d/%d 处理完成，获取到 %d 个结果 (日期: %s)",
					idx+1, len(detailPages), len(pageResults), pageInfo.DateTime.Format("2006-01-02"))
			}
		}(i, detailPage)
	}

	wg.Wait()
	return results
}

// fetchDetailPageMagnetLinks 获取单个详情页的磁力链接
func (p *Xb6vPlugin) fetchDetailPageMagnetLinks(client *http.Client, detailURL string, publishDate time.Time) []model.SearchResult {
	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if results, ok := cached.([]model.SearchResult); ok {
			if p.debugMode {
				log.Printf("[Xb6v] 使用缓存的详情页结果: %s", detailURL)
			}
			return results
		}
	}

	// 请求详情页
	resp, err := p.doRequest(client, "GET", detailURL, "", p.currentBase)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xb6v] 获取详情页失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Xb6v] 详情页响应状态码异常: %d", resp.StatusCode)
		}
		return nil
	}

	// 解析HTML
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xb6v] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	// 提取页面信息
	title := strings.TrimSpace(doc.Find("h1").Text())
	if title == "" {
		title = "未知标题"
	}

	// 清理title，移除网站名称
	title = p.cleanTitle(title)

	// 提取分类信息
	category := strings.TrimSpace(doc.Find(".info_category a").Text())

	// 提取磁力链接
	magnetLinks, linkInfos := p.extractMagnetLinks(doc, title)

	if len(magnetLinks) == 0 {
		if p.debugMode {
			log.Printf("[Xb6v] 详情页无磁力链接: %s", detailURL)
		}
		return nil
	}

	// 生成多个SearchResult，每个磁力链接一个
	var results []model.SearchResult
	for i, linkInfo := range linkInfos {
		// 生成唯一的资源ID
		resourceID := fmt.Sprintf("%s-%d", p.extractResourceID(detailURL), i)

		// 构建"主标题-子标题"格式的标题
		resultTitle := fmt.Sprintf("%s-%s", title, linkInfo.SubTitle)

		result := model.SearchResult{
			Title:     resultTitle,
			Content:   fmt.Sprintf("分类：%s\n磁力链接：%s", category, linkInfo.SubTitle),
			Channel:   "", // 插件搜索结果必须为空字符串
			MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
			Datetime:  publishDate,                  // 使用从搜索结果页面提取的真实发布日期
			Links:     []model.Link{magnetLinks[i]}, // 每个结果只包含一个链接
			Tags:      []string{category},
		}

		results = append(results, result)
	}

	// 缓存所有结果（使用主标题作为键）
	p.detailCache.Store(detailURL, results)

	// 设置缓存过期
	go func() {
		time.Sleep(p.cacheTTL)
		p.detailCache.Delete(detailURL)
	}()

	if p.debugMode {
		log.Printf("[Xb6v] 提取到磁力链接: %s, 链接数: %d", title, len(magnetLinks))
	}

	return results
}

// MagnetLinkInfo 磁力链接信息（包含标题）
type MagnetLinkInfo struct {
	URL      string
	SubTitle string
}

// extractMagnetLinks 从详情页提取磁力链接
func (p *Xb6vPlugin) extractMagnetLinks(doc *goquery.Document, mainTitle string) ([]model.Link, []MagnetLinkInfo) {
	var links []model.Link
	var linkInfos []MagnetLinkInfo
	linkMap := make(map[string]bool) // 去重

	// 查找包含"磁力："的表格单元格
	doc.Find("td").Each(func(i int, s *goquery.Selection) {
		text := s.Text()
		if strings.Contains(text, "磁力：") {
			// 查找该单元格中的磁力链接
			s.Find("a[href^='magnet:']").Each(func(j int, a *goquery.Selection) {
				href, exists := a.Attr("href")
				if !exists || href == "" {
					return
				}

				// 去重
				if linkMap[href] {
					return
				}
				linkMap[href] = true

				// 获取链接子标题
				subTitle := strings.TrimSpace(a.Text())
				if subTitle == "" {
					subTitle = "磁力链接"
				}

				links = append(links, model.Link{
					URL:  href,
					Type: "magnet",
				})

				linkInfos = append(linkInfos, MagnetLinkInfo{
					URL:      href,
					SubTitle: subTitle,
				})

				if p.debugMode {
					log.Printf("[Xb6v] 提取磁力链接: %s - %s", mainTitle, subTitle)
				}
			})
		}
	})

	// 如果没有在表格中找到，尝试在整个页面查找
	if len(links) == 0 {
		doc.Find("a[href^='magnet:']").Each(func(i int, s *goquery.Selection) {
			href, exists := s.Attr("href")
			if !exists || href == "" {
				return
			}

			// 去重
			if linkMap[href] {
				return
			}
			linkMap[href] = true

			subTitle := strings.TrimSpace(s.Text())
			if subTitle == "" {
				subTitle = "磁力链接"
			}

			links = append(links, model.Link{
				URL:  href,
				Type: "magnet",
			})

			linkInfos = append(linkInfos, MagnetLinkInfo{
				URL:      href,
				SubTitle: subTitle,
			})

			if p.debugMode {
				log.Printf("[Xb6v] 提取磁力链接: %s - %s", mainTitle, subTitle)
			}
		})
	}

	return links, linkInfos
}

// extractResourceID 从详情页URL提取资源ID
func (p *Xb6vPlugin) extractResourceID(detailURL string) string {
	// 从URL中提取ID，如：/dianshiju/guoju/26608.html -> 26608
	re := regexp.MustCompile(`/(\d+)\.html`)
	matches := re.FindStringSubmatch(detailURL)
	if len(matches) > 1 {
		return matches[1]
	}

	// 如果提取失败，使用时间戳
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// filterValidResults 过滤有效结果（去掉没有磁力链接的）
func (p *Xb6vPlugin) filterValidResults(results []model.SearchResult) []model.SearchResult {
	var validResults []model.SearchResult

	for _, result := range results {
		if len(result.Links) > 0 {
			validResults = append(validResults, result)
		} else if p.debugMode {
			log.Printf("[Xb6v] 忽略无磁力链接结果: %s", result.Title)
		}
	}

	return validResults
}

func init() {
	plugin.RegisterGlobalPlugin(NewXb6vPlugin())
}
