package erxiaopan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName = "erxiaopan"
	// 站点域名走 share-dns 轮换，CNAME 的 TTL 只有 1 秒，解析会间歇失败；
	// 站点换域名时只需要改这一处。
	defaultBaseURL    = "https://www.2xiaopan.one"
	searchTimeout     = 25 * time.Second
	maxResponseBytes  = 4 << 20
	maxSearchPages    = 2
	maxResults        = 20
	detailConcurrency = 5
	detailCacheTTL    = 30 * time.Minute
	maxCacheEntries   = 512
	requestAttempts   = 3
	retryBackoff      = 300 * time.Millisecond
	castLimit         = 6
	userAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	detailPathPattern = regexp.MustCompile(`^/(?:index\.php/)?vod/detail/id/([0-9]+)\.html$`)
	searchPathPattern = regexp.MustCompile(`^/(?:index\.php/)?vod/search(?:/|\.html$)`)
	sharePathPattern  = regexp.MustCompile(`^/s/[A-Za-z0-9_-]+/?$`)
	tianyiPathPattern = regexp.MustCompile(`^/t/[A-Za-z0-9]+/?$`)
	shareCodePattern  = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	// 站点把说明文字直接拼在分享地址后面，如“https://cloud.189.cn/t/xxx（访问码：9rnf）”，
	// 这里只吃 ASCII 字符，全角括号与中文说明会被留在原串里交给密码正则处理。
	bareURLPattern = regexp.MustCompile(`https?://[A-Za-z0-9._~:/?#\[\]@!$&+,;=%-]+`)
	magnetPattern  = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-fA-F]{40}[^\s"'<>]*`)
	ed2kPattern    = regexp.MustCompile(`ed2k://\|file\|.+\|\d+\|[0-9a-fA-F]{32}\|/?`)
	// 提取码可能来自 URL 参数，也可能写在链接旁的“提取码：”“访问码：”文本里。
	passwordPattern      = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|password)\s*[:：=]\s*([A-Za-z0-9]{4,8})(?:[^A-Za-z0-9]|$)`)
	passwordValuePattern = regexp.MustCompile(`^[A-Za-z0-9]{4,8}$`)
	shareHosts           = map[string]string{
		"pan.quark.cn":    "quark",
		"drive.uc.cn":     "uc",
		"pan.baidu.com":   "baidu",
		"alipan.com":      "aliyun",
		"aliyundrive.com": "aliyun",
		"pan.xunlei.com":  "xunlei",
		"115.com":         "115",
		"115cdn.com":      "115",
		"anxia.com":       "115",
		"mypikpak.com":    "pikpak",
		"guangyapan.com":  "guangya",
		"123pan.com":      "123",
		"123pan.cn":       "123",
		"123684.com":      "123",
		"123685.com":      "123",
		"123865.com":      "123",
		"123912.com":      "123",
		"123592.com":      "123",
	}
)

func init() {
	plugin.RegisterGlobalPlugin(NewErxiaopanPlugin())
}

// ErxiaopanPlugin 从二小盘（DYXS2 模板）的搜索页与详情页提取公开网盘分享。
type ErxiaopanPlugin struct {
	*plugin.BaseAsyncPlugin
	baseURL     string
	detailSlots chan struct{}
	cacheMu     sync.Mutex
	detailCache map[string]cachedDetail
}

type cachedDetail struct {
	result    model.SearchResult
	expiresAt time.Time
}

type searchEntry struct {
	url    string
	result model.SearchResult
}

type downloadRow struct {
	label string
	raw   string
	text  string
}

func NewErxiaopanPlugin() *ErxiaopanPlugin {
	return &ErxiaopanPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 2),
		baseURL:         defaultBaseURL,
		detailSlots:     make(chan struct{}, detailConcurrency),
		detailCache:     make(map[string]cachedDetail),
	}
}

func (p *ErxiaopanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *ErxiaopanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *ErxiaopanPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	entries := make([]searchEntry, 0, maxResults)
	seenIDs, seenPages := make(map[string]bool), make(map[string]bool)
	nextURL := p.searchURL(keyword, 1)
	referer := p.baseURL + "/"
	var lastErr error
	for page := 0; page < maxSearchPages && nextURL != "" && len(entries) < maxResults; page++ {
		if seenPages[nextURL] {
			break
		}
		seenPages[nextURL] = true
		doc, err := p.fetchDocument(ctx, client, nextURL, referer)
		if err == nil && doc.Find(".module-search-item, .mac_total").Length() == 0 {
			err = fmt.Errorf("[%s] 未识别到搜索页面，站点结构可能已变更", p.Name())
		}
		if err != nil {
			if len(entries) == 0 {
				return nil, err
			}
			lastErr = err
			break
		}
		referer = nextURL
		for _, entry := range p.parseSearchEntries(doc) {
			if !seenIDs[entry.result.UniqueID] && len(entries) < maxResults {
				seenIDs[entry.result.UniqueID] = true
				entries = append(entries, entry)
			}
		}
		// 分页里“下一页”和“尾页”都用 page-next 样式，必须按标题挑选。
		href, _ := doc.Find(`#page a.page-next[title="下一页"]`).First().Attr("href")
		nextURL = p.internalURL(href, searchPathPattern)
	}

	refresh, _ := ext["refresh"].(bool)
	resolved := make([]model.SearchResult, len(entries))
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		go func(i int, entry searchEntry) {
			defer wg.Done()
			select {
			case p.detailSlots <- struct{}{}:
				defer func() { <-p.detailSlots }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			resolved[i], errs[i] = p.resolveEntry(ctx, client, entry, referer, refresh)
		}(i, entry)
	}
	wg.Wait()

	results := make([]model.SearchResult, 0, len(entries))
	for i, result := range resolved {
		if errs[i] != nil {
			lastErr = errs[i]
		}
		if len(result.Links) > 0 {
			results = append(results, result)
		}
	}
	if len(results) == 0 && lastErr != nil {
		return nil, fmt.Errorf("[%s] 获取网盘资源失败: %w", p.Name(), lastErr)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// searchURL 第 1 页走 /vod/search/wd/{kw}.html，后续页走 /vod/search/page/{n}/wd/{kw}.html。
func (p *ErxiaopanPlugin) searchURL(keyword string, page int) string {
	escaped := url.QueryEscape(keyword)
	if page <= 1 {
		return fmt.Sprintf("%s/index.php/vod/search/wd/%s.html", p.baseURL, escaped)
	}
	return fmt.Sprintf("%s/index.php/vod/search/page/%d/wd/%s.html", p.baseURL, page, escaped)
}

func (p *ErxiaopanPlugin) parseSearchEntries(doc *goquery.Document) []searchEntry {
	entries := make([]searchEntry, 0, 10)
	doc.Find(".module-search-item").Each(func(_ int, card *goquery.Selection) {
		// .module-item-pic 里的标题带“立刻播放”前缀，只有 h3 才是干净标题。
		anchor := card.Find(".video-info-header h3 a").First()
		title := cleanText(anchor.Text())
		if title == "" {
			title, _ = anchor.Attr("title")
			title = cleanText(title)
		}
		href, _ := anchor.Attr("href")
		detailURL := p.internalURL(href, detailPathPattern)
		if title == "" || detailURL == "" {
			return
		}
		parsed, _ := url.Parse(detailURL)
		id := detailPathPattern.FindStringSubmatch(parsed.Path)[1]

		result := model.SearchResult{
			UniqueID: pluginName + "-" + id,
			Channel:  "",
			Title:    title,
		}
		meta := parseMeta(card)
		result.Content = meta.content(detailURL)
		result.Tags = meta.tags(strings.TrimPrefix(cleanText(card.Find(".video-info-header .video-serial").Text()), " "))
		if image := imageURL(card.Find(".module-item-pic img").First(), p.baseURL); image != "" {
			result.Images = []string{image}
		}
		entries = append(entries, searchEntry{url: detailURL, result: result})
	})
	return entries
}

func (p *ErxiaopanPlugin) resolveEntry(ctx context.Context, client *http.Client, entry searchEntry, referer string, refresh bool) (model.SearchResult, error) {
	if !refresh {
		if cached, ok := p.getCachedDetail(entry.url); ok {
			return cached, nil
		}
	}
	doc, err := p.fetchDocument(ctx, client, entry.url, referer)
	if err != nil {
		return model.SearchResult{}, err
	}
	if doc.Find("#download-list, .video-info-header h1").Length() == 0 {
		return model.SearchResult{}, fmt.Errorf("[%s] 未识别到详情页面: %s", p.Name(), entry.url)
	}
	result := parseDetail(doc, entry)
	if len(result.Links) > 0 {
		p.putCachedDetail(entry.url, result)
	}
	return result, nil
}

// pageMeta 汇总详情页与搜索页共用的影片信息块。
type pageMeta struct {
	category string
	year     string
	areas    []string
	classes  []string
	director string
	cast     []string
	plot     string
	alias    string
}

func parseMeta(scope *goquery.Selection) pageMeta {
	meta := pageMeta{}
	scope.Find(".video-info-aux a").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		label := cleanText(a.Text())
		if label == "" {
			return
		}
		switch {
		case strings.Contains(href, "/year/"):
			meta.year = label
		case strings.Contains(href, "/area/"):
			for _, area := range splitMulti(label) {
				meta.areas = appendUnique(meta.areas, area)
			}
		case strings.Contains(href, "/vod/type/"):
			if meta.category == "" {
				meta.category = label
			}
		case strings.Contains(href, "/class/"):
			for _, class := range splitMulti(label) {
				meta.classes = appendUnique(meta.classes, class)
			}
		}
	})
	scope.Find(".video-info-items").Each(func(_ int, item *goquery.Selection) {
		label := strings.TrimRight(cleanText(item.Find(".video-info-itemtitle").First().Text()), ":：")
		switch label {
		case "导演":
			meta.director = joinNames(item.Find(".video-info-item a"))
			if meta.director == "" {
				meta.director = stripSlashes(item.Find(".video-info-item").First().Text())
			}
		case "主演":
			item.Find(".video-info-item a").EachWithBreak(func(_ int, a *goquery.Selection) bool {
				meta.cast = appendUnique(meta.cast, cleanText(a.Text()))
				return len(meta.cast) < castLimit
			})
			if len(meta.cast) == 0 {
				meta.cast = appendUnique(meta.cast, stripSlashes(item.Find(".video-info-item").First().Text()))
			}
		case "剧情", "简介":
			value := cleanText(item.Find(".video-info-item").First().Text())
			if value != "" && !strings.Contains(meta.plot, value) {
				meta.plot = value
			}
		}
	})
	return meta
}

