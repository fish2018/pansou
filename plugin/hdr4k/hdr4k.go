package hdr4k

import (
	"fmt"
	"math/rand"
	"net"
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

// 缓存相关变量
var (
	// 详情页缓存
	detailPageCache = sync.Map{}

	// 搜索结果缓存
	searchResultCache = sync.Map{}

	// 链接类型判断缓存
	linkTypeCache = sync.Map{}

	// 最后一次清理缓存的时间
	lastCacheCleanTime = time.Now()

	// 缓存有效期
	cacheTTL = 1 * time.Hour
)

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) Gecko/20100101 Firefox/120.0",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
}

// 缓存响应结构
type cachedResponse struct {
	data      interface{}
	timestamp time.Time
}

// 初始化插件
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewHdr4kAsyncPlugin())

	// 启动缓存清理
	go startCacheCleaner()

	// 初始化随机数种子
	rand.Seed(time.Now().UnixNano())
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	// 每小时清理一次缓存
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		// 清空所有缓存
		detailPageCache = sync.Map{}
		searchResultCache = sync.Map{}
		linkTypeCache = sync.Map{}
		lastCacheCleanTime = time.Now()
	}
}

// getRandomUA 获取随机UA
func getRandomUA() string {
	return userAgents[rand.Intn(len(userAgents))]
}

const (
	// 搜索API
	SearchURL = "https://www.4khdr.cn/search.php?mod=forum"
	// 详情页URL模式
	ThreadURLPattern = "https://www.4khdr.cn/thread-%s-1-1.html"
	// 默认超时时间
	DefaultTimeout = 10 * time.Second
	// 最大重试次数
	MaxRetries = 2
	// 最大并发数
	MaxConcurrency = 20
)

// Hdr4kAsyncPlugin 4KHDR网站搜索异步插件
type Hdr4kAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewHdr4kAsyncPlugin 创建新的4KHDR搜索异步插件
func NewHdr4kAsyncPlugin() *Hdr4kAsyncPlugin {
	return &Hdr4kAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("hdr4k", 1), // 高优先级
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *Hdr4kAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *Hdr4kAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *Hdr4kAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 处理ext参数
	searchKeyword := keyword
	if ext != nil {
		// 使用类型断言安全地获取参数
		if titleEn, ok := ext["title_en"].(string); ok && titleEn != "" {
			// 使用英文标题替换关键词
			searchKeyword = titleEn
		}
	}

	// 构建POST请求数据
	data := url.Values{}
	data.Set("srchtxt", searchKeyword)
	data.Set("searchsubmit", "yes")

	// 发送POST请求
	req, err := http.NewRequest("POST", SearchURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", getRandomUA())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://www.4khdr.cn/")

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	var wg sync.WaitGroup
	resultChan := make(chan model.SearchResult, 20)
	errorChan := make(chan error, 20)

	// 创建信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)

	// 预先收集所有需要处理的项
	var items []*goquery.Selection

	// 将关键词转为小写，用于不区分大小写的比较
	lowerKeyword := strings.ToLower(keyword)

	// 将关键词按空格分割，用于支持多关键词搜索
	keywords := strings.Fields(lowerKeyword)

	// 预先过滤不包含关键词的帖子
	doc.Find(".slst.mtw ul li.pbw").Each(func(i int, s *goquery.Selection) {
		// 提取帖子ID
		postID, exists := s.Attr("id")
		if !exists || postID == "" {
			return
		}

		// 提取标题
		titleElement := s.Find("h3.xs3 a")
		title := p.cleanHTML(titleElement.Text())
		title = strings.TrimSpace(title)
		lowerTitle := strings.ToLower(title)

		if title == "" {
			return
		}

		// 提取内容描述
		contentElement := s.Find("p").First()
		content := p.cleanHTML(contentElement.Text())
		content = strings.TrimSpace(content)
		lowerContent := strings.ToLower(content)

		// 检查每个关键词是否在标题或内容中
		matched := true
		for _, kw := range keywords {
			// 对于所有关键词，检查是否在标题或内容中
			if !strings.Contains(lowerTitle, kw) && !strings.Contains(lowerContent, kw) {
				matched = false
				break
			}
		}

		// 只添加匹配的帖子
		if matched {
			items = append(items, s)
		}
	})

	// 并发处理每个搜索结果项
	for i, s := range items {
		wg.Add(1)

		go func(index int, s *goquery.Selection) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 提取帖子ID
			postID, exists := s.Attr("id")
			if !exists || postID == "" {
				errorChan <- fmt.Errorf("无法提取帖子ID: index=%d", index)
				return
			}

			// 提取标题
			titleElement := s.Find("h3.xs3 a")
			title := p.cleanHTML(titleElement.Text())
			title = strings.TrimSpace(title)

			// 提取内容描述
			contentElement := s.Find("p").First()
			content := p.cleanHTML(contentElement.Text())
			content = strings.TrimSpace(content)

			// 提取日期时间
			var datetime time.Time
			dateElements := s.Find("p span")
			if dateElements.Length() > 0 {
				dateStr := strings.TrimSpace(dateElements.First().Text())
				if dateStr != "" {
					parsedTime, err := p.parseDateTime(dateStr)
					if err == nil {
						datetime = parsedTime
					}
				}
			}

			// 提取分类标签
			var tags []string
			categoryElement := s.Find("p span a.xi1")
			if categoryElement.Length() > 0 {
				category := strings.TrimSpace(categoryElement.Text())
				if category != "" {
					tags = append(tags, category)
				}
			}

			// 获取详情页链接，并尝试获取下载链接
			links, detailContent, err := p.getLinksFromDetail(client, postID)
			if err != nil {
				// 如果获取链接失败，仍然返回结果，但没有链接
				links = []model.Link{}
			}

			// 如果从详情页获取到了更详细的内容，使用详情页的内容
			if detailContent != "" {
				content = detailContent
			}

			// 检查是否是无意义的求片帖（没有实际资源的求片帖）
			if p.isEmptyRequestPost(title, links) {
				return
			}

			// 创建搜索结果
			result := model.SearchResult{
				UniqueID: fmt.Sprintf("hdr4k-%s", postID),
				Title:    title,
				Content:  content,
				Datetime: datetime,
				Links:    links,
				Tags:     tags,
			}

			resultChan <- result
		}(i, s)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultChan)
		close(errorChan)
	}()

	// 收集结果
	var results []model.SearchResult
	for result := range resultChan {
		results = append(results, result)
	}

	// 由于我们已经在前面过滤了不匹配的帖子，这里不需要再次过滤
	return results, nil
}

