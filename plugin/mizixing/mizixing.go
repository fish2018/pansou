package mizixing

import (
	"context"
	"fmt"
	"hash/crc32"
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
	pluginName      = "mizixing"
	defaultPriority = 3

	baseURL             = "https://mizixing.com"
	searchEndpoint      = baseURL + "/"
	searchLimit         = 12
	detailWorkers       = 6
	requestTimeout      = 12 * time.Second
	detailTimeout       = 10 * time.Second
	httpMaxIdleConns    = 64
	httpMaxIdlePerHost  = 16
	httpMaxConnsPerHost = 32
	retryBaseDelay      = 200 * time.Millisecond
	maxRequestRetries   = 3
)

var (
	linkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/(?:s|g)/[0-9A-Za-z]+`), "quark"},
		{regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9A-Za-z\-_?=&]+`), "baidu"},
		{regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9A-Za-z\-_?=&]+`), "xunlei"},
		{regexp.MustCompile(`https?://(?:www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9A-Za-z]+`), "aliyun"},
		{regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9A-Za-z]+`), "uc"},
		{regexp.MustCompile(`https?://(?:www\.)?(123pan\.com|123pan\.cn|123684\.com|123685\.com|123912\.com|123592\.com)/s/[0-9A-Za-z]+`), "123"},
		{regexp.MustCompile(`https?://(?:www\.)?mypikpak\.com/s/[0-9A-Za-z]+`), "pikpak"},
		{regexp.MustCompile(`https?://caiyun\.139\.com/[^\s<>"']+`), "mobile"},
		{regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9A-Za-z]+`), "magnet"},
		{regexp.MustCompile(`ed2k://[^\s<>"']+`), "ed2k"},
	}

	passwordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}

	textURLRegex = regexp.MustCompile(`https?://[^\s<>"']+`)
)

// MizixingPlugin implements the async plugin for mizixing.com
type MizixingPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewMizixingPlugin())
}

// NewMizixingPlugin builds plugin instance
func NewMizixingPlugin() *MizixingPlugin {
	return &MizixingPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search compatibility wrapper
func (p *MizixingPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult entrypoint
func (p *MizixingPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *MizixingPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchKeyword := strings.TrimSpace(keyword)
	if searchKeyword == "" {
		return nil, fmt.Errorf("[%s] 关键词不能为空", p.Name())
	}

	items, err := p.fetchSearchResults(client, searchKeyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("[%s] 未找到相关资源", p.Name())
	}

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, detailWorkers)
		resultM sync.Mutex
		results []model.SearchResult
	)

	for _, item := range items {
		item := item
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			detail, err := p.fetchDetailData(client, item.URL)
			if err != nil || len(detail.links) == 0 {
				return
			}

			content := item.Summary
			if content == "" {
				content = detail.description
			}

			result := model.SearchResult{
				UniqueID: buildUniqueID(item.URL),
				Title:    item.Title,
				Content:  strings.TrimSpace(content),
				Links:    detail.links,
				Tags:     mergeTags(item.Category, detail.tags),
				Channel:  "",
				Datetime: detail.datetime,
			}

			resultM.Lock()
			results = append(results, result)
			resultM.Unlock()
		}()
	}

	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("[%s] 未能抓取到有效网盘链接", p.Name())
	}

	return plugin.FilterResultsByKeyword(results, searchKeyword), nil
}

type searchItem struct {
	Title    string
	URL      string
	Category string
	Summary  string
}

func (p *MizixingPlugin) fetchSearchResults(client *http.Client, keyword string) ([]searchItem, error) {
	searchURL := fmt.Sprintf("%s?s=%s", searchEndpoint, url.QueryEscape(keyword))

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}

	var items []searchItem
	doc.Find("article.excerpt").Each(func(_ int, s *goquery.Selection) {
		if len(items) >= searchLimit {
			return
		}

		titleNode := s.Find("h2 a")
		urlStr, ok := titleNode.Attr("href")
		if !ok || strings.TrimSpace(urlStr) == "" {
			return
		}

		category := strings.TrimSpace(s.Find("header .label").Text())
		summary := strings.TrimSpace(s.Find("p.note").Text())
		title := strings.TrimSpace(titleNode.Text())

		if title == "" {
			title = strings.TrimSpace(s.Find("h2").Text())
		}

		items = append(items, searchItem{
			Title:    title,
			URL:      normalizeURL(urlStr),
			Category: category,
			Summary:  summary,
		})
	})

	return items, nil
}

type detailData struct {
	links       []model.Link
	datetime    time.Time
	tags        []string
	description string
}

