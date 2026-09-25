package xiaozhang

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
	"pansou/util"
)

const (
	BaseURL        = "https://xzys.fun"
	SearchPath     = "/search.html"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxConcurrency = 20 // 详情页最大并发数
	MaxPages       = 1  // 最大搜索页数（暂时只搜索第一页）
)

// XiaozhangPlugin 校长影视插件
type XiaozhangPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
}

// NewXiaozhangPlugin 创建新的校长影视插件实例
func NewXiaozhangPlugin() *XiaozhangPlugin {
	// 检查调试模式
	debugMode := false

	p := &XiaozhangPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("xiaozhang", 3),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,
	}

	return p
}

// Name 返回插件名称
func (p *XiaozhangPlugin) Name() string {
	return "xiaozhang"
}

// DisplayName 返回插件显示名称
func (p *XiaozhangPlugin) DisplayName() string {
	return "校长影视"
}

// Description 返回插件描述
func (p *XiaozhangPlugin) Description() string {
	return "校长影视 - 影视资源搜索"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *XiaozhangPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XiaozhangPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// setRequestHeaders 设置请求头
func (p *XiaozhangPlugin) setRequestHeaders(req *http.Request, referer string) {
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

// doRequest 发送HTTP请求（带重定向控制）
func (p *XiaozhangPlugin) doRequest(client *http.Client, url string, referer string, followRedirect bool) (*http.Response, error) {
	// 创建临时客户端，控制重定向行为
	tempClient := &http.Client{
		Timeout: client.Timeout,
		Transport: &http.Transport{
			Proxy:              util.ProxyFuncForTransport(),
			DisableCompression: true, // 禁用自动gzip解压，我们手动处理
		},
	}

	if !followRedirect {
		tempClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	p.setRequestHeaders(req, referer)

	if p.debugMode {
		log.Printf("[Xiaozhang] 发送请求: %s", url)
	}

	resp, err := tempClient.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xiaozhang] 请求失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[Xiaozhang] 响应状态: %d", resp.StatusCode)
	}

	return resp, nil
}

// searchImpl 实际的搜索实现
func (p *XiaozhangPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s%s?keyword=%s", BaseURL, SearchPath, url.QueryEscape(keyword))

	if p.debugMode {
		log.Printf("[Xiaozhang] 开始搜索: %s", keyword)
		log.Printf("[Xiaozhang] 搜索URL: %s", searchURL)
	}

	// 发送搜索请求
	resp, err := p.doRequest(client, searchURL, BaseURL, true)
	if err != nil {
		return nil, fmt.Errorf("发送搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索响应状态码异常: %d", resp.StatusCode)
	}

	// 处理响应体（可能是gzip压缩的）
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if p.debugMode {
		log.Printf("[Xiaozhang] Content-Encoding: %s", contentEncoding)
		log.Printf("[Xiaozhang] Content-Type: %s", resp.Header.Get("Content-Type"))
	}

	// 如果是gzip压缩，手动解压
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip reader失败: %w", err)
		}
		defer gzReader.Close()
		reader = gzReader
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	results := p.extractSearchResults(doc, keyword)

	if p.debugMode {
		log.Printf("[Xiaozhang] 找到 %d 个搜索结果", len(results))
	}

	// 并发获取详情页链接
	results = p.enrichWithDetailLinks(client, results, keyword)

	// 过滤结果
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	if p.debugMode {
		log.Printf("[Xiaozhang] 过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// extractSearchResults 从HTML中提取搜索结果
func (p *XiaozhangPlugin) extractSearchResults(doc *goquery.Document, keyword string) []model.SearchResult {
	var results []model.SearchResult

	if p.debugMode {
		// 调试：检查页面标题
		pageTitle := doc.Find("title").Text()
		log.Printf("[Xiaozhang] 页面标题: %s", pageTitle)

		// 调试：检查是否找到list-boxes
		listBoxes := doc.Find(".list-boxes")
		log.Printf("[Xiaozhang] 找到 .list-boxes 元素数量: %d", listBoxes.Length())

		// 调试：尝试其他可能的选择器
		if listBoxes.Length() == 0 {
			// 输出页面部分HTML用于调试
			bodyHTML, _ := doc.Find("body").Html()
			if len(bodyHTML) > 500 {
				bodyHTML = bodyHTML[:500] + "..."
			}
			log.Printf("[Xiaozhang] 页面body前500字符: %s", bodyHTML)
		}
	}

	// 选择所有搜索结果项
	doc.Find(".list-boxes").Each(func(i int, s *goquery.Selection) {
		// 提取标题和详情页链接
		titleElem := s.Find("a.text_title_p")
		title := strings.TrimSpace(titleElem.Text())
		detailPath, _ := titleElem.Attr("href")

		if p.debugMode {
			log.Printf("[Xiaozhang] 处理第 %d 个结果: title=%s, path=%s", i+1, title, detailPath)
		}

		if title == "" || detailPath == "" {
			if p.debugMode {
				log.Printf("[Xiaozhang] 跳过第 %d 个结果：标题或链接为空", i+1)
			}
			return
		}

		// 构建完整的详情页URL
		detailURL := BaseURL + detailPath

		// 提取描述
		content := strings.TrimSpace(s.Find("p.text_p").Text())

		// 提取发布时间
		timeText := strings.TrimSpace(s.Find(".list-actions span").First().Text())
		timeText = strings.ReplaceAll(timeText, "&nbsp;", " ")
		timeText = strings.TrimSpace(timeText)

		// 解析时间（格式：2025-08-16）
		var publishTime time.Time
		if timeText != "" {
			// 尝试解析日期
			parsedTime, err := time.Parse("2006-01-02", timeText)
			if err != nil {
				// 如果解析失败，使用当前时间
				publishTime = time.Now()
				if p.debugMode {
					log.Printf("[Xiaozhang] 解析时间失败: %s, 错误: %v", timeText, err)
				}
			} else {
				publishTime = parsedTime
			}
		} else {
			publishTime = time.Now()
		}

		// 从详情页路径提取ID（如：/subject/9861.html -> 9861）
		idMatch := xiaozhangRe1.FindStringSubmatch(detailPath)
		resourceID := ""
		if len(idMatch) > 1 {
			resourceID = idMatch[1]
		} else {
			resourceID = fmt.Sprintf("%d", time.Now().UnixNano())
		}

		if p.debugMode {
			log.Printf("[Xiaozhang] 提取结果 %d: %s, URL: %s, 时间: %s", i+1, title, detailURL, timeText)
		}

		result := model.SearchResult{
			Title:     title,
			Content:   content,
			Channel:   "",
			MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
			Datetime:  publishTime,
			Links:     []model.Link{}, // 稍后填充
		}

		// 将详情页URL存储在Tags中供后续使用
		result.Tags = []string{detailURL}

		results = append(results, result)
	})

	return results
}

// enrichWithDetailLinks 并发获取详情页的下载链接
func (p *XiaozhangPlugin) enrichWithDetailLinks(client *http.Client, results []model.SearchResult, keyword string) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	if p.debugMode {
		log.Printf("[Xiaozhang] 开始获取 %d 个详情页的下载链接", len(results))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, MaxConcurrency)

	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加小延迟避免请求过快
			time.Sleep(time.Duration(idx*50) * time.Millisecond)

			// 从Tags中获取详情页URL
			if len(results[idx].Tags) > 0 {
				detailURL := results[idx].Tags[0]
				links := p.fetchDetailPageLinks(client, detailURL, keyword)

				mu.Lock()
				results[idx].Links = links
				// 清空Tags，避免返回给用户
				results[idx].Tags = nil
				mu.Unlock()

				if p.debugMode {
					log.Printf("[Xiaozhang] 详情页 %d/%d 获取到 %d 个链接", idx+1, len(results), len(links))
				}
			}
		}(i)
	}

	wg.Wait()

	return results
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *XiaozhangPlugin) fetchDetailPageLinks(client *http.Client, detailURL string, keyword string) []model.Link {
	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[Xiaozhang] 使用缓存的详情页结果: %s", detailURL)
			}
			return links
		}
	}

	// 第一步：获取重定向位置
	resp, err := p.doRequest(client, detailURL, BaseURL, false)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xiaozhang] 获取详情页失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	// 获取Location头
	location := resp.Header.Get("Location")
	if location == "" {
		// 如果没有重定向，可能直接就是详情页
		if resp.StatusCode == http.StatusOK {
			return p.extractDetailPageLinks(resp, detailURL)
		}
		if p.debugMode {
			log.Printf("[Xiaozhang] 未找到重定向位置，状态码: %d", resp.StatusCode)
		}
		return nil
	}

	// 构建真实的详情页URL
	realDetailURL := BaseURL + location
	if p.debugMode {
		log.Printf("[Xiaozhang] 重定向到: %s", realDetailURL)
	}

	// 第二步：访问真实的详情页
	resp2, err := p.doRequest(client, realDetailURL, detailURL, true)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xiaozhang] 获取真实详情页失败: %v", err)
		}
		return nil
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Xiaozhang] 真实详情页响应状态码异常: %d", resp2.StatusCode)
		}
		return nil
	}

	links := p.extractDetailPageLinks(resp2, realDetailURL)

	// 缓存结果
	p.detailCache.Store(detailURL, links)

	// 设置缓存过期
	go func() {
		time.Sleep(p.cacheTTL)
		p.detailCache.Delete(detailURL)
	}()

	return links
}