// 站点会把同一栏目的多个值合成一个链接，如“美国 / 日本”“剧情 / 动作 / 冒险 / 运动”。
func splitMulti(value string) []string {
	var values []string
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '/' || r == ',' || r == '，' || r == '、'
	}) {
		values = appendUnique(values, part)
	}
	return values
}

func joinNames(anchors *goquery.Selection) string {
	var names []string
	anchors.Each(func(_ int, a *goquery.Selection) {
		names = appendUnique(names, cleanText(a.Text()))
	})
	return strings.Join(names, " / ")
}

// 站点用 <span class="slash">/</span> 分隔同一栏目的多个值，直接取文本会带上斜杠。
func stripSlashes(value string) string {
	return cleanText(strings.ReplaceAll(value, "/", " "))
}

func (m pageMeta) apply(result *model.SearchResult, tags []string) {
	var lines []string
	if header := m.header(); header != "" {
		lines = append(lines, header)
	}
	if m.director != "" {
		lines = append(lines, "导演："+m.director)
	}
	if len(m.cast) > 0 {
		lines = append(lines, "主演："+strings.Join(m.cast, " / "))
	}
	if m.alias != "" {
		lines = append(lines, "又名："+m.alias)
	}
	if m.plot != "" {
		lines = append(lines, "剧情："+m.plot)
	}
	if len(lines) > 0 {
		result.Content = strings.Join(lines, "\n")
	}
	for _, tag := range append([]string{m.category, m.year}, append(m.areas, m.classes...)...) {
		result.Tags = appendUnique(result.Tags, tag)
	}
	for _, tag := range tags {
		result.Tags = appendUnique(result.Tags, tag)
	}
}

