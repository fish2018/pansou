package yuhuage

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"pansou/util"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	BaseURL        = "https://www.iyuhuage.fun"
	SearchPath     = "/search/"
	UserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
	MaxConcurrency = 5 // 详情页最大并发数
	MaxRetryCount  = 2 // 最大重试次数
)

// YuhuagePlugin 雨花阁插件
type YuhuagePlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
	rateLimited int32 // 429限流标志位
}

func init() {
	p := &YuhuagePlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("yuhuage", 3, true),
		debugMode:       false,
		cacheTTL:        30 * time.Minute,
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 搜索接口实现
func (p *YuhuagePlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *YuhuagePlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现方法
func (p *YuhuagePlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[YUHUAGE] 开始搜索: %s", keyword)
	}

	// 检查限流状态
	if atomic.LoadInt32(&p.rateLimited) == 1 {
		if p.debugMode {
			log.Printf("[YUHUAGE] 当前处于限流状态，跳过搜索")
		}
		return nil, fmt.Errorf("rate limited")
	}

	// 构建搜索URL
	encodedQuery := url.QueryEscape(keyword)
	searchURL := fmt.Sprintf("%s%s%s-%d-time.html", BaseURL, SearchPath, encodedQuery, 1)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", BaseURL+"/")

	// 发送HTTP请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 429 {
		atomic.StoreInt32(&p.rateLimited, 1)
		go func() {
			time.Sleep(60 * time.Second)
			atomic.StoreInt32(&p.rateLimited, 0)
		}()
		return nil, fmt.Errorf("[%s] 请求被限流", p.Name())
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] HTTP错误: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应
	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	// 解析搜索结果
	results, err := p.parseSearchResults(string(body))
	if err != nil {
		return nil, err
	}

	if p.debugMode {
		log.Printf("[YUHUAGE] 搜索完成，获得 %d 个结果", len(results))
	}

	// 关键词过滤
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// parseSearchResults 解析搜索结果
func (p *YuhuagePlugin) parseSearchResults(html string) ([]model.SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	var results []model.SearchResult
	var detailURLs []string

	// 提取搜索结果
	doc.Find(".search-item.detail-width").Each(func(i int, s *goquery.Selection) {
		title := strings.TrimSpace(p.cleanTitle(s.Find(".item-title h3 a").Text()))
		detailHref, exists := s.Find(".item-title h3 a").Attr("href")

		if !exists || title == "" {
			return
		}

		detailURL := BaseURL + detailHref
		detailURLs = append(detailURLs, detailURL)

		// 提取基本信息
		createTime := strings.TrimSpace(s.Find(".item-bar span:contains('创建时间') b").Text())
		size := strings.TrimSpace(s.Find(".item-bar .cpill.blue-pill").Text())
		fileCount := strings.TrimSpace(s.Find(".item-bar .cpill.yellow-pill").Text())
		hot := strings.TrimSpace(s.Find(".item-bar span:contains('热度') b").Text())
		lastDownload := strings.TrimSpace(s.Find(".item-bar span:contains('最近下载') b").Text())

		// 构建内容描述
		content := fmt.Sprintf("创建时间: %s | 大小: %s | 文件数: %s | 热度: %s",
			createTime, size, fileCount, hot)
		if lastDownload != "" {
			content += fmt.Sprintf(" | 最近下载: %s", lastDownload)
		}

		result := model.SearchResult{
			Title:    title,
			Content:  content,
			Channel:  "", // 插件搜索结果必须为空字符串
			Tags:     []string{"磁力链接"},
			Datetime: p.parseDateTime(createTime),
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), p.extractHashFromURL(detailURL)),
		}

		results = append(results, result)
	})

	if p.debugMode {
		log.Printf("[YUHUAGE] 解析到 %d 个搜索结果，准备获取详情", len(results))
	}

	// 同步获取详情页链接
	p.fetchDetailsSync(detailURLs, results)

	return results, nil
}

// fetchDetailsSync 同步获取详情页信息
func (p *YuhuagePlugin) fetchDetailsSync(detailURLs []string, results []model.SearchResult) {
	if len(detailURLs) == 0 {
		return
	}

	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup

	for i, detailURL := range detailURLs {
		if i >= len(results) {
			break
		}

		wg.Add(1)
		go func(url string, result *model.SearchResult) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			links := p.fetchDetailLinks(url)
			if len(links) > 0 {
				result.Links = links
				if p.debugMode {
					log.Printf("[YUHUAGE] 为结果设置了 %d 个链接", len(links))
				}
			} else if p.debugMode {
				log.Printf("[YUHUAGE] 详情页没有找到有效链接: %s", url)
			}
		}(detailURL, &results[i])
	}

	wg.Wait()
	if p.debugMode {
		log.Printf("[YUHUAGE] 详情页获取完成")
	}
}