// extractDetailPageLinks 从详情页响应中提取下载链接
func (p *XiaozhangPlugin) extractDetailPageLinks(resp *http.Response, pageURL string) []model.Link {
	// 处理响应体（可能是gzip压缩的）
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			if p.debugMode {
				log.Printf("[Xiaozhang] 创建gzip reader失败: %v", err)
			}
			return nil
		}
		defer gzReader.Close()
		reader = gzReader
	}

	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if p.debugMode {
			log.Printf("[Xiaozhang] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	var links []model.Link
	linkMap := make(map[string]bool) // 用于去重

	// 查找所有包含下载链接的p标签
	doc.Find("p").Each(func(i int, s *goquery.Selection) {
		// 查找p标签内的链接
		s.Find("a[href]").Each(func(j int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			if !exists || href == "" {
				return
			}

			// 过滤非网盘链接
			if !isValidPanLink(href) {
				return
			}

			// 去重
			if linkMap[href] {
				return
			}
			linkMap[href] = true

			// 提取密码（可能在p标签的文本中）
			password := ""
			pText := strings.TrimSpace(s.Text())

			// 尝试从文本中提取密码
			if strings.Contains(pText, "提取码") || strings.Contains(pText, "密码") {
				passwordMatch := xiaozhangRe2.FindStringSubmatch(pText)
				if len(passwordMatch) > 1 {
					password = passwordMatch[1]
				}
			}

			// 尝试从URL中提取密码
			if password == "" && strings.Contains(href, "pwd=") {
				if u, err := url.Parse(href); err == nil {
					password = u.Query().Get("pwd")
				}
			}

			// 判断链接类型
			linkType := determineLinkType(href)

			link := model.Link{
				URL:      href,
				Type:     linkType,
				Password: password,
			}

			if p.debugMode {
				log.Printf("[Xiaozhang] 提取链接: %s, 类型: %s, 密码: %s", href, linkType, password)
			}

			links = append(links, link)
		})
	})

	return links
}

// isValidPanLink 判断是否是有效的网盘链接
func isValidPanLink(url string) bool {
	panPatterns := []string{
		"pan.baidu.com",
		"pan.quark.cn",
		"www.aliyundrive.com",
		"www.alipan.com",
		"115.com",
		"cloud.189.cn",
		"pan.xunlei.com",
		"www.123pan.com",
		"www.jianguoyun.com",
		"cowtransfer.com",
		"weidian.com",
	}

	for _, pattern := range panPatterns {
		if strings.Contains(url, pattern) {
			return true
		}
	}

	return false
}

// determineLinkType 判断链接类型
func determineLinkType(url string) string {
	linkTypeMap := map[string]string{
		"pan.baidu.com":       "baidu",
		"pan.quark.cn":        "quark",
		"www.aliyundrive.com": "aliyun",
		"www.alipan.com":      "aliyun",
		"115.com":             "115",
		"cloud.189.cn":        "tianyi",
		"pan.xunlei.com":      "xunlei",
		"www.123pan.com":      "123",
		"www.jianguoyun.com":  "jianguo",
		"cowtransfer.com":     "cowtransfer",
		"weidian.com":         "weidian",
	}

	for pattern, linkType := range linkTypeMap {
		if strings.Contains(url, pattern) {
			return linkType
		}
	}

	return "other"
}

func init() {
	plugin.RegisterGlobalPlugin(NewXiaozhangPlugin())
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	xiaozhangRe1 = regexp.MustCompile(`/subject/(\d+)\.html`)
	xiaozhangRe2 = regexp.MustCompile(`(?:提取码|密码)[：:]?\s*([a-zA-Z0-9]+)`)
)
