package dy4k

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/proxy"
	"pansou/model"
	"pansou/plugin"
)

// 常量定义
const (
	// 基础URL
	BaseURL = "https://4kdy.vip"

	// 搜索URL格式
	SearchURL = BaseURL + "/4K-search/-------------.html?wd=%s"

	// 分页搜索URL格式
	SearchPageURL = BaseURL + "/4K-search/-------------.html?wd=%s&page=%d"

	// 详情页URL格式
	DetailURL = BaseURL + "/4K-detail/%s.html"

	// 默认超时时间 - 增加超时时间避免网络慢的问题
	DefaultTimeout = 15 * time.Second

	// 代理配置
	DefaultHTTPProxy   = "http://154.219.110.34:51422"
	DefaultSocks5Proxy = "socks5://154.219.110.34:51423"

	// 调试开关 - 默认关闭
	DebugMode = false

	// 代理开关 - 默认关闭
	ProxyEnabled = false

	// 详情页响应较大，限制并发避免对源站和本机造成突发压力。
	MaxConcurrency = 6

	// 最大分页数（避免无限请求）
	MaxPages = 10

	// HTTP连接池配置
	MaxIdleConns        = 200
	MaxIdleConnsPerHost = 50
	MaxConnsPerHost     = 100
	IdleConnTimeout     = 90 * time.Second
)

// 预编译正则表达式
var (
	// 从详情页URL中提取ID的正则表达式
	detailIDRegex = regexp.MustCompile(`/4K-detail/(\d+)\.html`)

	// 磁力链接的正则表达式
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}[^"'\s]*`)

	// 电驴链接的正则表达式
	ed2kLinkRegex = regexp.MustCompile(`ed2k://\|file\|[^|]+\|[^|]+\|[^|]+\|/?`)

	// 年份提取正则表达式
	yearRegex = regexp.MustCompile(`(\d{4})`)

	// 网盘链接正则表达式
	panLinkRegexes = map[string]*regexp.Regexp{
		"quark":  regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z_-]+(?:\?[^"'\s]*)?`),
		"baidu":  regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_-]+(?:\?pwd=[0-9a-zA-Z]+)?(?:&v=\d+)?`),
		"aliyun": regexp.MustCompile(`https?://(?:www\.)?alipan\.com/s/[0-9a-zA-Z_-]+`),
		"tianyi": regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z_-]+(?:\([^)]*\))?`),
		"uc":     regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-fA-F]+(?:\?[^"\s]*)?`),
		"mobile": regexp.MustCompile(`https?://caiyun\.139\.com/[^"\s]+`),
		"115":    regexp.MustCompile(`https?://115\.com/s/[0-9a-zA-Z_-]+`),
		"pikpak": regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z_-]+`),
		"xunlei": regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_-]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		"123":    regexp.MustCompile(`https?://(?:www\.)?123pan\.com/s/[0-9a-zA-Z_-]+`),
	}

	// 密码提取正则表达式
	passwordRegexes = []*regexp.Regexp{
		regexp.MustCompile(`\?pwd=([0-9a-zA-Z]+)`),       // URL中的pwd参数
		regexp.MustCompile(`提取码[：:]\s*([0-9a-zA-Z]+)`),   // 提取码：xxxx
		regexp.MustCompile(`访问码[：:]\s*([0-9a-zA-Z]+)`),   // 访问码：xxxx
		regexp.MustCompile(`密码[：:]\s*([0-9a-zA-Z]+)`),    // 密码：xxxx
		regexp.MustCompile(`（访问码[：:]\s*([0-9a-zA-Z]+)）`), // （访问码：xxxx）
	}

	// 缓存相关
	detailCache     = sync.Map{} // 缓存详情页解析结果
	lastCleanupTime = time.Now()
	cacheTTL        = 1 * time.Hour // 缩短缓存时间

	// 性能统计（原子操作）
	searchRequests     int64 = 0
	detailPageRequests int64 = 0
	cacheHits          int64 = 0
	cacheMisses        int64 = 0
	totalSearchTime    int64 = 0 // 纳秒
	totalDetailTime    int64 = 0 // 纳秒
)

