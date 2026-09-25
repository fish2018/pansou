package haisou

import (
	"context"
	"fmt"
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

const (
	// 调试日志开关
	DebugLog = false
	// 默认每种网盘类型获取页数
	DefaultPagesPerType = 2
	// 最大允许每种网盘类型页数（防止过度请求）
	MaxAllowedPagesPerType = 3
)

// 支持的网盘类型列表 (haisou API支持的类型)
var SupportedCloudTypes = []string{"ali", "baidu", "quark", "xunlei", "tianyi"}

// HaisouPlugin 海搜插件
type HaisouPlugin struct {
	*plugin.BaseAsyncPlugin
}

// SearchAPIResponse 搜索API响应结构
type SearchAPIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Query string      `json:"query"`
		Count int         `json:"count"`
		Time  int         `json:"time"`
		Pages int         `json:"pages"`
		Page  int         `json:"page"`
		List  []ShareItem `json:"list"`
	} `json:"data"`
}

// ShareItem 搜索结果项
type ShareItem struct {
	HSID      string `json:"hsid"`       // 海搜ID，用于获取具体链接
	Platform  string `json:"platform"`   // 网盘类型
	ShareName string `json:"share_name"` // 分享名称，可能包含HTML标签
	StatFile  int    `json:"stat_file"`  // 文件数量
	StatSize  int64  `json:"stat_size"`  // 总大小(字节)
}

// FetchAPIResponse 链接获取API响应结构
type FetchAPIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ShareCode string  `json:"share_code"` // 网盘分享码
		SharePwd  *string `json:"share_pwd"`  // 网盘提取密码，可能为null
	} `json:"data"`
}

// PageResult 页面搜索结果
type PageResult struct {
	pageNo     int
	cloudType  string
	shareItems []ShareItem
	err        error
}

// LinkResult 链接获取结果
type LinkResult struct {
	hsid     string
	shareURL string
	password string
	err      error
}

