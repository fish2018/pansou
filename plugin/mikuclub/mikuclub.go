package mikuclub

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
	"pansou/util"
)

var (
	categoryIDs = []string{"9305", "942"}

	linkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/(s|g)/[0-9A-Za-z]+`), "quark"},
		{regexp.MustCompile(`https?://(?:www\.)?(aliyundrive\.com|alipan\.com)/s/[0-9A-Za-z]+`), "aliyun"},
		{regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9A-Za-z\-_]+`), "baidu"},
		{regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9A-Za-z\-_]+`), "xunlei"},
		{regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9A-Za-z]+`), "uc"},
		{regexp.MustCompile(`https?://(?:www\.)?mypikpak\.com/s/[0-9A-Za-z]+`), "pikpak"},
		{regexp.MustCompile(`https?://caiyun\.139\.com/[^\s]+`), "mobile"},
		{regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9A-Za-z]+`), "magnet"},
		{regexp.MustCompile(`https?://(?:www\.)?(123pan\.com|123pan\.cn|123684\.com|123685\.com|123912\.com|123592\.com)/s/[0-9A-Za-z]+`), "123"},
	}

	textURLRegex = regexp.MustCompile(`https?://[^\s<>"']+`)

	passwordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}

	detailCache          = sync.Map{}
	cacheTTL             = 1 * time.Hour
	cacheCleanupInterval = 30 * time.Minute
)

type cacheEntry struct {
	links     []model.Link
	expiresAt time.Time
}

const (
	pluginName            = "mikuclub"
	defaultPriority       = 2
	searchTimeout         = 12 * time.Second
	detailTimeout         = 10 * time.Second
	maxConcurrency        = 12
	maxIdleConns          = 64
	maxIdlePerHost        = 16
	maxConnsPerHost       = 32
	idleConnLifetime      = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second

	searchMaxRetries = 3
	detailMaxRetries = 2
	retryBaseDelay   = 200 * time.Millisecond
)

// MikuclubPlugin 插件
type MikuclubPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewMikuclubPlugin())
	go startCacheCleaner()
}

// NewMikuclubPlugin 创建实例
func NewMikuclubPlugin() *MikuclubPlugin {
	return &MikuclubPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search 兼容方法
func (p *MikuclubPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 主搜索
func (p *MikuclubPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 util.ProxyFuncForTransport(),
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		MaxConnsPerHost:       maxConnsPerHost,
		IdleConnTimeout:       idleConnLifetime,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ForceAttemptHTTP2:     true,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   searchTimeout,
	}
}

func (p *MikuclubPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	type catResult struct {
		posts []postItem
		err   error
	}

	var (
		wg        sync.WaitGroup
		resultCh  = make(chan catResult, len(categoryIDs))
		allPosts  []postItem
		firstErr  error
		seenPosts = make(map[int64]postItem)
	)

	for _, cat := range categoryIDs {
		wg.Add(1)
		go func(catID string) {
			defer wg.Done()
			posts, err := p.fetchCategoryPosts(client, keyword, catID)
			resultCh <- catResult{posts: posts, err: err}
		}(cat)
	}

	wg.Wait()
	close(resultCh)

	for res := range resultCh {
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		for _, post := range res.posts {
			if _, exists := seenPosts[post.ID]; exists {
				continue
			}
			seenPosts[post.ID] = post
			allPosts = append(allPosts, post)
		}
	}

	if len(allPosts) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("[%s] 未找到相关结果", p.Name())
	}

	var (
		results []model.SearchResult
		dwg     sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, maxConcurrency)
	)

	for _, post := range allPosts {
		post := post
		dwg.Add(1)
		sem <- struct{}{}
		go func() {
			defer dwg.Done()
			defer func() { <-sem }()

			links := p.fetchDetailLinks(client, post.ID, post.PostHref)
			if len(links) == 0 {
				return
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%d", p.Name(), post.ID),
				Title:    strings.TrimSpace(post.PostTitle),
				Content:  post.summary(),
				Links:    links,
				Tags:     post.tags(),
				Channel:  "",
				Datetime: post.publishTime(),
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}

	dwg.Wait()

	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *MikuclubPlugin) fetchCategoryPosts(client *http.Client, keyword, catID string) ([]postItem, error) {
	params := url.Values{}
	params.Set("search", keyword)
	params.Set("s", keyword)
	params.Set("page", "")
	params.Set("pagename", "search_page")
	params.Set("page_type", "search")
	params.Set("paged", "1")
	params.Set("custom_orderby", "relevance")
	params.Set("no_cache", "1")
	params.Set("custom_cat", catID)

	reqURL := fmt.Sprintf("https://www.mikuclub.uk/wp-json/utils/v2/post_list?%s", params.Encode())

	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setCommonHeaders(req, "https://www.mikuclub.uk/")

	resp, err := p.doRequestWithRetry(req, client, searchMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索返回状态码: %d", p.Name(), resp.StatusCode)
	}

	var payload postListResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}

	return payload.Posts, nil
}