// 缓存的详情页响应
type detailPageResponse struct {
	Title     string
	ImageURL  string
	Downloads []model.Link
	Tags      []string
	Content   string
	Timestamp time.Time
}

// Dy4kPlugin 4KDY搜索插件
type Dy4kPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

// createProxyTransport 创建支持代理的传输层
func createProxyTransport(proxyURL string) (*http.Transport, error) {
	transport := &http.Transport{
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		MaxConnsPerHost:     MaxConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		DisableKeepAlives:   false,
		DisableCompression:  false,
		WriteBufferSize:     16 * 1024,
		ReadBufferSize:      16 * 1024,
	}

	if proxyURL == "" {
		return transport, nil
	}

	if strings.HasPrefix(proxyURL, "socks5://") {
		// SOCKS5代理
		dialer, err := proxy.SOCKS5("tcp", strings.TrimPrefix(proxyURL, "socks5://"), nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("创建SOCKS5代理失败: %w", err)
		}
		transport.Dial = dialer.Dial
		debugPrintf("🔧 [Dy4k DEBUG] 使用SOCKS5代理: %s\n", proxyURL)
	} else {
		// HTTP代理
		parsedURL, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("解析代理URL失败: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsedURL)
		debugPrintf("🔧 [Dy4k DEBUG] 使用HTTP代理: %s\n", proxyURL)
	}

	return transport, nil
}

// createOptimizedHTTPClient 创建优化的HTTP客户端（支持代理）
func createOptimizedHTTPClient() *http.Client {
	var selectedProxy string

	if ProxyEnabled {
		// 随机选择代理类型
		proxyTypes := []string{"", DefaultHTTPProxy, DefaultSocks5Proxy}
		selectedProxy = proxyTypes[rand.Intn(len(proxyTypes))]
	} else {
		// 代理未启用，使用直连
		selectedProxy = ""
		debugPrintf("🔧 [Dy4k DEBUG] 代理功能已禁用，使用直连模式\n")
	}

	transport, err := createProxyTransport(selectedProxy)
	if err != nil {
		debugPrintf("❌ [Dy4k DEBUG] 创建代理传输层失败: %v，使用直连\n", err)
		transport, _ = createProxyTransport("")
	}

	if selectedProxy == "" && ProxyEnabled {
		debugPrintf("🔧 [Dy4k DEBUG] 使用直连模式\n")
	}

	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
	}
}

// NewDy4kPlugin 创建新的4KDY搜索异步插件
func NewDy4kPlugin() *Dy4kPlugin {
	return &Dy4kPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("dy4k", 3),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// debugPrintf 调试输出函数
func debugPrintf(format string, args ...interface{}) {
	if DebugMode {
		fmt.Printf(format, args...)
	}
}

// 初始化插件
func init() {
	plugin.RegisterGlobalPlugin(NewDy4kPlugin())

	// 启动缓存清理
	go startCacheCleaner()
}

// startCacheCleaner 定期清理缓存
func startCacheCleaner() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		// 清空详情页缓存
		detailCache = sync.Map{}
		lastCleanupTime = time.Now()
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *Dy4kPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *Dy4kPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	debugPrintf("🔧 [Dy4k DEBUG] SearchWithResult 开始 - keyword: %s, MainCacheKey: '%s'\n", keyword, p.MainCacheKey)

	result, err := p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)

	debugPrintf("🔧 [Dy4k DEBUG] SearchWithResult 完成 - 结果数: %d, IsFinal: %v, 错误: %v\n",
		len(result.Results), result.IsFinal, err)

	if len(result.Results) > 0 {
		debugPrintf("🔧 [Dy4k DEBUG] 前3个结果示例:\n")
		for i, r := range result.Results {
			if i >= 3 {
				break
			}
			debugPrintf("  %d. 标题: %s, 链接数: %d\n", i+1, r.Title, len(r.Links))
		}
	}

	return result, err
}