func init() {
	p := &HaisouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("haisou", 3),
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *HaisouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果（推荐方法）
func (p *HaisouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *HaisouPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[%s] 开始搜索，关键词: %s\n", p.Name(), keyword)
	}

	// 1. 从扩展参数中获取每种网盘类型的页数配置
	pagesPerType := DefaultPagesPerType
	if ext != nil {
		if pages, ok := ext["pages_per_type"].(int); ok && pages > 0 {
			pagesPerType = pages
			if pagesPerType > MaxAllowedPagesPerType {
				pagesPerType = MaxAllowedPagesPerType
				if DebugLog {
					fmt.Printf("[%s] 每种网盘类型页数限制在最大值: %d\n", p.Name(), MaxAllowedPagesPerType)
				}
			}
		} else if pagesFloat, ok := ext["pages_per_type"].(float64); ok && pagesFloat > 0 {
			pagesPerType = int(pagesFloat)
			if pagesPerType > MaxAllowedPagesPerType {
				pagesPerType = MaxAllowedPagesPerType
			}
		}
	}

	totalTasks := len(SupportedCloudTypes) * pagesPerType
	if DebugLog {
		fmt.Printf("[%s] 将分别搜索 %d 种网盘类型，每种 %d 页，总计 %d 个并发任务\n",
			p.Name(), len(SupportedCloudTypes), pagesPerType, totalTasks)
	}

	// 2. 第一阶段：并发搜索获取所有hsid
	var wg sync.WaitGroup
	shareItemsChan := make(chan PageResult, totalTasks)

	// 启动并发搜索任务
	for _, cloudType := range SupportedCloudTypes {
		for pageNo := 1; pageNo <= pagesPerType; pageNo++ {
			wg.Add(1)
			go func(cType string, page int) {
				defer wg.Done()

				shareItems, err := p.fetchSearchPage(client, keyword, page, cType)
				shareItemsChan <- PageResult{
					pageNo:     page,
					cloudType:  cType,
					shareItems: shareItems,
					err:        err,
				}
			}(cloudType, pageNo)
		}
	}

	// 等待所有搜索任务完成
	go func() {
		wg.Wait()
		close(shareItemsChan)
	}()

	// 3. 收集所有hsid
	var allShareItems []ShareItem
	successTasks := 0
	errorTasks := 0
	resultsByType := make(map[string]int)

	for pageResult := range shareItemsChan {
		if pageResult.err != nil {
			errorTasks++
			if DebugLog {
				fmt.Printf("[%s] %s网盘第%d页搜索失败: %v\n", p.Name(), pageResult.cloudType, pageResult.pageNo, pageResult.err)
			}
			continue
		}

		successTasks++
		allShareItems = append(allShareItems, pageResult.shareItems...)
		resultsByType[pageResult.cloudType] += len(pageResult.shareItems)
		if DebugLog {
			fmt.Printf("[%s] %s网盘第%d页成功获取 %d 个结果\n", p.Name(), pageResult.cloudType, pageResult.pageNo, len(pageResult.shareItems))
		}
	}

	if DebugLog {
		fmt.Printf("[%s] 搜索阶段完成: 成功%d任务, 失败%d任务, 总hsid%d个\n",
			p.Name(), successTasks, errorTasks, len(allShareItems))
		for cloudType, count := range resultsByType {
			fmt.Printf("[%s]   - %s网盘: %d个结果\n", p.Name(), cloudType, count)
		}
	}

	// 4. 如果所有搜索任务都失败，返回错误
	if successTasks == 0 {
		return nil, fmt.Errorf("[%s] 所有搜索任务都失败", p.Name())
	}

	// 5. 第二阶段：并发获取所有链接
	if DebugLog {
		fmt.Printf("[%s] 开始第二阶段：并发获取 %d 个链接\n", p.Name(), len(allShareItems))
	}

	linkResultsChan := make(chan LinkResult, len(allShareItems))
	var linkWg sync.WaitGroup

	// 启动并发链接获取任务
	for _, shareItem := range allShareItems {
		linkWg.Add(1)
		go func(item ShareItem) {
			defer linkWg.Done()

			shareURL, password, err := p.fetchShareLink(client, item.HSID, item.Platform)
			linkResultsChan <- LinkResult{
				hsid:     item.HSID,
				shareURL: shareURL,
				password: password,
				err:      err,
			}
		}(shareItem)
	}

	// 等待所有链接获取任务完成
	go func() {
		linkWg.Wait()
		close(linkResultsChan)
	}()

	// 6. 建立hsid到链接的映射
	hsidToLink := make(map[string]LinkResult)
	linkSuccessCount := 0
	linkErrorCount := 0

	for linkResult := range linkResultsChan {
		if linkResult.err != nil {
			linkErrorCount++
			if DebugLog {
				fmt.Printf("[%s] 获取链接失败 hsid=%s: %v\n", p.Name(), linkResult.hsid, linkResult.err)
			}
			continue
		}

		linkSuccessCount++
		hsidToLink[linkResult.hsid] = linkResult
	}

	if DebugLog {
		fmt.Printf("[%s] 链接获取阶段完成: 成功%d个, 失败%d个\n", p.Name(), linkSuccessCount, linkErrorCount)
	}

	// 7. 组合搜索结果和链接信息
	var results []model.SearchResult
	processedCount := 0
	skippedCount := 0

	for _, shareItem := range allShareItems {
		// 获取对应的链接信息
		linkResult, exists := hsidToLink[shareItem.HSID]
		if !exists {
			skippedCount++
			continue
		}

		// 清理HTML标签获取纯文本标题
		title := cleanHTMLTags(shareItem.ShareName)
		if title == "" {
			title = "未知资源"
		}

		// 创建链接对象
		link := model.Link{
			Type:     mapPlatformType(shareItem.Platform),
			URL:      linkResult.shareURL,
			Password: linkResult.password,
		}

		// 构建搜索结果
		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), shareItem.HSID),
			Title:    title,
			Content:  fmt.Sprintf("文件数量: %d | 网盘类型: %s | 大小: %s", shareItem.StatFile, shareItem.Platform, formatSize(shareItem.StatSize)),
			Links:    []model.Link{link},
			Tags:     []string{shareItem.Platform},
			Channel:  "", // 插件搜索结果必须为空字符串
			Datetime: time.Now(),
		}

		results = append(results, result)
		processedCount++
	}

	if DebugLog {
		fmt.Printf("[%s] 结果组合完成: 处理%d项 -> 有效%d项 -> 跳过%d项\n",
			p.Name(), len(allShareItems), processedCount, skippedCount)
	}

	// 8. 关键词过滤
	beforeFilterCount := len(results)
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	if DebugLog {
		fmt.Printf("[%s] 关键词过滤: 过滤前%d项 -> 过滤后%d项\n",
			p.Name(), beforeFilterCount, len(filteredResults))
	}

	return filteredResults, nil
}

// fetchSearchPage 获取指定网盘类型的单页搜索结果
func (p *HaisouPlugin) fetchSearchPage(client *http.Client, keyword string, pageNo int, panType string) ([]ShareItem, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("https://haisou.cc/api/pan/share/search?query=%s&scope=title&pan=%s&page=%d&filter_valid=true&filter_has_files=false",
		url.QueryEscape(keyword), panType, pageNo)

	if DebugLog {
		fmt.Printf("[%s] 请求%s网盘第%d页: %s\n", p.Name(), panType, pageNo, searchURL)
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页创建请求失败: %w", p.Name(), panType, pageNo, err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://haisou.cc/")

	// 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页请求失败: %w", p.Name(), panType, pageNo, err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] %s网盘第%d页返回状态码: %d", p.Name(), panType, pageNo, resp.StatusCode)
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页读取响应失败: %w", p.Name(), panType, pageNo, err)
	}

	// 解析响应
	var apiResp SearchAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页JSON解析失败: %w", p.Name(), panType, pageNo, err)
	}

	// 检查API响应状态
	if apiResp.Code != 0 {
		return nil, fmt.Errorf("[%s] %s网盘第%d页API错误: %s", p.Name(), panType, pageNo, apiResp.Msg)
	}

	if DebugLog {
		fmt.Printf("[%s] %s网盘第%d页获取到 %d 个搜索结果\n", p.Name(), panType, pageNo, len(apiResp.Data.List))
	}

	return apiResp.Data.List, nil
}

