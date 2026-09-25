package alupan

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
	articleIDRegex = regexp.MustCompile(`\?p=(\d+)`)

	linkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9A-Za-z]+`), "quark"},
		{regexp.MustCompile(`https?://www\.aliyundrive\.com/s/[0-9A-Za-z]+`), "aliyun"},
		{regexp.MustCompile(`https?://www\.aliyundrive\.com/drive/folder/[0-9A-Za-z]+`), "aliyun"},
	}

	pwdPatterns = []*regexp.Regexp{
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
	pluginName            = "alupan"
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

// AlupanPlugin 搜索插件
type AlupanPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewAlupanPlugin())
	go startCacheCleaner()
}

// NewAlupanPlugin 创建插件
func NewAlupanPlugin() *AlupanPlugin {
	return &AlupanPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search 兼容方法
func (p *AlupanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 主搜索方法
func (p *AlupanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
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

func (p *AlupanPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchURL := fmt.Sprintf("https://www.aliupan.com/?s=%s", url.QueryEscape(keyword))
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	setCommonHeaders(req, "https://www.aliupan.com/")

	resp, err := p.doRequestWithRetry(req, client, searchMaxRetries)
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

	doc.Find("article.excerpt").Each(func(_ int, item *goquery.Selection) {
		titleSel := item.Find("header h2 a")
		title := strings.TrimSpace(titleSel.Text())
		detailURL, ok := titleSel.Attr("href")
		if !ok || title == "" || detailURL == "" {
			return
		}

		articleID := extractArticleID(detailURL)
		if articleID == "" {
			return
		}

		category := strings.TrimSpace(item.Find("header .label").First().Text())
		var tags []string
		if category != "" {
			tags = append(tags, category)
		}

		summary := strings.TrimSpace(item.Find("p.note").Text())
		timeText := strings.TrimSpace(item.Find("p .icon-time").Parent().Text())
		publishTime := parsePublishTime(timeText)

		wg.Add(1)
		sem <- struct{}{}
		go func(title, detailURL, summary string, tags []string, publish time.Time, articleID string) {
			defer wg.Done()
			defer func() { <-sem }()

			links := p.fetchDetailLinks(client, detailURL, articleID)
			if len(links) == 0 {
				return
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), articleID),
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
		}(title, detailURL, summary, tags, publishTime, articleID)
	})

	wg.Wait()

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func extractArticleID(detailURL string) string {
	if matches := articleIDRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func parsePublishTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now()
	}

	if idx := strings.Index(value, "("); idx >= 0 && strings.HasSuffix(value, ")") {
		value = value[idx+1 : len(value)-1]
		value = strings.TrimSpace(value)
	}

	layouts := []string{
		"2006-01-02",
		"2006/01/02",
		"2006年01月02日",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}

	return time.Now()
}

func (p *AlupanPlugin) fetchDetailLinks(client *http.Client, detailURL, articleID string) []model.Link {
	if cached, ok := detailCache.Load(articleID); ok {
		if entry, valid := cached.(cacheEntry); valid {
			if time.Now().Before(entry.expiresAt) && len(entry.links) > 0 {
				return entry.links
			}
			detailCache.Delete(articleID)
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
		return nil
	}

	links := extractNetDiskLinks(doc)
	if len(links) > 0 {
		detailCache.Store(articleID, cacheEntry{
			links:     links,
			expiresAt: time.Now().Add(cacheTTL),
		})
	}
	return links
}

func extractNetDiskLinks(doc *goquery.Document) []model.Link {
	var (
		results []model.Link
		seen    = make(map[string]struct{})
	)

	doc.Find(".article-content a[href]").Each(func(_ int, node *goquery.Selection) {
		href, exists := node.Attr("href")
		if !exists {
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

		if _, found := seen[normalized]; found {
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

func extractPassword(link *goquery.Selection) string {
	candidates := []string{
		link.Text(),
	}

	if title, ok := link.Attr("title"); ok {
		candidates = append(candidates, title)
	}

	if parent := link.Parent(); parent != nil && parent.Length() > 0 {
		candidates = append(candidates, parent.Text())
		if next := parent.Next(); next.Length() > 0 {
			candidates = append(candidates, next.Text())
		}
	}

	if next := link.Next(); next.Length() > 0 {
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

	for _, pattern := range pwdPatterns {
		if matches := pattern.FindStringSubmatch(text); len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
}

func setCommonHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func (p *AlupanPlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int) (*http.Response, error) {
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
