package cyg

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 预编译的正则表达式（性能优化）
var (
	// 常见网盘链接的正则表达式（支持15+种类型）
	quarkLinkRegex  = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	ucLinkRegex     = regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-zA-Z]+`)
	baiduLinkRegex  = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+`)
	aliyunLinkRegex = regexp.MustCompile(`https?://(www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9a-zA-Z]+`)
	xunleiLinkRegex = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+`)
	tianyiLinkRegex = regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z]+`)
	link115Regex    = regexp.MustCompile(`https?://115\.com/s/[0-9a-zA-Z]+`)
	mobileLinkRegex = regexp.MustCompile(`https?://(caiyun\.feixin\.10086\.cn|caiyun\.139\.com|yun\.139\.com|cloud\.139\.com|pan\.139\.com)/.*`)
	link123Regex    = regexp.MustCompile(`https?://123pan\.com/s/[0-9a-zA-Z]+`)
	pikpakLinkRegex = regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z]+`)
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}`)
	ed2kLinkRegex   = regexp.MustCompile(`ed2k://\|file\|.+\|\d+\|[0-9a-fA-F]{32}\|/`)

	// HTML标签清理
	htmlTagRegex = regexp.MustCompile(`<[^>]*>`)
)

// CygPlugin CYG插件结构体
type CygPlugin struct {
	*plugin.BaseAsyncPlugin
}

const cygBaseURL = "https://www.acgndog.com"

// CygPost 搜索结果结构体
type CygPost struct {
	ID    int    `json:"id"`
	Date  string `json:"date"`
	Title struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Excerpt struct {
		Rendered string `json:"rendered"`
	} `json:"excerpt"`
	Link         string `json:"link"`
	CategoryName string `json:"category_name"`
	AuthorName   string `json:"author_name"`
	Pageviews    int    `json:"pageviews"`
	LikeCount    int    `json:"like_count"`
}

// CygDownload 下载链接结构体
type CygDownload struct {
	Name        string `json:"name"`        // 网盘类型名称
	URL         string `json:"url"`         // 网盘链接
	DownloadPwd string `json:"downloadPwd"` // 提取密码
	ExtractPwd  string `json:"extractPwd"`  // 解压密码
	ID          string `json:"id"`          // 链接ID
}

// CygSearchOptions 搜索选项
type CygSearchOptions struct {
	PerPage int    // 每页结果数 (默认: 20)
	Page    int    // 页码 (默认: 1)
	OrderBy string // 排序字段 (默认: date)
	Order   string // 排序方向 (默认: desc)
}

// init 注册插件
func init() {
	p := &CygPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("cyg", 3), // 优先级3，标准质量数据源
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *CygPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果（推荐方法）
func (p *CygPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现逻辑
func (p *CygPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 解析扩展参数
	opts := p.parseExtOptions(ext)

	// 1. 构建搜索URL
	searchURL := fmt.Sprintf(cygBaseURL+"/wp-json/wp/v2/posts?per_page=%d&orderby=%s&order=%s&page=%d&search=%s",
		opts.PerPage, opts.OrderBy, opts.Order, opts.Page, url.QueryEscape(keyword))

	// 2. 发送搜索请求
	posts, err := p.fetchSearchResults(client, searchURL)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	if len(posts) == 0 {
		return []model.SearchResult{}, nil
	}

	// 3. 并发获取每个帖子的下载链接
	results := p.fetchDownloadLinksAsync(client, posts, keyword)

	// 4. 关键词过滤
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	return filteredResults, nil
}

// fetchSearchResults 获取搜索结果列表
func (p *CygPlugin) fetchSearchResults(client *http.Client, searchURL string) ([]CygPost, error) {
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("HTTP请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP错误状态码: %d", resp.StatusCode)
	}

	// 解析响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var posts []CygPost
	if err := json.Unmarshal(body, &posts); err != nil {
		return nil, fmt.Errorf("JSON解析失败: %w", err)
	}

	return posts, nil
}

// fetchDownloadLinksAsync 并发获取下载链接
func (p *CygPlugin) fetchDownloadLinksAsync(client *http.Client, posts []CygPost, keyword string) []model.SearchResult {
	var wg sync.WaitGroup
	resultChan := make(chan model.SearchResult, len(posts))

	// 限制并发数量
	semaphore := make(chan struct{}, 10) // 最多10个并发

	for _, post := range posts {
		wg.Add(1)
		go func(p *CygPlugin, post CygPost) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 获取下载链接
			links, err := p.getDownloadLinks(client, post.ID)
			if err != nil {
				// 记录错误但不影响其他结果
				return
			}

			// 只返回有效链接的结果
			if len(links) > 0 {
				result := p.convertToSearchResult(post, links)
				resultChan <- result
			}
		}(p, post)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集结果
	var results []model.SearchResult
	for result := range resultChan {
		results = append(results, result)
	}

	return results
}

// getDownloadLinks 获取指定帖子的下载链接
func (p *CygPlugin) getDownloadLinks(client *http.Client, postID int) ([]model.Link, error) {
	// 构建下载链接获取URL
	downloadURL := fmt.Sprintf(cygBaseURL+"/wp-json/acg-studio/v1/download?id=%d", postID)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建下载链接请求失败: %w", err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("下载链接请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("下载链接请求状态码: %d", resp.StatusCode)
	}

	// 解析响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取下载链接响应失败: %w", err)
	}

	var downloadData []CygDownload
	if err := json.Unmarshal(body, &downloadData); err != nil {
		return nil, fmt.Errorf("下载链接JSON解析失败: %w", err)
	}

	// 转换为model.Link格式
	return p.convertToLinks(downloadData), nil
}

