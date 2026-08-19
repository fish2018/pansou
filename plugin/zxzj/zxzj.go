package zxzj

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

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	baseURL       = "https://www.zxzjys.com"
	searchPath    = "/vodsearch/-------------.html"
	maxResults    = 10
	maxConcurrent = 5
)

type ZXZJPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	p := &ZXZJPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("zxzj", 3),
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	plugin.RegisterGlobalPlugin(p)
}

func (p *ZXZJPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *ZXZJPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *ZXZJPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s%s?wd=%s&submit=", baseURL, searchPath, url.QueryEscape(keyword))

	items, err := p.fetchSearchResults(searchURL)
	if err != nil {
		return nil, err
	}

	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	if len(items) > maxResults {
		items = items[:maxResults]
	}

	results := p.processDetailPages(items)

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

type searchItem struct {
	ID        string
	Title     string
	DetailURL string
}

func (p *ZXZJPlugin) fetchSearchResults(searchURL string) ([]searchItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", baseURL)

	resp, err := p.doRequestWithRetry(req, p.client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	var items []searchItem
	doc.Find("ul.stui-vodlist li").Each(func(i int, s *goquery.Selection) {
		link := s.Find(".stui-vodlist__detail h4.title a")
		href, exists := link.Attr("href")
		if !exists {
			return
		}

		title := strings.TrimSpace(link.Text())
		if title == "" {
			return
		}

		re := regexp.MustCompile(`/(voddetail|detail)/(\d+)\.html`)
		matches := re.FindStringSubmatch(href)
		if len(matches) < 3 {
			return
		}

		items = append(items, searchItem{
			ID:        matches[2],
			Title:     title,
			DetailURL: p.buildAbsURL(href),
		})
	})

	return items, nil
}

func (p *ZXZJPlugin) processDetailPages(items []searchItem) []model.SearchResult {
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

			result := p.processDetailPage(it)
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

func (p *ZXZJPlugin) processDetailPage(item searchItem) *model.SearchResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", item.DetailURL, nil)
	if err != nil {
		return nil
	}

	p.setHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, p.client)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	title := strings.TrimSpace(doc.Find(".stui-content__detail h1.title").Text())
	if title == "" {
		title = item.Title
	}

	var description string
	var updateTime time.Time
	doc.Find(".stui-content__detail p.data").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text != "" {
			if description != "" {
				description += "\n"
			}
			description += text

			if updateTime.IsZero() && strings.Contains(text, "更新") {
				updateTime = p.parseUpdateTime(text)
			}
		}
	})

	if updateTime.IsZero() {
		updateTime = time.Now()
	}

	playLinks := p.extractPlayLinks(doc)
	if len(playLinks) == 0 {
		return nil
	}

	links := p.fetchPanLinks(playLinks)
	if len(links) == 0 {
		return nil
	}

	return &model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Title:    title,
		Content:  description,
		Links:    links,
		Channel:  "",
		Datetime: updateTime,
	}
}

type playLink struct {
	URL      string
	Label    string
	LineType string
}

func (p *ZXZJPlugin) extractPlayLinks(doc *goquery.Document) []playLink {
	var links []playLink

	doc.Find(".stui-vodlist__head").Each(func(i int, head *goquery.Selection) {
		lineTitle := strings.TrimSpace(head.Find("h3").Text())
		if lineTitle == "" {
			return
		}

		panType := p.detectPanType(lineTitle)
		if panType == "" {
			return
		}

		playlist := head.Next()
		for playlist.Length() > 0 && !playlist.Is("ul.stui-content__playlist") {
			playlist = playlist.Next()
		}

		if playlist.Length() == 0 {
			return
		}

		playlist.Find("li a").Each(func(j int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			if !exists {
				return
			}

			label := strings.TrimSpace(a.Text())
			links = append(links, playLink{
				URL:      p.buildAbsURL(href),
				Label:    label,
				LineType: panType,
			})
		})
	})

	return links
}