// isEmptyRequestPost 判断是否是没有实际资源的求片帖子
func (p *Hdr4kAsyncPlugin) isEmptyRequestPost(title string, links []model.Link) bool {
	lowerTitle := strings.ToLower(title)

	// 如果有实际的下载链接，不过滤
	if len(links) > 0 {
		return false
	}

	// 只过滤明确的无资源求片关键词
	emptyRequestKeywords := []string{
		"求片",
		"有资源吗",
		"有没有资源",
		"跪求",
		"求资源",
	}

	for _, keyword := range emptyRequestKeywords {
		if strings.Contains(lowerTitle, keyword) {
			return true
		}
	}

	// 对于求网盘的帖子，如果没有链接才过滤
	cloudRequestKeywords := []string{
		"求阿里云盘",
		"求百度网盘",
		"求夸克网盘",
		"求迅雷网盘",
		"求天翼云盘",
	}

	for _, keyword := range cloudRequestKeywords {
		if strings.Contains(lowerTitle, keyword) {
			// 只有当没有实际链接时才过滤
			return len(links) == 0
		}
	}

	// 检查是否以"求"开头，但要排除正常的电影名称
	if strings.HasPrefix(lowerTitle, "求") {
		// 如果标题很短且以"求"开头，且没有链接，很可能是求片帖
		if len([]rune(title)) < 10 && !strings.Contains(lowerTitle, "年") && !strings.Contains(lowerTitle, "季") && len(links) == 0 {
			return true
		}
	}

	return false
}

