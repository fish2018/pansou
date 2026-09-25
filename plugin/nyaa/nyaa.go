package nyaa

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// 预编译的正则表达式
var (
	// 从详情链接提取ID的正则表达式
	viewIDRegex = regexp.MustCompile(`/view/(\d+)`)

	// 磁力链接正则表达式
	magnetRegex = regexp.MustCompile(`magnet:\?xt=urn:btih:[a-zA-Z0-9]+[^\s'"<>]*`)
)

const (
	// 超时时间
	DefaultTimeout = 10 * time.Second

	// HTTP连接池配置
	MaxIdleConns        = 50
	MaxIdleConnsPerHost = 20
	MaxConnsPerHost     = 30
	IdleConnTimeout     = 90 * time.Second

	// 网站URL
	SiteURL = "https://nyaa.si"
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewNyaaPlugin())
}

// NyaaPlugin Nyaa BT搜索插件
type NyaaPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

// createOptimizedHTTPClient 创建优化的HTTP客户端
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        MaxIdleConns,
		MaxIdleConnsPerHost: MaxIdleConnsPerHost,
		MaxConnsPerHost:     MaxConnsPerHost,
		IdleConnTimeout:     IdleConnTimeout,
		DisableKeepAlives:   false,
	}
	return &http.Client{Transport: transport, Timeout: DefaultTimeout}
}

// NewNyaaPlugin 创建新的Nyaa插件
func NewNyaaPlugin() *NyaaPlugin {
	return &NyaaPlugin{
		// 优先级3：普通质量数据源，跳过Service层过滤（磁力搜索插件）
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("nyaa", 3, true),
		optimizedClient: createOptimizedHTTPClient(),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *NyaaPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *NyaaPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *NyaaPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 支持英文搜索优化
	searchKeyword := keyword
	if ext != nil {
		if titleEn, exists := ext["title_en"]; exists {
			if titleEnStr, ok := titleEn.(string); ok && titleEnStr != "" {
				searchKeyword = titleEnStr
			}
		}
	}

	// 1. 构建搜索URL
	searchURL := fmt.Sprintf("%s/?f=0&c=0_0&q=%s", SiteURL, url.QueryEscape(searchKeyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 3. 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置完整的请求头（避免反爬虫）
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", SiteURL)

	// 5. 发送请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 6. 解析搜索结果页面
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}

	// 7. 提取搜索结果
	var results []model.SearchResult

	// 查找种子列表表格
	table := doc.Find("table.torrent-list tbody")
	if table.Length() == 0 {
		return []model.SearchResult{}, nil // 没有搜索结果
	}

	// 8. 解析每个搜索结果行
	table.Find("tr").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchRow(s)
		if result.UniqueID != "" {
			results = append(results, result)
		}
	})

	// 9. 关键词过滤（插件层过滤，使用实际搜索的关键词）
	return plugin.FilterResultsByKeyword(results, searchKeyword), nil
}

// parseSearchRow 解析单个搜索结果行
func (p *NyaaPlugin) parseSearchRow(s *goquery.Selection) model.SearchResult {
	result := model.SearchResult{}

	// 1. 提取分类信息
	categoryLink := s.Find("td:nth-child(1) a")
	category := ""
	if categoryLink.Length() > 0 {
		category, _ = categoryLink.Attr("title")
	}

	// 2. 提取标题和详情链接
	titleLink := s.Find("td[colspan='2'] a")
	if titleLink.Length() == 0 {
		return result
	}

	title := strings.TrimSpace(titleLink.Text())
	if title == "" {
		// 如果text为空，尝试从title属性获取
		title, _ = titleLink.Attr("title")
	}

	detailHref, exists := titleLink.Attr("href")
	if !exists || detailHref == "" {
		return result
	}

	// 3. 从详情链接提取ID
	matches := viewIDRegex.FindStringSubmatch(detailHref)
	if len(matches) < 2 {
		return result
	}
	itemID := matches[1]
	result.UniqueID = fmt.Sprintf("%s-%s", p.Name(), itemID)
	result.Title = title

	// 4. 提取磁力链接
	magnetLink := s.Find("td.text-center a[href^='magnet:']")
	if magnetLink.Length() > 0 {
		magnetURL, exists := magnetLink.Attr("href")
		if exists && magnetURL != "" {
			result.Links = []model.Link{
				{
					Type:     "magnet",
					URL:      magnetURL,
					Password: "",
				},
			}
		}
	}

	// 如果没有找到磁力链接，返回空结果
	if len(result.Links) == 0 {
		result.UniqueID = ""
		return result
	}

	// 5. 提取文件大小
	sizeTd := s.Find("td.text-center").Eq(1) // 第4个td，索引从1开始（跳过链接td）
	size := strings.TrimSpace(sizeTd.Text())

	// 6. 提取发布时间
	dateTd := s.Find("td.text-center[data-timestamp]")
	timestamp := int64(0)
	if dateTd.Length() > 0 {
		if timestampStr, exists := dateTd.Attr("data-timestamp"); exists {
			if ts, err := strconv.ParseInt(timestampStr, 10, 64); err == nil {
				timestamp = ts
			}
		}
	}

	if timestamp > 0 {
		result.Datetime = time.Unix(timestamp, 0)
	} else {
		result.Datetime = time.Now()
	}

	// 7. 提取种子统计信息
	tds := s.Find("td.text-center")
	seeders := "0"
	leechers := "0"
	downloads := "0"

	if tds.Length() >= 6 {
		// 倒数第3个是做种数
		seeders = strings.TrimSpace(tds.Eq(tds.Length() - 3).Text())
		// 倒数第2个是下载数
		leechers = strings.TrimSpace(tds.Eq(tds.Length() - 2).Text())
		// 倒数第1个是完成数
		downloads = strings.TrimSpace(tds.Eq(tds.Length() - 1).Text())
	}

	// 8. 构建内容描述
	var contentParts []string
	if category != "" {
		contentParts = append(contentParts, fmt.Sprintf("分类: %s", category))
	}
	if size != "" {
		contentParts = append(contentParts, fmt.Sprintf("大小: %s", size))
	}
	contentParts = append(contentParts, fmt.Sprintf("做种: %s", seeders))
	contentParts = append(contentParts, fmt.Sprintf("下载: %s", leechers))
	contentParts = append(contentParts, fmt.Sprintf("完成: %s", downloads))

	result.Content = strings.Join(contentParts, " | ")

	// 9. 设置标签
	var tags []string
	if category != "" {
		tags = append(tags, category)
	}
	tags = append(tags, fmt.Sprintf("做种:%s", seeders))
	tags = append(tags, fmt.Sprintf("下载:%s", leechers))
	tags = append(tags, fmt.Sprintf("完成:%s", downloads))
	result.Tags = tags

	// 10. Channel必须为空字符串（插件搜索结果）
	result.Channel = ""

	return result
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *NyaaPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