// convertToSearchResult 转换为标准搜索结果格式
func (p *CygPlugin) convertToSearchResult(post CygPost, links []model.Link) model.SearchResult {
	return model.SearchResult{
		UniqueID: fmt.Sprintf("cyg-%d", post.ID),
		Title:    p.cleanHTML(post.Title.Rendered),
		Content:  p.cleanHTML(post.Excerpt.Rendered),
		Datetime: p.parseDateTime(post.Date),
		Tags:     []string{post.CategoryName},
		Links:    links,
		Channel:  "", // 插件搜索结果必须为空字符串
	}
}

// convertToLinks 转换下载链接数据
func (p *CygPlugin) convertToLinks(downloadData []CygDownload) []model.Link {
	links := make([]model.Link, 0, len(downloadData))
	for _, item := range downloadData {
		// 优先使用URL模式匹配，fallback到名称映射
		linkType := p.determineCloudTypeByURL(item.URL)
		if linkType == "others" {
			linkType = p.determineCloudType(item.Name)
		}

		link := model.Link{
			Type:     linkType,
			URL:      item.URL,
			Password: item.DownloadPwd, // 提取密码
		}
		links = append(links, link)
	}
	return links
}

// determineCloudTypeByURL 根据URL确定网盘类型（支持15+种类型）
func (p *CygPlugin) determineCloudTypeByURL(url string) string {
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
	case link123Regex.MatchString(url):
		return "123"
	case pikpakLinkRegex.MatchString(url):
		return "pikpak"
	case magnetLinkRegex.MatchString(url):
		return "magnet"
	case ed2kLinkRegex.MatchString(url):
		return "ed2k"
	default:
		return "others"
	}
}

// determineCloudType 根据名称确定网盘类型（支持15+种网盘类型的名称映射）
func (p *CygPlugin) determineCloudType(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "夸克", "夸克网盘":
		return "quark"
	case "uc", "uc网盘":
		return "uc"
	case "百度网盘", "百度", "baidu":
		return "baidu"
	case "阿里云盘", "阿里", "aliyun", "阿里网盘":
		return "aliyun"
	case "迅雷", "迅雷网盘", "xunlei":
		return "xunlei"
	case "天翼", "天翼云盘", "189", "189云盘":
		return "tianyi"
	case "115", "115网盘":
		return "115"
	case "移动云盘", "移动", "mobile", "和彩云", "139云盘", "139", "中国移动云盘":
		return "mobile"
	case "123网盘", "123pan", "123":
		return "123"
	case "pikpak", "pikpak网盘":
		return "pikpak"
	case "磁力链接", "magnet":
		return "magnet"
	case "ed2k":
		return "ed2k"
	default:
		return "others"
	}
}

// setRequestHeaders 设置请求头
func (p *CygPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("Referer", cygBaseURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Mobile/15E148 Safari/604.1")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *CygPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
		if err == nil {
			lastErr = fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
		}
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// parseExtOptions 从ext参数中解析搜索选项
func (p *CygPlugin) parseExtOptions(ext map[string]interface{}) CygSearchOptions {
	opts := CygSearchOptions{
		PerPage: 20,
		Page:    1,
		OrderBy: "date",
		Order:   "desc",
	}

	if ext == nil {
		return opts
	}

	if perPage, ok := ext["per_page"].(int); ok && perPage > 0 {
		opts.PerPage = perPage
	}

	if page, ok := ext["page"].(int); ok && page > 0 {
		opts.Page = page
	}

	if orderBy, ok := ext["order_by"].(string); ok && orderBy != "" {
		opts.OrderBy = orderBy
	}

	if order, ok := ext["order"].(string); ok && order != "" {
		opts.Order = order
	}

	return opts
}

// cleanHTML 清理HTML标签和实体编码
func (p *CygPlugin) cleanHTML(htmlContent string) string {
	// 移除HTML标签
	text := htmlTagRegex.ReplaceAllString(htmlContent, "")

	// 解码HTML实体
	text = html.UnescapeString(text)

	// 清理多余空白
	text = strings.TrimSpace(text)

	// 替换多个空白字符为单个空格
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")

	return text
}

// parseDateTime 解析时间字符串
func (p *CygPlugin) parseDateTime(dateStr string) time.Time {
	// 尝试解析ISO 8601格式
	if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
		return t
	}

	// 尝试解析其他常见格式
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}

	// 解析失败时返回当前时间
	return time.Now()
}