// searchImpl 实现具体的搜索逻辑（支持分页）
func (p *Dy4kPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	debugPrintf("🔧 [Dy4k DEBUG] searchImpl 开始执行 - keyword: %s\n", keyword)
	startTime := time.Now()
	atomic.AddInt64(&searchRequests, 1)

	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	encodedKeyword := url.QueryEscape(keyword)
	allResults := make([]model.SearchResult, 0)

	// 1. 搜索第一页，获取总页数
	firstPageResults, totalPages, err := p.searchPage(client, encodedKeyword, 1)
	if err != nil {
		return nil, err
	}
	allResults = append(allResults, firstPageResults...)

	// 2. 如果有多页，继续搜索其他页面（限制最大页数）
	maxPagesToSearch := totalPages
	if maxPagesToSearch > MaxPages {
		maxPagesToSearch = MaxPages
	}

	if totalPages > 1 && maxPagesToSearch > 1 {
		// 并发搜索其他页面
		var wg sync.WaitGroup
		var mu sync.Mutex
		results := make([][]model.SearchResult, maxPagesToSearch-1)

		for page := 2; page <= maxPagesToSearch; page++ {
			wg.Add(1)
			go func(pageNum int) {
				defer wg.Done()
				pageResults, _, err := p.searchPage(client, encodedKeyword, pageNum)
				if err == nil {
					mu.Lock()
					results[pageNum-2] = pageResults
					mu.Unlock()
				}
			}(page)
		}

		wg.Wait()

		// 合并所有页面的结果
		for _, pageResults := range results {
			allResults = append(allResults, pageResults...)
		}
	}

	// 3. 并发获取详情页信息
	allResults = p.enrichWithDetailInfo(allResults, client)

	// 4. 过滤关键词匹配的结果
	results := plugin.FilterResultsByKeyword(allResults, keyword)

	// 记录性能统计
	searchDuration := time.Since(startTime)
	atomic.AddInt64(&totalSearchTime, int64(searchDuration))

	debugPrintf("🔧 [Dy4k DEBUG] searchImpl 完成 - 原始结果: %d, 过滤后结果: %d, 耗时: %v\n",
		len(allResults), len(results), searchDuration)

	return results, nil
}

