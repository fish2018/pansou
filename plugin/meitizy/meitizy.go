package meitizy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	PluginName  = "meitizy"
	DisplayName = "美体资源"
	Description = "美体资源 - 影视资源网盘链接搜索"
	// The frontend and API are hosted on separate domains since the 2026 migration.
	BaseURL        = "https://apis.451024.xyz"
	FrontendURL    = "https://video.451024.xyz"
	SearchPath     = "/api/media/search"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults     = 100
	RequestTimeout = 30 * time.Second
	MaxPageSize    = 10 // 新版 API 仅接受网页使用的 size=10

	// HTTP连接池配置（性能优化）
	MaxIdleConns          = 100
	MaxIdleConnsPerHost   = 30
	MaxConnsPerHost       = 50
	IdleConnTimeout       = 90 * time.Second
	TLSHandshakeTimeout   = 10 * time.Second
	ExpectContinueTimeout = 1 * time.Second
)

// MeitizyPlugin 美体资源插件
type MeitizyPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client // 优化的HTTP客户端（连接池）
}

// API请求结构
type searchRequest struct {
	Title string `json:"title"`
	Page  int    `json:"page"`
	Size  int    `json:"size"`
}

// API响应结构
type searchResponse struct {
	Data  []apiItem `json:"data"`
	Total int       `json:"total"`
}

