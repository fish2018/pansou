package leijing

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
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

const (
	BaseURL        = "https://leijing.xyz"
	SearchPath     = "/search"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxConcurrency = 20 // 详情页最大并发数
	MaxPages       = 1  // 最大搜索页数（暂时只搜索第一页）
)

// LeijingPlugin 雷鲸小站插件
type LeijingPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	cacheTTL    time.Duration
}

// NewLeijingPlugin 创建新的雷鲸小站插件实例
func NewLeijingPlugin() *LeijingPlugin {
	// 检查调试模式
	debugMode := false // 默认关闭调试

	p := &LeijingPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("leijing", 2),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,
	}

	return p
}

// Name 返回插件名称
func (p *LeijingPlugin) Name() string {
	return "leijing"
}

// DisplayName 返回插件显示名称
func (p *LeijingPlugin) DisplayName() string {
	return "雷鲸小站"
}

// Description 返回插件描述
func (p *LeijingPlugin) Description() string {
	return "雷鲸小站 - 天翼云盘资源分享站"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *LeijingPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *LeijingPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// setRequestHeaders 设置请求头
func (p *LeijingPlugin) setRequestHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// doRequest 发送HTTP请求
func (p *LeijingPlugin) doRequest(client *http.Client, url string, referer string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	p.setRequestHeaders(req, referer)

	if p.debugMode {
		log.Printf("[Leijing] 发送请求: %s", url)
	}

	resp, err := client.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[Leijing] 请求失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[Leijing] 响应状态: %d", resp.StatusCode)
	}

	return resp, nil
}

// searchImpl 实际的搜索实现
func (p *LeijingPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s%s?keyword=%s", BaseURL, SearchPath, url.QueryEscape(keyword))

	if p.debugMode {
		log.Printf("[Leijing] 开始搜索: %s", keyword)
		log.Printf("[Leijing] 搜索URL: %s", searchURL)
	}

	// 发送搜索请求
	resp, err := p.doRequest(client, searchURL, BaseURL)
	if err != nil {
		return nil, fmt.Errorf("发送搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索响应状态码异常: %d", resp.StatusCode)
	}

	// 处理响应体（可能是gzip压缩的）
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil, err
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	results := p.extractSearchResults(doc, keyword)

	if p.debugMode {
		log.Printf("[Leijing] 找到 %d 个搜索结果", len(results))
	}

	// 对于没有直接提取到链接的结果，访问详情页获取链接
	results = p.enrichWithDetailLinks(client, results, keyword)

	// 过滤结果（去掉没有链接的）
	filteredResults := p.filterValidResults(results)

	if p.debugMode {
		log.Printf("[Leijing] 过滤后剩余 %d 个有效结果", len(filteredResults))
	}

	return filteredResults, nil
}

// getResponseReader 获取响应读取器（处理gzip压缩）
func (p *LeijingPlugin) getResponseReader(resp *http.Response) (io.Reader, error) {
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if p.debugMode {
		log.Printf("[Leijing] Content-Encoding: %s", contentEncoding)
	}

	// 如果是gzip压缩，手动解压
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip reader失败: %w", err)
		}
		reader = util.NewCappedReader(gzReader, util.MaxDecompressedBytes)
	}

	return reader, nil
}

// extractSearchResults 从HTML中提取搜索结果
func (p *LeijingPlugin) extractSearchResults(doc *goquery.Document, keyword string) []model.SearchResult {
	var results []model.SearchResult

	// 选择所有搜索结果项
	doc.Find(".topicItem").Each(func(i int, s *goquery.Selection) {
		// 提取标题和详情页链接
		titleElem := s.Find(".title a")
		title := strings.TrimSpace(titleElem.Text())
		detailPath, _ := titleElem.Attr("href")

		if title == "" || detailPath == "" {
			return
		}

		// 构建完整的详情页URL
		detailURL := BaseURL + "/" + strings.TrimPrefix(detailPath, "/")

		// 提取摘要（可能包含链接）
		summary := strings.TrimSpace(s.Find(".summary").Text())

		// 提取其他信息
		postTime := strings.TrimSpace(s.Find(".postTime").Text())
		postTime = strings.TrimPrefix(postTime, "发表时间：")

		// 从详情页路径提取ID（如：thread?topicId=42230 -> 42230）
		idMatch := leijingRe1.FindStringSubmatch(detailPath)
		resourceID := ""
		if len(idMatch) > 1 {
			resourceID = idMatch[1]
		} else {
			resourceID = fmt.Sprintf("%d", time.Now().UnixNano())
		}

		if p.debugMode {
			log.Printf("[Leijing] 提取结果 %d: %s, URL: %s", i+1, title, detailURL)
		}

		// 尝试从摘要中提取天翼云盘链接
		links := p.extractTianyiLinks(summary)

		if p.debugMode {
			log.Printf("[Leijing] 从摘要中提取到 %d 个链接", len(links))
		}

		// 解析时间
		var publishTime time.Time
		if postTime != "" {
			parsedTime, err := time.Parse("2006-01-02 15:04:05", postTime)
			if err == nil {
				publishTime = parsedTime
			} else {
				publishTime = time.Now()
			}
		} else {
			publishTime = time.Now()
		}

		result := model.SearchResult{
			Title:     title,
			Content:   summary,
			Channel:   "",
			MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
			Datetime:  publishTime,
			Links:     links,
		}

		// 如果没有从摘要中提取到链接，将详情页URL存储在Tags中供后续使用
		if len(links) == 0 {
			result.Tags = []string{detailURL}
		}

		results = append(results, result)
	})

	return results
}

