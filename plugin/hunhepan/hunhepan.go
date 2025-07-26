package hunhepan

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

// 在init函数中注册插件
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewHunhepanAsyncPlugin())
}

const (
	// API端点
	HunhepanAPI = "https://hunhepan.com/open/search/disk"
	QkpansoAPI  = "https://qkpanso.com/v1/search/disk"
	KuakeAPI    = "https://kuake8.com/v1/search/disk"
	
	// 默认页大小
	DefaultPageSize = 30
)

// HunhepanAsyncPlugin 混合盘搜索异步插件
type HunhepanAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewHunhepanAsyncPlugin 创建新的混合盘搜索异步插件
func NewHunhepanAsyncPlugin() *HunhepanAsyncPlugin {
	return &HunhepanAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("hunhepan", 3),
	}
}

// Search 执行搜索并返回结果
func (p *HunhepanAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 使用保存的主缓存键，传递ext参数但不使用
	return p.AsyncSearch(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *HunhepanAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 创建结果通道和错误通道
	resultChan := make(chan []HunhepanItem, 3)
	errChan := make(chan error, 3)
	
	// 创建等待组
	var wg sync.WaitGroup
	wg.Add(3)
	
	// 并行请求三个API
	go func() {
		defer wg.Done()
		items, err := p.searchAPI(client, HunhepanAPI, keyword)
		if err != nil {
			errChan <- fmt.Errorf("hunhepan API error: %w", err)
			return
		}
		resultChan <- items
	}()
	
	go func() {
		defer wg.Done()
		items, err := p.searchAPI(client, QkpansoAPI, keyword)
		if err != nil {
			errChan <- fmt.Errorf("qkpanso API error: %w", err)
			return
		}
		resultChan <- items
	}()
	
	go func() {
		defer wg.Done()
		items, err := p.searchAPI(client, KuakeAPI, keyword)
		if err != nil {
			errChan <- fmt.Errorf("kuake API error: %w", err)
			return
		}
		resultChan <- items
	}()
	
	// 启动一个goroutine等待所有请求完成并关闭通道
	go func() {
		wg.Wait()
		close(resultChan)
		close(errChan)
	}()
	
	// 收集结果
	var allItems []HunhepanItem
	var errors []error
	
	// 从通道读取结果
	for items := range resultChan {
		allItems = append(allItems, items...)
	}
	
	// 收集错误（不阻止处理）
	for err := range errChan {
		errors = append(errors, err)
	}
	
	// 如果没有获取到任何结果且有错误，则返回第一个错误
	if len(allItems) == 0 && len(errors) > 0 {
		return nil, errors[0]
	}
	
	// 去重处理
	uniqueItems := p.deduplicateItems(allItems)
	
	// 转换为标准格式
	results := p.convertResults(uniqueItems)
	
	return results, nil
}

// searchAPI 向单个API发送请求
func (p *HunhepanAsyncPlugin) searchAPI(client *http.Client, apiURL, keyword string) ([]HunhepanItem, error) {
	maxPages := 3 // 最多获取3页数据，可以根据需要调整
	
	// 创建结果通道和错误通道
	resultChan := make(chan []HunhepanItem, maxPages)
	errChan := make(chan error, maxPages)
	
	// 创建等待组，用于等待所有页面请求完成
	var wg sync.WaitGroup
	
	// 并发请求每一页
	for page := 1; page <= maxPages; page++ {
		wg.Add(1)
		
		go func(pageNum int) {
			defer wg.Done()
			
			// 构建请求体
			reqBody := map[string]interface{}{
				"q":      keyword,
				"exact":  true,
				"page":   pageNum,
				"size":   DefaultPageSize,
				"type":   "",
				"time":   "",
				"from":   "web",
				"user_id": 0,
				"filter": true,
			}
			
			jsonData, err := json.Marshal(reqBody)
			if err != nil {
				errChan <- fmt.Errorf("marshal request failed (page %d): %w", pageNum, err)
				return
			}
			
			req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
			if err != nil {
				errChan <- fmt.Errorf("create request failed (page %d): %w", pageNum, err)
				return
			}
			
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
			
			// 根据不同的API设置不同的Referer
			if strings.Contains(apiURL, "qkpanso.com") {
				req.Header.Set("Referer", "https://qkpanso.com/search")
			} else if strings.Contains(apiURL, "kuake8.com") {
				req.Header.Set("Referer", "https://kuake8.com/search")
			} else if strings.Contains(apiURL, "hunhepan.com") {
				req.Header.Set("Referer", "https://hunhepan.com/search")
			}
			
			// 发送请求
			resp, err := client.Do(req)
			if err != nil {
				errChan <- fmt.Errorf("request failed (page %d): %w", pageNum, err)
				return
			}
			defer resp.Body.Close()
			
			// 读取响应体
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				errChan <- fmt.Errorf("read response body failed (page %d): %w", pageNum, err)
				return
			}
			
			// 解析响应
			var apiResp HunhepanResponse
			if err := json.Unmarshal(respBody, &apiResp); err != nil {
				errChan <- fmt.Errorf("decode response failed (page %d): %w", pageNum, err)
				return
			}
			
			// 检查响应状态
			if apiResp.Code != 200 {
				errChan <- fmt.Errorf("API returned error (page %d): %s", pageNum, apiResp.Msg)
				return
			}
			
			// 将结果发送到通道
			resultChan <- apiResp.Data.List
		}(page)
	}
	
	// 启动一个goroutine等待所有页面请求完成并关闭通道
	go func() {
		wg.Wait()
		close(resultChan)
		close(errChan)
	}()
	
	// 收集结果
	var allItems []HunhepanItem
	for items := range resultChan {
		allItems = append(allItems, items...)
	}
	
	// 检查是否有错误
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}
	
	// 如果没有获取到任何结果且有错误，则返回第一个错误
	if len(allItems) == 0 && len(errors) > 0 {
		return nil, errors[0]
	}
	
	return allItems, nil
}