// searchPage 搜索指定页面
func (p *Dy4kPlugin) searchPage(client *http.Client, encodedKeyword string, page int) ([]model.SearchResult, int, error) {
	debugPrintf("🔧 [Dy4k DEBUG] searchPage 开始 - 第%d页, keyword: %s\n", page, encodedKeyword)

	// 1. 构建搜索URL
	var searchURL string
	if page == 1 {
		searchURL = fmt.Sprintf(SearchURL, encodedKeyword)
	} else {
		searchURL = fmt.Sprintf(SearchPageURL, encodedKeyword, page)
	}

	debugPrintf("🔧 [Dy4k DEBUG] 构建的URL: %s\n", searchURL)

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置完整的请求头（包含随机UA和IP）
	randomUA := getRandomUA()
	randomIP := generateRandomIP()

	req.Header.Set("User-Agent", randomUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("X-Forwarded-For", randomIP)
	req.Header.Set("X-Real-IP", randomIP)
	req.Header.Set("sec-ch-ua-platform", "macOS")

	debugPrintf("🔧 [Dy4k DEBUG] 使用随机UA: %s\n", randomUA)
	debugPrintf("🔧 [Dy4k DEBUG] 使用随机IP: %s\n", randomIP)

	// 5. 发送HTTP请求
	debugPrintf("🔧 [Dy4k DEBUG] 开始发送HTTP请求到: %s\n", searchURL)
	debugPrintf("🔧 [Dy4k DEBUG] 请求头信息:\n")
	if DebugMode {
		for key, values := range req.Header {
			for _, value := range values {
				debugPrintf("    %s: %s\n", key, value)
			}
		}
	}

	startTime := time.Now()
	resp, err := p.doRequestWithRetry(req, client)
	requestDuration := time.Since(startTime)

	if err != nil {
		debugPrintf("❌ [Dy4k DEBUG] HTTP请求失败 (耗时: %v): %v\n", requestDuration, err)
		debugPrintf("❌ [Dy4k DEBUG] 错误类型分析:\n")
		if netErr, ok := err.(*url.Error); ok {
			fmt.Printf("    URL错误: %v\n", netErr.Err)
			if netErr.Timeout() {
				fmt.Printf("    -> 这是超时错误\n")
			}
			if netErr.Temporary() {
				fmt.Printf("    -> 这是临时错误\n")
			}
		}
		return nil, 0, fmt.Errorf("[%s] 第%d页搜索请求失败: %w", p.Name(), page, err)
	}
	defer resp.Body.Close()

	debugPrintf("✅ [Dy4k DEBUG] HTTP请求成功 (耗时: %v)\n", requestDuration)

	// 6. 检查状态码
	debugPrintf("🔧 [Dy4k DEBUG] HTTP响应状态码: %d\n", resp.StatusCode)
	if resp.StatusCode != 200 {
		debugPrintf("❌ [Dy4k DEBUG] 状态码异常: %d\n", resp.StatusCode)
		return nil, 0, fmt.Errorf("[%s] 第%d页请求返回状态码: %d", p.Name(), page, resp.StatusCode)
	}

	// 7. 读取并打印HTML响应
	htmlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 第%d页读取响应失败: %w", p.Name(), page, err)
	}

	htmlContent := string(htmlBytes)
	debugPrintf("🔧 [Dy4k DEBUG] 第%d页 HTML长度: %d bytes\n", page, len(htmlContent))

	// 保存HTML到文件（仅在调试模式下）
	if DebugMode {
		htmlDir := "./html"
		os.MkdirAll(htmlDir, 0755)

		filename := fmt.Sprintf("dy4k_page_%d_%s.html", page, strings.ReplaceAll(encodedKeyword, "%", "_"))
		filepath := filepath.Join(htmlDir, filename)

		err = os.WriteFile(filepath, htmlBytes, 0644)
		if err != nil {
			debugPrintf("❌ [Dy4k DEBUG] 保存HTML文件失败: %v\n", err)
		} else {
			debugPrintf("✅ [Dy4k DEBUG] HTML已保存到: %s\n", filepath)
		}
	}

	// 解析HTML响应
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 第%d页HTML解析失败: %w", p.Name(), page, err)
	}

	// 8. 解析分页信息
	totalPages := p.parseTotalPages(doc)

	// 9. 提取搜索结果。旧版使用 .hl-list-item，新版 4KDY 使用 /4K-detail/ 链接。
	results := make([]model.SearchResult, 0)
	if legacyItems := doc.Find(".hl-list-item"); legacyItems.Length() > 0 {
		legacyItems.Each(func(_ int, s *goquery.Selection) {
			if result := p.parseSearchResultItem(s); result != nil {
				results = append(results, *result)
			}
		})
	} else {
		p.extractNewSearchResults(doc, &results)
	}

	return results, totalPages, nil
}

// extractNewSearchResults 解析 4KDY 新版搜索页的结果卡片。
func (p *Dy4kPlugin) extractNewSearchResults(doc *goquery.Document, results *[]model.SearchResult) {
	container := doc.Find("div.space-y-4").First()
	if container.Length() == 0 {
		return
	}
	container.Find("a[href*='/4K-detail/']").Each(func(_ int, titleLink *goquery.Selection) {
		href, ok := titleLink.Attr("href")
		if !ok || href == "" {
			return
		}
		matches := detailIDRegex.FindStringSubmatch(href)
		if len(matches) < 2 {
			return
		}

		card := titleLink.Parent().Parent()
		if card.Length() == 0 || card.Find("a[href^='/4K-vodplay/']").Length() == 0 {
			return
		}

		title := strings.TrimSpace(titleLink.Text())
		if title == "" {
			return
		}

		imageURL := ""
		image := card.Find("img").First()
		imageURL, _ = image.Attr("data-original")
		if imageURL == "" {
			imageURL, _ = image.Attr("src")
		}
		if strings.HasPrefix(imageURL, "/") {
			imageURL = BaseURL + imageURL
		}

		meta := strings.TrimSpace(card.Find("div.gap-x-3").First().Text())
		status := strings.TrimSpace(card.Find("a[href^='/4K-vodplay/'] span").Last().Text())
		description := strings.TrimSpace(card.Find("p").First().Text())
		content := meta
		if description != "" {
			if content != "" {
				content += "\n"
			}
			content += description
		}
		if status != "" {
			if content != "" {
				content += " | "
			}
			content += status
		}

		*results = append(*results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), matches[1]),
			Title:    title,
			Content:  content,
			Datetime: time.Now(),
			Images:   nonEmptyImage(imageURL),
			Links:    []model.Link{},
			Channel:  "",
		})
	})
}

