package qupansou

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 缓存相关变量
var (
	// API响应缓存，键为关键词，值为缓存的响应
	apiResponseCache = sync.Map{}
	
	// 最后一次清理缓存的时间
	lastCacheCleanTime = time.Now()
	
	// 缓存有效期（1小时）
	cacheTTL = 1 * time.Hour
)

// 在init函数中注册插件
func init() {
	// 使用全局超时时间创建插件实例并注册
	plugin.RegisterGlobalPlugin(NewQuPanSouPlugin())
	
	// 启动缓存清理goroutine
	go startCacheCleaner()
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	// 每小时清理一次缓存
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		// 清空所有缓存
		apiResponseCache = sync.Map{}
		lastCacheCleanTime = time.Now()
	}
}

const (
	// API端点
	ApiURL = "https://v.funletu.com/search"
	
	// 默认超时时间
	DefaultTimeout = 6 * time.Second
	
	// 默认页大小
	DefaultPageSize = 1000
)

// QuPanSouPlugin 趣盘搜插件
type QuPanSouPlugin struct {
	client  *http.Client
	timeout time.Duration
}

// NewQuPanSouPlugin 创建新的趣盘搜插件
func NewQuPanSouPlugin() *QuPanSouPlugin {
	timeout := DefaultTimeout
	
	return &QuPanSouPlugin{
		client: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// Name 返回插件名称
func (p *QuPanSouPlugin) Name() string {
	return "qupansou"
}

// Priority 返回插件优先级
func (p *QuPanSouPlugin) Priority() int {
	return 3 // 中等优先级
}

// Search 执行搜索并返回结果
func (p *QuPanSouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 生成缓存键
	cacheKey := keyword
	
	// 检查缓存中是否已有结果
	if cachedItems, ok := apiResponseCache.Load(cacheKey); ok {
		// 检查缓存是否过期
		cachedResult := cachedItems.(cachedResponse)
		if time.Since(cachedResult.timestamp) < cacheTTL {
			return cachedResult.results, nil
		}
	}
	
	// 发送API请求
	items, err := p.searchAPI(keyword)
	if err != nil {
		return nil, fmt.Errorf("qupansou API error: %w", err)
	}
	
	// 转换为标准格式
	results := p.convertResults(items)
	
	// 缓存结果
	apiResponseCache.Store(cacheKey, cachedResponse{
		results:   results,
		timestamp: time.Now(),
	})
	
	return results, nil
}

// 缓存响应结构
type cachedResponse struct {
	results   []model.SearchResult
	timestamp time.Time
}

// searchAPI 向API发送请求
func (p *QuPanSouPlugin) searchAPI(keyword string) ([]QuPanSouItem, error) {
	// 构建请求体
	reqBody := map[string]interface{}{
		"style": "get",
		"datasrc": "search",
		"query": map[string]interface{}{
			"id": "",
			"datetime": "",
			"courseid": 1,
			"categoryid": "",
			"filetypeid": "",
			"filetype": "",
			"reportid": "",
			"validid": "",
			"searchtext": keyword,
		},
		"page": map[string]interface{}{
			"pageSize": DefaultPageSize,
			"pageIndex": 1,
		},
		"order": map[string]interface{}{
			"prop": "sort",
			"order": "desc",
		},
		"message": "请求资源列表数据",
	}
	
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}
	
	req, err := http.NewRequest("POST", ApiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Referer", "https://pan.funletu.com/")
	
	// 发送请求
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	
	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body failed: %w", err)
	}
	
	// 解析响应
	var apiResp QuPanSouResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}
	
	// 检查响应状态
	if apiResp.Status != 200 {
		return nil, fmt.Errorf("API returned error: %s", apiResp.Message)
	}
	
	return apiResp.Data, nil
}

