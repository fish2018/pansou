package lou1

import (
	"context"
	"fmt"
	"hash/crc32"
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
	jsonutil "pansou/util/json"
)

const (
	pluginName      = "lou1"
	defaultPriority = 1

	baseURL             = "http://www.1lou.me"
	searchAPIPath       = baseURL + "/search/api/search.php"
	requestTimeout      = 12 * time.Second
	detailTimeout       = 12 * time.Second
	maxRequestRetries   = 3
	retryBaseDelay      = 200 * time.Millisecond
	searchLimit         = 12
	detailWorkers       = 6
	httpMaxIdleConns    = 64
	httpMaxIdlePerHost  = 16
	httpMaxConnsPerHost = 32
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
		{regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9A-Za-z]+`), "magnet"},
		{regexp.MustCompile(`ed2k://[^\s<>"']+`), "ed2k"},
	}

	passwordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}

	textURLRegex  = regexp.MustCompile(`https?://[^\s<>"']+`)
	threadIDRegex = regexp.MustCompile(`thread-(\d+)`)
)

// Lou1Plugin implements BT之家 1lou 插件
type Lou1Plugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewLou1Plugin())
}

// NewLou1Plugin creates plugin instance
func NewLou1Plugin() *Lou1Plugin {
	return &Lou1Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search compatibility helper
func (p *Lou1Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult entry
func (p *Lou1Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Lou1Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchKeyword := strings.TrimSpace(keyword)
	if searchKeyword == "" {
		return nil, fmt.Errorf("[%s] 关键词不能为空", p.Name())
	}

	threads, err := p.fetchSearchResults(client, searchKeyword)
	if err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return nil, fmt.Errorf("[%s] 未找到相关结果", p.Name())
	}

	var (
		wg      sync.WaitGroup
		resultM sync.Mutex
		results []model.SearchResult
		sem     = make(chan struct{}, detailWorkers)
	)

	for _, thread := range threads {
		thread := thread
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			detail, err := p.fetchDetail(client, thread.URL)
			if err != nil || len(detail.links) == 0 {
				return
			}

			content := thread.Summary
			if content == "" {
				content = detail.description
			}

			result := model.SearchResult{
				UniqueID: buildUniqueID(thread.URL),
				Title:    thread.Title,
				Content:  strings.TrimSpace(content),
				Links:    detail.links,
				Tags:     mergeTags(thread.Tags, detail.tags),
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

type searchThread struct {
	Title   string
	URL     string
	Tags    []string
	Summary string
}

func (p *Lou1Plugin) fetchSearchResults(client *http.Client, keyword string) ([]searchThread, error) {
	params := url.Values{}
	params.Set("q", keyword)
	params.Set("page", "1")
	params.Set("page_size", fmt.Sprintf("%d", searchLimit))
	searchURL := searchAPIPath + "?" + params.Encode()

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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索响应失败: %w", p.Name(), err)
	}

	var payload lou1SearchResponse
	if err := jsonutil.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索 JSON 失败: %w", p.Name(), err)
	}
	if !payload.OK {
		return nil, fmt.Errorf("[%s] 搜索接口返回失败", p.Name())
	}

	threads := make([]searchThread, 0, minInt(searchLimit, len(payload.Data.Hits)))
	for _, hit := range payload.Data.Hits {
		if len(threads) >= searchLimit {
			break
		}
		title := strings.TrimSpace(hit.Subject)
		threadURL := toAbsoluteURL(hit.ThreadURL)
		if title == "" || threadURL == "" {
			continue
		}
		threads = append(threads, searchThread{
			Title: title,
			URL:   threadURL,
		})
	}
	return threads, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type lou1SearchResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Hits []lou1SearchHit `json:"hits"`
	} `json:"data"`
}

type lou1SearchHit struct {
	Subject   string `json:"subject"`
	ThreadURL string `json:"thread_url"`
}

type detailResult struct {
	links       []model.Link
	datetime    time.Time
	tags        []string
	description string
}

func (p *Lou1Plugin) fetchDetail(client *http.Client, detailURL string) (detailResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return detailResult{}, fmt.Errorf("[%s] 创建详情页请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return detailResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return detailResult{}, fmt.Errorf("[%s] 详情页返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return detailResult{}, fmt.Errorf("[%s] 解析详情页失败: %w", p.Name(), err)
	}

	content := doc.Find("div.message[isfirst='1']")
	if content.Length() == 0 {
		content = doc.Find(".message")
	}
	if content.Length() == 0 {
		content = doc.Selection
	}

	content.Find("script, style").Remove()

	links := extractLinksFromSelection(content)
	links = filterQuarkLinks(links)
	description := strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	if description == "" {
		description = truncateString(strings.TrimSpace(content.Text()), 200)
	}

	tags := collectDetailTags(doc)
	datetime := extractPostDatetime(doc)

	return detailResult{
		links:       links,
		datetime:    datetime,
		tags:        tags,
		description: description,
	}, nil
}

func collectDetailTags(doc *goquery.Document) []string {
	tagSet := make(map[string]struct{})
	doc.Find(".breadcrumb a, ol.breadcrumb a").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text == "" || text == "首页" {
			return
		}
		tagSet[text] = struct{}{}
	})
	doc.Find("h4 a.badge").Each(func(_ int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if text != "" {
			tagSet[text] = struct{}{}
		}
	})

	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	return tags
}

func extractPostDatetime(doc *goquery.Document) time.Time {
	dateText := strings.TrimSpace(doc.Find(".card-thread span.date").First().Text())
	layouts := []string{
		"2006-01-02 15:04",
		time.RFC3339,
		"2006/1/2 15:04",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, dateText, time.Local); err == nil {
			return t
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
	if strings.HasPrefix(raw, "attach-download-") || strings.HasPrefix(raw, "/attach-download-") {
		return "others", toAbsoluteURL(raw)
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

func encodeKeyword(keyword string) string {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return ""
	}
	var builder strings.Builder
	for _, b := range []byte(keyword) {
		builder.WriteByte('_')
		builder.WriteString(strings.ToUpper(fmt.Sprintf("%02x", b)))
	}
	return builder.String()
}

func toAbsoluteURL(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "http") {
		return href
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return fmt.Sprintf("%s/%s", baseURL, strings.TrimLeft(href, "./"))
}

func mergeTags(a []string, b []string) []string {
	tagSet := make(map[string]struct{})
	for _, tag := range a {
		if tag = strings.TrimSpace(tag); tag != "" {
			tagSet[tag] = struct{}{}
		}
	}
	for _, tag := range b {
		if tag = strings.TrimSpace(tag); tag != "" {
			tagSet[tag] = struct{}{}
		}
	}
	var tags []string
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	return tags
}

func buildUniqueID(detailURL string) string {
	id := ""
	if matches := threadIDRegex.FindStringSubmatch(detailURL); len(matches) > 1 {
		id = matches[1]
	}
	if id == "" {
		sum := crc32.ChecksumIEEE([]byte(detailURL))
		id = fmt.Sprintf("%d", sum)
	}
	return fmt.Sprintf("%s-%s", pluginName, id)
}

func truncateString(text string, length int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= length {
		return string(runes)
	}
	return string(runes[:length])
}

func filterQuarkLinks(links []model.Link) []model.Link {
	// 1LOU now publishes torrent attachments in addition to quark links.
	// Keep all links that passed classifyLink; unrelated page URLs are never
	// classified and therefore do not reach this function.
	return links
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
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

func (p *Lou1Plugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		if attempt < maxRetries-1 {
			time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
		}
	}
	return nil, fmt.Errorf("重试 %d 次后失败: %w", maxRetries, lastErr)
}