func nonEmptyImage(imageURL string) []string {
	if imageURL == "" {
		return nil
	}
	return []string{imageURL}
}

// parseTotalPages 解析总页数
func (p *Dy4kPlugin) parseTotalPages(doc *goquery.Document) int {
	// 查找分页信息，格式为 "1 / 2"
	pageInfo := doc.Find(".hl-page-tips a").Text()
	if pageInfo == "" {
		return 1
	}

	// 解析 "1 / 2" 格式
	parts := strings.Split(pageInfo, "/")
	if len(parts) != 2 {
		return 1
	}

	totalPagesStr := strings.TrimSpace(parts[1])
	totalPages, err := strconv.Atoi(totalPagesStr)
	if err != nil || totalPages < 1 {
		return 1
	}

	return totalPages
}

// parseSearchResultItem 解析单个搜索结果项
func (p *Dy4kPlugin) parseSearchResultItem(s *goquery.Selection) *model.SearchResult {
	// 获取详情页链接
	linkElement := s.Find(".hl-item-pic a").First()
	href, exists := linkElement.Attr("href")
	if !exists || href == "" {
		return nil
	}

	// 补全URL
	if strings.HasPrefix(href, "/") {
		href = BaseURL + href
	}

	// 提取ID
	matches := detailIDRegex.FindStringSubmatch(href)
	if len(matches) < 2 {
		return nil
	}
	id := matches[1]

	// 获取标题
	titleElement := s.Find(".hl-item-title a").First()
	title := strings.TrimSpace(titleElement.Text())
	if title == "" {
		return nil
	}

	// 获取封面图片
	imgElement := s.Find(".hl-item-thumb")
	imageURL, _ := imgElement.Attr("data-original")
	if imageURL != "" && strings.HasPrefix(imageURL, "/") {
		imageURL = BaseURL + imageURL
	}

	// 获取资源状态
	status := strings.TrimSpace(s.Find(".hl-pic-text .remarks").Text())

	// 获取评分
	score := strings.TrimSpace(s.Find(".hl-text-conch.score").Text())

	// 获取基本信息（年份、地区、类型）
	basicInfo := strings.TrimSpace(s.Find(".hl-item-sub").First().Text())

	// 获取简介
	description := strings.TrimSpace(s.Find(".hl-item-sub").Last().Text())

	// 解析年份、地区、类型
	var year, region, category string
	if basicInfo != "" {
		parts := strings.Split(basicInfo, "·")
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}

			// 跳过评分
			if strings.Contains(part, score) {
				continue
			}

			// 第一个通常是年份
			if i == 0 || (i == 1 && strings.Contains(parts[0], score)) {
				if yearRegex.MatchString(part) {
					year = part
				}
			} else if region == "" {
				region = part
			} else if category == "" {
				category = part
			} else {
				category += " " + part
			}
		}
	}

	// 构建标签
	tags := make([]string, 0)
	if status != "" {
		tags = append(tags, status)
	}
	if year != "" {
		tags = append(tags, year)
	}
	if region != "" {
		tags = append(tags, region)
	}
	if category != "" {
		tags = append(tags, category)
	}

	// 构建内容描述
	content := description
	if basicInfo != "" {
		content = basicInfo + "\n" + description
	}
	if score != "" {
		content = "评分: " + score + "\n" + content
	}

	return &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), id),
		Title:    title,
		Content:  content,
		Datetime: time.Time{}, // 使用零值而不是nil，参考jikepan插件标准
		Tags:     tags,
		Links:    []model.Link{}, // 初始为空，后续在详情页中填充
		Channel:  "",             // 插件搜索结果，Channel必须为空
	}
}

