package jsnoteclub

import (
	"context"
	"encoding/json"
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
	pluginName      = "jsnoteclub"
	defaultPriority = 2

	postsCacheTTL       = time.Hour
	detailCacheTTL      = time.Hour
	maxMatchedPosts     = 30
	maxDetailWorkers    = 8
	requestTimeout      = 12 * time.Second
	detailTimeout       = 10 * time.Second
	httpMaxIdleConns    = 64
	httpMaxIdlePerHost  = 16
	httpMaxConnsPerHost = 32
	retryBaseDelay      = 200 * time.Millisecond
	maxRequestRetries   = 3
)

var (
	// Ghost keys may contain suffixes such as "_blocked" in addition to hex characters.
	dataKeyRegex = regexp.MustCompile(`data-key="([^"]+)"`)

	linkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/(?:s|g)/[0-9A-Za-z]+`), "quark"},
		{regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9A-Za-z\-_]+`), "xunlei"},
		{regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9A-Za-z\-_]+`), "baidu"},
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

	postsCache = struct {
		sync.RWMutex
		entries []ghostPost
		expire  time.Time
		key     string
	}{}

	detailCache sync.Map
)

type detailCacheEntry struct {
	links     []model.Link
	expiresAt time.Time
}

// JsNoteClubPlugin 实现灵犀笔记插件
type JsNoteClubPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewJsNoteClubPlugin())
	go startDetailCacheCleaner()
}

// NewJsNoteClubPlugin 创建插件实例
func NewJsNoteClubPlugin() *JsNoteClubPlugin {
	return &JsNoteClubPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search 兼容方法
func (p *JsNoteClubPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 扩展方法
func (p *JsNoteClubPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *JsNoteClubPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchKeyword := strings.TrimSpace(keyword)
	if searchKeyword == "" {
		return nil, fmt.Errorf("[%s] 关键词不能为空", p.Name())
	}
	if titleEn, ok := ext["title_en"].(string); ok {
		titleEn = strings.TrimSpace(titleEn)
		if titleEn != "" {
			searchKeyword = fmt.Sprintf("%s %s", searchKeyword, titleEn)
		}
	}

	allPosts, err := p.getAllPosts(client)
	if err != nil {
		return nil, err
	}

	matched := filterPostsByKeyword(allPosts, searchKeyword)
	if len(matched) == 0 {
		return nil, fmt.Errorf("[%s] 未找到相关资源", p.Name())
	}
	if len(matched) > maxMatchedPosts {
		matched = matched[:maxMatchedPosts]
	}

	var (
		wg      sync.WaitGroup
		resultM sync.Mutex
		results []model.SearchResult
		sem     = make(chan struct{}, maxDetailWorkers)
	)

	for _, post := range matched {
		post := post
		wg.Add(1)
		sem <- struct{}{}

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			links := p.fetchDetailLinks(client, post.URL)
			if len(links) == 0 {
				return
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), post.ID),
				Title:    strings.TrimSpace(post.Title),
				Content:  strings.TrimSpace(post.Excerpt),
				Links:    links,
				Tags:     []string{strings.TrimSpace(post.Slug)},
				Channel:  "",
				Datetime: post.updatedAtTime(),
			}

			resultM.Lock()
			results = append(results, result)
			resultM.Unlock()
		}()
	}

	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("[%s] 未能获取到有效网盘链接", p.Name())
	}

	return plugin.FilterResultsByKeyword(results, searchKeyword), nil
}

func (p *JsNoteClubPlugin) getAllPosts(client *http.Client) ([]ghostPost, error) {
	now := time.Now()

	postsCache.RLock()
	if len(postsCache.entries) > 0 && now.Before(postsCache.expire) {
		defer postsCache.RUnlock()
		return postsCache.entries, nil
	}
	postsCache.RUnlock()

	postsCache.Lock()
	defer postsCache.Unlock()

	// Double-check after acquiring write lock
	if len(postsCache.entries) > 0 && now.Before(postsCache.expire) {
		return postsCache.entries, nil
	}

	dataKey, err := p.fetchDataKey(client)
	if err != nil {
		return nil, err
	}

	posts, err := p.fetchPosts(client, dataKey)
	if err != nil {
		return nil, err
	}

	postsCache.entries = posts
	postsCache.expire = time.Now().Add(postsCacheTTL)
	postsCache.key = dataKey

	return posts, nil
}

func (p *JsNoteClubPlugin) fetchDataKey(client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://jsnoteclub.com/", nil)
	if err != nil {
		return "", fmt.Errorf("[%s] 创建首页请求失败: %w", p.Name(), err)
	}
	setHTMLHeaders(req, "https://jsnoteclub.com/")

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return "", fmt.Errorf("[%s] 访问首页失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("[%s] 首页返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("[%s] 解析首页失败: %w", p.Name(), err)
	}

	var htmlBuilder strings.Builder
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if html, err := goquery.OuterHtml(s); err == nil {
			htmlBuilder.WriteString(html)
		}
	})

	match := dataKeyRegex.FindStringSubmatch(htmlBuilder.String())
	if len(match) < 2 {
		return "", fmt.Errorf("[%s] 未能在首页找到 data-key", p.Name())
	}

	return match[1], nil
}

func (p *JsNoteClubPlugin) fetchPosts(client *http.Client, dataKey string) ([]ghostPost, error) {
	params := url.Values{}
	params.Set("key", dataKey)
	params.Set("limit", "10000")
	params.Set("fields", "id,slug,title,excerpt,url,updated_at,visibility")
	params.Set("order", "updated_at DESC")

	reqURL := fmt.Sprintf("https://jsnoteclub.com/ghost/api/content/posts/?%s", params.Encode())

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建内容请求失败: %w", p.Name(), err)
	}
	setAPIHeaders(req, "https://jsnoteclub.com/")

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取内容失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 内容接口返回状态码: %d", p.Name(), resp.StatusCode)
	}

	var payload ghostPostsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("[%s] 解析内容数据失败: %w", p.Name(), err)
	}

	return payload.Posts, nil
}

func (p *JsNoteClubPlugin) fetchDetailLinks(client *http.Client, detailURL string) []model.Link {
	if cached, ok := detailCache.Load(detailURL); ok {
		if entry, valid := cached.(detailCacheEntry); valid && time.Now().Before(entry.expiresAt) {
			return entry.links
		}
		detailCache.Delete(detailURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return nil
	}
	setHTMLHeaders(req, detailURL)

	resp, err := p.doRequestWithRetry(req, client, maxRequestRetries)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	content := doc.Find("section.gh-content")
	if content.Length() == 0 {
		content = doc.Find(".gh-content")
	}
	if content.Length() == 0 {
		content = doc.Find("article")
	}
	if content.Length() == 0 {
		content = doc.Selection
	}

	content.Find("aside").Remove()
	content.Find(".gh-sidebar").Remove()
	content.Find(".sidebar-left").Remove()
	content.Find(".left-ads").Remove()

	links := extractLinksFromSelection(content)
	if len(links) > 0 {
		detailCache.Store(detailURL, detailCacheEntry{
			links:     links,
			expiresAt: time.Now().Add(detailCacheTTL),
		})
	}
	return links
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
		href = strings.TrimSpace(href)
		if href == "" {
			return
		}

		linkType, normalized := classifyLink(href)
		if linkType == "" {
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

	for _, c := range candidates {
		if pwd := matchPassword(c); pwd != "" {
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

func filterPostsByKeyword(posts []ghostPost, keyword string) []ghostPost {
	if keyword == "" {
		return posts
	}
	lowerKeyword := strings.ToLower(keyword)
	parts := strings.Fields(lowerKeyword)

	var matched []ghostPost
	for _, post := range posts {
		target := strings.ToLower(fmt.Sprintf("%s %s %s", post.Title, post.Excerpt, post.Slug))
		match := true
		for _, part := range parts {
			if !strings.Contains(target, part) {
				match = false
				break
			}
		}
		if match {
			matched = append(matched, post)
		}
	}
	return matched
}

func (p *JsNoteClubPlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
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
		if err == nil {
			lastErr = fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
		}
		if attempt < maxRetries-1 {
			time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
		}
	}

	return nil, fmt.Errorf("重试 %d 次后失败: %w", maxRetries, lastErr)
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

func setAPIHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func startDetailCacheCleaner() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		detailCache.Range(func(key, value interface{}) bool {
			entry, ok := value.(detailCacheEntry)
			if !ok || now.After(entry.expiresAt) {
				detailCache.Delete(key)
			}
			return true
		})
	}
}

type ghostPostsResponse struct {
	Posts []ghostPost `json:"posts"`
}

type ghostPost struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Excerpt    string `json:"excerpt"`
	URL        string `json:"url"`
	UpdatedAt  string `json:"updated_at"`
	Visibility string `json:"visibility"`
}

func (p ghostPost) updatedAtTime() time.Time {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, p.UpdatedAt); err == nil {
			return t
		}
	}
	return time.Now()
}