func (m pageMeta) header() string {
	var parts []string
	if m.category != "" {
		parts = append(parts, "分类："+m.category)
	}
	if m.year != "" {
		parts = append(parts, "年份："+m.year)
	}
	if len(m.areas) > 0 {
		parts = append(parts, "地区："+strings.Join(m.areas, " / "))
	}
	return strings.Join(parts, " | ")
}

func (m pageMeta) content(sourceURL string) string {
	var result model.SearchResult
	m.apply(&result, nil)
	if sourceURL != "" {
		return strings.Join([]string{result.Content, "来源：" + sourceURL}, "\n")
	}
	return result.Content
}

func (m pageMeta) tags(extra string) []string {
	var result model.SearchResult
	m.apply(&result, nil)
	return appendUnique(result.Tags, extra)
}

func parseDetail(doc *goquery.Document, entry searchEntry) model.SearchResult {
	result := entry.result
	if title := cleanText(doc.Find(".video-info-header h1.page-title").First().Text()); title != "" {
		result.Title = title
	}
	result.Datetime = time.Now()
	meta := parseMeta(doc.Find(".view-heading").First())
	if meta.category == "" && meta.year == "" && len(meta.areas) == 0 {
		meta = parseMeta(doc.Selection)
	}
	meta.alias = strings.TrimPrefix(cleanText(doc.Find("h2.video-subtitle").First().Text()), "又名：")
	tags := []string{cleanText(doc.Find(".video-info-header .video-serial").First().Text())}
	meta.apply(&result, tags)
	result.Content = strings.Join(append([]string{result.Content}, "来源："+entry.url), "\n")
	if image := imageURL(doc.Find(".mobile-play img").First(), ""); image != "" {
		result.Images = []string{image}
	} else if image := imageURL(doc.Find(".video-cover img").First(), ""); image != "" {
		result.Images = []string{image}
	}
	result.Links = extractLinks(doc, result.Title)
	return result
}