// enrichWithDetailInfo 并发获取详情页信息并丰富搜索结果
func (p *Dy4kPlugin) enrichWithDetailInfo(results []model.SearchResult, client *http.Client) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	// 使用信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	var mutex sync.Mutex

	enrichedResults := make([]model.SearchResult, len(results))
	copy(enrichedResults, results)

	for i := range enrichedResults {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 从UniqueID中提取ID
			parts := strings.Split(enrichedResults[index].UniqueID, "-")
			if len(parts) < 2 {
				return
			}
			id := parts[len(parts)-1]

			// 获取详情页信息
			detailInfo := p.getDetailInfo(id, client)
			if detailInfo != nil {
				mutex.Lock()
				enrichedResults[index].Links = detailInfo.Downloads
				if detailInfo.Content != "" {
					enrichedResults[index].Content = detailInfo.Content
				}
				// 补充标签
				for _, tag := range detailInfo.Tags {
					found := false
					for _, existingTag := range enrichedResults[index].Tags {
						if existingTag == tag {
							found = true
							break
						}
					}
					if !found {
						enrichedResults[index].Tags = append(enrichedResults[index].Tags, tag)
					}
				}
				mutex.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// 过滤掉没有有效下载链接的结果
	var validResults []model.SearchResult
	for _, result := range enrichedResults {
		if len(result.Links) > 0 {
			validResults = append(validResults, result)
		}
	}

	return validResults
}

// getDetailInfo 获取详情页信息
func (p *Dy4kPlugin) getDetailInfo(id string, client *http.Client) *detailPageResponse {
	startTime := time.Now()
	atomic.AddInt64(&detailPageRequests, 1)

	// 检查缓存
	if cached, ok := detailCache.Load(id); ok {
		if detail, ok := cached.(*detailPageResponse); ok {
			if time.Since(detail.Timestamp) < cacheTTL {
				atomic.AddInt64(&cacheHits, 1)
				return detail
			}
		}
	}

	// 缓存未命中
	atomic.AddInt64(&cacheMisses, 1)

	// 构建详情页URL
	detailURL := fmt.Sprintf(DetailURL, id)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
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
	req.Header.Set("Referer", BaseURL+"/")

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	// 解析详情页信息
	detail := &detailPageResponse{
		Downloads: make([]model.Link, 0),
		Tags:      make([]string, 0),
		Timestamp: time.Now(),
	}

	// 获取标题
	detail.Title = strings.TrimSpace(doc.Find("h1.title-mobile").First().Text())
	if detail.Title == "" {
		detail.Title = strings.TrimSpace(doc.Find("h2.hl-dc-title").First().Text())
	}

	// 获取封面图片
	imgElement := doc.Find(".info-card-mobile img[data-original]").First()
	if imgElement.Length() == 0 {
		imgElement = doc.Find(".hl-dc-pic .hl-item-thumb").First()
	}
	if imageURL, exists := imgElement.Attr("data-original"); exists && imageURL != "" {
		if strings.HasPrefix(imageURL, "/") {
			imageURL = BaseURL + imageURL
		}
		detail.ImageURL = imageURL
	}

	// 获取剧情简介
	detail.Content = strings.TrimSpace(doc.Find(".info-card-mobile").First().Find("[class*='leading-relaxed']").First().Text())
	if detail.Content == "" {
		detail.Content = strings.TrimSpace(doc.Find(".hl-content-wrap .hl-content-text").First().Text())
	}

	// 提取详细信息作为标签
	doc.Find(".hl-vod-data ul li").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text != "" {
			// 清理标签文本
			text = strings.ReplaceAll(text, "：", ": ")
			if strings.Contains(text, "类型:") || strings.Contains(text, "地区:") || strings.Contains(text, "语言:") {
				detail.Tags = append(detail.Tags, text)
			}
		}
	})

	// 提取下载链接
	p.extractDownloadLinks(doc, detail)

	// 缓存结果
	detailCache.Store(id, detail)

	// 记录性能统计
	detailDuration := time.Since(startTime)
	atomic.AddInt64(&totalDetailTime, int64(detailDuration))

	return detail
}

