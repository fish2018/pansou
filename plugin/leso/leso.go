package leso

import (
	"context"
	"fmt"
	"html"
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
)

const (
	pluginName     = "leso"
	baseURL        = "https://www.leso.cc"
	requestTimeout = 20 * time.Second
	maxResults     = 30
	maxConcurrency = 6
)

var (
	threadIDRE   = regexp.MustCompile(`(?:tid=|thread-)([0-9]+)`)
	dateRE       = regexp.MustCompile(`20[0-9]{2}[-/]([0-9]{1,2})[-/]([0-9]{1,2})(?:\s+([0-9]{1,2}):([0-9]{2}))?`)
	urlRE        = regexp.MustCompile(`(?i)(?:https?://|magnet:\?|ed2k://)[^\s<>"]+`)
	linkPatterns = []struct {
		re  *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`(?i)https?://pan\.baidu\.com/s/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "baidu"},
		{regexp.MustCompile(`(?i)https?://pan\.quark\.cn/(?:s|g)/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "quark"},
		{regexp.MustCompile(`(?i)https?://(?:www\.)?(?:aliyundrive\.com|alipan\.com)/s/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "aliyun"},
		{regexp.MustCompile(`(?i)https?://pan\.xunlei\.com/s/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "xunlei"},
		{regexp.MustCompile(`(?i)https?://drive\.uc\.cn/s/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "uc"},
		{regexp.MustCompile(`(?i)https?://cloud\.189\.cn/(?:t|web/share)[^\s<>"]*`), "tianyi"},
		{regexp.MustCompile(`(?i)https?://(?:www\.)?115\.com/[a-zA-Z0-9/?=&._-]+`), "115"},
		{regexp.MustCompile(`(?i)https?://(?:www\.)?(?:123pan\.com|123684\.com|123865\.com|123685\.com|123592\.com|123912\.com)/s/[0-9A-Za-z_-]+(?:\?[^\s<>"]*)?`), "123"},
		{regexp.MustCompile(`(?i)magnet:\?[^\s<>"']+`), "magnet"},
		{regexp.MustCompile(`(?i)ed2k://[^\s<>"']+`), "ed2k"},
	}
	passwordRE = regexp.MustCompile(`(?i)(?:提取码|密码|访问码|提取密码|pwd|passcode|code)\s*[:：=]?\s*([0-9A-Za-z]{3,12})`)
)

type searchItem struct {
	id        string
	title     string
	detailURL string
	date      time.Time
	summary   string
}

type Plugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewPlugin())
}

func NewPlugin() *Plugin {
	return &Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		client:          &http.Client{Timeout: requestTimeout},
	}
}

func (p *Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if p.client != nil {
		client = p.client
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	doc, err := p.fetchSearch(client, keyword)
	if err != nil {
		return nil, err
	}
	items := parseSearchItems(doc)
	if len(items) > maxResults {
		items = items[:maxResults]
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	results := make([]model.SearchResult, 0, len(items))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, maxConcurrency)
	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result, ok := p.fetchDetail(client, item)
			if !ok {
				return
			}
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *Plugin) fetchSearch(client *http.Client, keyword string) (*goquery.Document, error) {
	form := url.Values{}
	form.Set("mod", "forum")
	form.Set("srchtxt", keyword)
	form.Set("searchsubmit", "yes")
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/search.php?searchsubmit=yes", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setHeaders(req, baseURL+"/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索请求返回 HTTP %d", p.Name(), resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}
	return doc, nil
}

func (p *Plugin) fetchDetail(client *http.Client, item searchItem) (model.SearchResult, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.detailURL, nil)
	if err != nil {
		return model.SearchResult{}, false
	}
	setHeaders(req, baseURL+"/")
	resp, err := client.Do(req)
	if err != nil {
		return model.SearchResult{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return model.SearchResult{}, false
	}
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return model.SearchResult{}, false
	}
	contentNode := doc.Find("td[id^='postmessage_'], td.t_f").First()
	if contentNode.Length() == 0 {
		contentNode = doc.Find("body").First()
	}
	contentHTML, _ := contentNode.Html()
	contentText := cleanText(contentNode.Text())
	links := extractLinks(contentHTML + "\n" + contentText)
	if len(links) == 0 {
		return model.SearchResult{}, false
	}

	title := item.title
	if title == "" {
		title = cleanText(doc.Find("#thread_subject").First().Text())
	}
	if title == "" {
		title = "乐搜资源"
	}
	if len(contentText) > 600 {
		contentText = contentText[:600] + "..."
	}
	for i := range links {
		if links[i].WorkTitle == "" {
			links[i].WorkTitle = title
		}
	}
	imageURL := ""
	if src, ok := contentNode.Find("img").First().Attr("src"); ok {
		imageURL = absoluteURL(src)
	}
	result := model.SearchResult{
		UniqueID:  fmt.Sprintf("%s-%s", p.Name(), item.id),
		MessageID: fmt.Sprintf("%s-%s", p.Name(), item.id),
		Title:     title,
		Content:   contentText,
		Links:     links,
		Channel:   "",
		Datetime:  item.date,
	}
	if imageURL != "" {
		result.Images = []string{imageURL}
	}
	return result, true
}

func parseSearchItems(doc *goquery.Document) []searchItem {
	items := make([]searchItem, 0)
	seen := make(map[string]struct{})
	doc.Find(".slst li.pbw").Each(func(_ int, s *goquery.Selection) {
		anchor := s.Find("h3 a").First()
		if anchor.Length() == 0 {
			return
		}
		href := absoluteURL(anchor.AttrOr("href", ""))
		if href == "" {
			return
		}
		id := ""
		if match := threadIDRE.FindStringSubmatch(href); len(match) > 1 {
			id = match[1]
		}
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		title := cleanText(anchor.Text())
		if title == "" {
			return
		}
		text := cleanText(s.Text())
		date := parseDate(text)
		summary := text
		if len(summary) > 240 {
			summary = summary[:240] + "..."
		}
		seen[id] = struct{}{}
		items = append(items, searchItem{id: id, title: title, detailURL: href, date: date, summary: summary})
	})
	return items
}

func extractLinks(text string) []model.Link {
	text = html.UnescapeString(text)
	links := make([]model.Link, 0)
	seen := make(map[string]struct{})
	for _, match := range urlRE.FindAllStringIndex(text, -1) {
		raw := strings.TrimRight(text[match[0]:match[1]], ".,;，。；)）]】")
		typ, normalized := classifyLink(raw)
		if typ == "" || normalized == "" {
			continue
		}
		key := typ + "|" + normalized
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		contextText := text[match[0]:match[1]]
		start := match[0] - 100
		if start < 0 {
			start = 0
		}
		end := match[1] + 100
		if end > len(text) {
			end = len(text)
		}
		contextText = text[start:end]
		links = append(links, model.Link{Type: typ, URL: normalized, Password: extractPassword(contextText)})
	}
	return links
}

func classifyLink(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	for _, pattern := range linkPatterns {
		if match := pattern.re.FindString(raw); match != "" {
			return pattern.typ, match
		}
	}
	return "", ""
}

func extractPassword(text string) string {
	if match := passwordRE.FindStringSubmatch(text); len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	if parsed, err := url.Parse(strings.TrimSpace(text)); err == nil {
		for _, key := range []string{"pwd", "passcode", "code"} {
			if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
				return value
			}
		}
	}
	return ""
}

func parseDate(text string) time.Time {
	if match := dateRE.FindStringSubmatch(text); len(match) > 0 {
		layout := "2006-1-2"
		value := strings.ReplaceAll(match[0], "/", "-")
		if strings.Contains(match[0], ":") {
			layout = "2006-1-2 15:04"
		}
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed
		}
	}
	return time.Now()
}

func absoluteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "magnet:") || strings.HasPrefix(raw, "ed2k:") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return baseURL + "/" + strings.TrimPrefix(raw, "/")
}

func setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", referer)
}

func cleanText(value string) string {
	value = strings.ReplaceAll(value, "\u00a0", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}
