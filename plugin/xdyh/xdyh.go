package xdyh

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
	"strings"
	"sync"
	"time"
)

// 预编译的常量
const (
	pluginName = "xdyh"
	apiURL     = "https://ys.66ds.de/search"
	refererURL = "https://ys.66ds.de/"

	// 超时时间配置
	DefaultTimeout = 15 * time.Second // API聚合搜索需要更长时间

	// 并发数配置
	MaxConcurrency = 10

	// HTTP连接池配置
	MaxIdleConns        = 100
	MaxIdleConnsPerHost = 30
	MaxConnsPerHost     = 50
	IdleConnTimeout     = 90 * time.Second
)

// 缓存相关
var (
	searchCache     = sync.Map{} // 缓存搜索结果
	lastCleanupTime = time.Now()
	cacheTTL        = 30 * time.Minute // API搜索结果缓存时间相对较短
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewXdyhPlugin())

	// 启动缓存清理goroutine
	go startCacheCleaner()
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	ticker := time.NewTicker(20 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		// 清空所有缓存
		searchCache = sync.Map{}
		lastCleanupTime = time.Now()
	}
}

// XdyhAsyncPlugin XDYH异步插件
type XdyhAsyncPlugin struct {
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
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: transport, Timeout: DefaultTimeout}
}

// NewXdyhPlugin 创建新的XDYH异步插件
func NewXdyhPlugin() *XdyhAsyncPlugin {
	return &XdyhAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 兼容性方法，实际调用SearchWithResult
func (p *XdyhAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XdyhAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 具体的搜索实现
func (p *XdyhAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 检查缓存
	cacheKey := fmt.Sprintf("%s_%s", pluginName, keyword)
	if cached, ok := searchCache.Load(cacheKey); ok {
		if results, ok := cached.([]model.SearchResult); ok {
			return results, nil
		}
	}

	// 2. 构建请求体
	requestBody := SearchRequest{
		Keyword:    keyword,
		Sites:      nil, // null表示搜索所有站点
		MaxWorkers: 10,  // API默认并发数
		SaveToFile: false,
		SplitLinks: true,
	}

	// 3. JSON序列化
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("[%s] JSON序列化失败: %w", pluginName, err)
	}

	// 4. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 5. 创建请求
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", pluginName, err)
	}

	// 6. 设置请求头
	p.setRequestHeaders(req)

	// 7. 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", pluginName, err)
	}
	defer resp.Body.Close()

	// 8. 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", pluginName, resp.StatusCode)
	}

	// 9. 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", pluginName, err)
	}

	// 10. 解析JSON响应
	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("[%s] JSON解析失败: %w", pluginName, err)
	}

	// 11. 检查API响应状态
	if apiResp.Status != "success" {
		return nil, fmt.Errorf("[%s] API返回错误状态: %s", pluginName, apiResp.Status)
	}

	// 12. 转换为标准格式
	results := p.convertToSearchResults(apiResp, keyword)

	// 13. 缓存结果
	if len(results) > 0 {
		searchCache.Store(cacheKey, results)
	}

	// 14. 关键词过滤
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// setRequestHeaders 设置HTTP请求头
func (p *XdyhAsyncPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", refererURL)
	req.Header.Set("Origin", "https://ys.66ds.de")
	req.Header.Set("Cache-Control", "max-age=0")
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *XdyhAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
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

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// convertToSearchResults 将API响应转换为标准搜索结果
func (p *XdyhAsyncPlugin) convertToSearchResults(apiResp APIResponse, keyword string) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(apiResp.Data))
	seenTitles := make(map[string]bool) // 去重用

	for i, item := range apiResp.Data {
		// 简单去重处理
		titleKey := fmt.Sprintf("%s_%s", item.Title, item.SourceSite)
		if seenTitles[titleKey] {
			continue
		}
		seenTitles[titleKey] = true

		// 转换链接
		links := p.convertDriveLinks(item)
		if len(links) == 0 {
			continue // 跳过没有有效链接的结果
		}

		// 解析时间
		datetime := p.parseDateTime(item.PostDate)

		// 构建内容描述
		content := p.buildContentDescription(item)

		// 提取标签
		tags := p.extractTags(item.Title, item.SourceSite)

		// 创建搜索结果
		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%d", pluginName, i),
			Title:    item.Title,
			Content:  content,
			Datetime: datetime,
			Tags:     tags,
			Links:    links,
			Channel:  "", // 插件搜索结果必须为空字符串
		}

		results = append(results, result)
	}

	return results
}

// convertDriveLinks 转换网盘链接
func (p *XdyhAsyncPlugin) convertDriveLinks(item SearchResultItem) []model.Link {
	links := make([]model.Link, 0, len(item.DriveLinks))

	for _, driveURL := range item.DriveLinks {
		if driveURL == "" {
			continue
		}

		// 验证链接有效性
		if !p.isValidURL(driveURL) {
			continue
		}

		// 确定网盘类型
		linkType := p.determineCloudType(driveURL)

		// 创建链接对象
		link := model.Link{
			Type:     linkType,
			URL:      driveURL,
			Password: item.Password, // API已提供密码字段
		}

		links = append(links, link)
	}

	return links
}

