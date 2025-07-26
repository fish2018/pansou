package jikepan

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 在init函数中注册插件
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewJikepanAsyncV2Plugin())
}

const (
	// JikepanAPIURL 即刻盘API地址
	JikepanAPIURL = "https://api.jikepan.xyz/search"
)

// JikepanAsyncV2Plugin 即刻盘搜索异步V2插件
type JikepanAsyncV2Plugin struct {
	*plugin.BaseAsyncPlugin
}

// NewJikepanAsyncV2Plugin 创建新的即刻盘搜索异步V2插件
func NewJikepanAsyncV2Plugin() *JikepanAsyncV2Plugin {
	return &JikepanAsyncV2Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("jikepan", 3),
	}
}

// Search 执行搜索并返回结果
func (p *JikepanAsyncV2Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 使用保存的主缓存键，传递ext参数但不使用
	return p.AsyncSearch(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *JikepanAsyncV2Plugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 构建请求
	reqBody := map[string]interface{}{
		"name":   keyword,
		"is_all": false,
	}
	
	// 检查ext中是否包含title_en参数，如果有则使用它
	if ext != nil {
		if isAll, ok := ext["is_all"].(bool); ok && isAll {
			// 使用全量搜索，时间大约10秒
			reqBody["is_all"] = true
		}
	}
	
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}
	
	req, err := http.NewRequest("POST", JikepanAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("referer", "https://jikepan.xyz/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	
	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	
	// 解析响应
	var apiResp JikepanResponse
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body failed: %w", err)
	}
	
	if err := json.Unmarshal(bodyBytes, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}
	
	// 检查响应状态
	if apiResp.Msg != "success" {
		return nil, fmt.Errorf("API returned error: %s", apiResp.Msg)
	}
	
	// 转换结果格式
	results := p.convertResults(apiResp.List)
	
	return results, nil
}

// convertResults 将API响应转换为标准SearchResult格式
func (p *JikepanAsyncV2Plugin) convertResults(items []JikepanItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	
	for i, item := range items {
		// 跳过没有链接的结果
		if len(item.Links) == 0 {
			continue
		}
		
		// 创建链接列表
		links := make([]model.Link, 0, len(item.Links))
		for _, link := range item.Links {
			linkType := p.convertLinkType(link.Service)
			
			// 特殊处理other类型，检查链接URL
			if linkType == "others" && strings.Contains(strings.ToLower(link.Link), "drive.uc.cn") {
				linkType = "uc"
			}
			
			// 跳过未知类型的链接（linkType为空）
			if linkType == "" {
				continue
			}
			
			// 创建链接
			links = append(links, model.Link{
				URL:      link.Link,
				Type:     linkType,
				Password: link.Pwd,
			})
		}
		
		// 创建唯一ID：插件名-索引
		uniqueID := fmt.Sprintf("jikepan-%d", i)
		
		// 创建搜索结果
		result := model.SearchResult{
			UniqueID:  uniqueID,
			Title:     item.Name,
			Datetime:  time.Time{}, // 使用零值而不是nil
			Links:     links,
		}
		
		results = append(results, result)
	}
	
	return results
}

// convertLinkType 将API的服务类型转换为标准链接类型
func (p *JikepanAsyncV2Plugin) convertLinkType(service string) string {
	service = strings.ToLower(service)
	
	switch service {
	case "baidu":
		return "baidu"
	case "aliyun":
		return "aliyun"
	case "xunlei":
		return "xunlei"
	case "quark":
		return "quark"
	case "189cloud":
		return "tianyi"
	case "115":
		return "115"
	case "123":
		return "123"
	case "weiyun":
		return "weiyun"
	case "pikpak":
		return "pikpak"
	case "lanzou":
		return "lanzou"
	case "jianguoyun":
		return "jianguoyun"
	case "caiyun":
		return "mobile"
	case "chengtong":
		return "chengtong"
	case "ed2k":
		return "ed2k"
	case "magnet":
		return "magnet"
	case "unknown":
		// 对于未知类型，返回空字符串，以便在后续处理中跳过
		return ""
	default:
		return "others"
	}
}

// JikepanResponse API响应结构
type JikepanResponse struct {
	Msg  string        `json:"msg"`
	List []JikepanItem `json:"list"`
}

// JikepanItem API响应中的单个结果项
type JikepanItem struct {
	Name  string        `json:"name"`
	Links []JikepanLink `json:"links"`
}

// JikepanLink API响应中的链接信息
type JikepanLink struct {
	Service string `json:"service"`
	Link    string `json:"link"`
	Pwd     string `json:"pwd,omitempty"`
} 