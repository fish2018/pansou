package pianku

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"pansou/model"
	"pansou/plugin"

	"github.com/PuerkitoBio/goquery"
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewPiankuPlugin())
}

const (
	// 基础URL
	BaseURL    = "https://btnull.pro"
	SearchPath = "/search/-------------.html"

	// 默认参数
	MaxRetries     = 3
	TimeoutSeconds = 30
)

// 预编译的正则表达式
var (
	// 提取电影ID的正则表达式
	movieIDRegex = regexp.MustCompile(`/movie/(\d+)\.html`)

	// 年份提取正则
	yearRegex = regexp.MustCompile(`\((\d{4})\)`)

	// 地区和类型分离正则
	regionTypeRegex = regexp.MustCompile(`地区：([^　]*?)　+类型：(.*)`)

	// 磁力链接正则
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}[^"'\s]*`)

	// ED2K链接正则
	ed2kLinkRegex = regexp.MustCompile(`ed2k://\|file\|[^|]+\|[^|]+\|[^|]+\|/?`)

	// 网盘链接正则表达式
	panLinkRegexes = map[string]*regexp.Regexp{
		"baidu":  regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_-]+(?:\?pwd=[0-9a-zA-Z]+)?(?:&v=\d+)?`),
		"aliyun": regexp.MustCompile(`https?://(?:www\.)?alipan\.com/s/[0-9a-zA-Z_-]+`),
		"tianyi": regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z_-]+(?:\([^)]*\))?`),
		"uc":     regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-fA-F]+(?:\?[^"\s]*)?`),
		"mobile": regexp.MustCompile(`https?://caiyun\.139\.com/[^"\s]+`),
		"115":    regexp.MustCompile(`https?://(?:115\.com|115cdn\.com)/s/[0-9a-zA-Z_-]+(?:\?[^"\s]*)?`),
		"pikpak": regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z_-]+`),
		"xunlei": regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_-]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		"123":    regexp.MustCompile(`https?://(?:www\.)?(?:123pan\.com|123684\.com)/s/[0-9a-zA-Z_-]+(?:\?[^"\s]*)?`),
		"quark":  regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-fA-F]+(?:\?pwd=[0-9a-zA-Z]+)?`),
	}

	// 密码提取正则表达式
	passwordRegexes = []*regexp.Regexp{
		regexp.MustCompile(`[?&]pwd=([0-9a-zA-Z]+)`),      // URL中的pwd参数
		regexp.MustCompile(`[?&]password=([0-9a-zA-Z]+)`), // URL中的password参数
		regexp.MustCompile(`提取码[：:]\s*([0-9a-zA-Z]+)`),    // 提取码：xxxx
		regexp.MustCompile(`访问码[：:]\s*([0-9a-zA-Z]+)`),    // 访问码：xxxx
		regexp.MustCompile(`密码[：:]\s*([0-9a-zA-Z]+)`),     // 密码：xxxx
		regexp.MustCompile(`验证码[：:]\s*([0-9a-zA-Z]+)`),    // 验证码：xxxx
		regexp.MustCompile(`口令[：:]\s*([0-9a-zA-Z]+)`),     // 口令：xxxx
		regexp.MustCompile(`（访问码[：:]\s*([0-9a-zA-Z]+)）`),  // （访问码：xxxx）
	}
)

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.107 Safari/537.36",
}

// PiankuPlugin 片库网搜索插件
type PiankuPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewPiankuPlugin 创建新的片库网插件
func NewPiankuPlugin() *PiankuPlugin {
	return &PiankuPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("pianku", 3), // 优先级3，标准质量数据源
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *PiankuPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *PiankuPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *PiankuPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 处理扩展参数
	searchKeyword := keyword
	if ext != nil {
		if titleEn, exists := ext["title_en"]; exists {
			if titleEnStr, ok := titleEn.(string); ok && titleEnStr != "" {
				searchKeyword = titleEnStr
			}
		}
	}

	// 构建请求URL
	searchURL := fmt.Sprintf("%s%s?wd=%s", BaseURL, SearchPath, url.QueryEscape(searchKeyword))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSeconds*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	// 提取搜索结果基本信息
	searchResults := p.extractSearchResults(doc)

	// 为每个搜索结果获取详情页的下载链接
	var finalResults []model.SearchResult
	for _, result := range searchResults {
		// 获取详情页链接
		if len(result.Links) == 0 {
			continue
		}
		detailURL := result.Links[0].URL

		// 请求详情页并解析下载链接
		downloadLinks, err := p.fetchDetailPageLinks(client, detailURL)
		if err != nil {
			// 如果获取详情页失败，仍然保留原始结果
			finalResults = append(finalResults, result)
			continue
		}

		// 更新结果的链接为真正的下载链接
		if len(downloadLinks) > 0 {
			result.Links = downloadLinks
			finalResults = append(finalResults, result)
		}
	}

	// 关键词过滤
	return plugin.FilterResultsByKeyword(finalResults, searchKeyword), nil
}

// setRequestHeaders 设置请求头
func (p *PiankuPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgents[0])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", BaseURL+"/")
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *PiankuPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
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

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", MaxRetries, lastErr)
}

// extractSearchResults 提取搜索结果
func (p *PiankuPlugin) extractSearchResults(doc *goquery.Document) []model.SearchResult {
	var results []model.SearchResult

	// 查找搜索结果容器
	doc.Find(".sr_lists dl").Each(func(i int, s *goquery.Selection) {
		result := p.extractSingleResult(s)
		if result.UniqueID != "" && len(result.Links) > 0 {
			results = append(results, result)
		}
	})

	return results
}

// extractSingleResult 提取单个搜索结果
func (p *PiankuPlugin) extractSingleResult(s *goquery.Selection) model.SearchResult {
	// 提取链接和ID
	link, exists := s.Find("dt a").Attr("href")
	if !exists {
		return model.SearchResult{} // 返回空结果
	}

	// 提取电影ID
	movieID := p.extractMovieID(link)
	if movieID == "" {
		return model.SearchResult{}
	}

	// 提取封面图片（暂时不使用，但保留用于未来扩展）
	_, _ = s.Find("dt a img").Attr("src")

	// 提取标题
	title := strings.TrimSpace(s.Find("dd p:first-child strong a").Text())
	if title == "" {
		return model.SearchResult{}
	}

	// 提取状态标签
	status := strings.TrimSpace(s.Find("dd p:first-child span.ss1").Text())

	// 解析详细信息
	var actors, description, region, types, altName string

	s.Find("dd p").Each(func(j int, p *goquery.Selection) {
		text := strings.TrimSpace(p.Text())

		if strings.HasPrefix(text, "又名：") {
			altName = strings.TrimPrefix(text, "又名：")
		} else if strings.Contains(text, "地区：") && strings.Contains(text, "类型：") {
			// 解析地区和类型
			region, types = parseRegionAndTypes(text)
		} else if strings.HasPrefix(text, "主演：") {
			actors = strings.TrimPrefix(text, "主演：")
		} else if strings.HasPrefix(text, "简介：") {
			description = strings.TrimPrefix(text, "简介：")
		} else if !strings.Contains(text, "名称：") && !strings.Contains(text, "又名：") &&
			!strings.Contains(text, "地区：") && !strings.Contains(text, "主演：") && text != "" {
			// 可能是简介（没有"简介："前缀的情况）
			if description == "" && len(text) > 10 {
				description = text
			}
		}
	})

	// 构建完整的详情页URL
	fullLink := p.buildFullURL(link)

	// 设置标签
	tags := []string{}
	if region != "" {
		tags = append(tags, region)
	}
	if types != "" {
		// 分割类型标签
		typeList := strings.Split(types, ",")
		for _, t := range typeList {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}
	if status != "" {
		tags = append(tags, status)
	}

	// 构建内容描述
	content := description
	if actors != "" && content != "" {
		content = fmt.Sprintf("主演：%s\n%s", actors, content)
	} else if actors != "" {
		content = fmt.Sprintf("主演：%s", actors)
	}

	if altName != "" {
		if content != "" {
			content = fmt.Sprintf("又名：%s\n%s", altName, content)
		} else {
			content = fmt.Sprintf("又名：%s", altName)
		}
	}

	// 创建链接（使用详情页作为主要链接）
	links := []model.Link{
		{
			Type: "others", // 详情页链接
			URL:  fullLink,
		},
	}

	result := model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), movieID),
		Title:    title,
		Content:  content,
		Datetime: time.Now(), // 无法从搜索结果获取准确时间，使用当前时间
		Tags:     tags,
		Links:    links,
		Channel:  "", // 插件搜索结果必须为空字符串
	}

	return result
}

// extractMovieID 从URL中提取电影ID
func (p *PiankuPlugin) extractMovieID(url string) string {
	matches := movieIDRegex.FindStringSubmatch(url)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// parseRegionAndTypes 解析地区和类型信息
func parseRegionAndTypes(text string) (region, types string) {
	matches := regionTypeRegex.FindStringSubmatch(text)
	if len(matches) > 2 {
		region = strings.TrimSpace(matches[1])
		types = strings.TrimSpace(matches[2])
	}
	return
}

// buildFullURL 构建完整的URL
func (p *PiankuPlugin) buildFullURL(path string) string {
	if strings.HasPrefix(path, "http") {
		return path
	}
	return BaseURL + path
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *PiankuPlugin) fetchDetailPageLinks(client *http.Client, detailURL string) ([]model.Link, error) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSeconds*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建详情页请求失败: %w", err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送HTTP请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("详情页请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("详情页请求返回状态码: %d", resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("详情页HTML解析失败: %w", err)
	}

	// 提取下载链接
	return p.extractDownloadLinks(doc), nil
}

// extractDownloadLinks 提取详情页中的下载链接
func (p *PiankuPlugin) extractDownloadLinks(doc *goquery.Document) []model.Link {
	var links []model.Link
	seenURLs := make(map[string]bool) // 用于去重

	// 查找下载链接区域
	doc.Find("#donLink .down-list2").Each(func(i int, s *goquery.Selection) {
		linkURL, exists := s.Find(".down-list3 a").Attr("href")
		if !exists || linkURL == "" {
			return
		}

		// 获取链接标题
		title := strings.TrimSpace(s.Find(".down-list3 a").Text())
		if title == "" {
			return
		}

		// 验证链接有效性
		if !p.isValidLink(linkURL) {
			return
		}

		// 去重检查
		if seenURLs[linkURL] {
			return
		}
		seenURLs[linkURL] = true

		// 判断链接类型
		linkType := p.determineLinkType(linkURL)

		// 提取密码
		password := p.extractPassword(linkURL, title)

		// 创建链接对象
		link := model.Link{
			Type:     linkType,
			URL:      linkURL,
			Password: password,
		}

		links = append(links, link)
	})

	return links
}

// isValidLink 验证链接是否有效
func (p *PiankuPlugin) isValidLink(url string) bool {
	// 检查是否为磁力链接
	if magnetLinkRegex.MatchString(url) {
		return true
	}

	// 检查是否为ED2K链接
	if ed2kLinkRegex.MatchString(url) {
		return true
	}

	// 检查是否为有效的网盘链接
	for _, regex := range panLinkRegexes {
		if regex.MatchString(url) {
			return true
		}
	}

	// 如果都不匹配，则不是有效链接
	return false
}

// determineLinkType 判断链接类型
func (p *PiankuPlugin) determineLinkType(url string) string {
	// 检查磁力链接
	if magnetLinkRegex.MatchString(url) {
		return "magnet"
	}

	// 检查ED2K链接
	if ed2kLinkRegex.MatchString(url) {
		return "ed2k"
	}

	// 检查网盘链接
	for panType, regex := range panLinkRegexes {
		if regex.MatchString(url) {
			return panType
		}
	}

	return "others"
}

// extractPassword 提取密码
func (p *PiankuPlugin) extractPassword(url, title string) string {
	// 首先从链接URL中提取密码
	for _, regex := range passwordRegexes {
		if matches := regex.FindStringSubmatch(url); len(matches) > 1 {
			return matches[1]
		}
	}

	// 然后从标题文本中提取密码
	for _, regex := range passwordRegexes {
		if matches := regex.FindStringSubmatch(title); len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}
