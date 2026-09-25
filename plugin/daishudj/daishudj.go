package daishudj

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
)

var (
	idRegex    = regexp.MustCompile(`/(\d+)/`)
	textURLReg = regexp.MustCompile(`https?://[^\s<>"']+`)

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
	pluginName            = "daishudj"
	defaultPriority       = 3
	searchTimeout         = 10 * time.Second
	detailTimeout         = 8 * time.Second
	maxConcurrency        = 10
	maxIdleConns          = 64
	maxIdlePerHost        = 16
	maxConnsPerHost       = 32
	idleConnLifetime      = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
	maxRetries            = 3
	retryBaseDelay        = 200 * time.Millisecond
)

// DaishuPlugin 袋鼠短剧插件
type DaishuPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewDaishuPlugin())
	go startCacheCleaner()
}

// NewDaishuPlugin 构造函数
func NewDaishuPlugin() *DaishuPlugin {
	return &DaishuPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search 兼容方法
func (p *DaishuPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 主流程
func (p *DaishuPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
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

func (p *DaishuPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchURL := fmt.Sprintf("https://www.daishuduanju.com/?s=%s", url.QueryEscape(keyword))
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setCommonHeaders(req, "https://www.daishuduanju.com/")

	resp, err := p.doRequestWithRetry(req, client)
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

	var (
		results []model.SearchResult
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, maxConcurrency)
	)

	doc.Find(".item-jx.item-blog").Each(func(_ int, item *goquery.Selection) {
		titleSel := item.Find(".subtitle h5 a")
		title := strings.TrimSpace(titleSel.Text())
		detailURL, ok := titleSel.Attr("href")
		if !ok || title == "" || detailURL == "" {
			return
		}

		postID := extractPostID(detailURL)
		if postID == "" {
			return
		}

		summary := strings.TrimSpace(item.Find(".subtitle p.pdesc").Text())

		var tags []string
		if cat := strings.TrimSpace(item.Find(".sortbox a.sort").Text()); cat != "" {
			tags = append(tags, cat)
		}

		dateText := strings.TrimSpace(item.Find(".pmbox .time").Text())
		publishTime := parseChineseDate(dateText)

		wg.Add(1)
		sem <- struct{}{}
		go func(title, detailURL, summary, postID string, tags []string, publish time.Time) {
			defer wg.Done()
			defer func() { <-sem }()

			links := p.fetchDetailLinks(client, detailURL, postID)
			if len(links) == 0 {
				return
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), postID),
				Title:    title,
				Content:  summary,
				Links:    links,
				Tags:     tags,
				Channel:  "",
				Datetime: publish,
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(title, detailURL, summary, postID, append([]string{}, tags...), publishTime)
	})

	wg.Wait()

	if len(results) == 0 {
		return nil, fmt.Errorf("[%s] 未找到相关资源", p.Name())
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *DaishuPlugin) fetchDetailLinks(client *http.Client, detailURL, postID string) []model.Link {
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
	setCommonHeaders(req, "https://www.daishuduanju.com/")

	resp, err := p.doRequestWithRetry(req, client)
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

	container := doc.Find(".article-body")
	if container.Length() == 0 {
		container = doc.Find("article.post")
	}
	if container.Length() == 0 {
		container = doc.Selection
	}

	links := extractLinks(container)
	if len(links) > 0 {
		detailCache.Store(postID, cacheEntry{
			links:     links,
			expiresAt: time.Now().Add(cacheTTL),
		})
	}
	return links
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

	text := selection.Text()
	for _, idx := range textURLReg.FindAllStringIndex(text, -1) {
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

func extractPostID(detailURL string) string {
	if matches := idRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func parseChineseDate(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now()
	}
	value = strings.ReplaceAll(value, "年", "-")
	value = strings.ReplaceAll(value, "月", "-")
	value = strings.ReplaceAll(value, "日", "")
	layouts := []string{
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Now()
}

func setCommonHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *DaishuPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
