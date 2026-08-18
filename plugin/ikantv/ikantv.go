package ikantv

import (
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
	pluginName     = "ikantv"
	searchAPI      = "https://api.naspt.vip/api/open/pansou/search"
	defaultReferer = "https://api.naspt.vip/"
	defaultTimeout = 30 * time.Second
	defaultLimit   = 50
	priority       = 3
)

var allowedPans = map[string]struct{}{
	"quark": {}, "uc": {}, "baidu": {}, "aliyun": {}, "guangya": {},
	"xunlei": {}, "tianyi": {}, "115": {}, "123": {}, "mobile": {},
	"pikpak": {}, "magnet": {}, "ed2k": {},
}

func init() {
	plugin.RegisterGlobalPlugin(NewIkanTVAsyncPlugin())
}

// IkanTVAsyncPlugin 爱看公开搜索异步插件
type IkanTVAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewIkanTVAsyncPlugin 创建爱看搜索插件
func NewIkanTVAsyncPlugin() *IkanTVAsyncPlugin {
	return &IkanTVAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, priority),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *IkanTVAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含 IsFinal 标记的结果
func (p *IkanTVAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际搜索实现
func (p *IkanTVAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s?kw=%s&limit=%d", searchAPI, url.QueryEscape(keyword), defaultLimit)
	if titleEn, ok := ext["title_en"].(string); ok && titleEn != "" {
		searchURL += "&title_en=" + url.QueryEscape(titleEn)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", defaultReferer)

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("[%s] JSON解析失败: %w", p.Name(), err)
	}
	if apiResp.Code != 0 {
		return nil, fmt.Errorf("[%s] API错误: %s", p.Name(), apiResp.Message)
	}

	results := convertResults(apiResp.Data)
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func convertResults(items []apiItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	for _, item := range items {
		result, ok := convertResult(item)
		if !ok {
			continue
		}
		results = append(results, result)
	}
	return results
}

func convertResult(item apiItem) (model.SearchResult, bool) {
	links := convertLinks(item.Links)
	if len(links) == 0 {
		return model.SearchResult{}, false
	}

	itemID := strings.TrimSpace(item.UniqueID)
	if itemID == "" {
		itemID = strings.TrimSpace(item.MessageID)
	}
	if itemID == "" {
		return model.SearchResult{}, false
	}
	itemID = strings.TrimPrefix(itemID, pluginName+"-")

	return model.SearchResult{
		MessageID: item.MessageID,
		UniqueID:  fmt.Sprintf("%s-%s", pluginName, itemID),
		Channel:   "",
		Datetime:  parseDatetime(item.Datetime),
		Title:     item.Title,
		Content:   item.Content,
		Links:     links,
		Tags:      item.Tags,
		Images:    item.Images,
	}, true
}

func convertLinks(raw []apiLink) []model.Link {
	links := make([]model.Link, 0, len(raw))
	for _, link := range raw {
		pan := strings.ToLower(strings.TrimSpace(link.Type))
		if _, ok := allowedPans[pan]; !ok {
			continue
		}
		href := strings.TrimSpace(link.URL)
		if href == "" || !isValidURL(pan, href) {
			continue
		}
		links = append(links, model.Link{
			Type:      pan,
			URL:       href,
			Password:  link.Password,
			Datetime:  parseDatetime(link.Datetime),
			WorkTitle: link.WorkTitle,
		})
	}
	return links
}

func isValidURL(pan, raw string) bool {
	if pan == "magnet" {
		return strings.HasPrefix(strings.ToLower(raw), "magnet:?")
	}
	if pan == "ed2k" {
		return strings.HasPrefix(strings.ToLower(raw), "ed2k://")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.HasPrefix(u.Scheme, "http")
}

func parseDatetime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", value); err == nil {
		return t
	}
	return time.Time{}
}

func (p *IkanTVAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

		reqClone := req.Clone(req.Context())
		resp, err := client.Do(reqClone)
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		if lastErr == nil {
			lastErr = fmt.Errorf("status %d", statusCode(resp))
		}
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

func statusCode(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

type apiResponse struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    []apiItem `json:"data"`
}

type apiItem struct {
	MessageID string    `json:"message_id"`
	UniqueID  string    `json:"unique_id"`
	Channel   string    `json:"channel"`
	Datetime  string    `json:"datetime"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	Images    []string  `json:"images"`
	Links     []apiLink `json:"links"`
}

type apiLink struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Password  string `json:"password"`
	Datetime  string `json:"datetime"`
	WorkTitle string `json:"work_title"`
}
