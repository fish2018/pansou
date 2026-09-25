package xuexizhinan

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	// 搜索URL
	SearchURL = "https://xuexizhinan.com/?post_type=book&s=%s"

	// 详情页URL正则表达式
	DetailURLPattern = `https://xuexizhinan.com/book/(\d+)\.html`

	// 默认超时时间
	DefaultTimeout = 10 * time.Second

	// 并发数限制
	MaxConcurrency = 8 // 提高并发数以提高性能
)

// 预编译正则表达式
var (
	detailURLRegex  = regexp.MustCompile(DetailURLPattern)
	magnetLinkRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-zA-Z]+`)
	dateRegex       = regexp.MustCompile(`上映日期: (\d{4}-\d{2}-\d{2})`)
)

// 缓存相关变量
var (
	// 详情页缓存
	detailPageCache = sync.Map{}

	// 最后一次清理缓存的时间
	lastCacheCleanTime = time.Now()

	// 缓存有效期
	cacheTTL = 24 * time.Hour
)

// 缓存的详情页响应
type detailPageResponse struct {
	Title       string
	ImageURL    string
	MagnetLinks []string
	QuarkLinks  []model.Link
	Tags        []string
	Content     string
	Timestamp   time.Time
}

// XuexizhinanPlugin 4K指南搜索插件
type XuexizhinanPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewXuexizhinanPlugin 创建新的4K指南搜索异步插件
func NewXuexizhinanPlugin() *XuexizhinanPlugin {
	return &XuexizhinanPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("xuexizhinan", 1), // 高优先级
	}
}

// 初始化插件
func init() {
	plugin.RegisterGlobalPlugin(NewXuexizhinanPlugin())

	// 启动缓存清理
	go startCacheCleaner()
}

// startCacheCleaner 定期清理缓存
func startCacheCleaner() {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		// 清空详情页缓存
		detailPageCache = sync.Map{}
		lastCacheCleanTime = time.Now()
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *XuexizhinanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *XuexizhinanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *XuexizhinanPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf(SearchURL, url.QueryEscape(keyword))

	// 发送请求
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.127 Safari/537.36")

	// 添加请求超时控制
	ctx, cancel := context.WithTimeout(req.Context(), DefaultTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求返回状态码: %d", resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	type searchItem struct {
		url   string
		title string
	}

	// 将关键词转为小写，用于不区分大小写的比较
	lowerKeywords := strings.ToLower(keyword)
	// 将关键词按空格分割，用于支持多关键词搜索
	keywords := strings.Fields(lowerKeywords)

	// 存储符合条件的搜索项
	var validItems []searchItem

	// 使用更高效的选择器直接获取所有链接和标题
	doc.Find(".url-card").Each(func(i int, s *goquery.Selection) {
		// 提取标题和链接
		titleElem := s.Find(".list-title")
		title := strings.TrimSpace(titleElem.Text())
		link, exists := titleElem.Attr("href")

		if !exists || link == "" || title == "" {
			return
		}

		// 标题转小写，用于不区分大小写的比较
		lowerTitle := strings.ToLower(title)

		// 检查标题是否包含所有关键词
		matched := true
		for _, kw := range keywords {
			if !strings.Contains(lowerTitle, kw) {
				matched = false
				break
			}
		}

		// 如果标题包含所有关键词，则添加到有效项中
		if matched {
			validItems = append(validItems, searchItem{url: link, title: title})
		}
	})

	// 如果没有搜索结果，返回空结果
	if len(validItems) == 0 {
		return nil, nil
	}

	// 创建信号量控制并发
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup

	// 使用带缓冲的通道减少阻塞
	bufferSize := len(validItems) * 2
	resultCh := make(chan model.SearchResult, bufferSize)
	errorCh := make(chan error, bufferSize)

	// 获取详情页信息
	for _, item := range validItems {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 从详情页获取完整信息
			result, err := p.processDetailPage(client, url)
			if err != nil {
				errorCh <- err
				return
			}

			if result != nil {
				resultCh <- *result
			}
		}(item.url)
	}

	// 等待所有goroutine完成
	wg.Wait()
	close(resultCh)
	close(errorCh)

	// 收集结果和错误
	var errs []error
	for err := range errorCh {
		errs = append(errs, err)
	}

	// 预分配结果数组容量以提高性能
	var results = make([]model.SearchResult, 0, len(validItems))
	for result := range resultCh {
		results = append(results, result)
	}

	// 如果有结果，返回结果；如果没有结果，但有错误，返回第一个错误
	if len(results) > 0 {
		// 使用过滤功能过滤结果
		filteredResults := plugin.FilterResultsByKeyword(results, keyword)

		return filteredResults, nil
	} else if len(errs) > 0 {
		return nil, errs[0]
	}

	return nil, nil
}

// processDetailPage 处理详情页，提取网盘链接和资源信息
func (p *XuexizhinanPlugin) processDetailPage(client *http.Client, detailURL string) (*model.SearchResult, error) {
	// 检查缓存
	if cachedResult, ok := detailPageCache.Load(detailURL); ok {
		cachedResponse := cachedResult.(detailPageResponse)

		// 检查缓存是否过期
		if time.Since(cachedResponse.Timestamp) < cacheTTL {
			return p.detailResponseToResult(detailURL, cachedResponse), nil
		}
	}

	// 正则匹配提取ID - 使用预编译的正则表达式
	matches := detailURLRegex.FindStringSubmatch(detailURL)
	if len(matches) < 2 {
		return nil, fmt.Errorf("无效的详情页URL格式: %s", detailURL)
	}

	// 发送请求
	req, err := http.NewRequest("GET", detailURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.127 Safari/537.36")

	// 添加请求超时控制
	ctx, cancel := context.WithTimeout(req.Context(), DefaultTimeout)
	defer cancel()
	req = req.WithContext(ctx)

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求返回状态码: %d", resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取详情信息
	response := detailPageResponse{
		Timestamp: time.Now(),
	}

	// 1. 提取标题
	response.Title = strings.TrimSpace(doc.Find(".book-header h1").Text())
	if response.Title == "" {
		// 尝试从页面标题获取
		title := doc.Find("title").Text()
		response.Title = strings.TrimSuffix(title, " | 4K指南")
		response.Title = strings.TrimSpace(response.Title)
	}

	// 2. 提取封面图片
	response.ImageURL = doc.Find(".book-cover img").AttrOr("src", "")

	// 3. 提取标签
	doc.Find(".book-header .my-2 a").Each(func(i int, s *goquery.Selection) {
		tag := strings.TrimSpace(s.Text())
		if tag != "" {
			response.Tags = append(response.Tags, tag)
		}
	})

	// 4. 提取资源详情
	response.Content = strings.TrimSpace(doc.Find(".panel-body.single").Text())

	// 5. 提取磁力链接和6. 提取夸克网盘链接
	// 一次性查找所有可能包含链接的元素，减少DOM遍历
	doc.Find("li, .site-go a").Each(func(i int, s *goquery.Selection) {
		if s.Is("li") {
			// 提取磁力链接
			text := s.Text()
			if strings.Contains(text, "magnet:?xt=urn:btih:") {
				// 使用预编译的正则表达式
				magnetMatch := magnetLinkRegex.FindString(text)
				if magnetMatch != "" {
					response.MagnetLinks = append(response.MagnetLinks, magnetMatch)
				}
			}
		} else if s.Is("a") {
			// 提取夸克网盘链接
			href := s.AttrOr("href", "")
			title := s.AttrOr("title", "")
			name := s.Find(".b-name").Text()

			if strings.Contains(href, "pan.quark.cn") || strings.Contains(name, "夸克") || strings.Contains(title, "夸克") {
				link := model.Link{
					URL:      href,
					Type:     "quark",
					Password: "", // 夸克网盘通常不需要单独的提取码
				}
				response.QuarkLinks = append(response.QuarkLinks, link)
			}
		}
	})

	// 缓存结果
	detailPageCache.Store(detailURL, response)

	// 转换为搜索结果
	return p.detailResponseToResult(detailURL, response), nil
}

// detailResponseToResult 将详情页响应转换为搜索结果
func (p *XuexizhinanPlugin) detailResponseToResult(detailURL string, response detailPageResponse) *model.SearchResult {
	if response.Title == "" && len(response.MagnetLinks) == 0 && len(response.QuarkLinks) == 0 {
		return nil
	}

	// 提取ID - 使用预编译的正则表达式
	matches := detailURLRegex.FindStringSubmatch(detailURL)
	id := "unknown"
	if len(matches) >= 2 {
		id = matches[1]
	}

	// 创建唯一ID
	uniqueID := fmt.Sprintf("xuexizhinan-%s", id)

	// 提取日期 - 使用预编译的正则表达式
	var datetime time.Time
	// 尝试从内容中提取上映日期
	dateMatches := dateRegex.FindStringSubmatch(response.Content)
	if len(dateMatches) >= 2 {
		// 尝试解析日期
		if t, err := time.Parse("2006-01-02", dateMatches[1]); err == nil {
			datetime = t
		}
	}

	// 预分配链接数组的容量
	totalLinks := len(response.MagnetLinks) + len(response.QuarkLinks)
	links := make([]model.Link, 0, totalLinks)

	// 添加磁力链接
	for _, magnetLink := range response.MagnetLinks {
		links = append(links, model.Link{
			Type:     "magnet",
			URL:      magnetLink,
			Password: "",
		})
	}

	// 添加夸克网盘链接
	links = append(links, response.QuarkLinks...)

	// 创建搜索结果
	result := &model.SearchResult{
		UniqueID: uniqueID,
		Title:    response.Title,
		Content:  response.Content,
		Datetime: datetime,
		Links:    links,
		Tags:     response.Tags,
	}

	return result
}
