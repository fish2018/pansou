package qingying

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

const (
	baseURL       = "http://revohd.com"
	searchPath    = "/vodsearch/-------------.html"
	maxResults    = 10
	maxConcurrent = 3
)

var debugMode = false

func debugPrintf(format string, args ...interface{}) {
	if debugMode {
		fmt.Printf("[QingYing DEBUG] "+format, args...)
	}
}

type QingYingPlugin struct {
	*plugin.BaseAsyncPlugin
}

func init() {
	p := &QingYingPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("qingying", 3),
	}
	plugin.RegisterGlobalPlugin(p)
}

func (p *QingYingPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *QingYingPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *QingYingPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	debugPrintf("🔍 开始搜索 - keyword: %s\n", keyword)
	searchURL := fmt.Sprintf("%s%s?wd=%s", baseURL, searchPath, url.QueryEscape(keyword))
	debugPrintf("📝 搜索URL: %s\n", searchURL)

	items, err := p.fetchSearchResults(searchURL, client)
	if err != nil {
		debugPrintf("❌ 获取搜索结果失败: %v\n", err)
		return nil, err
	}

	debugPrintf("✅ 获取到 %d 个搜索结果\n", len(items))

	if len(items) == 0 {
		debugPrintf("⚠️ 没有搜索结果\n")
		return []model.SearchResult{}, nil
	}

	filteredItems := p.filterItemsByKeyword(items, keyword)
	debugPrintf("🔎 标题过滤后剩余 %d 个结果（从 %d 个）\n", len(filteredItems), len(items))

	if len(filteredItems) == 0 {
		debugPrintf("⚠️ 标题过滤后没有匹配的结果\n")
		return []model.SearchResult{}, nil
	}

	if len(filteredItems) > maxResults {
		debugPrintf("✂️ 限制结果数量从 %d 到 %d\n", len(filteredItems), maxResults)
		filteredItems = filteredItems[:maxResults]
	}

	results := p.processDetailPages(filteredItems, client)
	debugPrintf("📊 处理完成，获得 %d 个有效结果\n", len(results))

	return results, nil
}

type searchItem struct {
	ID        string
	Title     string
	DetailURL string
}

func (p *QingYingPlugin) filterItemsByKeyword(items []searchItem, keyword string) []searchItem {
	lowerKeyword := strings.ToLower(keyword)
	var filtered []searchItem

	for _, item := range items {
		lowerTitle := strings.ToLower(item.Title)
		if strings.Contains(lowerTitle, lowerKeyword) {
			debugPrintf("✅ 标题匹配: %s\n", item.Title)
			filtered = append(filtered, item)
		} else {
			debugPrintf("❌ 标题不匹配，跳过: %s\n", item.Title)
		}
	}

	return filtered
}