func (p *MikuclubPlugin) fetchDetailLinks(client *http.Client, postID int64, detailURL string) []model.Link {
	if cached, ok := detailCache.Load(postID); ok {
		if entry, valid := cached.(cacheEntry); valid {
			if time.Now().Before(entry.expiresAt) && len(entry.links) > 0 {
				return entry.links
			}
			detailCache.Delete(postID)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return nil
	}
	setCommonHeaders(req, detailURL)

	resp, err := p.doRequestWithRetry(req, client, detailMaxRetries)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		fmt.Printf("[%s] 解析详情页失败: %v\n", pluginName, err)
		return nil
	}

	container := doc.Find(".article_content")
	if container.Length() == 0 {
		container = doc.Find("article.post, .entry-content")
	}
	if container.Length() == 0 {
		container = doc.Selection
	}

	links := extractLinksFromSelection(container)
	if len(links) > 0 {
		detailCache.Store(postID, cacheEntry{
			links:     links,
			expiresAt: time.Now().Add(cacheTTL),
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
	for _, idx := range textURLRegex.FindAllStringIndex(text, -1) {
		raw := text[idx[0]:idx[1]]
		linkType, normalized := classifyLink(raw)
		if linkType == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}

		context := substring(text, idx[0]-80, idx[1]+80)
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
	candidates := []string{
		node.Text(),
	}

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
		if matches := pattern.FindStringSubmatch(text); len(matches) >= 2 {
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

func setCommonHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/html;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *MikuclubPlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
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
			backoff := retryBaseDelay * time.Duration(1<<attempt)
			time.Sleep(backoff)
		}
	}

	return nil, fmt.Errorf("重试 %d 次后失败: %w", maxRetries, lastErr)
}

func startCacheCleaner() {
	ticker := time.NewTicker(cacheCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		detailCache.Range(func(key, value interface{}) bool {
			entry, ok := value.(cacheEntry)
			if !ok || now.After(entry.expiresAt) {
				detailCache.Delete(key)
			}
			return true
		})
	}
}

type postListResponse struct {
	Posts []postItem `json:"posts"`
}

type postItem struct {
	ID              int64  `json:"id"`
	PostTitle       string `json:"post_title"`
	PostHref        string `json:"post_href"`
	PostMainCatName string `json:"post_main_cat_name"`
	PostCatName     string `json:"post_cat_name"`
	PostDate        string `json:"post_date"`
	PostRank        string `json:"post_rank_description"`
	PostViews       int64  `json:"post_views"`
}

func (p postItem) summary() string {
	builder := strings.Builder{}
	if p.PostRank != "" {
		builder.WriteString(fmt.Sprintf("口碑：%s ", strings.TrimSpace(p.PostRank)))
	}
	if p.PostViews > 0 {
		builder.WriteString(fmt.Sprintf("浏览：%d ", p.PostViews))
	}
	if builder.Len() == 0 {
		return ""
	}
	return strings.TrimSpace(builder.String())
}

func (p postItem) tags() []string {
	var tags []string
	if p.PostMainCatName != "" {
		tags = append(tags, p.PostMainCatName)
	}
	if p.PostCatName != "" {
		tags = append(tags, p.PostCatName)
	}
	return tags
}

func (p postItem) publishTime() time.Time {
	layouts := []string{
		"2006-01-02 15:04:05",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, p.PostDate); err == nil {
			return t
		}
	}

	return time.Now()
}