// getLinksFromDetail 从详情页获取下载链接（改进版，支持重试）
func (p *Hdr4kAsyncPlugin) getLinksFromDetail(client *http.Client, postID string) ([]model.Link, string, error) {
	// 生成缓存键
	cacheKey := fmt.Sprintf("detail:%s", postID)

	// 检查缓存中是否已有结果
	if cachedData, ok := detailPageCache.Load(cacheKey); ok {
		// 检查缓存是否过期
		cachedResult := cachedData.(cachedResponse)
		if time.Since(cachedResult.timestamp) < cacheTTL {
			data := cachedResult.data.(struct {
				Links   []model.Link
				Content string
			})
			return data.Links, data.Content, nil
		}
	}

	// 构建详情页URL
	detailURL := fmt.Sprintf(ThreadURLPattern, postID)

	// 发送GET请求获取详情页
	req, err := http.NewRequest("GET", detailURL, nil)
	if err != nil {
		return []model.Link{}, "", fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", getRandomUA())
	req.Header.Set("Referer", "https://www.4khdr.cn/")

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return []model.Link{}, "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return []model.Link{}, "", fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取详情页内容
	var links []model.Link
	var detailContent string

	// 查找帖子内容区域和回复区域
	contentSelectors := []string{
		".t_f",               // 主帖内容
		"[id^=postmessage_]", // 回复内容（以postmessage_开头的id）
	}

	for _, selector := range contentSelectors {
		doc.Find(selector).Each(func(i int, contentArea *goquery.Selection) {
			// 如果还没有提取到详细内容，提取剧情简介等
			if detailContent == "" {
				content := p.cleanHTML(contentArea.Text())
				content = strings.TrimSpace(content)

				// 提取前500个字符作为详细描述
				if len(content) > 500 {
					detailContent = content[:500] + "..."
				} else if len(content) > 50 {
					detailContent = content
				}
			}

			// 提取下载链接
			contentArea.Find("a").Each(func(j int, linkElement *goquery.Selection) {
				href, exists := linkElement.Attr("href")
				if !exists || href == "" {
					return
				}

				// 检查是否是网盘链接
				linkType := p.determineLinkType(href, "")
				if linkType != "others" {
					// 检查是否已经存在相同的链接
					exists := false
					for _, existingLink := range links {
						if existingLink.URL == href {
							exists = true
							break
						}
					}

					if !exists {
						link := model.Link{
							URL:      href,
							Type:     linkType,
							Password: "", // 4KHDR通常不提供密码
						}
						links = append(links, link)
					}
				}
			})
		})
	}

	// 缓存结果
	cacheData := struct {
		Links   []model.Link
		Content string
	}{
		Links:   links,
		Content: detailContent,
	}

	detailPageCache.Store(cacheKey, cachedResponse{
		data:      cacheData,
		timestamp: time.Now(),
	})

	return links, detailContent, nil
}

// determineLinkType 根据URL和名称确定链接类型（改进版）
func (p *Hdr4kAsyncPlugin) determineLinkType(url, name string) string {
	// 生成缓存键
	cacheKey := fmt.Sprintf("%s:%s", url, name)

	// 检查缓存
	if cachedType, ok := linkTypeCache.Load(cacheKey); ok {
		return cachedType.(string)
	}

	lowerURL := strings.ToLower(url)
	lowerName := strings.ToLower(name)

	var linkType string

	// 根据URL判断
	switch {
	case strings.Contains(lowerURL, "pan.quark.cn"):
		linkType = "quark"
	case strings.Contains(lowerURL, "pan.baidu.com"):
		linkType = "baidu"
	case strings.Contains(lowerURL, "alipan.com") || strings.Contains(lowerURL, "aliyundrive.com"):
		linkType = "aliyun"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		linkType = "xunlei"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		linkType = "tianyi"
	case strings.Contains(lowerURL, "115.com"):
		linkType = "115"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		linkType = "uc"
	case strings.Contains(lowerURL, "caiyun.139.com"):
		linkType = "mobile"
	case strings.Contains(lowerURL, "share.weiyun.com"):
		linkType = "weiyun"
	case strings.Contains(lowerURL, "lanzou"):
		linkType = "lanzou"
	case strings.Contains(lowerURL, "jianguoyun.com"):
		linkType = "jianguoyun"
	case strings.Contains(lowerURL, "123pan.com"):
		linkType = "123"
	case strings.Contains(lowerURL, "mypikpak.com"):
		linkType = "pikpak"
	case strings.HasPrefix(lowerURL, "magnet:"):
		linkType = "magnet"
	case strings.HasPrefix(lowerURL, "ed2k:"):
		linkType = "ed2k"
	default:
		// 根据名称判断
		switch {
		case strings.Contains(lowerName, "百度"):
			linkType = "baidu"
		case strings.Contains(lowerName, "阿里"):
			linkType = "aliyun"
		case strings.Contains(lowerName, "迅雷"):
			linkType = "xunlei"
		case strings.Contains(lowerName, "夸克"):
			linkType = "quark"
		case strings.Contains(lowerName, "天翼"):
			linkType = "tianyi"
		case strings.Contains(lowerName, "115"):
			linkType = "115"
		case strings.Contains(lowerName, "uc"):
			linkType = "uc"
		case strings.Contains(lowerName, "移动") || strings.Contains(lowerName, "彩云"):
			linkType = "mobile"
		case strings.Contains(lowerName, "微云"):
			linkType = "weiyun"
		case strings.Contains(lowerName, "蓝奏"):
			linkType = "lanzou"
		case strings.Contains(lowerName, "坚果"):
			linkType = "jianguoyun"
		case strings.Contains(lowerName, "123"):
			linkType = "123"
		case strings.Contains(lowerName, "pikpak"):
			linkType = "pikpak"
		default:
			linkType = "others"
		}
	}

	// 缓存结果
	linkTypeCache.Store(cacheKey, linkType)

	return linkType
}

// doRequestWithRetry 发送HTTP请求并支持重试
func (p *Hdr4kAsyncPlugin) doRequestWithRetry(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i <= maxRetries; i++ {
		// 如果不是第一次尝试，等待一段时间
		if i > 0 {
			// 指数退避算法
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			time.Sleep(backoff)
		}

		// 克隆请求，避免重用同一个请求对象
		reqClone := req.Clone(req.Context())

		// 发送请求
		resp, err = client.Do(reqClone)

		// 如果请求成功或者是不可重试的错误，则退出循环
		if err == nil || !p.isRetriableError(err) {
			break
		}
	}

	return resp, err
}

// isRetriableError 判断错误是否可以重试
func (p *Hdr4kAsyncPlugin) isRetriableError(err error) bool {
	if err == nil {
		return false
	}

	// 判断是否是网络错误或超时错误
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout() || netErr.Temporary()
	}

	// 其他可能需要重试的错误类型
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "EOF")
}

