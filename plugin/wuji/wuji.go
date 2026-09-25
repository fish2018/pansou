package wuji

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"pansou/util"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

// 常量定义
const (
	// 基础URL
	BaseURL = "https://xcili.net"

	// 搜索URL格式：/search?q={keyword}&page={page}
	SearchURL = BaseURL + "/search?q=%s&page=%d"

	// 默认参数
	MaxRetries     = 3
	TimeoutSeconds = 30

	// 并发控制参数
	MaxConcurrency = 10 // 最大并发数
	MaxPages       = 5  // 最大搜索页数
)

// 预编译的正则表达式
var (
	// 磁力链接正则
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}[^"'\s]*`)

	// 磁力链接缓存，键为详情页URL，值为磁力链接
	magnetCache = sync.Map{}
	cacheTTL    = 1 * time.Hour // 缓存1小时
)

// 缓存的磁力链接响应
type magnetCacheEntry struct {
	MagnetLink string
	Timestamp  time.Time
}

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:89.0) Gecko/20100101 Firefox/89.0",
}

// WujiPlugin 无极磁链搜索插件
type WujiPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewWujiPlugin 创建新的无极磁链插件实例
func NewWujiPlugin() *WujiPlugin {
	return &WujiPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("wuji", 3, true),
	}
}

// Name 返回插件名称
func (p *WujiPlugin) Name() string {
	return "wuji"
}

// DisplayName 返回插件显示名称
func (p *WujiPlugin) DisplayName() string {
	return "无极磁链"
}

// Description 返回插件描述
func (p *WujiPlugin) Description() string {
	return "ØMagnet 无极磁链 - 磁力链接搜索引擎"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *WujiPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *WujiPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *WujiPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 首先搜索第一页
	firstPageResults, err := p.searchPage(client, keyword, 1)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索第一页失败: %w", p.Name(), err)
	}

	// 存储所有结果
	var allResults []model.SearchResult
	allResults = append(allResults, firstPageResults...)

	// 2. 并发搜索其他页面（第2页到第5页）
	if MaxPages > 1 {
		var wg sync.WaitGroup
		var mu sync.Mutex

		// 使用信号量控制并发数
		semaphore := make(chan struct{}, MaxConcurrency)

		// 存储每页结果
		pageResults := make(map[int][]model.SearchResult)

		for page := 2; page <= MaxPages; page++ {
			wg.Add(1)
			go func(pageNum int) {
				defer wg.Done()

				// 获取信号量
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// 添加小延迟避免过于频繁的请求
				time.Sleep(time.Duration(pageNum%3) * 100 * time.Millisecond)

				currentPageResults, err := p.searchPage(client, keyword, pageNum)
				if err == nil && len(currentPageResults) > 0 {
					mu.Lock()
					pageResults[pageNum] = currentPageResults
					mu.Unlock()
				}
			}(page)
		}

		wg.Wait()

		// 按页码顺序合并所有页面的结果
		for page := 2; page <= MaxPages; page++ {
			if results, exists := pageResults[page]; exists {
				allResults = append(allResults, results...)
			}
		}
	}

	// 3. 并发获取每个结果的详情页磁力链接
	finalResults := p.enrichWithMagnetLinks(allResults, client)

	// 4. 关键词过滤
	searchKeyword := keyword
	if searchParam, ok := ext["search"]; ok {
		if searchStr, ok := searchParam.(string); ok && searchStr != "" {
			searchKeyword = searchStr
		}
	}

	return plugin.FilterResultsByKeyword(finalResults, searchKeyword), nil
}

// searchPage 搜索指定页面
func (p *WujiPlugin) searchPage(client *http.Client, keyword string, page int) ([]model.SearchResult, error) {
	// URL编码关键词
	encodedKeyword := url.QueryEscape(keyword)
	searchURL := fmt.Sprintf(SearchURL, encodedKeyword, page)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSeconds*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送HTTP请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应体内容
	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	// 提取搜索结果
	return p.extractSearchResults(doc), nil
}

// extractSearchResults 提取搜索结果
func (p *WujiPlugin) extractSearchResults(doc *goquery.Document) []model.SearchResult {
	var results []model.SearchResult

	// 查找所有搜索结果
	doc.Find("table.file-list tbody tr").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchResult(s)
		if result.Title != "" {
			results = append(results, result)
		}
	})

	return results
}

