package qupanshe

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"pansou/util"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	BaseURL    = "https://www.qupanshe.com"
	UserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
	MaxRetries = 3
)

var (
	DebugLog = false // Debug开关，默认开启
)

// QupanshePlugin 趣盘社插件结构
type QupanshePlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewQupanshePlugin 创建趣盘社插件实例
func NewQupanshePlugin() *QupanshePlugin {
	return &QupanshePlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("qupanshe", 3), // 优先级3 = 普通质量数据源
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *QupanshePlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *QupanshePlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现搜索逻辑
func (p *QupanshePlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[qupanshe] 开始搜索: keyword=%s\n", keyword)
	}

	// 创建带有Cookie管理的专用客户端，确保整个搜索过程使用同一个session
	sessionClient, err := p.createSessionClient(client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建session客户端失败: %w", p.Name(), err)
	}

	if DebugLog {
		fmt.Printf("[qupanshe] 创建session客户端成功，开始三步搜索流程\n")
	}

	// Step 1: 获取首页formhash（使用session客户端）
	formhash, err := p.getFormhash(sessionClient)
	if err != nil {
		if DebugLog {
			fmt.Printf("[qupanshe] 获取formhash失败: %v\n", err)
		}
		return nil, fmt.Errorf("[%s] 获取formhash失败: %w", p.Name(), err)
	}
	if DebugLog {
		fmt.Printf("[qupanshe] 获取到formhash: %s\n", formhash)
	}

	// Step 2: POST请求获取搜索结果URL（使用同一个session客户端）
	searchURL, err := p.postSearchRequest(sessionClient, keyword, formhash)
	if err != nil {
		if DebugLog {
			fmt.Printf("[qupanshe] POST搜索请求失败: %v\n", err)
		}
		return nil, fmt.Errorf("[%s] POST搜索请求失败: %w", p.Name(), err)
	}
	if DebugLog {
		fmt.Printf("[qupanshe] 获取搜索URL成功: %s\n", searchURL)
	}

	// Step 3: GET请求获取搜索结果（使用同一个session客户端）
	results, err := p.getSearchResults(sessionClient, searchURL, keyword)
	if err != nil {
		if DebugLog {
			fmt.Printf("[qupanshe] 获取搜索结果失败: %v\n", err)
		}
		return nil, fmt.Errorf("[%s] 获取搜索结果失败: %w", p.Name(), err)
	}
	if DebugLog {
		fmt.Printf("[qupanshe] 获取搜索结果成功: 结果数=%d\n", len(results))
	}

	// Step 4: 关键词过滤
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)
	if DebugLog {
		fmt.Printf("[qupanshe] 关键词过滤后: 过滤前=%d, 过滤后=%d\n", len(results), len(filteredResults))
	}

	return filteredResults, nil
}

// createSessionClient 创建带有Cookie管理的HTTP客户端
func (p *QupanshePlugin) createSessionClient(baseClient *http.Client) (*http.Client, error) {
	// 创建Cookie Jar来管理cookies
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("创建cookie jar失败: %w", err)
	}

	// 创建新的客户端，复制基础客户端的配置但添加Cookie管理
	sessionClient := &http.Client{
		Timeout:   baseClient.Timeout,
		Transport: baseClient.Transport,
		Jar:       jar, // ⭐ 关键：添加Cookie管理
	}

	if DebugLog {
		fmt.Printf("[qupanshe] 创建带Cookie管理的session客户端，超时时间: %v\n", sessionClient.Timeout)
	}

	return sessionClient, nil
}