// deduplicateItems 去重处理
func (p *HunhepanAsyncPlugin) deduplicateItems(items []HunhepanItem) []HunhepanItem {
	// 使用map进行去重
	uniqueMap := make(map[string]HunhepanItem)
	
	for _, item := range items {
		// 清理DiskName中的HTML标签
		cleanedName := cleanTitle(item.DiskName)
		item.DiskName = cleanedName
		
		// 创建复合键：优先使用DiskID，如果为空则使用Link+DiskName组合
		var key string
		if item.DiskID != "" {
			key = item.DiskID
		} else if item.Link != "" {
			// 使用Link和清理后的DiskName组合作为键
			key = item.Link + "|" + cleanedName
		} else {
			// 如果DiskID和Link都为空，则使用DiskName+DiskType作为键
			key = cleanedName + "|" + item.DiskType
		}
		
		// 如果已存在，保留信息更丰富的那个
		if existing, exists := uniqueMap[key]; exists {
			// 比较文件列表长度和其他信息
			existingScore := len(existing.Files)
			newScore := len(item.Files)
			
			// 如果新项有密码而现有项没有，增加新项分数
			if existing.DiskPass == "" && item.DiskPass != "" {
				newScore += 5
			}
			
			// 如果新项有时间而现有项没有，增加新项分数
			if existing.SharedTime == "" && item.SharedTime != "" {
				newScore += 3
			}
			
			if newScore > existingScore {
				uniqueMap[key] = item
			}
		} else {
			uniqueMap[key] = item
		}
	}
	
	// 将map转回切片
	result := make([]HunhepanItem, 0, len(uniqueMap))
	for _, item := range uniqueMap {
		result = append(result, item)
	}
	
	return result
}

// convertResults 将API响应转换为标准SearchResult格式
func (p *HunhepanAsyncPlugin) convertResults(items []HunhepanItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	
	for i, item := range items {
		// 创建链接
		link := model.Link{
			URL:      item.Link,
			Type:     p.convertDiskType(item.DiskType),
			Password: item.DiskPass,
		}
		
		// 创建唯一ID
		uniqueID := fmt.Sprintf("hunhepan-%s", item.DiskID)
		if item.DiskID == "" {
			// 使用索引作为后备
			uniqueID = fmt.Sprintf("hunhepan-%d", i)
		}
		
		// 解析时间
		var datetime time.Time
		if item.SharedTime != "" {
			// 尝试解析时间，格式：2025-07-07 13:19:48
			parsedTime, err := time.Parse("2006-01-02 15:04:05", item.SharedTime)
			if err == nil {
				datetime = parsedTime
			}
		}
		
		// 如果时间解析失败，使用零值
		if datetime.IsZero() {
			datetime = time.Time{}
		}
		
		// 创建搜索结果
		result := model.SearchResult{
			UniqueID:  uniqueID,
			Title:     cleanTitle(item.DiskName),
			Content:   item.Files,
			Datetime:  datetime,
			Links:     []model.Link{link},
		}
		
		results = append(results, result)
	}
	
	return results
}

// convertDiskType 将API的网盘类型转换为标准链接类型
func (p *HunhepanAsyncPlugin) convertDiskType(diskType string) string {
	switch diskType {
	case "BDY":
		return "baidu"
	case "ALY":
		return "aliyun"
	case "QUARK":
		return "quark"
	case "TIANYI":
		return "tianyi"
	case "UC":
		return "uc"
	case "CAIYUN":
		return "mobile"
	case "115":
		return "115"
	case "XUNLEI":
		return "xunlei"
	case "123PAN":
		return "123"
	case "PIKPAK":
		return "pikpak"
	default:
		return "others"
	}
}

// cleanTitle 清理标题中的HTML标签
func cleanTitle(title string) string {
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
	
	result := title
	for tag, replacement := range replacements {
		result = strings.Replace(result, tag, replacement, -1)
	}
	
	// 移除多余的空格
	return strings.TrimSpace(result)
}

// HunhepanResponse API响应结构
type HunhepanResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Total   int           `json:"total"`
		PerSize int           `json:"per_size"`
		List    []HunhepanItem `json:"list"`
	} `json:"data"`
}

// HunhepanItem API响应中的单个结果项
type HunhepanItem struct {
	DiskID     string `json:"disk_id"`
	DiskName   string `json:"disk_name"`
	DiskPass   string `json:"disk_pass"`
	DiskType   string `json:"disk_type"`
	Files      string `json:"files"`
	DocID      string `json:"doc_id"`
	ShareUser  string `json:"share_user"`
	SharedTime string `json:"shared_time"`
	Link       string `json:"link"`
	Enabled    bool   `json:"enabled"`
	Weight     int    `json:"weight"`
	Status     int    `json:"status"`
} 