func extractLinks(doc *goquery.Document, title string) []model.Link {
	type resolvedRow struct {
		row  downloadRow
		link model.Link
	}
	var resolved []resolvedRow
	seen := make(map[string]bool)
	for _, row := range collectDownloadRows(doc) {
		link, ok := normalizeShareLink(row.raw, row.text)
		if !ok {
			continue
		}
		key := link.URL + "\x00" + link.Password
		if seen[key] {
			continue
		}
		seen[key] = true
		resolved = append(resolved, resolvedRow{row: row, link: link})
	}

	// 只有带分享链接的下载项参与集数判断；站内跳转之类的干扰行不该制造歧义。
	// 同一作品的所有下载项共用一种集数标签时不加后缀，避免把整部分享写成“第1集”。
	suffixes := make(map[string]bool)
	for _, item := range resolved {
		if suffix := workSuffix(item.row.label, title); suffix != "" {
			suffixes[suffix] = true
		}
	}
	ambiguous := len(suffixes) > 1

	links := make([]model.Link, 0, len(resolved))
	for _, item := range resolved {
		item.link.WorkTitle = title
		if ambiguous {
			if suffix := workSuffix(item.row.label, title); suffix != "" {
				item.link.WorkTitle = strings.TrimSpace(title + " " + suffix)
			}
		}
		links = append(links, item.link)
	}
	return links
}

func collectDownloadRows(doc *goquery.Document) []downloadRow {
	var rows []downloadRow
	doc.Find("#download-list .module-row-one").Each(func(_ int, row *goquery.Selection) {
		label := cleanText(row.Find(".module-row-title h4").First().Text())
		raw, _ := row.Find("a.copy[data-clipboard-text]").First().Attr("data-clipboard-text")
		if strings.TrimSpace(raw) == "" {
			raw, _ = row.Find(".module-row-shortcuts a[href]").First().Attr("href")
		}
		if strings.TrimSpace(raw) == "" {
			return
		}
		rows = append(rows, downloadRow{label: label, raw: raw, text: cleanText(row.Text())})
	})
	if len(rows) > 0 {
		return rows
	}
	// 兜底：结构变化时直接扫下载区里带 data-clipboard-text 的锚点。
	doc.Find("#download-list a[data-clipboard-text]").Each(func(_ int, a *goquery.Selection) {
		raw, _ := a.Attr("data-clipboard-text")
		rows = append(rows, downloadRow{label: cleanText(a.Find("h4").First().Text()), raw: raw, text: cleanText(a.Text())})
	})
	return rows
}

// workSuffix 去掉下载项标签里的作品名前缀，留下“第N集”这类区分信息。
func workSuffix(label, title string) string {
	label = cleanText(label)
	if label == "" || label == title {
		return ""
	}
	if title != "" {
		if rest, ok := strings.CutPrefix(label, title); ok {
			return cleanText(strings.TrimLeft(rest, "-–—_~·:： "))
		}
	}
	if strings.Contains(label, " - ") {
		return ""
	}
	return label
}