func (p *MizixingPlugin) fetchDetailData(client *http.Client, detailURL string) (detailData, error) {
	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return detailData{}, fmt.Errorf("[%s] 创建详情页请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, detailURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return detailData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return detailData{}, fmt.Errorf("[%s] 详情页返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return detailData{}, fmt.Errorf("[%s] 解析详情页失败: %w", p.Name(), err)
	}

	content := doc.Find("article.article-content")
	if content.Length() == 0 {
		content = doc.Find(".article-content")
	}
	if content.Length() == 0 {
		content = doc.Find(".entry-content")
	}
	if content.Length() == 0 {
		content = doc.Selection
	}

	content.Find("script, style, .bdsharebuttonbox, #respond, .post-views, .share, .relates").Remove()

	links := extractLinksFromSelection(content)

	description := strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))

	tags := collectTags(doc)
	datetime := extractDateTime(doc)

	return detailData{
		links:       links,
		datetime:    datetime,
		tags:        tags,
		description: description,
	}, nil
}

func collectTags(doc *goquery.Document) []string {
	tagSet := make(map[string]struct{})

	doc.Find(".breadcrumbs a").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.Contains(text, "首页") {
			return
		}
		tagSet[text] = struct{}{}
	})

	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	return tags
}

func extractDateTime(doc *goquery.Document) time.Time {
	selectors := []string{
		"meta[property='article:modified_time']",
		"meta[property='article:published_time']",
		"meta[name='article:modified_time']",
		"meta[name='article:published_time']",
	}

	for _, sel := range selectors {
		if node := doc.Find(sel); node.Length() > 0 {
			if value, ok := node.Attr("content"); ok {
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
					return t
				}
				layouts := []string{
					"2006-01-02 15:04:05",
					time.RFC3339Nano,
				}
				for _, layout := range layouts {
					if t, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
						return t
					}
				}
			}
		}
	}

	return time.Now()
}

func extractLinksFromSelection(sel *goquery.Selection) []model.Link {
	var (
		results []model.Link
		seen    = make(map[string]struct{})
	)

	sel.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
		href, ok := node.Attr("href")
		if !ok {
			return
		}
		linkType, normalized := classifyLink(href)
		if linkType == "" || normalized == "" {
			return
		}
		if _, exists := seen[normalized]; exists {
			return
		}

		password := extractPassword(node)
		results = append(results, model.Link{
			Type:     linkType,
			URL:      normalized,
			Password: password,
		})
		seen[normalized] = struct{}{}
	})

	text := sel.Text()
	for _, loc := range textURLRegex.FindAllStringIndex(text, -1) {
		raw := text[loc[0]:loc[1]]
		linkType, normalized := classifyLink(raw)
		if linkType == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}

		context := substring(text, loc[0]-80, loc[1]+80)
		password := matchPassword(context)
		results = append(results, model.Link{
			Type:     linkType,
			URL:      normalized,
			Password: password,
		})
		seen[normalized] = struct{}{}
	}

	return results
}

func classifyLink(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	for _, pattern := range linkPatterns {
		if loc := pattern.reg.FindString(raw); loc != "" {
			return pattern.typ, loc
		}
	}
	return "", ""
}

func extractPassword(node *goquery.Selection) string {
	candidates := []string{node.Text()}

	if title, ok := node.Attr("title"); ok {
		candidates = append(candidates, title)
	}
	if parent := node.Parent(); parent != nil && parent.Length() > 0 {
		candidates = append(candidates, parent.Text())
		if next := parent.Next(); next.Length() > 0 {
			candidates = append(candidates, next.Text())
		}
	}
	if sibling := node.Next(); sibling.Length() > 0 {
		candidates = append(candidates, sibling.Text())
	}

	for _, text := range candidates {
		if pwd := matchPassword(text); pwd != "" {
			return pwd
		}
	}
	return ""
}

func matchPassword(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, pattern := range passwordPatterns {
		if matches := pattern.FindStringSubmatch(text); len(matches) > 1 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func substring(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}

func mergeTags(primary string, extra []string) []string {
	tagSet := make(map[string]struct{})
	if primary != "" {
		tagSet[strings.TrimSpace(primary)] = struct{}{}
	}
	for _, tag := range extra {
		if tag == "" {
			continue
		}
		tagSet[strings.TrimSpace(tag)] = struct{}{}
	}
	var tags []string
	for tag := range tagSet {
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func buildUniqueID(detailURL string) string {
	sum := crc32.ChecksumIEEE([]byte(detailURL))
	return fmt.Sprintf("%s-%d", pluginName, sum)
}

func normalizeURL(raw string) string {
	if strings.HasPrefix(raw, "http") {
		return raw
	}
	return baseURL + strings.TrimSpace(raw)
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:               util.ProxyFuncForTransport(),
			MaxIdleConns:        httpMaxIdleConns,
			MaxIdleConnsPerHost: httpMaxIdlePerHost,
			MaxConnsPerHost:     httpMaxConnsPerHost,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

func setHTMLHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *MizixingPlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
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
		if attempt < maxRetries-1 {
			time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
		}
	}

	return nil, fmt.Errorf("重试 %d 次后失败: %w", maxRetries, lastErr)
}