func (p *QingYingPlugin) fetchSearchResults(searchURL string, client *http.Client) ([]searchItem, error) {
	debugPrintf("🌐 请求搜索页面: %s\n", searchURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	p.setHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	debugPrintf("📡 HTTP状态码: %d\n", resp.StatusCode)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	var items []searchItem
	doc.Find("div.module-search-item").Each(func(i int, s *goquery.Selection) {
		link := s.Find(".video-info .video-info-header h3 a")
		href, exists := link.Attr("href")
		if !exists {
			debugPrintf("⚠️ 第%d个结果没有href属性\n", i+1)
			return
		}

		title := strings.TrimSpace(link.Text())
		if title == "" {
			title, _ = link.Attr("title")
			title = strings.TrimSpace(title)
		}

		if title == "" {
			debugPrintf("⚠️ 第%d个结果标题为空\n", i+1)
			return
		}

		re := qingyingRe1
		matches := re.FindStringSubmatch(href)
		if len(matches) < 2 {
			debugPrintf("⚠️ 无法从href提取ID: %s\n", href)
			return
		}

		item := searchItem{
			ID:        matches[1],
			Title:     title,
			DetailURL: p.buildAbsURL(href),
		}
		debugPrintf("📌 找到影片: ID=%s, Title=%s\n", item.ID, item.Title)
		items = append(items, item)
	})

	debugPrintf("✅ 解析到 %d 个搜索项\n", len(items))
	return items, nil
}

func (p *QingYingPlugin) processDetailPages(items []searchItem, client *http.Client) []model.SearchResult {
	var results []model.SearchResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for _, item := range items {
		wg.Add(1)
		go func(it searchItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := p.processDetailPage(it, client)
			if result != nil {
				mu.Lock()
				results = append(results, *result)
				mu.Unlock()
			}
		}(item)
	}

	wg.Wait()
	return results
}

func (p *QingYingPlugin) processDetailPage(item searchItem, client *http.Client) *model.SearchResult {
	debugPrintf("🎬 处理详情页: %s (ID: %s)\n", item.Title, item.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", item.DetailURL, nil)
	if err != nil {
		debugPrintf("❌ 创建请求失败: %v\n", err)
		return nil
	}

	p.setHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		debugPrintf("❌ 详情页请求失败: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		debugPrintf("❌ 详情页状态码: %d\n", resp.StatusCode)
		return nil
	}

	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		debugPrintf("❌ 读取响应失败: %v\n", err)
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		debugPrintf("❌ HTML解析失败: %v\n", err)
		return nil
	}

	title := strings.TrimSpace(doc.Find(".video-info .video-info-header h1.page-title a").Text())
	if title == "" {
		title = item.Title
	}
	debugPrintf("📝 影片标题: %s\n", title)

	var description string
	var updateTime time.Time
	doc.Find(".video-info-items").Each(func(i int, s *goquery.Selection) {
		itemTitle := strings.TrimSpace(s.Find(".video-info-itemtitle").Text())

		if strings.Contains(itemTitle, "更新") {
			timeText := strings.TrimSpace(s.Find(".video-info-item").Text())
			debugPrintf("🕐 找到更新时间文本: %s\n", timeText)
			updateTime = p.parseUpdateTimeFromHTML(timeText)
			if !updateTime.IsZero() {
				debugPrintf("✅ 解析更新时间成功: %v\n", updateTime)
			}
		}

		if strings.Contains(itemTitle, "剧情") {
			content := s.Find(".video-info-item.video-info-content span")
			if content.Length() > 0 {
				description = strings.TrimSpace(content.Text())
			} else {
				description = strings.TrimSpace(s.Find(".video-info-item").Text())
			}
			if len(description) > 50 {
				debugPrintf("📖 剧情简介: %s...\n", description[:50])
			} else {
				debugPrintf("📖 剧情简介: %s\n", description)
			}
		}
	})

	if updateTime.IsZero() {
		updateTime = time.Now()
		debugPrintf("⚠️ 未找到更新时间，使用当前时间\n")
	}

	panLink := p.extract123PanLink(doc)
	if panLink == nil {
		debugPrintf("❌ 未找到123网盘链接\n")
		return nil
	}

	debugPrintf("✅ 找到123网盘链接: %s (密码: %s)\n", panLink.URL, panLink.Password)

	return &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Title:    title,
		Content:  description,
		Links:    []model.Link{*panLink},
		Channel:  "",
		Datetime: updateTime,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (p *QingYingPlugin) extract123PanLink(doc *goquery.Document) *model.Link {
	debugPrintf("🔎 开始提取123网盘链接\n")
	var panURL string

	found := false
	doc.Find(".module-heading h2.module-title").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		debugPrintf("📋 找到标题: %s\n", text)
		if strings.Contains(text, "123") && strings.Contains(text, "云盘") {
			found = true
			debugPrintf("✅ 匹配到123云盘标题\n")
		}
	})

	if !found {
		debugPrintf("❌ 未找到123云盘标题区域\n")
		return nil
	}

	doc.Find(".module-downlist .module-row-text").Each(func(i int, s *goquery.Selection) {
		if panURL != "" {
			return
		}

		clipboardText, exists := s.Attr("data-clipboard-text")
		debugPrintf("🔗 检查链接 #%d: exists=%v, text=%s\n", i+1, exists, clipboardText)
		if exists {
			url := strings.TrimSpace(clipboardText)
			if strings.Contains(url, "123684.com") || strings.Contains(url, "123685.com") ||
				strings.Contains(url, "123912.com") || strings.Contains(url, "123pan.com") ||
				strings.Contains(url, "123pan.cn") || strings.Contains(url, "123592.com") {
				panURL = url
				debugPrintf("✅ 找到123网盘链接: %s\n", panURL)
			}
		}
	})

	if panURL == "" {
		debugPrintf("❌ 未找到123网盘链接\n")
		return nil
	}

	password := p.extractPassword(panURL)
	debugPrintf("🔑 提取密码: %s\n", password)

	return &model.Link{
		Type:     "123",
		URL:      panURL,
		Password: password,
	}
}

func (p *QingYingPlugin) parseUpdateTimeFromHTML(timeText string) time.Time {
	re := qingyingRe2
	matches := re.FindStringSubmatch(timeText)
	if len(matches) < 2 {
		debugPrintf("❌ 无法从文本提取时间: %s\n", timeText)
		return time.Time{}
	}

	timeStr := strings.TrimSpace(matches[1])
	debugPrintf("🔍 提取到时间字符串: %s\n", timeStr)

	t, err := time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.Local)
	if err != nil {
		debugPrintf("❌ 时间解析失败: %v\n", err)
		return time.Time{}
	}

	return t
}

func (p *QingYingPlugin) extractPassword(panURL string) string {
	parsed, err := url.Parse(panURL)
	if err != nil {
		return ""
	}

	pwd := parsed.Query().Get("pwd")
	if pwd != "" && len(pwd) == 4 {
		return pwd
	}

	pwdRegex := qingyingRe3
	if matches := pwdRegex.FindStringSubmatch(panURL); len(matches) > 1 {
		return matches[1]
	}

	return ""
}

func (p *QingYingPlugin) buildAbsURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if strings.HasPrefix(path, "//") {
		return "https:" + path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
}

func (p *QingYingPlugin) setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *QingYingPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

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
	qingyingRe1 = regexp.MustCompile(`/voddetail/(\d+)\.html`)
	qingyingRe2 = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`)
	qingyingRe3 = regexp.MustCompile(`pwd=([a-zA-Z0-9]{4})`)
)