// getFormhash 从首页获取真实的formhash值
func (p *QupanshePlugin) getFormhash(client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BaseURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建GET请求失败: %w", err)
	}

	p.setRequestHeaders(req)

	if DebugLog {
		fmt.Printf("[qupanshe] 请求首页获取formhash: %s\n", BaseURL)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 调试：显示从首页获取的cookies
	if DebugLog && client.Jar != nil {
		if u, _ := url.Parse(BaseURL); u != nil {
			cookies := client.Jar.Cookies(u)
			fmt.Printf("[qupanshe] 从首页获取到 %d 个cookies:\n", len(cookies))
			for i, cookie := range cookies {
				fmt.Printf("  Cookie[%d]: %s=%s\n", i, cookie.Name, cookie.Value)
			}
		}
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("首页请求返回状态码: %d", resp.StatusCode)
	}

	// 处理可能的gzip压缩
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return "", fmt.Errorf("创建gzip读取器失败: %w", err)
		}
		defer gzipReader.Close()
		reader = util.NewCappedReader(gzipReader, util.MaxDecompressedBytes)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return "", fmt.Errorf("解析HTML失败: %w", err)
	}

	// 查找formhash
	formhash := ""
	inputCount := doc.Find("input[name='formhash']").Length()
	if DebugLog {
		fmt.Printf("[qupanshe] 找到input[name='formhash']元素数量: %d\n", inputCount)
	}

	doc.Find("input[name='formhash']").Each(func(i int, s *goquery.Selection) {
		if value, exists := s.Attr("value"); exists && value != "" {
			formhash = value
			if DebugLog {
				fmt.Printf("[qupanshe] 找到formhash[%d]: %s\n", i, value)
			}
		}
	})

	if formhash == "" {
		return "", fmt.Errorf("未找到formhash值")
	}

	return formhash, nil
}

// postSearchRequest 发送POST请求获取搜索结果URL
func (p *QupanshePlugin) postSearchRequest(client *http.Client, keyword, formhash string) (string, error) {
	// 添加延时，避免请求过快
	time.Sleep(2 * time.Second)

	// 构建POST请求
	searchURL := fmt.Sprintf("%s/search.php?mod=forum", BaseURL)
	data := url.Values{}
	data.Set("formhash", formhash)
	data.Set("srchtxt", keyword)
	data.Set("searchsubmit", "yes")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	postData := data.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", searchURL, strings.NewReader(postData))
	if err != nil {
		return "", fmt.Errorf("创建POST请求失败: %w", err)
	}

	p.setRequestHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// 详细日志：请求信息
	if DebugLog {
		fmt.Printf("[qupanshe] POST请求URL: %s\n", searchURL)
		fmt.Printf("[qupanshe] POST请求数据: %s\n", postData)
		fmt.Printf("[qupanshe] POST请求头:\n")
		for key, values := range req.Header {
			for _, value := range values {
				fmt.Printf("  %s: %s\n", key, value)
			}
		}

		// 显示将要发送的cookies
		if client.Jar != nil {
			if u, _ := url.Parse(searchURL); u != nil {
				cookies := client.Jar.Cookies(u)
				fmt.Printf("[qupanshe] POST请求将发送 %d 个cookies:\n", len(cookies))
				for i, cookie := range cookies {
					fmt.Printf("  Cookie[%d]: %s=%s\n", i, cookie.Name, cookie.Value)
				}
			}
		}
	}

	// 不自动跟随重定向
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	defer func() { client.CheckRedirect = nil }()

	// 带重试机制的请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return "", fmt.Errorf("POST请求失败: %w", err)
	}
	defer resp.Body.Close()

	if DebugLog {
		fmt.Printf("[qupanshe] POST请求响应: status=%d\n", resp.StatusCode)
		fmt.Printf("[qupanshe] 响应头: %v\n", resp.Header)
	}

	// 从响应头获取Location
	location := resp.Header.Get("Location")
	if DebugLog {
		fmt.Printf("[qupanshe] Location header: %s\n", location)
	}

	// 读取响应体用于调试（非重定向状态码时）
	if resp.StatusCode != 302 && resp.StatusCode != 301 && DebugLog {
		body, readErr := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		if readErr == nil {
			bodyStr := string(body)
			if len(bodyStr) > 1000 {
				fmt.Printf("[qupanshe] 响应体(前1000字符): %s\n", bodyStr[:1000])
			} else {
				fmt.Printf("[qupanshe] 响应体: %s\n", bodyStr)
			}
		}
	}

	if location == "" {
		return "", fmt.Errorf("未获取到重定向URL，状态码: %d", resp.StatusCode)
	}

	// 将相对路径转换为完整URL
	fullURL := BaseURL + "/" + strings.TrimPrefix(location, "/")

	return fullURL, nil
}