func (p *ZXZJPlugin) detectPanType(title string) string {
	lower := strings.ToLower(title)

	if strings.Contains(lower, "百度") {
		return "baidu"
	}
	if strings.Contains(lower, "夸克") {
		return "quark"
	}
	if strings.Contains(lower, "迅雷") {
		return "xunlei"
	}

	return ""
}

func (p *ZXZJPlugin) fetchPanLinks(playLinks []playLink) []model.Link {
	var links []model.Link
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for _, pl := range playLinks {
		wg.Add(1)
		go func(playLink playLink) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			link := p.fetchSinglePanLink(playLink)
			if link != nil {
				mu.Lock()
				links = append(links, *link)
				mu.Unlock()
			}
		}(pl)
	}

	wg.Wait()
	return links
}

func (p *ZXZJPlugin) fetchSinglePanLink(pl playLink) *model.Link {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", pl.URL, nil)
	if err != nil {
		return nil
	}

	p.setHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, p.client)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	panURL, password := p.parsePlayerData(body)
	if panURL == "" {
		return nil
	}

	cloudType := p.determinePanType(panURL, pl.LineType)
	if cloudType == "" {
		return nil
	}

	return &model.Link{
		Type:     cloudType,
		URL:      panURL,
		Password: password,
	}
}

type playerData struct {
	URL  string `json:"url"`
	From string `json:"from"`
}

func (p *ZXZJPlugin) parsePlayerData(body []byte) (string, string) {
	re := regexp.MustCompile(`var\s+player_aaaa\s*=\s*(\{[^;]+\})`)
	matches := re.FindSubmatch(body)
	if len(matches) < 2 {
		return "", ""
	}

	var data playerData
	if err := json.Unmarshal(matches[1], &data); err != nil {
		return "", ""
	}

	panURL := strings.TrimSpace(data.URL)
	if panURL == "" {
		return "", ""
	}

	panURL = strings.ReplaceAll(panURL, `\/`, `/`)

	password := p.extractPassword(panURL)

	return panURL, password
}

func (p *ZXZJPlugin) extractPassword(panURL string) string {
	parsed, err := url.Parse(panURL)
	if err != nil {
		return ""
	}

	pwd := parsed.Query().Get("pwd")
	if pwd != "" && len(pwd) == 4 {
		return pwd
	}

	if strings.Contains(panURL, "|") {
		parts := strings.Split(panURL, "|")
		if len(parts) >= 2 {
			pwd := strings.TrimSpace(parts[1])
			if len(pwd) == 4 {
				return pwd
			}
		}
	}

	pwdRegex := regexp.MustCompile(`pwd=([a-zA-Z0-9]{4})`)
	if matches := pwdRegex.FindStringSubmatch(panURL); len(matches) > 1 {
		return matches[1]
	}

	return ""
}

func (p *ZXZJPlugin) determinePanType(panURL, lineType string) string {
	lower := strings.ToLower(panURL)

	if strings.Contains(lower, "pan.baidu.com") {
		return "baidu"
	}
	if strings.Contains(lower, "pan.quark.cn") {
		return "quark"
	}
	if strings.Contains(lower, "pan.xunlei.com") {
		return "xunlei"
	}
	if strings.Contains(lower, "aliyundrive.com") || strings.Contains(lower, "alipan.com") {
		return "aliyun"
	}

	if lineType != "" {
		return lineType
	}

	return ""
}

func (p *ZXZJPlugin) buildAbsURL(path string) string {
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

func (p *ZXZJPlugin) setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *ZXZJPlugin) parseUpdateTime(text string) time.Time {
	updateRegex := regexp.MustCompile(`更新[：:]\s*(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2}|\d{4}-\d{2}-\d{2})`)
	matches := updateRegex.FindStringSubmatch(text)
	if len(matches) < 2 {
		return time.Time{}
	}

	timeStr := strings.TrimSpace(matches[1])

	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, timeStr, time.Local); err == nil {
			return t
		}
	}

	return time.Time{}
}

func (p *ZXZJPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
			resp.Body.Close()
		}
		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}
