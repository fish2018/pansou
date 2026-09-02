package quarkres

import (
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

// 热门夸克资源插件：直连 squark.cc.cd 数据库（TG频道 @quark_res 的数据面），
// 返回的链接全部经过实时有效性校验，转存成功率高。

const (
	// apiBase 搜索接口
	apiBase = "https://squark.cc.cd/api/search?kw="
)

// 在 init 函数中注册插件（优先级 2：数据源质量良好，链接实时校验过）
func init() {
	plugin.RegisterGlobalPlugin(NewQuarkResPlugin())
}

// QuarkResPlugin 热门夸克资源异步插件
type QuarkResPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewQuarkResPlugin 创建新的热门夸克资源异步插件
func NewQuarkResPlugin() *QuarkResPlugin {
	return &QuarkResPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("quarkres", 2),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *QuarkResPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含 IsFinal 标记的结果
func (p *QuarkResPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// apiLink 链接结构
type apiLink struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	Password string `json:"password"`
}

// apiItem 单条结果
type apiItem struct {
	UniqueID string    `json:"unique_id"`
	Title    string    `json:"title"`
	Content  string    `json:"content"`
	Datetime time.Time `json:"datetime"`
	Links    []apiLink `json:"links"`
}

// apiResp 接口响应
type apiResp struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Total   int       `json:"total"`
	Data    []apiItem `json:"data"`
}

// doSearch 实际的搜索实现
func (p *QuarkResPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := apiBase + url.QueryEscape(keyword)

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[quarkres] create request failed: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://squark.cc.cd/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[quarkres] request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[quarkres] HTTP %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[quarkres] read response failed: %w", err)
	}

	var ar apiResp
	if err := json.Unmarshal(bodyBytes, &ar); err != nil {
		return nil, fmt.Errorf("[quarkres] decode response failed: %w", err)
	}

	if ar.Code != 200 {
		return nil, fmt.Errorf("[quarkres] API error: %s", ar.Message)
	}

	results := make([]model.SearchResult, 0, len(ar.Data))
	for _, item := range ar.Data {
		links := make([]model.Link, 0, len(item.Links))
		for _, l := range item.Links {
			// 只保留夸克链接（本源只有夸克）
			if strings.Contains(l.URL, "pan.quark.cn") {
				links = append(links, model.Link{
					Type:     "quark",
					URL:      l.URL,
					Password: l.Password,
					Datetime: item.Datetime,
				})
			}
		}
		if len(links) == 0 {
			continue
		}
		results = append(results, model.SearchResult{
			UniqueID: item.UniqueID,
			Title:    item.Title,
			Content:  item.Content,
			Datetime: item.Datetime,
			Links:    links,
			Channel:  "", // 插件结果 Channel 必须为空
		})
	}

	return results, nil
}