// extractTianyiLinks 从文本中提取天翼云盘链接
func (p *LeijingPlugin) extractTianyiLinks(text string) []model.Link {
	var links []model.Link

	// 天翼云盘链接正则
	tianyiRegex := leijingRe2
	matches := tianyiRegex.FindAllString(text, -1)

	// 去重
	linkMap := make(map[string]bool)
	for _, match := range matches {
		if !linkMap[match] {
			linkMap[match] = true
			links = append(links, model.Link{
				URL:  match,
				Type: "tianyi",
			})
		}
	}

	return links
}

// enrichWithDetailLinks 并发获取详情页的下载链接
func (p *LeijingPlugin) enrichWithDetailLinks(client *http.Client, results []model.SearchResult, keyword string) []model.SearchResult {
	if p.debugMode {
		log.Printf("[Leijing] 开始获取详情页链接")
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, MaxConcurrency)

	for i := range results {
		// 如果已经有链接了，跳过
		if len(results[i].Links) > 0 {
			continue
		}

		// 如果没有详情页URL，跳过
		if len(results[i].Tags) == 0 {
			continue
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加小延迟避免请求过快
			time.Sleep(time.Duration(idx*50) * time.Millisecond)

			detailURL := results[idx].Tags[0]
			links := p.fetchDetailPageLinks(client, detailURL)

			mu.Lock()
			if len(links) > 0 {
				results[idx].Links = links
			}
			// 清空Tags
			results[idx].Tags = nil
			mu.Unlock()

			if p.debugMode {
				log.Printf("[Leijing] 详情页 %d/%d 获取到 %d 个链接", idx+1, len(results), len(links))
			}
		}(i)
	}

	wg.Wait()

	return results
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *LeijingPlugin) fetchDetailPageLinks(client *http.Client, detailURL string) []model.Link {
	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[Leijing] 使用缓存的详情页结果: %s", detailURL)
			}
			return links
		}
	}

	// 访问详情页
	resp, err := p.doRequest(client, detailURL, BaseURL)
	if err != nil {
		if p.debugMode {
			log.Printf("[Leijing] 获取详情页失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Leijing] 详情页响应状态码异常: %d", resp.StatusCode)
		}
		return nil
	}

	// 处理响应体
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if p.debugMode {
			log.Printf("[Leijing] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	// 提取详情页中的天翼云盘链接
	links := p.extractDetailPageLinks(doc)

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

// extractDetailPageLinks 从详情页HTML中提取天翼云盘链接
func (p *LeijingPlugin) extractDetailPageLinks(doc *goquery.Document) []model.Link {
	var links []model.Link
	linkMap := make(map[string]bool) // 用于去重

	// 从详情页内容中查找所有链接
	doc.Find(".topicContent a[href*='cloud.189.cn']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 去重
		if linkMap[href] {
			return
		}
		linkMap[href] = true

		links = append(links, model.Link{
			URL:  href,
			Type: "tianyi",
		})

		if p.debugMode {
			log.Printf("[Leijing] 提取到天翼云盘链接: %s", href)
		}
	})

	// 如果没有找到链接，尝试从文本中提取
	if len(links) == 0 {
		content := doc.Find(".topicContent").Text()
		links = p.extractTianyiLinks(content)
	}

	return links
}

// filterValidResults 过滤有效结果（去掉没有链接的）
func (p *LeijingPlugin) filterValidResults(results []model.SearchResult) []model.SearchResult {
	var validResults []model.SearchResult

	for _, result := range results {
		if len(result.Links) > 0 {
			validResults = append(validResults, result)
		} else if p.debugMode {
			log.Printf("[Leijing] 忽略无链接结果: %s", result.Title)
		}
	}

	return validResults
}

func init() {
	plugin.RegisterGlobalPlugin(NewLeijingPlugin())
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	leijingRe1 = regexp.MustCompile(`topicId=(\d+)`)
	leijingRe2 = regexp.MustCompile(`https://cloud\.189\.cn/t/[a-zA-Z0-9]+`)
)
