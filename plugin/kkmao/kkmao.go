package kkmao

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
	articleIDRegex = regexp.MustCompile(`/(\d+)\.html`)
	quarkRegex     = regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9A-Za-z]+`)
	pwdPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}
	detailCache = sync.Map{}

	cacheTTL             = 1 * time.Hour
	cacheCleanupInterval = 30 * time.Minute
)

type detailCacheEntry struct {
	links     []model.Link
	expiresAt time.Time
}

const (
	pluginName            = "kkmao"
	defaultPriority       = 2
	searchTimeout         = 12 * time.Second
	detailTimeout         = 10 * time.Second
	maxConcurrency        = 8
	maxIdleConns          = 64
	maxIdlePerHost        = 8
	maxConnsPerHost       = 32
	idleConnLifetime      = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second

	searchMaxRetries = 3
	detailMaxRetries = 2
	retryBaseDelay   = 200 * time.Millisecond
)

// KkMaoPlugin 夸克猫插件
type KkMaoPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewKkMaoPlugin())
	go startDetailCacheCleaner()
}

// NewKkMaoPlugin 构造函数
func NewKkMaoPlugin() *KkMaoPlugin {
	return &KkMaoPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client:          newHTTPClient(),
	}
}

// Search 兼容方法
func (p *KkMaoPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 主搜索实现
func (p *KkMaoPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
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

func (p *KkMaoPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchURL := fmt.Sprintf("https://www.kuakemao.com/?s=%s", url.QueryEscape(keyword))
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	setCommonHeaders(req, "https://www.kuakemao.com/")

	resp, err := p.doRequestWithRetry(req, client, searchMaxRetries, retryBaseDelay)
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

		summary := strings.TrimSpace(item.Find("p.note").Text())

		var tags []string
		category := strings.TrimSpace(item.Find(".meta a.cat").First().Text())
		if category != "" {
			tags = append(tags, category)
		}

		rawTime := strings.TrimSpace(item.Find(".meta time").Text())
		publishTime := parsePublishTime(rawTime)

		wg.Add(1)
		sem <- struct{}{}
		go func(title, detailURL, articleID, summary string, tags []string, publishTime time.Time) {
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
				Datetime: publishTime,
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(title, detailURL, articleID, summary, tags, publishTime)
	})

	wg.Wait()

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func extractArticleID(detailURL string) string {
	matches := articleIDRegex.FindStringSubmatch(detailURL)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func parsePublishTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Now()
	}

	layouts := []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}

	return time.Now()
}

func (p *KkMaoPlugin) fetchDetailLinks(client *http.Client, detailURL, articleID string) []model.Link {
	if cached, ok := detailCache.Load(articleID); ok {
		if entry, valid := cached.(detailCacheEntry); valid {
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

	resp, err := p.doRequestWithRetry(req, client, detailMaxRetries, retryBaseDelay)
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

	links := extractQuarkLinks(doc)
	if len(links) > 0 {
		detailCache.Store(articleID, detailCacheEntry{
			links:     links,
			expiresAt: time.Now().Add(cacheTTL),
		})
	}
	return links
}

func extractQuarkLinks(doc *goquery.Document) []model.Link {
	var (
		results []model.Link
		seen    = make(map[string]struct{})
	)

	doc.Find(".article-content a[href]").Each(func(_ int, link *goquery.Selection) {
		href, _ := link.Attr("href")
		href = strings.TrimSpace(href)
		if href == "" {
			return
		}

		loc := quarkRegex.FindString(href)
		if loc == "" {
			return
		}

		if _, exists := seen[loc]; exists {
			return
		}

		password := extractPassword(link)

		results = append(results, model.Link{
			Type:     "quark",
			URL:      loc,
			Password: password,
		})
		seen[loc] = struct{}{}
	})

	return results
}

func extractPassword(link *goquery.Selection) string {
	if pwd := matchPassword(link.Text()); pwd != "" {
		return pwd
	}

	if title, ok := link.Attr("title"); ok {
		if pwd := matchPassword(title); pwd != "" {
			return pwd
		}
	}

	if parent := link.Parent(); parent != nil && parent.Length() > 0 {
		if pwd := matchPassword(parent.Text()); pwd != "" {
			return pwd
		}
		if next := parent.Next(); next.Length() > 0 {
			if pwd := matchPassword(next.Text()); pwd != "" {
				return pwd
			}
		}
	}

	if sibling := link.Next(); sibling.Length() > 0 {
		if pwd := matchPassword(sibling.Text()); pwd != "" {
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

func (p *KkMaoPlugin) doRequestWithRetry(req *http.Request, client *http.Client, maxRetries int, baseDelay time.Duration) (*http.Response, error) {
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
			backoff := baseDelay * time.Duration(1<<attempt)
			time.Sleep(backoff)
		}
	}

	return nil, fmt.Errorf("重试 %d 次后失败: %w", maxRetries, lastErr)
}

func startDetailCacheCleaner() {
	ticker := time.NewTicker(cacheCleanupInterval)
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
