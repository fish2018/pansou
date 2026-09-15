package hjzhencai

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
	pluginName        = "hjzhencai"
	defaultBaseURL    = "https://www.hjzhencai.top"
	searchTimeout     = 25 * time.Second
	maxResponseBytes  = 2 << 20
	maxSearchPages    = 3
	maxResults        = 48
	detailConcurrency = 3
	detailCacheTTL    = 30 * time.Minute
	maxCacheEntries   = 512
	userAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var (
	detailPathPattern    = regexp.MustCompile(`^/(?:index\.php/)?vod/detail/id/([0-9]+)\.html$`)
	searchPathPattern    = regexp.MustCompile(`^/(?:index\.php/)?vod/search(?:/|\.html$)`)
	sharePathPattern     = regexp.MustCompile(`^/s/[A-Za-z0-9_-]+/?$`)
	tianyiPathPattern    = regexp.MustCompile(`^/t/[A-Za-z0-9]+/?$`)
	shareCodePattern     = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	textURLPattern       = regexp.MustCompile(`https?://[A-Za-z0-9./?&=%#_~:+-]+`)
	passwordPattern      = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|password)\s*[:：=]\s*([a-z0-9]{4,8})(?:[^a-z0-9]|$)`)
	passwordValuePattern = regexp.MustCompile(`^[A-Za-z0-9]{4,8}$`)
	sourceTimezone       = time.FixedZone("CST", 8*60*60)
	shareHosts           = map[string]string{
		"pan.quark.cn": "quark", "pan.baidu.com": "baidu", "drive.uc.cn": "uc",
		"alipan.com": "aliyun", "aliyundrive.com": "aliyun", "pan.xunlei.com": "xunlei",
		"115.com": "115", "115cdn.com": "115", "anxia.com": "115",
		"123pan.com": "123", "123pan.cn": "123", "123684.com": "123",
		"123685.com": "123", "123865.com": "123", "123912.com": "123", "123592.com": "123",
		"guangyapan.com": "guangya",
	}
)

func init() {
	plugin.RegisterGlobalPlugin(NewHJZhencaiPlugin())
}

// HJZhencaiPlugin 从花卷资源的搜索页和详情页提取公开网盘分享。
type HJZhencaiPlugin struct {
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

func NewHJZhencaiPlugin() *HJZhencaiPlugin {
	return &HJZhencaiPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		baseURL:         defaultBaseURL,
		detailSlots:     make(chan struct{}, detailConcurrency),
		detailCache:     make(map[string]cachedDetail),
	}
}

func (p *HJZhencaiPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *HJZhencaiPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *HJZhencaiPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	searchURL := p.baseURL + "/index.php/vod/search.html?wd=" + url.QueryEscape(keyword)
	nextURL := searchURL
	entries := make([]searchEntry, 0, 16)
	seenIDs, seenPages := make(map[string]bool), make(map[string]bool)
	var lastErr error
	for page := 0; page < maxSearchPages && nextURL != "" && len(entries) < maxResults; page++ {
		if seenPages[nextURL] {
			break
		}
		seenPages[nextURL] = true
		doc, err := p.fetchDocument(ctx, client, nextURL, p.baseURL+"/")
		if err == nil && doc.Find(".module-heading-search, .module-card-item").Length() == 0 {
			err = fmt.Errorf("[%s] 未识别到搜索页面，可能需要验证或站点结构已变更", p.Name())
		}
		if err != nil {
			if len(entries) == 0 {
				return nil, err
			}
			lastErr = err
			break
		}
		for _, entry := range p.parseSearchEntries(doc) {
			if !seenIDs[entry.result.UniqueID] && len(entries) < maxResults {
				seenIDs[entry.result.UniqueID] = true
				entries = append(entries, entry)
			}
		}
		// 尾页也使用 page-next 样式，必须按标题选择真正的下一页。
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
			resolved[i], errs[i] = p.resolveEntry(ctx, client, entry, searchURL, refresh)
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

func (p *HJZhencaiPlugin) parseSearchEntries(doc *goquery.Document) []searchEntry {
	entries := make([]searchEntry, 0, 16)
	doc.Find(".module-card-item").Each(func(_ int, card *goquery.Selection) {
		anchor := card.Find(".module-card-item-title a").First()
		title := cleanText(anchor.Text())
		href, _ := anchor.Attr("href")
		detailURL := p.internalURL(href, detailPathPattern)
		if title == "" || detailURL == "" {
			return
		}
		u, _ := url.Parse(detailURL)
		id := detailPathPattern.FindStringSubmatch(u.Path)[1]
		result := model.SearchResult{
			UniqueID: pluginName + "-" + id,
			Channel:  "",
			Title:    title,
		}
		var content []string
		card.Find(".module-card-item-class, .module-item-note, .module-info-item-content").Each(func(_ int, s *goquery.Selection) {
			content = appendUnique(content, cleanText(s.Text()))
		})
		result.Content = strings.Join(content, " | ")
		result.Tags = appendUnique(result.Tags, cleanText(card.Find(".module-card-item-class").Text()))
		if image := imageURL(card.Find("img").First(), detailURL); image != "" {
			result.Images = []string{image}
		}
		entries = append(entries, searchEntry{url: detailURL, result: result})
	})
	return entries
}

func (p *HJZhencaiPlugin) resolveEntry(ctx context.Context, client *http.Client, entry searchEntry, referer string, refresh bool) (model.SearchResult, error) {
	if !refresh {
		if cached, ok := p.getCachedDetail(entry.url); ok {
			return cached, nil
		}
	}
	doc, err := p.fetchDocument(ctx, client, entry.url, referer)
	if err != nil {
		return model.SearchResult{}, err
	}
	if doc.Find(".module-info-main, .down-wrap").Length() == 0 {
		return model.SearchResult{}, fmt.Errorf("[%s] 未识别到详情页面: %s", p.Name(), entry.url)
	}
	result := parseDetail(doc, entry)
	if len(result.Links) > 0 {
		p.putCachedDetail(entry.url, result)
	}
	return result, nil
}

func parseDetail(doc *goquery.Document, entry searchEntry) model.SearchResult {
	result := entry.result
	if title := cleanText(doc.Find(".module-info-heading h1").First().Text()); title != "" {
		result.Title = title
	}
	result.Datetime = time.Now()
	content := appendUnique(nil, result.Content)
	content = appendUnique(content, cleanText(doc.Find(".module-info-introduction-content").First().Text()))
	doc.Find(".module-info-main .module-info-item").Each(func(_ int, item *goquery.Selection) {
		label := strings.TrimRight(cleanText(item.Find(".module-info-item-title").Text()), ":：")
		value := cleanText(item.Find(".module-info-item-content").Text())
		if label == "更新" {
			for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
				if date, err := time.ParseInLocation(layout, value, sourceTimezone); err == nil {
					result.Datetime = date
					break
				}
			}
		} else if label != "" && value != "" {
			content = appendUnique(content, label+"："+value)
		}
	})
	result.Content = strings.Join(append(content, "来源："+entry.url), "\n")
	doc.Find(".module-info-tag a").Each(func(_ int, s *goquery.Selection) {
		result.Tags = appendUnique(result.Tags, cleanText(s.Text()))
	})
	if image := imageURL(doc.Find(".module-info-poster img").First(), entry.url); image != "" {
		result.Images = []string{image}
	}
	result.Links = extractLinks(doc, result.Title)
	return result
}

func extractLinks(doc *goquery.Document, title string) []model.Link {
	var links []model.Link
	seen := make(map[string]bool)
	doc.Find(".down-wrap .down-card").Each(func(_ int, card *goquery.Selection) {
		label := cleanText(card.Find(".down-card-name").Text())
		workTitle := title
		group := card.PrevAllFiltered(".down-src").First().NextUntil(".down-src").Filter(".down-card")
		// 站点将单个整部分享也标成“第1集”，仅在同线路有多项时保留这个标签。
		if label != "" && label != title && (label != "第1集" || group.Length() > 1) {
			workTitle += " " + label
		}
		nearby := cleanText(textURLPattern.ReplaceAllString(card.Text(), " "))
		add := func(raw string) {
			link, ok := normalizeShareLink(raw, nearby)
			if !ok {
				return
			}
			key := link.URL + "\x00" + link.Password
			if !seen[key] {
				seen[key] = true
				link.WorkTitle = workTitle
				links = append(links, link)
			}
		}
		card.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
			href, _ := a.Attr("href")
			add(href)
		})
		if card.Find("a[href]").Length() == 0 {
			for _, raw := range textURLPattern.FindAllString(card.Text(), -1) {
				add(raw)
			}
		}
	})
	return links
}

func normalizeShareLink(raw, nearby string) (model.Link, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" {
		return model.Link{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return model.Link{}, false
	}
	linkType := shareHosts[host]
	if host == "cloud.189.cn" {
		if !tianyiPathPattern.MatchString(u.Path) && !(u.Path == "/web/share" && shareCodePattern.MatchString(query.Get("code"))) {
			return model.Link{}, false
		}
		linkType = "tianyi"
	} else if linkType == "" || !sharePathPattern.MatchString(u.Path) {
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
		if match := passwordPattern.FindStringSubmatch(nearby); len(match) > 1 {
			password = match[1]
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return model.Link{Type: linkType, URL: u.String(), Password: password}, true
}

func (p *HJZhencaiPlugin) fetchDocument(ctx context.Context, client *http.Client, target, referer string) (*goquery.Document, error) {
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
		return nil, fmt.Errorf("[%s] 请求页面失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 页面返回 HTTP %d: %s", p.Name(), resp.StatusCode, target)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取页面失败: %w", p.Name(), err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("[%s] 页面超过大小限制: %s", p.Name(), target)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析页面失败: %w", p.Name(), err)
	}
	return doc, nil
}

func (p *HJZhencaiPlugin) internalURL(raw string, pathPattern *regexp.Regexp) string {
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
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	for _, attr := range []string{"data-original", "data-src", "src"} {
		raw, _ := image.Attr(attr)
		if raw = strings.TrimSpace(raw); raw == "" || strings.Contains(raw, "/load.gif") {
			continue
		}
		if u, err := base.Parse(raw); err == nil && u.User == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
			return u.String()
		}
	}
	return ""
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func appendUnique(values []string, value string) []string {
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

func (p *HJZhencaiPlugin) getCachedDetail(key string) (model.SearchResult, bool) {
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

func (p *HJZhencaiPlugin) putCachedDetail(key string, result model.SearchResult) {
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