// fetchShareLink 通过hsid获取具体的分享链接
func (p *HaisouPlugin) fetchShareLink(client *http.Client, hsid string, platform string) (string, string, error) {
	// 构建获取链接的URL
	fetchURL := fmt.Sprintf("https://haisou.cc/api/pan/share/%s/fetch", hsid)

	if DebugLog {
		fmt.Printf("[%s] 获取链接 hsid=%s platform=%s: %s\n", p.Name(), hsid, platform, fetchURL)
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("[%s] hsid=%s创建链接请求失败: %w", p.Name(), hsid, err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://haisou.cc/")

	// 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return "", "", fmt.Errorf("[%s] hsid=%s链接请求失败: %w", p.Name(), hsid, err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("[%s] hsid=%s链接请求返回状态码: %d", p.Name(), hsid, resp.StatusCode)
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("[%s] hsid=%s读取响应失败: %w", p.Name(), hsid, err)
	}

	// 解析响应
	var apiResp FetchAPIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", "", fmt.Errorf("[%s] hsid=%s链接JSON解析失败: %w", p.Name(), hsid, err)
	}

	// 检查API响应状态
	if apiResp.Code != 0 {
		return "", "", fmt.Errorf("[%s] hsid=%s链接API错误: %s", p.Name(), hsid, apiResp.Msg)
	}

	// 根据平台类型构建完整的分享链接
	shareURL := buildShareURL(platform, apiResp.Data.ShareCode)
	if shareURL == "" {
		return "", "", fmt.Errorf("[%s] hsid=%s不支持的网盘平台: %s", p.Name(), hsid, platform)
	}

	// 获取密码
	password := ""
	if apiResp.Data.SharePwd != nil {
		password = *apiResp.Data.SharePwd
	}

	if DebugLog {
		fmt.Printf("[%s] hsid=%s成功获取链接: %s password=%s\n", p.Name(), hsid, shareURL, password)
	}

	return shareURL, password, nil
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *HaisouPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// buildShareURL 根据平台类型和分享码构建完整的分享链接
func buildShareURL(platform, shareCode string) string {
	switch strings.ToLower(platform) {
	case "ali":
		return fmt.Sprintf("https://www.alipan.com/s/%s", shareCode)
	case "baidu":
		return fmt.Sprintf("https://pan.baidu.com/s/%s", shareCode)
	case "quark":
		return fmt.Sprintf("https://pan.quark.cn/s/%s", shareCode)
	case "xunlei":
		return fmt.Sprintf("https://pan.xunlei.com/s/%s", shareCode)
	case "tianyi":
		return fmt.Sprintf("https://cloud.189.cn/t/%s", shareCode)
	default:
		return ""
	}
}

// mapPlatformType 映射网盘平台类型到PanSou标准类型
func mapPlatformType(platform string) string {
	switch strings.ToLower(platform) {
	case "ali":
		return "aliyun" // PanSou内部使用aliyun标识阿里云盘
	case "baidu":
		return "baidu"
	case "quark":
		return "quark"
	case "xunlei":
		return "xunlei"
	case "tianyi":
		return "tianyi"
	default:
		return "others"
	}
}

// cleanHTMLTags 清理HTML标签
func cleanHTMLTags(text string) string {
	// 移除高亮标签 <span class="highlight">...</span>
	re := haisouRe1
	cleaned := re.ReplaceAllString(text, "$1")

	// 移除其他可能的HTML标签
	re2 := haisouRe2
	cleaned = re2.ReplaceAllString(cleaned, "")

	return strings.TrimSpace(cleaned)
}

// formatSize 格式化文件大小显示
func formatSize(size int64) string {
	const (
		B  = 1
		KB = 1024 * B
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
	)

	switch {
	case size >= TB:
		return fmt.Sprintf("%.2f TB", float64(size)/float64(TB))
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/float64(GB))
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/float64(MB))
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/float64(KB))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	haisouRe1 = regexp.MustCompile(`<span[^>]*class="highlight"[^>]*>(.*?)</span>`)
	haisouRe2 = regexp.MustCompile(`<[^>]*>`)
)
