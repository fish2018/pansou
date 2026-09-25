package yiove

import (
	"context"
	"fmt"
	"hash/crc32"
	"net/http"
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
	pluginName      = "yiove"
	defaultPriority = 3

	baseURL             = "https://bbs.yiove.com"
	searchPathFormat    = baseURL + "/search-%s-1.htm"
	requestTimeout      = 12 * time.Second
	detailTimeout       = 12 * time.Second
	retryBaseDelay      = 200 * time.Millisecond
	maxRequestRetries   = 3
	searchResultLimit   = 12
	detailLinkLimit     = 6
	detailWorkerCount   = 6
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
		{regexp.MustCompile(`https?://caiyun\.139\.com/[^\s<>"']+`), "mobile"},
		{regexp.MustCompile(`https?://tianyi\.cloud/[^\s<>"']+`), "tianyi"},
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

// YiovePlugin implements YiOVE forum search
type YiovePlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewYiovePlugin())
}

// NewYiovePlugin creates plugin instance
func NewYiovePlugin() *YiovePlugin {
	return &YiovePlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter(pluginName, defaultPriority, true),
		client:          newHTTPClient(),
	}
}

// Search compatibility helper
func (p *YiovePlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult entry point
func (p *YiovePlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *YiovePlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	debug := false
	if ext != nil {
		switch v := ext["debug"].(type) {
		case bool:
			debug = v
		case string:
			debug = strings.EqualFold(v, "true")
		}
	}

	searchKeyword := strings.TrimSpace(keyword)
	if searchKeyword == "" {
		return nil, fmt.Errorf("[%s] 关键词不能为空", p.Name())
	}

	logDebug(debug, "[%s] 开始搜索，关键词=%s", p.Name(), searchKeyword)

	threads, err := p.fetchSearchResults(client, searchKeyword, debug)
	if err != nil {
		logDebug(debug, "[%s] 搜索阶段报错: %v", p.Name(), err)
		return nil, err
	}
	logDebug(debug, "[%s] 搜索结果数量=%d", p.Name(), len(threads))
	if len(threads) == 0 {
		logDebug(debug, "[%s] 搜索结果为空", p.Name())
		return nil, fmt.Errorf("[%s] 未找到相关结果", p.Name())
	}

	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, detailWorkerCount)
		resultM sync.Mutex
		results []model.SearchResult
	)

	for _, thread := range threads {
		thread := thread
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			logDebug(debug, "[%s] 准备抓取详情 title=%s url=%s", p.Name(), thread.Title, thread.URL)

			detail, err := p.fetchDetail(client, thread.URL, debug)
			if err != nil {
				logDebug(debug, "[%s] 详情页抓取失败 URL=%s err=%v", p.Name(), thread.URL, err)
				return
			}
			if len(detail.links) == 0 {
				logDebug(debug, "[%s] 详情页无链接 URL=%s", p.Name(), thread.URL)
				return
			}

			linksWithTitle := applyWorkTitle(detail.links, thread.Title)

			result := model.SearchResult{
				UniqueID: buildUniqueID(thread.URL),
				Title:    thread.Title,
				Content:  detail.description,
				Links:    limitLinks(linksWithTitle, detailLinkLimit),
				Tags:     mergeTags(thread.Tags, detail.tags),
				Channel:  "",
				Datetime: detail.datetime,
			}

			resultM.Lock()
			results = append(results, result)
			resultM.Unlock()

			logDebug(debug, "[%s] 详情抓取成功 URL=%s 链接数=%d", p.Name(), thread.URL, len(result.Links))
		}()
	}

	wg.Wait()

	if len(results) == 0 {
		logDebug(debug, "[%s] 所有线程抓取完成但无有效链接", p.Name())
		return nil, fmt.Errorf("[%s] 未能抓取到有效网盘链接", p.Name())
	}

	filtered := plugin.FilterResultsByKeyword(results, searchKeyword)
	logDebug(debug, "[%s] 过滤后结果数=%d", p.Name(), len(filtered))
	for idx, res := range filtered {
		linkSummaries := make([]string, 0, len(res.Links))
		for _, link := range res.Links {
			linkSummaries = append(linkSummaries, fmt.Sprintf("%s(%s)", link.Type, link.URL))
		}
		logDebug(
			debug,
			"[%s] Result#%d | UID=%s | Title=%s | Links=%d | LinkDetail=%v",
			p.Name(),
			idx,
			res.UniqueID,
			res.Title,
			len(res.Links),
			linkSummaries,
		)
	}

	return filtered, nil
}

type searchThread struct {
	Title string
	URL   string
	Tags  []string
}