// convertResults 将API响应转换为标准SearchResult格式
func (p *QuPanSouPlugin) convertResults(items []QuPanSouItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	
	for _, item := range items {
		// 跳过无效的URL
		if item.URL == "" {
			continue
		}
		
		// 创建链接
		link := model.Link{
			URL:      item.URL,
			Type:     p.determineLinkType(item.URL),
			Password: "", // 趣盘搜API不返回密码
		}
		
		// 创建唯一ID
		uniqueID := fmt.Sprintf("qupansou-%d", item.ID)
		
		// 解析时间
		var datetime time.Time
		if item.UpdateTime != "" {
			// 尝试解析时间，格式：2025-07-05 00:31:38
			parsedTime, err := time.Parse("2006-01-02 15:04:05", item.UpdateTime)
			if err == nil {
				datetime = parsedTime
			}
		}
		
		// 如果时间解析失败，使用零值
		if datetime.IsZero() {
			datetime = time.Time{}
		}
		
		// 清理标题中的HTML标签
		title := cleanHTML(item.Title)
		
		// 创建搜索结果
		result := model.SearchResult{
			UniqueID:  uniqueID,
			Title:     title,
			Content:   fmt.Sprintf("类别: %s, 文件类型: %s, 大小: %s", item.Category, item.FileType, item.Size),
			Datetime:  datetime,
			Links:     []model.Link{link},
		}
		
		results = append(results, result)
	}
	
	return results
}

// determineLinkType 根据URL确定链接类型
func (p *QuPanSouPlugin) determineLinkType(url string) string {
	lowerURL := strings.ToLower(url)
	
	if strings.Contains(lowerURL, "pan.baidu.com") {
		return "baidu"
	} else if strings.Contains(lowerURL, "aliyundrive.com") || strings.Contains(lowerURL, "alipan.com") {
		return "aliyun"
	} else if strings.Contains(lowerURL, "pan.quark.cn") {
		return "quark"
	} else if strings.Contains(lowerURL, "cloud.189.cn") {
		return "tianyi"
	} else if strings.Contains(lowerURL, "pan.xunlei.com") {
		return "xunlei"
	} else if strings.Contains(lowerURL, "caiyun.139.com") || strings.Contains(lowerURL, "www.caiyun.139.com") {
		return "mobile"
	} else if strings.Contains(lowerURL, "115.com") {
		return "115"
	} else if strings.Contains(lowerURL, "drive.uc.cn") {
		return "uc"
	} else if strings.Contains(lowerURL, "pan.123.com") || strings.Contains(lowerURL, "123pan.com") {
		return "123"
	} else if strings.Contains(lowerURL, "mypikpak.com") {
		return "pikpak"
	} else if strings.Contains(lowerURL, "lanzou") {
		return "lanzou"
	} else {
		return "others"
	}
}

// cleanHTML 清理HTML标签
func cleanHTML(html string) string {
	// 一次性替换所有常见HTML标签
	replacements := map[string]string{
		"<em>": "",
		"</em>": "",
		"<b>": "",
		"</b>": "",
		"<strong>": "",
		"</strong>": "",
		"<i>": "",
		"</i>": "",
	}
	
	result := html
	for tag, replacement := range replacements {
		result = strings.Replace(result, tag, replacement, -1)
	}
	
	// 移除多余的空格
	return strings.TrimSpace(result)
}

// QuPanSouResponse API响应结构
type QuPanSouResponse struct {
	Text    string         `json:"text"`
	Data    []QuPanSouItem `json:"data"`
	Total   int            `json:"total"`
	Status  int            `json:"status"`
	Message string         `json:"message"`
}

// QuPanSouItem API响应中的单个结果项
type QuPanSouItem struct {
	ID           int    `json:"id"`
	Title        string `json:"title"`
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	Link         string `json:"link"`
	SearchText   string `json:"searchtext"`
	ExtCode      string `json:"extcode"`
	UnzipCode    string `json:"unzipcode"`
	Size         string `json:"size"`
	CategoryID   int    `json:"categoryid"`
	Category     string `json:"category"`
	CourseID     int    `json:"courseid"`
	Course       string `json:"course"`
	FileTypeID   int    `json:"filetypeid"`
	FileType     string `json:"filetype"`
	UpdateTime   string `json:"updatetime"`
	CreateTime   string `json:"createtime"`
	Views        int    `json:"views"`
	ViewsHistory int    `json:"viewshistory"`
	Diff         int    `json:"diff"`
	Violate      int    `json:"violate"`
	State        int    `json:"state"`
	Sort         int    `json:"sort"`
	Top          int    `json:"top"`
	Valid        int    `json:"valid"`
} 