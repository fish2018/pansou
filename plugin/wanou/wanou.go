package wanou

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	// 默认超时时间 - 优化为更短时间
	DefaultTimeout = 8 * time.Second

	// HTTP连接池配置
	MaxIdleConns        = 200
	MaxIdleConnsPerHost = 50
	MaxConnsPerHost     = 100
	IdleConnTimeout     = 90 * time.Second
)

// 性能统计（原子操作）
var (
	searchRequests  int64 = 0
	totalSearchTime int64 = 0 // 纳秒
)

func init() {
	plugin.RegisterGlobalPlugin(NewWanouPlugin())
}

// 预编译的正则表达式
var (
	// 密码提取正则表达式
	passwordRegex = regexp.MustCompile(`\?pwd=([0-9a-zA-Z]+)`)

	// 常见网盘链接的正则表达式（支持16种类型）
	quarkLinkRegex  = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	ucLinkRegex     = regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9a-zA-Z]+(\?[^"'\s]*)?`)
	baiduLinkRegex  = regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+(\?pwd=[0-9a-zA-Z]+)?`)
	aliyunLinkRegex = regexp.MustCompile(`https?://(www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9a-zA-Z]+`)
	xunleiLinkRegex = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+(\?pwd=[0-9a-zA-Z]+)?`)
	tianyiLinkRegex = regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z]+`)
	link115Regex    = regexp.MustCompile(`https?://115\.com/s/[0-9a-zA-Z]+`)
	mobileLinkRegex = regexp.MustCompile(`https?://caiyun\.feixin\.10086\.cn/[0-9a-zA-Z]+`)
	link123Regex    = regexp.MustCompile(`https?://123pan\.com/s/[0-9a-zA-Z]+`)
	pikpakLinkRegex = regexp.MustCompile(`https?://mypikpak\.com/s/[0-9a-zA-Z]+`)
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}`)
	ed2kLinkRegex   = regexp.MustCompile(`ed2k://\|file\|.+\|\d+\|[0-9a-fA-F]{32}\|/`)
)

// WanouAsyncPlugin Wanou异步插件
type WanouAsyncPlugin struct {
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
	}

	return &http.Client{
		Transport: transport,
		Timeout:   DefaultTimeout,
	}
}