// parseDateTime 解析日期时间
func (p *XdyhAsyncPlugin) parseDateTime(dateStr string) time.Time {
	// 尝试不同的时间格式
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}

	// 解析失败时返回当前时间
	return time.Now()
}

// buildContentDescription 构建内容描述
func (p *XdyhAsyncPlugin) buildContentDescription(item SearchResultItem) string {
	parts := []string{}

	// 来源站点
	if item.SourceSite != "" {
		parts = append(parts, fmt.Sprintf("来源: %s", item.SourceSite))
	}

	// 链接数量
	if item.LinkCount > 0 {
		parts = append(parts, fmt.Sprintf("链接数: %d", item.LinkCount))
	}

	// 密码信息
	if item.HasPassword && item.Password != "" {
		parts = append(parts, fmt.Sprintf("密码: %s", item.Password))
	}

	// 文件预览
	if item.FilePreview != "" {
		preview := strings.ReplaceAll(item.FilePreview, "<em>", "")
		preview = strings.ReplaceAll(preview, "</em>", "")
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		parts = append(parts, fmt.Sprintf("预览: %s", preview))
	}

	return strings.Join(parts, " | ")
}

// extractTags 提取标签
func (p *XdyhAsyncPlugin) extractTags(title, sourceSite string) []string {
	tags := []string{}

	// 添加来源站点作为标签
	if sourceSite != "" {
		tags = append(tags, sourceSite)
	}

	// 从标题中提取常见标签
	title = strings.ToLower(title)
	tagKeywords := map[string]string{
		"4k":    "4K",
		"1080p": "1080P",
		"720p":  "720P",
		"蓝光":    "蓝光",
		"高清":    "高清",
		"更新":    "更新中",
		"完结":    "完结",
		"电影":    "电影",
		"剧集":    "剧集",
		"动漫":    "动漫",
		"综艺":    "综艺",
	}

	for keyword, tag := range tagKeywords {
		if strings.Contains(title, keyword) {
			tags = append(tags, tag)
		}
	}

	return tags
}

// isValidURL 验证URL是否有效
func (p *XdyhAsyncPlugin) isValidURL(urlStr string) bool {
	if urlStr == "" {
		return false
	}

	// 检查基本的URL格式
	if strings.HasPrefix(urlStr, "http://") || strings.HasPrefix(urlStr, "https://") {
		// HTTP/HTTPS链接需要有域名
		if len(urlStr) <= 8 || urlStr == "http://" || urlStr == "https://" {
			return false
		}
		// 简单检查是否包含域名
		return strings.Contains(urlStr[8:], ".")
	}

	return false
}

// determineCloudType 确定网盘类型
func (p *XdyhAsyncPlugin) determineCloudType(url string) string {
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
	case strings.Contains(url, "115.com") || strings.Contains(url, "115cdn.com"):
		return "115"
	case strings.Contains(url, "123pan.com"):
		return "123"
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	default:
		return "others"
	}
}

// API请求结构体
type SearchRequest struct {
	Keyword    string      `json:"keyword"`
	Sites      interface{} `json:"sites"` // null or []string
	MaxWorkers int         `json:"max_workers"`
	SaveToFile bool        `json:"save_to_file"`
	SplitLinks bool        `json:"split_links"`
}

// API响应结构体
type APIResponse struct {
	Status          string             `json:"status"`
	Keyword         string             `json:"keyword"`
	SearchTimestamp string             `json:"search_timestamp"`
	Summary         Summary            `json:"summary"`
	SuccessfulSites []string           `json:"successful_sites"`
	FailedSites     []string           `json:"failed_sites"`
	Data            []SearchResultItem `json:"data"`
	Performance     Performance        `json:"performance"`
}

type Summary struct {
	TotalSitesSearched    int `json:"total_sites_searched"`
	SuccessfulSites       int `json:"successful_sites"`
	FailedSites           int `json:"failed_sites"`
	TotalSearchResults    int `json:"total_search_results"`
	TotalSuccessfulParses int `json:"total_successful_parses"`
	TotalDriveLinks       int `json:"total_drive_links"`
	UniqueLinks           int `json:"unique_links"`
}

type SearchResultItem struct {
	Title       string   `json:"title"`
	PostDate    string   `json:"post_date"`
	DriveLinks  []string `json:"drive_links"`
	HasLinks    bool     `json:"has_links"`
	LinkCount   int      `json:"link_count"`
	Password    string   `json:"password,omitempty"`
	HasPassword bool     `json:"has_password,omitempty"`
	SourceSite  string   `json:"source_site"`
	SourceAPI   string   `json:"source_api,omitempty"`
	FilePreview string   `json:"file_preview,omitempty"`
}

type Performance struct {
	TotalSearchTime float64 `json:"total_search_time"`
	SitesSearched   int     `json:"sites_searched"`
	AvgTimePerSite  float64 `json:"avg_time_per_site"`
	Optimization    string  `json:"optimization"`
	Timestamp       string  `json:"timestamp"`
}