// API返回的单个结果项
type apiItem struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Link      string `json:"link"`
	LinkType  string `json:"link_type"`
	Tags      string `json:"tags"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewMeitizyPlugin())
}

// NewMeitizyPlugin 创建新的美体资源插件实例
func NewMeitizyPlugin() *MeitizyPlugin {
	p := &MeitizyPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 2), // 质量良好，优先级2
		optimizedClient: createOptimizedHTTPClient(),
	}

	return p
}

// createOptimizedHTTPClient 创建优化的HTTP客户端（连接池配置）
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          MaxIdleConns,
		MaxIdleConnsPerHost:   MaxIdleConnsPerHost,
		MaxConnsPerHost:       MaxConnsPerHost,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ExpectContinueTimeout: ExpectContinueTimeout,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     false,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   RequestTimeout,
	}
}

// Name 插件名称
func (p *MeitizyPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *MeitizyPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *MeitizyPlugin) Description() string {
	return Description
}

// Search 搜索接口
func (p *MeitizyPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *MeitizyPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现
func (p *MeitizyPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 构建请求体
	reqBody := searchRequest{
		Title: keyword,
		Page:  1,
		Size:  MaxPageSize,
	}

	// 序列化请求体
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("[%s] 序列化请求体失败: %w", p.Name(), err)
	}

	// 构建请求URL
	apiURL := BaseURL + SearchPath

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	// 创建POST请求
	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Origin", FrontendURL)
	req.Header.Set("Referer", FrontendURL+"/")

	// 使用优化的客户端发送请求（带重试）
	resp, err := p.doRequestWithRetry(req, p.optimizedClient)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求HTTP状态错误: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应体失败: %w", p.Name(), err)
	}

	// 解析JSON响应
	var apiResp searchResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("[%s] JSON解析失败: %w", p.Name(), err)
	}

	// 转换为标准格式
	results := p.convertToSearchResults(apiResp.Data)

	// 关键词过滤（标准网盘插件需要过滤）
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	return filteredResults, nil
}

// convertToSearchResults 将API响应转换为标准SearchResult格式
func (p *MeitizyPlugin) convertToSearchResults(items []apiItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for _, item := range items {
		// Skip malformed or empty links returned by the API.
		linkURL := strings.TrimSpace(item.Link)
		if linkURL == "" {
			continue
		}
		parsedURL, err := url.Parse(linkURL)
		if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
			continue
		}

		// 解析发布时间
		publishTime := p.parseTime(item.CreatedAt)
		if publishTime.IsZero() {
			publishTime = p.parseTime(item.UpdatedAt)
		}
		if publishTime.IsZero() {
			publishTime = time.Now()
		}

		// 映射网盘类型
		linkType := p.mapLinkType(item.LinkType)
		// 如果无法从link_type识别，尝试从URL中识别
		if linkType == "others" {
			linkType = p.determineCloudTypeFromURL(linkURL)
		}

		// 构建链接
		links := []model.Link{
			{
				Type:     linkType,
				URL:      linkURL,
				Password: passwordFromURL(parsedURL),
			},
		}

		// 构建标签
		var tags []string
		if item.Tags != "" {
			tags = []string{item.Tags}
		}

		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%d", p.Name(), item.ID),
			Title:    item.Title,
			Content:  item.Content,
			Channel:  "", // 插件搜索结果必须为空字符串
			Datetime: publishTime,
			Links:    links,
			Tags:     tags,
		}

		results = append(results, result)
	}

	return results
}

func passwordFromURL(linkURL *url.URL) string {
	for _, key := range []string{"pwd", "password", "passcode", "code"} {
		if value := strings.TrimSpace(linkURL.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// mapLinkType 映射API返回的link_type到系统网盘类型
func (p *MeitizyPlugin) mapLinkType(apiLinkType string) string {
	switch strings.ToLower(apiLinkType) {
	case "alipan":
		return "aliyun"
	case "xunlei":
		return "xunlei"
	case "baidu":
		return "baidu"
	case "quark":
		return "quark"
	case "uc":
		return "uc"
	case "115":
		return "115"
	case "123":
		return "123"
	case "tianyi":
		return "tianyi"
	case "mobile":
		return "mobile"
	case "pikpak":
		return "pikpak"
	default:
		// 如果无法识别，返回others，后续会从URL中判断
		return "others"
	}
}

// determineCloudTypeFromURL 从URL中自动识别网盘类型（备选方案）
func (p *MeitizyPlugin) determineCloudTypeFromURL(url string) string {
	switch {
	case strings.Contains(url, "pan.quark.cn"):
		return "quark"
	case strings.Contains(url, "drive.uc.cn"):
		return "uc"
	case strings.Contains(url, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(url, "aliyundrive.com") || strings.Contains(url, "alipan.com") || strings.Contains(url, "www.alipan.com"):
		return "aliyun"
	case strings.Contains(url, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(url, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "115.com") || strings.Contains(url, "115cdn.com") || strings.Contains(url, "anxia.com"):
		return "115"
	case strings.Contains(url, "123684.com") || strings.Contains(url, "123685.com") ||
		strings.Contains(url, "123912.com") || strings.Contains(url, "123pan.com") ||
		strings.Contains(url, "123pan.cn") || strings.Contains(url, "123592.com"):
		return "123"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	case strings.Contains(url, "magnet:"):
		return "magnet"
	case strings.Contains(url, "ed2k://"):
		return "ed2k"
	default:
		return "others"
	}
}

// parseTime 解析ISO 8601格式的时间字符串
func (p *MeitizyPlugin) parseTime(timeStr string) time.Time {
	if timeStr == "" {
		return time.Time{}
	}

	// 尝试多种时间格式
	timeFormats := []string{
		time.RFC3339,               // 2006-01-02T15:04:05Z07:00
		"2006-01-02T15:04:05.000Z", // 2025-11-25T22:59:53.000Z
		"2006-01-02T15:04:05Z",     // 2006-01-02T15:04:05Z
		"2006-01-02 15:04:05",      // 2006-01-02 15:04:05
		"2006-01-02",               // 2006-01-02
	}

	for _, format := range timeFormats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t
		}
	}

	return time.Time{}
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *MeitizyPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题（需要重新创建body）
		reqClone := req.Clone(req.Context())
		if req.Body != nil {
			// 读取原始body
			bodyBytes, err := io.ReadAll(req.Body)
			if err != nil {
				lastErr = err
				continue
			}
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			reqClone.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		resp, err := client.Do(reqClone)
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		if resp != nil {
			if err == nil {
				lastErr = fmt.Errorf("unexpected HTTP status: %s", resp.Status)
			}
			resp.Body.Close()
		}
		if err != nil {
			lastErr = err
		}
	}

	return nil, fmt.Errorf("[%s] 重试 %d 次后仍然失败: %w", p.Name(), maxRetries, lastErr)
}
