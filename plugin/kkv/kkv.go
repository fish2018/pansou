package kkv

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

const (
	baseURL       = "http://kkv.q-23.cn"
	searchPath    = "/"
	maxResults    = 10
	maxConcurrent = 3
)

var debugMode = false

func debugPrintf(format string, args ...interface{}) {
	if debugMode {
		fmt.Printf("[KKV DEBUG] "+format, args...)
	}
}

type KKVPlugin struct {
	*plugin.BaseAsyncPlugin
}

func init() {
	p := &KKVPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("kkv", 3),
	}
	plugin.RegisterGlobalPlugin(p)
}

func (p *KKVPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *KKVPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *KKVPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	debugPrintf("🔍 开始搜索 - keyword: %s\n", keyword)
	searchURL := fmt.Sprintf("%s%s?s=%s", baseURL, searchPath, url.QueryEscape(keyword))
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

func (p *KKVPlugin) filterItemsByKeyword(items []searchItem, keyword string) []searchItem {
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

func (p *KKVPlugin) fetchSearchResults(searchURL string, client *http.Client) ([]searchItem, error) {
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
	doc.Find("article.post").Each(func(i int, s *goquery.Selection) {
		link := s.Find(".entry-header h2.entry-title a")
		href, exists := link.Attr("href")
		if !exists {
			debugPrintf("⚠️ 第%d个结果没有href属性\n", i+1)
			return
		}

		title := strings.TrimSpace(link.Text())
		if title == "" {
			debugPrintf("⚠️ 第%d个结果标题为空\n", i+1)
			return
		}

		re := kkvRe1
		matches := re.FindStringSubmatch(href)
		if len(matches) < 2 {
			debugPrintf("⚠️ 无法从href提取ID: %s\n", href)
			return
		}

		item := searchItem{
			ID:        matches[1],
			Title:     title,
			DetailURL: href,
		}
		debugPrintf("📌 找到影片: ID=%s, Title=%s\n", item.ID, item.Title)
		items = append(items, item)
	})

	debugPrintf("✅ 解析到 %d 个搜索项\n", len(items))
	return items, nil
}

func (p *KKVPlugin) processDetailPages(items []searchItem, client *http.Client) []model.SearchResult {
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

func (p *KKVPlugin) processDetailPage(item searchItem, client *http.Client) *model.SearchResult {
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

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		debugPrintf("❌ HTML解析失败: %v\n", err)
		return nil
	}

	title := strings.TrimSpace(doc.Find(".entry-header h1.entry-title").Text())
	if title == "" {
		title = item.Title
	}
	debugPrintf("📝 影片标题: %s\n", title)

	var description string
	doc.Find(".entry-content p").First().Each(func(i int, s *goquery.Selection) {
		description = strings.TrimSpace(s.Text())
		if len(description) > 200 {
			description = description[:200] + "..."
		}
	})

	updateTime := p.extractUpdateTime(doc)
	debugPrintf("🕐 更新时间: %v\n", updateTime)

	panLinks := p.extractPanLinks(doc)
	if len(panLinks) == 0 {
		debugPrintf("❌ 未找到网盘链接\n")
		return nil
	}

	debugPrintf("✅ 找到 %d 个网盘链接\n", len(panLinks))

	return &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Title:    title,
		Content:  description,
		Links:    panLinks,
		Channel:  "",
		Datetime: updateTime,
	}
}

func (p *KKVPlugin) extractUpdateTime(doc *goquery.Document) time.Time {
	timeStr, exists := doc.Find("time.updated").Attr("datetime")
	if !exists {
		debugPrintf("⚠️ 未找到更新时间\n")
		return time.Now()
	}

	debugPrintf("🔍 提取到时间字符串: %s\n", timeStr)

	t, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		debugPrintf("❌ 时间解析失败: %v\n", err)
		return time.Now()
	}

	return t
}

func (p *KKVPlugin) extractPanLinks(doc *goquery.Document) []model.Link {
	debugPrintf("🔎 开始提取网盘链接\n")
	var links []model.Link

	doc.Find(".entry-content p").Each(func(i int, s *goquery.Selection) {
		s.Find("a").Each(func(j int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			if !exists {
				return
			}

			href = strings.TrimSpace(href)
			cloudType := p.determinePanType(href)
			if cloudType == "" {
				return
			}

			debugPrintf("🔗 找到%s链接: %s\n", cloudType, href)

			password := p.extractPassword(href, s.Text())
			debugPrintf("🔑 密码: %s\n", password)

			links = append(links, model.Link{
				Type:     cloudType,
				URL:      href,
				Password: password,
			})
		})
	})

	debugPrintf("✅ 共提取到 %d 个网盘链接\n", len(links))
	return links
}

func (p *KKVPlugin) determinePanType(panURL string) string {
	lower := strings.ToLower(panURL)

	switch {
	case strings.Contains(lower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lower, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lower, "aliyundrive.com"), strings.Contains(lower, "alipan.com"):
		return "aliyun"
	case strings.Contains(lower, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lower, "115.com"), strings.Contains(lower, "115cdn.com"), strings.Contains(lower, "anxia.com"):
		return "115"
	case strings.Contains(lower, "123684.com"), strings.Contains(lower, "123685.com"),
		strings.Contains(lower, "123912.com"), strings.Contains(lower, "123pan.com"),
		strings.Contains(lower, "123pan.cn"), strings.Contains(lower, "123592.com"):
		return "123"
	case strings.Contains(lower, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(lower, "mypikpak.com"):
		return "pikpak"
	default:
		return ""
	}
}

func (p *KKVPlugin) extractPassword(panURL, contextText string) string {
	parsed, err := url.Parse(panURL)
	if err == nil {
		pwd := parsed.Query().Get("pwd")
		if pwd != "" && len(pwd) == 4 {
			return pwd
		}
	}

	pwdPatterns := []*regexp.Regexp{
		kkvRe2,
		kkvRe3,
		kkvRe4,
	}

	for _, pattern := range pwdPatterns {
		if matches := pattern.FindStringSubmatch(contextText); len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

func (p *KKVPlugin) setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *KKVPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
	kkvRe1 = regexp.MustCompile(`\?p=(\d+)`)
	kkvRe2 = regexp.MustCompile(`提取码[：:]\s*([a-zA-Z0-9]{4})`)
	kkvRe3 = regexp.MustCompile(`密码[：:]\s*([a-zA-Z0-9]{4})`)
	kkvRe4 = regexp.MustCompile(`pwd[：:]\s*([a-zA-Z0-9]{4})`)
)
