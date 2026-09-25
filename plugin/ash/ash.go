package ash

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

type AshPlugin struct {
	*plugin.BaseAsyncPlugin
}

const (
	// 错误的夸克域名
	wrongQuarkDomain = "pan.qualk.cn"
	// 正确的夸克域名
	correctQuarkDomain = "pan.quark.cn"
)

var (
	// 提取JSON数据的正则表达式（预编译）
	jsonDataRegex = regexp.MustCompile(`var jsonData = '(\[.*?\])';`)

	// 控制字符清理正则（预编译）
	controlCharRegex = regexp.MustCompile(`[\x00-\x1F\x7F]`)
)

// AshResult 表示ASH搜索结果的数据结构
type AshResult struct {
	ID               int         `json:"id"`
	SourceCategoryID int         `json:"source_category_id"`
	Title            string      `json:"title"`
	IsType           int         `json:"is_type"`
	Code             interface{} `json:"code"` // 可能是null或string
	URL              string      `json:"url"`
	IsTime           int         `json:"is_time"`
	Name             string      `json:"name"`
	Times            string      `json:"times"`
	Category         interface{} `json:"category"` // 可能是null或string
}

func init() {
	p := &AshPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("ash", 2), // 优先级2，质量良好的影视资源
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果
func (p *AshPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *AshPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现（优化版本）
func (p *AshPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("https://so.allsharehub.com/s/%s.html", url.QueryEscape(keyword))

	// 创建带超时的上下文（减少超时时间，提高响应速度）
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送请求（优化重试）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应（使用有限制的读取，避免读取过大内容）
	// ASH页面通常不会太大，限制在2MB以内
	limitReader := io.LimitReader(resp.Body, 2*1024*1024)
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	// 从HTML中提取JSON数据（直接传递字节，避免字符串转换）
	results, err := p.extractResultsFromBytes(body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 提取搜索结果失败: %w", p.Name(), err)
	}

	// 关键词过滤
	filtered := plugin.FilterResultsByKeyword(results, keyword)

	return filtered, nil
}

// extractResultsFromBytes 从字节数组中提取搜索结果（优化版本，避免字符串转换）
func (p *AshPlugin) extractResultsFromBytes(data []byte) ([]model.SearchResult, error) {
	// 直接在字节数组中查找JSON数据（避免转换为字符串）
	html := string(data) // 只转换一次

	// 查找JSON数据
	matches := jsonDataRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return []model.SearchResult{}, nil // 没有找到数据，返回空结果
	}

	// 提取JSON字符串
	jsonStr := matches[1]

	// 清理JSON字符串（批量操作，减少内存分配）
	if strings.Contains(jsonStr, "\\/") {
		jsonStr = strings.ReplaceAll(jsonStr, "\\/", "/")
	}
	jsonStr = controlCharRegex.ReplaceAllString(jsonStr, "")

	// 解析JSON - 使用高性能的sonic库
	var ashResults []AshResult
	if err := json.Unmarshal([]byte(jsonStr), &ashResults); err != nil {
		return nil, fmt.Errorf("JSON解析失败: %w", err)
	}

	// 如果没有结果，直接返回
	if len(ashResults) == 0 {
		return []model.SearchResult{}, nil
	}

	// 预分配切片容量，避免动态扩容
	results := make([]model.SearchResult, 0, len(ashResults))

	// 批量处理所有结果
	for i := range ashResults {
		item := &ashResults[i]

		// 提前检查URL是否有效，避免无效处理
		if item.URL == "" {
			continue
		}

		// 处理网盘链接
		panURL := p.fixPanURL(item.URL)
		if panURL == "" {
			continue
		}

		// 确定网盘类型（内联优化）
		var panType string
		switch item.IsType {
		case 0:
			panType = "quark"
		case 2:
			panType = "baidu"
		case 3:
			panType = "uc"
		case 4:
			panType = "xunlei"
		default:
			panType = "quark"
		}

		// 处理提取码
		var password string
		if item.Code != nil {
			if codeStr, ok := item.Code.(string); ok && codeStr != "" {
				password = codeStr
			}
		}

		// 解析时间
		var datetime time.Time
		if item.Times != "" {
			if parsedTime, err := time.Parse("2006-01-02", item.Times); err == nil {
				datetime = parsedTime
			} else {
				datetime = time.Now()
			}
		} else {
			datetime = time.Now()
		}

		// 获取标签
		var tags []string
		if item.SourceCategoryID > 0 && item.SourceCategoryID <= 6 {
			categoryNames := [...]string{"短剧", "电影", "电视剧", "动漫", "综艺", "充电视频"}
			tags = []string{categoryNames[item.SourceCategoryID-1]}
		}

		// 构建搜索结果
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%d", p.Name(), item.ID),
			Title:    item.Title,
			Content:  item.Name,
			Datetime: datetime,
			Channel:  "",
			Links: []model.Link{{
				Type:     panType,
				URL:      panURL,
				Password: password,
			}},
			Tags: tags,
		})
	}

	return results, nil
}

// fixPanURL 修复网盘链接 - 关键功能！（优化版本）
func (p *AshPlugin) fixPanURL(url string) string {
	// 快速检查是否为有效的HTTP/HTTPS链接
	if len(url) < 8 { // 最短的URL: http://a
		return ""
	}

	// 验证链接协议（使用更快的检查方式）
	if url[0] != 'h' || (url[4] != ':' && url[5] != ':') {
		return ""
	}

	// 只在包含错误域名时才进行替换，避免不必要的字符串操作
	if strings.Contains(url, wrongQuarkDomain) {
		return strings.Replace(url, wrongQuarkDomain, correctQuarkDomain, 1)
	}

	return url
}

// setRequestHeaders 设置请求头
func (p *AshPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", "https://so.allsharehub.com/")
}

// doRequestWithRetry 带重试机制的HTTP请求（优化版本）
func (p *AshPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 2 // 减少重试次数，提高响应速度
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 更短的退避时间
			backoff := time.Duration(100<<uint(i-1)) * time.Millisecond
			time.Sleep(backoff)
		}

		// 克隆请求（只在重试时克隆）
		var reqToUse *http.Request
		if i == 0 {
			reqToUse = req
		} else {
			reqToUse = req.Clone(req.Context())
		}

		resp, err := client.Do(reqToUse)

		// 成功返回
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		// 清理响应
		if resp != nil {
			resp.Body.Close()
		}

		lastErr = err

		// 如果是上下文取消或超时，不再重试
		if req.Context().Err() != nil {
			break
		}
	}

	if lastErr != nil {
		return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败", maxRetries)
}