// parseSearchResult 解析单个搜索结果
func (p *WujiPlugin) parseSearchResult(s *goquery.Selection) model.SearchResult {
	result := model.SearchResult{
		Channel:  "", // 插件搜索结果必须为空字符串
		Datetime: time.Now(),
	}

	// 提取标题和详情页链接
	titleCell := s.Find("td").First()
	titleLink := titleCell.Find("a")

	// 详情页链接
	detailPath, exists := titleLink.Attr("href")
	if !exists || detailPath == "" {
		return result
	}

	// 构造完整的详情页URL
	detailURL := BaseURL + detailPath

	// 提取标题（排除 p.sample 的内容）
	titleText := titleLink.Clone()
	titleText.Find("p.sample").Remove()
	title := strings.TrimSpace(titleText.Text())
	result.Title = p.cleanTitle(title)

	// 提取文件名预览
	sampleText := strings.TrimSpace(titleLink.Find("p.sample").Text())

	// 提取文件大小
	sizeText := strings.TrimSpace(s.Find("td.td-size").Text())

	// 构造内容
	var contentParts []string
	if sampleText != "" {
		contentParts = append(contentParts, "文件: "+sampleText)
	}
	if sizeText != "" {
		contentParts = append(contentParts, "大小: "+sizeText)
	}
	result.Content = strings.Join(contentParts, "\n")

	// 暂时将详情页链接作为占位符（后续会被磁力链接替换）
	result.Links = []model.Link{{
		Type: "detail",
		URL:  detailURL,
	}}

	// 生成唯一ID
	result.UniqueID = fmt.Sprintf("%s-%d", p.Name(), time.Now().UnixNano())

	// 添加标签
	result.Tags = []string{"magnet"}

	return result
}

// fetchMagnetLink 获取详情页的磁力链接（带缓存）
func (p *WujiPlugin) fetchMagnetLink(client *http.Client, detailURL string) (string, error) {
	// 检查缓存
	if cached, ok := magnetCache.Load(detailURL); ok {
		if entry, ok := cached.(magnetCacheEntry); ok {
			if time.Since(entry.Timestamp) < cacheTTL {
				// 缓存命中
				return entry.MagnetLink, nil
			}
			// 缓存过期，删除
			magnetCache.Delete(detailURL)
		}
	}
	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSeconds*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建详情页请求失败: %w", err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送HTTP请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return "", fmt.Errorf("详情页请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("详情页返回状态码: %d", resp.StatusCode)
	}

	// 读取响应体内容
	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return "", fmt.Errorf("读取详情页响应失败: %w", err)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("详情页HTML解析失败: %w", err)
	}

	// 提取磁力链接
	magnetInput := doc.Find("input#input-magnet")
	if magnetInput.Length() == 0 {
		return "", fmt.Errorf("未找到磁力链接输入框")
	}

	magnetLink, exists := magnetInput.Attr("value")
	if !exists || magnetLink == "" {
		return "", fmt.Errorf("磁力链接为空")
	}

	// 存入缓存
	magnetCache.Store(detailURL, magnetCacheEntry{
		MagnetLink: magnetLink,
		Timestamp:  time.Now(),
	})

	return magnetLink, nil
}

// cleanTitle 清理标题中的广告内容
func (p *WujiPlugin) cleanTitle(title string) string {
	// 移除【】之间的广告内容
	title = wujiRe1.ReplaceAllString(title, "")
	// 移除数字+【】格式的广告
	title = wujiRe2.ReplaceAllString(title, "")
	// 移除[]之间的内容（如有需要）
	title = wujiRe3.ReplaceAllString(title, "")
	// 移除多余的空格
	title = wujiRe4.ReplaceAllString(title, " ")
	return strings.TrimSpace(title)
}

// setRequestHeaders 设置请求头
func (p *WujiPlugin) setRequestHeaders(req *http.Request) {
	// 使用第一个稳定的UA
	ua := userAgents[0]
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
}

// doRequestWithRetry 带重试的HTTP请求
func (p *WujiPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		if i < MaxRetries-1 {
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}

	return nil, fmt.Errorf("请求失败，已重试%d次: %w", MaxRetries, lastErr)
}

// enrichWithMagnetLinks 并发获取磁力链接并丰富搜索结果
func (p *WujiPlugin) enrichWithMagnetLinks(results []model.SearchResult, client *http.Client) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	// 使用信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	var mutex sync.Mutex

	enrichedResults := make([]model.SearchResult, len(results))
	copy(enrichedResults, results)

	for i := range enrichedResults {
		// 检查是否有详情页链接
		if len(enrichedResults[i].Links) == 0 {
			continue
		}

		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 获取详情页URL
			detailURL := enrichedResults[index].Links[0].URL

			// 添加适当的间隔避免请求过于频繁
			time.Sleep(time.Duration(index%5) * 100 * time.Millisecond)

			// 请求详情页并解析磁力链接
			magnetLink, err := p.fetchMagnetLink(client, detailURL)
			if err == nil && magnetLink != "" {
				mutex.Lock()
				enrichedResults[index].Links = []model.Link{{
					Type: "magnet",
					URL:  magnetLink,
				}}
				mutex.Unlock()
			} else if err != nil {
				fmt.Printf("[%s] 获取磁力链接失败 [%d]: %v\n", p.Name(), index, err)
			}
		}(i)
	}

	wg.Wait()

	// 过滤掉没有有效磁力链接的结果
	var validResults []model.SearchResult
	for _, result := range enrichedResults {
		if len(result.Links) > 0 && result.Links[0].Type == "magnet" {
			validResults = append(validResults, result)
		}
	}

	return validResults
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewWujiPlugin())
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	wujiRe1 = regexp.MustCompile(`【[^】]*】`)
	wujiRe2 = regexp.MustCompile(`^\d+【[^】]*】`)
	wujiRe3 = regexp.MustCompile(`\[[^\]]*\]`)
	wujiRe4 = regexp.MustCompile(`\s+`)
)