func (p *YiovePlugin) fetchSearchResults(client *http.Client, keyword string, debug bool) ([]searchThread, error) {
	searchURL := fmt.Sprintf(searchPathFormat, encodeKeyword(keyword))
	logDebug(debug, "[%s] 搜索URL=%s", p.Name(), searchURL)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, baseURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		logDebug(debug, "[%s] 搜索请求失败: %v", p.Name(), err)
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logDebug(debug, "[%s] 搜索返回非200: %d", p.Name(), resp.StatusCode)
		return nil, fmt.Errorf("[%s] 搜索返回状态码: %d", p.Name(), resp.StatusCode)
	}
	logDebug(debug, "[%s] 搜索响应状态: %d", p.Name(), resp.StatusCode)

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		logDebug(debug, "[%s] 解析搜索页面失败: %v", p.Name(), err)
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}

	var threads []searchThread
	doc.Find("ul.threadlist li.thread").Each(func(_ int, li *goquery.Selection) {
		if len(threads) >= searchResultLimit {
			return
		}

		subject := li.Find(".subject a").First()
		href, exists := subject.Attr("href")
		if !exists || strings.TrimSpace(href) == "" {
			return
		}

		title := strings.TrimSpace(subject.Text())
		if title == "" {
			return
		}

		var tags []string
		li.Find(".subject a.badge").Each(func(_ int, node *goquery.Selection) {
			tag := strings.TrimSpace(node.Text())
			if tag != "" {
				tags = append(tags, tag)
			}
		})

		threadURL := toAbsoluteURL(href)
		if threadURL == "" {
			return
		}

		threads = append(threads, searchThread{
			Title: title,
			URL:   threadURL,
			Tags:  tags,
		})
		logDebug(debug, "[%s] 解析到线程：title=%s url=%s", p.Name(), title, threadURL)
	})
	logDebug(debug, "[%s] 解析到线程数量=%d", p.Name(), len(threads))

	return threads, nil
}

type detailPayload struct {
	links       []model.Link
	tags        []string
	description string
	datetime    time.Time
}

func (p *YiovePlugin) fetchDetail(client *http.Client, detailURL string, debug bool) (detailPayload, error) {
	logDebug(debug, "[%s] 抓取详情 URL=%s", p.Name(), detailURL)
	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return detailPayload{}, fmt.Errorf("[%s] 创建详情页请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, detailURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		logDebug(debug, "[%s] 详情请求失败: %v", p.Name(), err)
		return detailPayload{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logDebug(debug, "[%s] 详情返回非200: %d", p.Name(), resp.StatusCode)
		return detailPayload{}, fmt.Errorf("[%s] 详情页返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		logDebug(debug, "[%s] 解析详情页面失败: %v", p.Name(), err)
		return detailPayload{}, fmt.Errorf("[%s] 解析详情页失败: %w", p.Name(), err)
	}

	content := doc.Find("div.message[isfirst='1']")
	if content.Length() == 0 {
		content = doc.Find(".message").First()
	}
	if content.Length() == 0 {
		content = doc.Selection
	}

	content.Find("script, style").Remove()

	links := extractLinks(content)
	description := strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	if description == "" {
		description = truncateText(content.Text(), 200)
	}
	logDebug(debug, "[%s] 详情解析完成 URL=%s 链接数=%d", p.Name(), detailURL, len(links))

	return detailPayload{
		links:       links,
		tags:        collectTags(doc),
		description: description,
		datetime:    extractDatetime(doc),
	}, nil
}

func logDebug(enabled bool, format string, args ...interface{}) {
	if !enabled {
		return
	}
	fmt.Printf(format+"\n", args...)
}

func collectTags(doc *goquery.Document) []string {
	tagSet := make(map[string]struct{})

	doc.Find(".breadcrumb a, ol.breadcrumb a").Each(func(_ int, node *goquery.Selection) {
		text := strings.TrimSpace(node.Text())
		if text == "" || strings.Contains(text, "首页") {
			return
		}
		tagSet[text] = struct{}{}
	})

	doc.Find("h4 a.badge").Each(func(_ int, node *goquery.Selection) {
		text := strings.TrimSpace(node.Text())
		if text != "" {
			tagSet[text] = struct{}{}
		}
	})

	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	return tags
}

func extractDatetime(doc *goquery.Document) time.Time {
	dateText := strings.TrimSpace(doc.Find(".card-thread .date").First().Text())
	if dateText == "" {
		return time.Now()
	}

	layouts := []string{
		"2006-01-02 15:04",
		"2006/01/02 15:04",
		"2006-01-02",
		"2006/01/02",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, dateText, time.Local); err == nil {
			return t
		}
	}

	return time.Now()
}

func extractLinks(selection *goquery.Selection) []model.Link {
	var (
		results []model.Link
		seen    = make(map[string]struct{})
	)

	selection.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
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

	text := selection.Text()
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
		if sibling := parent.Next(); sibling.Length() > 0 {
			candidates = append(candidates, sibling.Text())
		}
	}

	if next := node.Next(); next.Length() > 0 {
		candidates = append(candidates, next.Text())
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

func limitLinks(links []model.Link, limit int) []model.Link {
	if limit <= 0 || len(links) <= limit {
		return links
	}
	return links[:limit]
}

func applyWorkTitle(links []model.Link, title string) []model.Link {
	if title == "" || len(links) == 0 {
		return links
	}
	for i := range links {
		links[i].WorkTitle = title
	}
	return links
}

func mergeTags(a, b []string) []string {
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

	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	return tags
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

func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
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

func buildUniqueID(detailURL string) string {
	if matches := threadIDRegex.FindStringSubmatch(detailURL); len(matches) > 1 {
		return fmt.Sprintf("%s-%s", pluginName, matches[1])
	}
	sum := crc32.ChecksumIEEE([]byte(detailURL))
	return fmt.Sprintf("%s-%d", pluginName, sum)
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

func (p *YiovePlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
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