// parseDateTime 解析日期时间字符串
func (p *Hdr4kAsyncPlugin) parseDateTime(dateStr string) (time.Time, error) {
	// 4KHDR的时间格式：2025-4-9 19:55
	layouts := []string{
		"2006-1-2 15:04",
		"2006-01-02 15:04:05",
		"2006-1-2 15:04:05",
		"2006-01-02 15:04",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, dateStr); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("无法解析日期时间: %s", dateStr)
}

// cleanHTML 清理HTML标签和特殊字符
func (p *Hdr4kAsyncPlugin) cleanHTML(html string) string {
	// 替换常见HTML标签和实体
	replacements := map[string]string{
		"<strong>":                 "",
		"</strong>":                "",
		"<font color=\"#ff0000\">": "",
		"</font>":                  "",
		"<em>":                     "",
		"</em>":                    "",
		"<b>":                      "",
		"</b>":                     "",
		"<br>":                     "\n",
		"<br/>":                    "\n",
		"<br />":                   "\n",
		"&nbsp;":                   " ",
		"&hellip;":                 "...",
		"&amp;":                    "&",
		"&lt;":                     "<",
		"&gt;":                     ">",
		"&quot;":                   "\"",
		"&#039;":                   "'",
	}

	result := html
	for old, new := range replacements {
		result = strings.ReplaceAll(result, old, new)
	}

	// 移除其他HTML标签（简单的正则表达式）
	re := hdr4kRe1
	result = re.ReplaceAllString(result, "")

	// 清理多余的空白字符
	re = hdr4kRe2
	result = re.ReplaceAllString(result, " ")

	return strings.TrimSpace(result)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	hdr4kRe1 = regexp.MustCompile(`<[^>]*>`)
	hdr4kRe2 = regexp.MustCompile(`\s+`)
)