// fetchDetailLinks 获取详情页链接
func (p *YuhuagePlugin) fetchDetailLinks(detailURL string) []model.Link {
	// 检查缓存
	if cached, exists := p.detailCache.Load(detailURL); exists {
		if links, ok := cached.([]model.Link); ok {
			return links
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}

	for retry := 0; retry <= MaxRetryCount; retry++ {
		req, err := http.NewRequest("GET", detailURL, nil)
		if err != nil {
			continue
		}

		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Referer", BaseURL+"/")

		resp, err := client.Do(req)
		if err != nil {
			if retry < MaxRetryCount {
				time.Sleep(time.Duration(retry+1) * time.Second)
				continue
			}
			break
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			if retry < MaxRetryCount {
				time.Sleep(time.Duration(retry+1) * time.Second)
				continue
			}
			break
		}

		body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		resp.Body.Close()

		if err != nil {
			if retry < MaxRetryCount {
				time.Sleep(time.Duration(retry+1) * time.Second)
				continue
			}
			break
		}

		links := p.parseDetailLinks(string(body))

		// 缓存结果
		if len(links) > 0 {
			p.detailCache.Store(detailURL, links)
			// 设置缓存过期
			go func() {
				time.Sleep(p.cacheTTL)
				p.detailCache.Delete(detailURL)
			}()
		}

		return links
	}

	return nil
}

// parseDetailLinks 解析详情页链接
func (p *YuhuagePlugin) parseDetailLinks(html string) []model.Link {
	var links []model.Link

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return links
	}

	// 提取磁力链接
	doc.Find("a.download[href^='magnet:']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && href != "" {
			if p.debugMode {
				log.Printf("[YUHUAGE] 找到磁力链接: %s", href)
			}
			links = append(links, model.Link{
				URL:  href,
				Type: "magnet",
			})
		}
	})

	// 提取迅雷链接
	doc.Find("a.download[href^='thunder:']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && href != "" {
			if p.debugMode {
				log.Printf("[YUHUAGE] 找到迅雷链接: %s", href)
			}
			links = append(links, model.Link{
				URL:  href,
				Type: "others",
			})
		}
	})

	if p.debugMode && len(links) > 0 {
		log.Printf("[YUHUAGE] 从详情页解析到 %d 个链接", len(links))
	}

	return links
}

// extractHashFromURL 从URL中提取哈希ID
func (p *YuhuagePlugin) extractHashFromURL(detailURL string) string {
	re := yuhuageRe1
	matches := re.FindStringSubmatch(detailURL)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// cleanTitle 清理标题
func (p *YuhuagePlugin) cleanTitle(title string) string {
	title = strings.TrimSpace(title)
	// 移除HTML标签（如<b>标签）
	re := yuhuageRe2
	title = re.ReplaceAllString(title, "")
	// 移除多余的空格
	re = yuhuageRe3
	title = re.ReplaceAllString(title, " ")
	return strings.TrimSpace(title)
}

// parseDateTime 解析时间字符串
func (p *YuhuagePlugin) parseDateTime(timeStr string) time.Time {
	if timeStr == "" {
		return time.Time{}
	}

	// 尝试不同的时间格式
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, timeStr); err == nil {
			return t
		}
	}

	return time.Time{}
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *YuhuagePlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
			status := resp.StatusCode
			resp.Body.Close()
			if err == nil {
				// Do 成功但状态码非 200。此前这里只执行 lastErr = err，
				// err 为 nil 时会把 lastErr 清空，三次失败后仅报出
				// "%!w(<nil>)"，真实状态码被丢掉、无法定位失败原因。
				err = fmt.Errorf("HTTP 状态码 %d", status)
			}
		}
		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	yuhuageRe1 = regexp.MustCompile(`/hash/(\d+)\.html`)
	yuhuageRe2 = regexp.MustCompile(`<[^>]*>`)
	yuhuageRe3 = regexp.MustCompile(`\s+`)
)