// getSearchResults 获取搜索结果
func (p *QupanshePlugin) getSearchResults(client *http.Client, searchURL, keyword string) ([]model.SearchResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建GET请求失败: %w", err)
	}

	p.setRequestHeaders(req)

	if DebugLog {
		fmt.Printf("[qupanshe] GET搜索结果URL: %s\n", searchURL)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if DebugLog {
			fmt.Printf("[qupanshe] 搜索结果页请求失败: status=%d\n", resp.StatusCode)
		}
		return nil, fmt.Errorf("请求返回状态码: %d", resp.StatusCode)
	}

	// 处理可能的gzip压缩
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip读取器失败: %w", err)
		}
		defer gzipReader.Close()
		reader = util.NewCappedReader(gzipReader, util.MaxDecompressedBytes)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if DebugLog {
			fmt.Printf("[qupanshe] 解析HTML失败: %v\n", err)
		}
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	return p.extractSearchResults(doc), nil
}

// extractSearchResults 提取搜索结果
func (p *QupanshePlugin) extractSearchResults(doc *goquery.Document) []model.SearchResult {
	var results []model.SearchResult

	liCount := doc.Find("li.pbw").Length()
	if DebugLog {
		fmt.Printf("[qupanshe] 找到li.pbw元素数量: %d\n", liCount)
	}

	doc.Find("li.pbw").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchResult(s)
		if result.Title != "" {
			results = append(results, result)
			if DebugLog {
				fmt.Printf("[qupanshe] 解析结果[%d]: title=%s, links=%d\n", i, result.Title, len(result.Links))
			}
		} else {
			if DebugLog {
				fmt.Printf("[qupanshe] 解析结果[%d]: 标题为空，跳过\n", i)
			}
		}
	})

	if DebugLog {
		fmt.Printf("[qupanshe] 提取到有效结果数: %d\n", len(results))
	}

	return results
}