// NewWanouPlugin 创建新的Wanou异步插件
func NewWanouPlugin() *WanouAsyncPlugin {
	return &WanouAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("wanou", 1),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 同步搜索接口
func (p *WanouAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 带结果统计的搜索接口
func (p *WanouAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现
func (p *WanouAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 性能统计
	start := time.Now()
	atomic.AddInt64(&searchRequests, 1)
	defer func() {
		duration := time.Since(start).Nanoseconds()
		atomic.AddInt64(&totalSearchTime, duration)
	}()

	// 使用优化的客户端
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}

	// 构建API搜索URL
	searchURL := fmt.Sprintf("https://woog.nxog.eu.org/api.php/provide/vod?ac=detail&wd=%s", url.QueryEscape(keyword))

	// 创建HTTP请求
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://woog.nxog.eu.org/")
	req.Header.Set("Cache-Control", "no-cache")

	// 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 解析JSON响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	var apiResponse WanouAPIResponse
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, fmt.Errorf("[%s] 解析JSON响应失败: %w", p.Name(), err)
	}

	// 检查API响应状态
	if apiResponse.Code != 1 {
		return nil, fmt.Errorf("[%s] API返回错误: %s", p.Name(), apiResponse.Msg)
	}

	// 解析搜索结果
	var results []model.SearchResult
	for _, item := range apiResponse.List {
		if result := p.parseAPIItem(item); result.Title != "" {
			results = append(results, result)
		}
	}

	return results, nil
}

// WanouAPIResponse API响应结构
type WanouAPIResponse struct {
	Code      int            `json:"code"`
	Msg       string         `json:"msg"`
	Page      int            `json:"page"`
	PageCount int            `json:"pagecount"`
	Limit     int            `json:"limit"`
	Total     int            `json:"total"`
	List      []WanouAPIItem `json:"list"`
}

// WanouAPIItem API数据项
type WanouAPIItem struct {
	VodID       int    `json:"vod_id"`
	VodName     string `json:"vod_name"`
	VodActor    string `json:"vod_actor"`
	VodDirector string `json:"vod_director"`
	VodDownFrom string `json:"vod_down_from"`
	VodDownURL  string `json:"vod_down_url"`
	VodRemarks  string `json:"vod_remarks"`
	VodPubdate  string `json:"vod_pubdate"`
	VodArea     string `json:"vod_area"`
	VodYear     string `json:"vod_year"`
	VodContent  string `json:"vod_content"`
	VodPic      string `json:"vod_pic"`
}

// parseAPIItem 解析API数据项
func (p *WanouAsyncPlugin) parseAPIItem(item WanouAPIItem) model.SearchResult {
	// 构建唯一ID
	uniqueID := fmt.Sprintf("%s-%d", p.Name(), item.VodID)

	// 构建标题
	title := strings.TrimSpace(item.VodName)
	if title == "" {
		return model.SearchResult{}
	}

	// 构建描述
	var contentParts []string
	if item.VodActor != "" {
		contentParts = append(contentParts, fmt.Sprintf("主演: %s", item.VodActor))
	}
	if item.VodDirector != "" {
		contentParts = append(contentParts, fmt.Sprintf("导演: %s", item.VodDirector))
	}
	if item.VodArea != "" {
		contentParts = append(contentParts, fmt.Sprintf("地区: %s", item.VodArea))
	}
	if item.VodYear != "" {
		contentParts = append(contentParts, fmt.Sprintf("年份: %s", item.VodYear))
	}
	if item.VodRemarks != "" {
		contentParts = append(contentParts, fmt.Sprintf("状态: %s", item.VodRemarks))
	}
	content := strings.Join(contentParts, " | ")

	// 解析下载链接
	links := p.parseDownloadLinks(item.VodDownFrom, item.VodDownURL)

	// 提取封面图片
	var images []string
	if item.VodPic != "" {
		images = append(images, item.VodPic)
	}

	// 构建标签
	var tags []string
	if item.VodYear != "" {
		tags = append(tags, item.VodYear)
	}
	if item.VodArea != "" {
		tags = append(tags, item.VodArea)
	}

	return model.SearchResult{
		UniqueID: uniqueID,
		Title:    title,
		Content:  content,
		Links:    links,
		Tags:     tags,
		Images:   images,
		Channel:  "",          // 插件搜索结果Channel为空
		Datetime: time.Time{}, // 使用零值而不是nil，参考jikepan插件标准
	}
}

// parseDownloadLinks 解析下载链接
func (p *WanouAsyncPlugin) parseDownloadLinks(vodDownFrom, vodDownURL string) []model.Link {
	if vodDownFrom == "" || vodDownURL == "" {
		return nil
	}

	// 按$$$分隔
	fromParts := strings.Split(vodDownFrom, "$$$")
	urlParts := strings.Split(vodDownURL, "$$$")

	// 确保数组长度一致
	minLen := len(fromParts)
	if len(urlParts) < minLen {
		minLen = len(urlParts)
	}

	var links []model.Link
	for i := 0; i < minLen; i++ {
		fromType := strings.TrimSpace(fromParts[i])
		urlStr := strings.TrimSpace(urlParts[i])

		if urlStr == "" {
			continue
		}

		// 直接确定链接类型（合并验证和类型判断，避免重复正则匹配）
		linkType := p.determineLinkTypeOptimized(fromType, urlStr)
		if linkType == "" {
			continue
		}

		// 提取密码
		password := p.extractPassword(urlStr)

		links = append(links, model.Link{
			Type:     linkType,
			URL:      urlStr,
			Password: password,
		})
	}

	return links
}

// determineLinkTypeOptimized 优化的链接类型判断（避免重复正则匹配）
func (p *WanouAsyncPlugin) determineLinkTypeOptimized(apiType, url string) string {
	// 基本验证（包含原 isValidNetworkDriveURL 的逻辑）
	if strings.Contains(url, "javascript:") ||
		strings.Contains(url, "#") ||
		url == "" ||
		(!strings.HasPrefix(url, "http") && !strings.HasPrefix(url, "magnet:") && !strings.HasPrefix(url, "ed2k:")) {
		return ""
	}

	// 优先根据API标识快速映射（避免正则匹配）
	switch strings.ToUpper(apiType) {
	case "BD":
		if baiduLinkRegex.MatchString(url) {
			return "baidu"
		}
	case "KG":
		if quarkLinkRegex.MatchString(url) {
			return "quark"
		}
	case "UC":
		if ucLinkRegex.MatchString(url) {
			return "uc"
		}
	case "ALY":
		if aliyunLinkRegex.MatchString(url) {
			return "aliyun"
		}
	case "XL":
		if xunleiLinkRegex.MatchString(url) {
			return "xunlei"
		}
	case "TY":
		if tianyiLinkRegex.MatchString(url) {
			return "tianyi"
		}
	case "115":
		if link115Regex.MatchString(url) {
			return "115"
		}
	case "MB":
		if mobileLinkRegex.MatchString(url) {
			return "mobile"
		}
	case "123":
		if link123Regex.MatchString(url) {
			return "123"
		}
	case "PIKPAK":
		if pikpakLinkRegex.MatchString(url) {
			return "pikpak"
		}
	}

	// 如果API标识匹配失败，回退到URL正则匹配（一次性匹配）
	switch {
	case baiduLinkRegex.MatchString(url):
		return "baidu"
	case ucLinkRegex.MatchString(url):
		return "uc"
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
	case quarkLinkRegex.MatchString(url):
		return "quark"
	default:
		return "" // 不支持的类型
	}
}

// determineLinkType 根据URL确定链接类型（支持16种类型）
func (p *WanouAsyncPlugin) determineLinkType(url string) string {
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
		return "" // 不支持的类型返回空字符串
	}
}

// extractPassword 从URL中提取密码
func (p *WanouAsyncPlugin) extractPassword(url string) string {
	matches := passwordRegex.FindStringSubmatch(url)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// doRequestWithRetry 带重试的HTTP请求（优化JSON API的重试策略）
func (p *WanouAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 2 // 对于JSON API减少重试次数
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				return resp, nil
			}
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP状态码: %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		// JSON API快速重试：只等待很短时间
		if i < maxRetries-1 {
			time.Sleep(100 * time.Millisecond) // 从秒级改为100毫秒
		}
	}

	return nil, fmt.Errorf("[%s] 请求失败，重试%d次后仍失败: %w", p.Name(), maxRetries, lastErr)
}

// GetPerformanceStats 获取性能统计信息
func (p *WanouAsyncPlugin) GetPerformanceStats() map[string]interface{} {
	totalRequests := atomic.LoadInt64(&searchRequests)
	totalTime := atomic.LoadInt64(&totalSearchTime)

	var avgTime float64
	if totalRequests > 0 {
		avgTime = float64(totalTime) / float64(totalRequests) / 1e6 // 转换为毫秒
	}

	return map[string]interface{}{
		"search_requests":      totalRequests,
		"avg_search_time_ms":   avgTime,
		"total_search_time_ns": totalTime,
	}
}