func normalizeShareLink(raw, nearby string) (model.Link, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return model.Link{}, false
	}
	if match := magnetPattern.FindString(raw); match != "" {
		return model.Link{Type: "magnet", URL: match}, true
	}
	if match := ed2kPattern.FindString(raw); match != "" {
		return model.Link{Type: "ed2k", URL: match}, true
	}
	candidate := bareURLPattern.FindString(raw)
	if candidate == "" {
		return model.Link{}, false
	}
	u, err := url.Parse(candidate)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return model.Link{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return model.Link{}, false
	}
	linkType := ""
	switch host {
	case "cloud.189.cn":
		if tianyiPathPattern.MatchString(u.Path) || (u.Path == "/web/share" && shareCodePattern.MatchString(query.Get("code"))) {
			linkType = "tianyi"
		}
	case "caiyun.feixin.10086.cn", "yun.139.com":
		if u.Path != "" && u.Path != "/" {
			linkType = "mobile"
		}
	default:
		if shareHosts[host] != "" && sharePathPattern.MatchString(u.Path) {
			linkType = shareHosts[host]
		}
	}
	if linkType == "" {
		return model.Link{}, false
	}
	password := ""
	for _, key := range []string{"pwd", "password"} {
		if value := query.Get(key); passwordValuePattern.MatchString(value) {
			password = value
			break
		}
	}
	if password == "" {
		if match := passwordPattern.FindStringSubmatch(raw + " " + nearby); len(match) > 1 {
			password = match[1]
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return model.Link{Type: linkType, URL: u.String(), Password: password}, true
}

func (p *ErxiaopanPlugin) fetchDocument(ctx context.Context, client *http.Client, target, referer string) (*goquery.Document, error) {
	var lastErr error
	for attempt := 0; attempt < requestAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			// 站点域名走 share-dns 轮换（TTL 1 秒），解析失败是常态而非异常，
			// 每次重试都会重新建连并重新解析，退避重试比直接放弃更可靠。
			case <-time.After(time.Duration(attempt) * retryBackoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Referer", referer)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, target)
			// 4xx 是目标明确拒绝，重试没有意义；429 除外。
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				break
			}
			continue
		}
		if len(body) > maxResponseBytes {
			return nil, fmt.Errorf("[%s] 页面超过大小限制: %s", p.Name(), target)
		}
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		return doc, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("请求未执行")
	}
	return nil, fmt.Errorf("[%s] 请求页面失败: %w", p.Name(), lastErr)
}

func (p *ErxiaopanPlugin) internalURL(raw string, pathPattern *regexp.Regexp) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	base, err := url.Parse(p.baseURL)
	if err != nil {
		return ""
	}
	u, err := base.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || !strings.EqualFold(u.Host, base.Host) || (u.Scheme != "http" && u.Scheme != "https") || !pathPattern.MatchString(u.Path) {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

func imageURL(image *goquery.Selection, baseURL string) string {
	for _, attr := range []string{"data-src", "data-original", "src"} {
		value, _ := image.Attr(attr)
		raw := strings.TrimSpace(value)
		if raw == "" || strings.Contains(raw, "/loading.png") || strings.Contains(raw, "load.gif") {
			continue
		}
		if strings.HasPrefix(raw, "//") {
			return "https:" + raw
		}
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			return raw
		}
		if baseURL != "" {
			if base, err := url.Parse(baseURL); err == nil {
				if u, err := base.Parse(raw); err == nil && u.Host != "" {
					return u.String()
				}
			}
		}
	}
	return ""
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func appendUnique(values []string, value string) []string {
	value = cleanText(value)
	if value != "" && !slices.Contains(values, value) {
		return append(values, value)
	}
	return values
}

func cloneResult(result model.SearchResult) model.SearchResult {
	result.Links = slices.Clone(result.Links)
	result.Tags = slices.Clone(result.Tags)
	result.Images = slices.Clone(result.Images)
	return result
}

func (p *ErxiaopanPlugin) getCachedDetail(key string) (model.SearchResult, bool) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	entry, ok := p.detailCache[key]
	if !ok {
		return model.SearchResult{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(p.detailCache, key)
		return model.SearchResult{}, false
	}
	return cloneResult(entry.result), true
}

func (p *ErxiaopanPlugin) putCachedDetail(key string, result model.SearchResult) {
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	if len(p.detailCache) >= maxCacheEntries {
		oldestKey := ""
		var oldestTime time.Time
		for k, entry := range p.detailCache {
			if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
				oldestKey, oldestTime = k, entry.expiresAt
			}
		}
		delete(p.detailCache, oldestKey)
	}
	p.detailCache[key] = cachedDetail{result: cloneResult(result), expiresAt: time.Now().Add(detailCacheTTL)}
}