// parseSearchResult 解析单个搜索结果
func (p *QupanshePlugin) parseSearchResult(s *goquery.Selection) model.SearchResult {
	// 提取帖子ID
	postID, _ := s.Attr("id")

	// 提取标题和详情页链接
	titleLink := s.Find("h3.xs3 a").First()
	titleHTML, _ := titleLink.Html()
	title := p.cleanTitle(titleHTML)
	detailPath, _ := titleLink.Attr("href")

	var detailURL string
	if detailPath != "" {
		if strings.HasPrefix(detailPath, "http") {
			detailURL = detailPath
		} else {
			detailURL = BaseURL + "/" + strings.TrimPrefix(detailPath, "/")
		}
	}

	// 提取统计信息（回复数和查看数）
	statsText := s.Find("p.xg1").First().Text()
	var replyCount, viewCount int
	p.parseStats(statsText, &replyCount, &viewCount)

	// 提取内容摘要（第二个p标签）
	var content string
	s.Find("p").Each(func(i int, p *goquery.Selection) {
		if i == 1 { // 第二个p标签是内容摘要
			content = strings.TrimSpace(p.Text())
		}
	})

	// ⭐ 重要：直接从搜索结果页的内容摘要中提取网盘链接
	var links []model.Link

	// 1. 从HTML中提取<a>标签链接
	aTagCount := s.Find("p").Eq(1).Find("a").Length()
	if DebugLog && aTagCount > 0 {
		fmt.Printf("[qupanshe] [%s] 找到<a>标签数量: %d\n", postID, aTagCount)
	}
	s.Find("p").Eq(1).Find("a").Each(func(i int, a *goquery.Selection) {
		href, exists := a.Attr("href")
		if !exists {
			return
		}
		if DebugLog {
			fmt.Printf("[qupanshe] [%s] 检查链接[%d]: %s\n", postID, i, href)
		}
		linkType := p.determineLinkType(href)
		if linkType != "" {
			password := p.extractPasswordFromContent(content, href)
			links = append(links, model.Link{
				URL:      href,
				Type:     linkType,
				Password: password,
			})
			if DebugLog {
				fmt.Printf("[qupanshe] [%s] 识别到%s链接: %s\n", postID, linkType, href)
			}
		}
	})

	// 2. 从纯文本中提取链接（可能没有<a>标签）
	if DebugLog {
		fmt.Printf("[qupanshe] [%s] 从文本提取链接: content长度=%d\n", postID, len(content))
	}
	textLinks := p.extractLinksFromText(content)
	if DebugLog && len(textLinks) > 0 {
		fmt.Printf("[qupanshe] [%s] 从文本提取到链接数: %d\n", postID, len(textLinks))
	}
	links = append(links, textLinks...)

	// 去重
	beforeDedupe := len(links)
	links = p.deduplicateLinks(links)
	if DebugLog && beforeDedupe != len(links) {
		fmt.Printf("[qupanshe] [%s] 链接去重: 去重前=%d, 去重后=%d\n", postID, beforeDedupe, len(links))
	}

	// 提取时间、作者、分类信息（最后一个p标签）
	var publishTime, author, category string
	lastP := s.Find("p").Last()
	spans := lastP.Find("span")
	if spans.Length() >= 3 {
		publishTime = strings.TrimSpace(spans.Eq(0).Text())
		author = strings.TrimSpace(spans.Eq(1).Find("a").Text())
		category = strings.TrimSpace(spans.Eq(2).Find("a").Text())
	}

	// 转换时间格式
	parsedTime := p.parseTime(publishTime)

	// 构建包含详情页URL的Content
	enrichedContent := content
	if detailURL != "" {
		enrichedContent = fmt.Sprintf("%s | 作者: %s | 分类: %s | 详情: %s", content, author, category, detailURL)
	}

	// 如果没有找到帖子ID，使用时间戳
	if postID == "" {
		postID = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return model.SearchResult{
		MessageID: fmt.Sprintf("%s-%s", p.Name(), postID),
		UniqueID:  fmt.Sprintf("%s-%s", p.Name(), postID),
		Title:     title,
		Content:   enrichedContent,
		Datetime:  parsedTime,
		Links:     links, // ⭐ 直接使用从搜索结果页提取的链接
		Channel:   "",    // ⭐ 重要：插件搜索结果Channel必须为空
	}
}

// cleanTitle 清理标题中的HTML标签
func (p *QupanshePlugin) cleanTitle(titleHTML string) string {
	// 移除所有HTML标签
	re := qupansheRe1
	title := re.ReplaceAllString(titleHTML, "")

	// 清理HTML实体
	title = strings.ReplaceAll(title, "&nbsp;", " ")
	title = strings.ReplaceAll(title, "&amp;", "&")
	title = strings.ReplaceAll(title, "&lt;", "<")
	title = strings.ReplaceAll(title, "&gt;", ">")
	title = strings.ReplaceAll(title, "&quot;", "\"")

	return strings.TrimSpace(title)
}

// determineLinkType 确定链接类型
func (p *QupanshePlugin) determineLinkType(urlStr string) string {
	linkPatterns := map[string]string{
		`pan\.quark\.cn`:   "quark",
		`pan\.baidu\.com`:  "baidu",
		`www\.alipan\.com`: "aliyun",
		`aliyundrive\.com`: "aliyun",
		`pan\.xunlei\.com`: "xunlei",
		`cloud\.189\.cn`:   "tianyi",
		`pan\.uc\.cn`:      "uc",
		`www\.123pan\.com`: "123",
		`www\.123684\.com`: "123",
		`115cdn\.com`:      "115",
		`115\.com`:         "115",
		`pan\.pikpak\.com`: "pikpak",
		`mypikpak\.com`:    "pikpak",
		`caiyun\.139\.cn`:  "mobile",
	}

	for pattern, linkType := range linkPatterns {
		matched, _ := regexp.MatchString(pattern, urlStr)
		if matched {
			return linkType
		}
	}

	return ""
}

// extractLinksFromText 从文本中提取链接
func (p *QupanshePlugin) extractLinksFromText(text string) []model.Link {
	var links []model.Link

	// 网盘链接正则模式（支持更宽泛的字符集）
	patterns := []string{
		`https?://pan\.quark\.cn/s/[a-zA-Z0-9_-]+`,
		`https?://pan\.baidu\.com/s/[a-zA-Z0-9_-]+(?:\?pwd=[a-zA-Z0-9]+)?`, // 支持pwd参数
		`https?://www\.alipan\.com/s/[a-zA-Z0-9_-]+`,
		`https?://aliyundrive\.com/s/[a-zA-Z0-9_-]+`,
		`https?://pan\.xunlei\.com/s/[a-zA-Z0-9_-]+`,
		`https?://cloud\.189\.cn/[a-zA-Z0-9_/-]+`, // 天翼云支持多级路径
		`https?://pan\.uc\.cn/s/[a-zA-Z0-9_-]+`,
		`https?://www\.123pan\.com/s/[a-zA-Z0-9_-]+`,
		`https?://www\.123684\.com/s/[a-zA-Z0-9_-]+`,
		`https?://115cdn\.com/[a-zA-Z0-9_/-]+`,
		`https?://115\.com/[a-zA-Z0-9_/-]+`,
		`https?://pan\.pikpak\.com/s/[a-zA-Z0-9_-]+`,
		`https?://mypikpak\.com/s/[a-zA-Z0-9_-]+`,
		`https?://caiyun\.139\.com/[a-zA-Z0-9_/-]+`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllString(text, -1)

		for _, match := range matches {
			linkType := p.determineLinkType(match)
			if linkType != "" {
				// 从URL参数或周围文本提取密码
				password := p.extractPasswordFromContent(text, match)
				links = append(links, model.Link{
					URL:      match,
					Type:     linkType,
					Password: password,
				})
			}
		}
	}

	return links
}

// extractPasswordFromContent 从内容文本中提取指定链接的密码
func (p *QupanshePlugin) extractPasswordFromContent(content, linkURL string) string {
	// 先尝试从URL中提取pwd参数
	if parsedURL, err := url.Parse(linkURL); err == nil {
		if pwd := parsedURL.Query().Get("pwd"); pwd != "" {
			return pwd
		}
	}

	// 查找链接在内容中的位置
	linkIndex := strings.Index(content, linkURL)
	if linkIndex == -1 {
		return ""
	}

	// 提取链接周围的文本（前20字符，后100字符）
	start := linkIndex - 20
	if start < 0 {
		start = 0
	}
	end := linkIndex + len(linkURL) + 100
	if end > len(content) {
		end = len(content)
	}

	surroundingText := content[start:end]

	// 查找密码模式
	passwordPatterns := []string{
		`提取码[：:]\s*([A-Za-z0-9]+)`,
		`密码[：:]\s*([A-Za-z0-9]+)`,
		`pwd[：:=]\s*([A-Za-z0-9]+)`,
		`password[：:=]\s*([A-Za-z0-9]+)`,
	}

	for _, pattern := range passwordPatterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(surroundingText)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// deduplicateLinks 去重链接
func (p *QupanshePlugin) deduplicateLinks(links []model.Link) []model.Link {
	linkMap := make(map[string]model.Link)

	for _, link := range links {
		// 提取和设置密码
		normalizedURL, password := p.extractPasswordFromURL(link.URL)

		// 创建带密码信息的新链接
		newLink := model.Link{
			URL:      link.URL,
			Type:     link.Type,
			Password: password,
		}

		// 如果链接本身没有密码但我们找到了密码，使用找到的密码
		if newLink.Password == "" && link.Password != "" {
			newLink.Password = link.Password
		}

		// 使用标准化URL作为key进行去重
		if existingLink, exists := linkMap[normalizedURL]; exists {
			// 如果已存在，保留更完整的版本（优先带密码的）
			if newLink.Password != "" && existingLink.Password == "" {
				linkMap[normalizedURL] = newLink
			}
		} else {
			linkMap[normalizedURL] = newLink
		}
	}

	// 转换为切片
	var result []model.Link
	for _, link := range linkMap {
		result = append(result, link)
	}

	return result
}

// extractPasswordFromURL 从URL中提取密码并返回标准化URL
func (p *QupanshePlugin) extractPasswordFromURL(rawURL string) (normalizedURL string, password string) {
	// 解析URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return rawURL, ""
	}

	// 获取查询参数
	query := parsedURL.Query()

	// 检查常见的密码参数
	passwordKeys := []string{"pwd", "password", "pass", "code"}
	for _, key := range passwordKeys {
		if val := query.Get(key); val != "" {
			password = val
			break
		}
	}

	// 构建标准化URL（去除密码参数）
	for _, key := range passwordKeys {
		query.Del(key)
	}

	parsedURL.RawQuery = query.Encode()
	normalizedURL = parsedURL.String()

	// 如果查询参数为空，去掉问号
	if parsedURL.RawQuery == "" {
		normalizedURL = strings.TrimSuffix(normalizedURL, "?")
	}

	return normalizedURL, password
}

// parseStats 解析统计信息
func (p *QupanshePlugin) parseStats(statsText string, replyCount, viewCount *int) {
	// 解析如 "18 个回复 - 5926 次查看" 格式
	re := qupansheRe2
	matches := re.FindStringSubmatch(statsText)
	if len(matches) >= 3 {
		if reply, err := strconv.Atoi(matches[1]); err == nil {
			*replyCount = reply
		}
		if view, err := strconv.Atoi(matches[2]); err == nil {
			*viewCount = view
		}
	}
}

// parseTime 解析时间字符串
func (p *QupanshePlugin) parseTime(timeStr string) time.Time {
	// 解析如 "2024-10-8 20:58" 格式（注意月和日可能是单数字）
	timeStr = strings.TrimSpace(timeStr)

	formats := []string{
		"2006-1-2 15:04",
		"2006-1-2 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t
		}
	}

	// 如果解析失败，返回当前时间
	return time.Now()
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *QupanshePlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 2 * time.Second
			if DebugLog {
				fmt.Printf("[qupanshe] 重试第%d次，等待%v\n", i, backoff)
			}
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err == nil {
			// 检查状态码
			if resp.StatusCode == 503 {
				if DebugLog {
					fmt.Printf("[qupanshe] 服务器返回503，继续重试\n")
				}
				resp.Body.Close()
				lastErr = fmt.Errorf("服务器返回503")
				continue
			}
			return resp, nil
		}

		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", MaxRetries, lastErr)
}

// setRequestHeaders 设置请求头
func (p *QupanshePlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
}

func init() {
	p := NewQupanshePlugin()
	plugin.RegisterGlobalPlugin(p)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	qupansheRe1 = regexp.MustCompile(`<[^>]*>`)
	qupansheRe2 = regexp.MustCompile(`(\d+)\s*个回复\s*-\s*(\d+)\s*次查看`)
)