// GetPerformanceStats 获取性能统计信息（调试用）
func (p *Dy4kPlugin) GetPerformanceStats() map[string]interface{} {
	totalSearches := atomic.LoadInt64(&searchRequests)
	totalDetails := atomic.LoadInt64(&detailPageRequests)
	hits := atomic.LoadInt64(&cacheHits)
	misses := atomic.LoadInt64(&cacheMisses)
	searchTime := atomic.LoadInt64(&totalSearchTime)
	detailTime := atomic.LoadInt64(&totalDetailTime)

	stats := map[string]interface{}{
		"search_requests":      totalSearches,
		"detail_page_requests": totalDetails,
		"cache_hits":           hits,
		"cache_misses":         misses,
		"cache_hit_rate":       float64(hits) / float64(hits+misses) * 100,
	}

	if totalSearches > 0 {
		stats["avg_search_time_ms"] = float64(searchTime) / float64(totalSearches) / 1000000
	}
	if totalDetails > 0 {
		stats["avg_detail_time_ms"] = float64(detailTime) / float64(totalDetails) / 1000000
	}

	return stats
}

// extractDownloadLinks 提取详情页中的磁力、电驴和网盘链接。
// 4KDY 新版把真实地址放在 .download-item a[href]，不能只读取 doc.Text()。
func (p *Dy4kPlugin) extractDownloadLinks(doc *goquery.Document, detail *detailPageResponse) {
	// 新版下载列表和旧版下载列表都通过 a[href] 提取，链接旁的文本用于补充提取码。
	doc.Find(".download-item a[href], .hl-downs-list a[href], .hl-rb-playlist a[href]").Each(func(_ int, link *goquery.Selection) {
		href, exists := link.Attr("href")
		if !exists || strings.TrimSpace(href) == "" {
			return
		}
		itemText := strings.TrimSpace(link.Parent().Text())
		p.processFoundLink(detail, href, itemText)
	})

	// 兼容链接直接出现在页面文本中的旧模板。
	pageText := doc.Text()
	p.extractLinksFromText(detail, pageText, "")
}

// processFoundLink 处理找到的链接，contextText 用于从链接旁的文字提取密码。
func (p *Dy4kPlugin) processFoundLink(detail *detailPageResponse, link, contextText string) {
	link = strings.TrimSpace(link)
	if link == "" {
		return
	}

	if matched := magnetLinkRegex.FindString(link); matched != "" {
		p.addDownloadLink(detail, "magnet", matched, "")
		return
	}
	if matched := ed2kLinkRegex.FindString(link); matched != "" {
		p.addDownloadLink(detail, "ed2k", matched, "")
		return
	}

	for panType, regex := range panLinkRegexes {
		matched := regex.FindString(link)
		if matched == "" {
			continue
		}
		password := p.extractPasswordFromText(contextText, matched)
		p.addDownloadLink(detail, panType, matched, password)
		return
	}
}

// extractLinksFromText 从文本中提取各种类型的链接
func (p *Dy4kPlugin) extractLinksFromText(detail *detailPageResponse, text, quality string) {
	// 磁力链接
	magnetMatches := magnetLinkRegex.FindAllString(text, -1)
	for _, magnetLink := range magnetMatches {
		p.addDownloadLink(detail, "magnet", magnetLink, "")
	}

	// 电驴链接
	ed2kMatches := ed2kLinkRegex.FindAllString(text, -1)
	for _, ed2kLink := range ed2kMatches {
		p.addDownloadLink(detail, "ed2k", ed2kLink, "")
	}

	// 网盘链接
	for panType, regex := range panLinkRegexes {
		matches := regex.FindAllString(text, -1)
		for _, panLink := range matches {
			password := p.extractPasswordFromText(text, panLink)
			p.addDownloadLink(detail, panType, panLink, password)
		}
	}
}

