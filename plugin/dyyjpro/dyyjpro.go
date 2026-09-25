package dyyjpro

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
	"pansou/util"
)

const (
	pluginName      = "dyyjpro"
	baseURL         = "https://dyyjpro.com"
	searchURL       = baseURL + "/?cat=&s=%s"
	defaultPriority = 2
	searchTimeout   = 15 * time.Second
	detailTimeout   = 10 * time.Second
	maxRetries      = 3
	maxConcurrency  = 12
)

var (
	postIDRegex = regexp.MustCompile(`/(\d+)\.html?$`)
	textURLReg  = regexp.MustCompile(`https?://[^\s<>"']+`)
	spaceRegex  = regexp.MustCompile(`\s+`)

	linkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/(?:s|g)/[0-9A-Za-z]+(?:\?pwd=[0-9A-Za-z]+)?`), "quark"},
		{regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9A-Za-z\-_]+(?:\?pwd=[0-9A-Za-z]+)?`), "baidu"},
		{regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9A-Za-z\-_]+`), "xunlei"},
		{regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9A-Za-z]+`), "uc"},
		{regexp.MustCompile(`https?://(?:www\.)?(?:alipan\.com|aliyundrive\.com)/s/[0-9A-Za-z]+`), "aliyun"},
		{regexp.MustCompile(`https?://(?:www\.)?(?:123pan\.com|123684\.com|123865\.com|123685\.com|123592\.com|123912\.com)/s/[0-9A-Za-z]+`), "123"},
		{regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9A-Za-z]+`), "magnet"},
	}

	passwordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}
)

type DyyjproPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type articleItem struct {
	ID         string
	Title      string
	DetailURL  string
	ImageURL   string
	Category   string
	PublishRaw string
}

func init() {
	plugin.RegisterGlobalPlugin(NewDyyjproPlugin())
}

func NewDyyjproPlugin() *DyyjproPlugin {
	return &DyyjproPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client: &http.Client{
			Timeout: searchTimeout,
			Transport: &http.Transport{
				Proxy:               util.ProxyFuncForTransport(),
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				MaxConnsPerHost:     24,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *DyyjproPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *DyyjproPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *DyyjproPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	items, err := p.fetchSearchResults(client, keyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, maxConcurrency)
		results = make([]model.SearchResult, 0, len(items))
	)

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}

		go func(item articleItem) {
			defer wg.Done()
			defer func() { <-sem }()

			links, content, images := p.fetchDetail(client, item.DetailURL)
			if len(links) == 0 {
				return
			}

			tags := make([]string, 0, 1)
			if item.Category != "" {
				tags = append(tags, item.Category)
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
				Title:    item.Title,
				Content:  content,
				Links:    links,
				Tags:     tags,
				Images:   images,
				Channel:  "",
				Datetime: parseDateTime(item.PublishRaw),
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *DyyjproPlugin) fetchSearchResults(client *http.Client, keyword string) ([]articleItem, error) {
	requestURL := fmt.Sprintf(searchURL, url.QueryEscape(keyword))
	doc, err := fetchDocument(client, requestURL, searchTimeout, baseURL+"/")
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	items := make([]articleItem, 0)
	doc.Find("article.post-item.item-grid").Each(func(_ int, s *goquery.Selection) {
		titleLink := s.Find("h2.entry-title a").First()
		title := cleanText(titleLink.Text())
		detailURL, ok := titleLink.Attr("href")
		if !ok || detailURL == "" || title == "" {
			return
		}

		id := extractPostID(detailURL)
		if id == "" {
			return
		}

		imageURL := strings.TrimSpace(s.Find("a.media-img").AttrOr("data-bg", ""))
		category := cleanText(s.Find("span.meta-cat-dot a").First().Text())
		publishRaw := strings.TrimSpace(s.Find("time.pub-date").AttrOr("datetime", ""))

		items = append(items, articleItem{
			ID:         id,
			Title:      title,
			DetailURL:  detailURL,
			ImageURL:   normalizeURL(imageURL),
			Category:   category,
			PublishRaw: publishRaw,
		})
	})

	return items, nil
}

func (p *DyyjproPlugin) fetchDetail(client *http.Client, detailURL string) ([]model.Link, string, []string) {
	doc, err := fetchDocument(client, detailURL, detailTimeout, baseURL+"/")
	if err != nil {
		return nil, "", nil
	}

	contentNode := doc.Find("article.post-content").First()
	if contentNode.Length() == 0 {
		contentNode = doc.Selection
	}

	links := extractLinks(contentNode)
	if len(links) == 0 {
		return nil, "", nil
	}

	content := cleanText(contentNode.Text())
	if len(content) > 300 {
		content = content[:300] + "..."
	}

	images := []string{}
	if cover := doc.Find("div.archive-hero-bg").First(); cover.Length() > 0 {
		if bg, ok := cover.Attr("data-bg"); ok {
			images = append(images, normalizeURL(bg))
		}
	}
	if img := contentNode.Find("img").First(); img.Length() > 0 {
		if src, ok := img.Attr("src"); ok {
			images = append(images, normalizeURL(src))
		}
	}

	return links, content, dedupeStrings(images)
}

func extractLinks(selection *goquery.Selection) []model.Link {
	results := make([]model.Link, 0)
	seen := make(map[string]struct{})

	addLink := func(rawURL string, context string) {
		linkType, normalized := classifyLink(rawURL)
		if linkType == "" || normalized == "" {
			return
		}
		key := linkType + "|" + normalized
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		results = append(results, model.Link{
			Type:     linkType,
			URL:      normalized,
			Password: matchPassword(context),
		})
	}

	selection.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
		href, ok := node.Attr("href")
		if !ok {
			return
		}
		addLink(href, node.Parent().Text()+" "+node.Text())
	})

	text := selection.Text()
	for _, idx := range textURLReg.FindAllStringIndex(text, -1) {
		raw := text[idx[0]:idx[1]]
		addLink(raw, substring(text, idx[0]-80, idx[1]+80))
	}

	return results
}

func classifyLink(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	for _, pattern := range linkPatterns {
		if matched := pattern.reg.FindString(value); matched != "" {
			return pattern.typ, matched
		}
	}
	return "", ""
}

func matchPassword(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, pattern := range passwordPatterns {
		if matches := pattern.FindStringSubmatch(text); len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	return extractURLPassword(text)
}

func extractPostID(detailURL string) string {
	if matches := postIDRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	if strings.HasPrefix(raw, "/") {
		return baseURL + raw
	}
	return baseURL + "/" + raw
}

func parseDateTime(raw string) time.Time {
	raw = cleanText(raw)
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed
		}
	}
	return time.Now()
}

func substring(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	if start >= end {
		return ""
	}
	return text[start:end]
}

func dedupeStrings(items []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func cleanText(text string) string {
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	return strings.TrimSpace(spaceRegex.ReplaceAllString(text, " "))
}

func extractURLPassword(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, key := range []string{"pwd", "passcode", "code"} {
		if value := strings.TrimSpace(u.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func fetchDocument(client *http.Client, requestURL string, timeout time.Duration, referer string) (*goquery.Document, error) {
	body, err := fetchBody(client, requestURL, timeout, referer)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(strings.NewReader(string(body)))
}

func fetchBody(client *http.Client, requestURL string, timeout time.Duration, referer string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Referer", referer)

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			data, readErr := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
			cancel()
			return data, readErr
		}
		if resp != nil {
			resp.Body.Close()
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		cancel()
		if attempt < maxRetries-1 {
			time.Sleep(200 * time.Millisecond * time.Duration(1<<attempt))
		}
	}
	return nil, lastErr
}