// extractPasswordFromLink 从链接URL中提取密码
func (p *Dy4kPlugin) extractPasswordFromLink(link string) string {
	// 首先检查URL参数中的密码
	for _, regex := range passwordRegexes {
		if matches := regex.FindStringSubmatch(link); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// extractPasswordFromText 从文本中提取指定链接的密码
func (p *Dy4kPlugin) extractPasswordFromText(text, link string) string {
	// 首先从链接本身提取密码
	if password := p.extractPasswordFromLink(link); password != "" {
		return password
	}

	// 然后从周围文本中查找密码
	for _, regex := range passwordRegexes {
		if matches := regex.FindStringSubmatch(text); len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// addDownloadLink 添加下载链接
func (p *Dy4kPlugin) addDownloadLink(detail *detailPageResponse, linkType, linkURL, password string) {
	if linkURL == "" {
		return
	}

	// 检查是否已存在
	for _, existingLink := range detail.Downloads {
		if existingLink.URL == linkURL {
			return
		}
	}

	// 创建链接对象
	link := model.Link{
		Type:      linkType,
		URL:       linkURL,
		Password:  password,
		WorkTitle: detail.Title,
	}

	detail.Downloads = append(detail.Downloads, link)
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *Dy4kPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	debugPrintf("🔄 [Dy4k DEBUG] 开始重试机制 - 最大重试次数: %d\n", maxRetries)

	for i := 0; i < maxRetries; i++ {
		debugPrintf("🔄 [Dy4k DEBUG] 第 %d/%d 次尝试\n", i+1, maxRetries)

		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			debugPrintf("⏳ [Dy4k DEBUG] 等待 %v 后重试\n", backoff)
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		attemptStart := time.Now()
		resp, err := client.Do(reqClone)
		attemptDuration := time.Since(attemptStart)

		debugPrintf("🔧 [Dy4k DEBUG] 第 %d 次尝试耗时: %v\n", i+1, attemptDuration)

		if err != nil {
			debugPrintf("❌ [Dy4k DEBUG] 第 %d 次尝试失败: %v\n", i+1, err)
			lastErr = err
			continue
		}

		debugPrintf("🔧 [Dy4k DEBUG] 第 %d 次尝试获得响应 - 状态码: %d\n", i+1, resp.StatusCode)

		if resp.StatusCode == 200 {
			debugPrintf("✅ [Dy4k DEBUG] 第 %d 次尝试成功!\n", i+1)
			return resp, nil
		}

		debugPrintf("❌ [Dy4k DEBUG] 第 %d 次尝试状态码异常: %d\n", i+1, resp.StatusCode)

		// 读取响应体以便调试
		if resp.Body != nil {
			bodyBytes, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && len(bodyBytes) > 0 {
				bodyPreview := string(bodyBytes)
				if len(bodyPreview) > 200 {
					bodyPreview = bodyPreview[:200] + "..."
				}
				debugPrintf("🔧 [Dy4k DEBUG] 响应体预览: %s\n", bodyPreview)
			}
		}

		lastErr = fmt.Errorf("状态码 %d", resp.StatusCode)
	}

	debugPrintf("❌ [Dy4k DEBUG] 所有重试都失败了!\n")
	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// getRandomUA 获取随机User-Agent
func getRandomUA() string {
	userAgents := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.1 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:109.0) Gecko/20100101 Firefox/119.0",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36 Edg/119.0.0.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/118.0.0.0 Safari/537.36",
	}
	return userAgents[rand.Intn(len(userAgents))]
}

// generateRandomIP 生成随机IP地址
func generateRandomIP() string {
	// 生成随机的私有IP地址段
	segments := [][]int{
		{192, 168, rand.Intn(256), rand.Intn(256)},
		{10, rand.Intn(256), rand.Intn(256), rand.Intn(256)},
		{172, 16 + rand.Intn(16), rand.Intn(256), rand.Intn(256)},
	}

	segment := segments[rand.Intn(len(segments))]
	return fmt.Sprintf("%d.%d.%d.%d", segment[0], segment[1], segment[2], segment[3])
}
