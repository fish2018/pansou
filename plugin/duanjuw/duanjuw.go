package duanjuw

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
	duanjuwIDRegex         = regexp.MustCompile(`[?&]id=(\d+)`)
	duanjuwTextURLReg      = regexp.MustCompile(`https?://[^\s<>"']+`)
	duanjuwSpaceReg        = regexp.MustCompile(`\s+`)
	duanjuwPwdURLRegex     = regexp.MustCompile(`[?&](?:pwd|passcode|code)=([0-9A-Za-z]+)`)
	duanjuwSearchItemStart = regexp.MustCompile(`(?m)(?:^|\n)\s*\d+\s*[、.．]\s*《`)
	duanjuwBreakTag        = regexp.MustCompile(`(?i)<br\s*/?>`)

	duanjuwLinkPatterns = []struct {
		reg *regexp.Regexp
		typ string
	}{
		{regexp.MustCompile(`https?://pan\.quark\.cn/(?:s|g)/[0-9A-Za-z]+(?:\?pwd=[0-9A-Za-z]+)?`), "quark"},
		{regexp.MustCompile(`https?://(?:www\.)?(?:aliyundrive\.com|alipan\.com)/s/[0-9A-Za-z]+`), "aliyun"},
		{regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9A-Za-z\-_]+(?:\?pwd=[0-9A-Za-z]+)?`), "baidu"},
		{regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9A-Za-z\-_]+`), "xunlei"},
		{regexp.MustCompile(`https?://drive\.uc\.cn/s/[0-9A-Za-z]+`), "uc"},
		{regexp.MustCompile(`https?://(?:www\.)?mypikpak\.com/s/[0-9A-Za-z]+`), "pikpak"},
		{regexp.MustCompile(`https?://(?:www\.)?(?:123pan\.com|123pan\.cn|123684\.com|123685\.com|123912\.com|123592\.com|123865\.com)/s/[0-9A-Za-z]+`), "123"},
	}

	duanjuwPasswordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`密码[:：]?\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`pwd\s*[=:：]\s*([0-9A-Za-z]+)`),
		regexp.MustCompile(`code\s*[=:：]\s*([0-9A-Za-z]+)`),
	}

	duanjuwDetailCache          = sync.Map{}
	duanjuwCacheTTL             = 1 * time.Hour
	duanjuwCacheCleanupInterval = 30 * time.Minute
)

const (
	duanjuwPluginName      = "duanjuw"
	duanjuwBaseURL         = "https://sm3.cc"
	duanjuwSearchURL       = duanjuwBaseURL + "/so/search.php?act=search&q=%s&page=1"
	duanjuwDefaultPriority = 3
	duanjuwSearchTimeout   = 12 * time.Second
	duanjuwDetailTimeout   = 10 * time.Second
	duanjuwMaxConcurrency  = 8
	duanjuwMaxRetries      = 3
	duanjuwRetryDelay      = 200 * time.Millisecond
)

type duanjuwDetailCacheEntry struct {
	links     []model.Link
	content   string
	expiresAt time.Time
}

type DuanjuwPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewDuanjuwPlugin())
	go startDuanjuwCacheCleaner()
}

func NewDuanjuwPlugin() *DuanjuwPlugin {
	return &DuanjuwPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(duanjuwPluginName, duanjuwDefaultPriority),
		client:          newDuanjuwHTTPClient(duanjuwSearchTimeout),
	}
}

func (p *DuanjuwPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *DuanjuwPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *DuanjuwPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchURL := fmt.Sprintf(duanjuwSearchURL, url.QueryEscape(keyword))
	ctx, cancel := context.WithTimeout(context.Background(), duanjuwSearchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setDuanjuwHeaders(req, duanjuwBaseURL+"/")

	resp, err := doDuanjuwRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索返回状态码: %d", p.Name(), resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}

	items := p.parseSearchResults(doc)
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}
	// The current site renders search results as numbered chat entries with
	// direct pan links. Older pages still use result cards and need detail fetches.
	for _, item := range items {
		if len(item.Links) > 0 {
			return plugin.FilterResultsByKeyword(items, keyword), nil
		}
	}

	results := p.enrichResults(client, items)
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *DuanjuwPlugin) parseSearchResults(doc *goquery.Document) []model.SearchResult {
	results := make([]model.SearchResult, 0)

	if bubble := doc.Find(".message.system .bubble").First(); bubble.Length() > 0 {
		return p.parseChatSearchResults(bubble)
	}

	doc.Find("li.col-6").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("h3.f-14 a").First()
		title := normalizeDuanjuwText(linkNode.Text())
		detailURL, ok := linkNode.Attr("href")
		if !ok || title == "" {
			return
		}

		detailURL = normalizeDuanjuwURL(detailURL)
		if detailURL == "" {
			return
		}

		uniquePart := extractDuanjuwID(detailURL)
		if uniquePart == "" {
			uniquePart = url.QueryEscape(detailURL)
		}

		pic := ""
		if picNode := item.Find("img.lazy").First(); picNode.Length() > 0 {
			if raw, exists := picNode.Attr("data-original"); exists {
				pic = normalizeDuanjuwURL(raw)
			}
		}

		content := strings.TrimSpace(item.Find("h3.f-14 a").AttrOr("title", ""))
		content = cleanDuanjuwDescription(content)
		if content == "" {
			content = detailURL
		}

		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), uniquePart),
			Title:    title,
			Content:  content,
			Channel:  "",
			Datetime: time.Now(),
		}
		if pic != "" {
			result.Images = []string{pic}
		}

		// 临时放入详情页地址，后续会被详情内容覆盖。
		result.MessageID = detailURL
		results = append(results, result)
	})

	return results
}

// parseChatSearchResults handles sm3.cc's current search response, where each
// numbered item contains a title followed by one or more direct pan links.
func (p *DuanjuwPlugin) parseChatSearchResults(bubble *goquery.Selection) []model.SearchResult {
	html, err := bubble.Html()
	if err != nil || strings.TrimSpace(html) == "" {
		return nil
	}
	html = duanjuwBreakTag.ReplaceAllString(html, "\n")
	starts := duanjuwSearchItemStart.FindAllStringIndex(html, -1)
	if len(starts) == 0 {
		return nil
	}

	results := make([]model.SearchResult, 0, len(starts))
	for i, start := range starts {
		end := len(html)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		segment := html[start[0]:end]
		open := strings.Index(segment, "《")
		if open < 0 {
			continue
		}
		close := strings.Index(segment[open+len("《"):], "》")
		if close < 0 {
			continue
		}
		close += open + len("《")
		titleHTML := segment[open+len("《") : close]
		title := normalizeDuanjuwText(htmlFragmentText(titleHTML))
		if title == "" {
			continue
		}

		fragment, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + segment + "</div>"))
		if err != nil {
			continue
		}
		links := extractDuanjuwLinks(fragment.Selection)
		if len(links) == 0 {
			continue
		}
		uniquePart := title
		if len(links) > 0 {
			uniquePart += "-" + links[0].URL
		}
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), url.QueryEscape(uniquePart)),
			Title:    title,
			Content:  cleanDuanjuwDescription(htmlFragmentText(segment)),
			Links:    links,
			Channel:  "",
			Datetime: time.Now(),
		})
	}
	return results
}

func htmlFragmentText(fragment string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div>" + fragment + "</div>"))
	if err != nil {
		return ""
	}
	return doc.Find("div").First().Text()
}

func (p *DuanjuwPlugin) enrichResults(client *http.Client, items []model.SearchResult) []model.SearchResult {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, duanjuwMaxConcurrency)
		results = make([]model.SearchResult, 0, len(items))
	)

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}

		go func(base model.SearchResult) {
			defer wg.Done()
			defer func() { <-sem }()

			links, content := p.fetchDetailInfo(client, base.MessageID)
			if len(links) == 0 {
				return
			}

			base.Links = links
			base.MessageID = ""
			if content != "" {
				base.Content = content
			}

			mu.Lock()
			results = append(results, base)
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return results
}

func (p *DuanjuwPlugin) fetchDetailInfo(client *http.Client, detailURL string) ([]model.Link, string) {
	if cached, ok := duanjuwDetailCache.Load(detailURL); ok {
		entry, valid := cached.(duanjuwDetailCacheEntry)
		if valid && time.Now().Before(entry.expiresAt) {
			return entry.links, entry.content
		}
		duanjuwDetailCache.Delete(detailURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), duanjuwDetailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return nil, ""
	}
	setDuanjuwHeaders(req, duanjuwBaseURL+"/")

	resp, err := doDuanjuwRequestWithRetry(req, client)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, ""
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, ""
	}

	contentNode := doc.Find("div.content").First()
	if contentNode.Length() == 0 {
		contentNode = doc.Find("div.tx-text").First()
	}
	if contentNode.Length() == 0 {
		contentNode = doc.Selection
	}

	links := extractDuanjuwLinks(contentNode)
	if len(links) == 0 {
		links = extractDuanjuwLinks(doc.Selection)
	}
	if len(links) == 0 {
		return nil, ""
	}

	content := ""
	if metaDesc, exists := doc.Find(`meta[name="description"]`).Attr("content"); exists {
		content = cleanDuanjuwDescription(metaDesc)
	}
	if content == "" {
		content = cleanDuanjuwDescription(contentNode.Text())
	}

	duanjuwDetailCache.Store(detailURL, duanjuwDetailCacheEntry{
		links:     links,
		content:   content,
		expiresAt: time.Now().Add(duanjuwCacheTTL),
	})

	return links, content
}

func extractDuanjuwLinks(selection *goquery.Selection) []model.Link {
	var (
		results []model.Link
		seen    = make(map[string]struct{})
	)

	selection.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
		href, exists := node.Attr("href")
		if !exists {
			return
		}

		linkType, normalized := classifyDuanjuwLink(href)
		if linkType == "" || normalized == "" {
			return
		}
		if _, ok := seen[normalized]; ok {
			return
		}

		password := extractDuanjuwPassword(node)
		results = append(results, model.Link{
			Type:     linkType,
			URL:      normalized,
			Password: password,
		})
		seen[normalized] = struct{}{}
	})

	text := selection.Text()
	for _, idx := range duanjuwTextURLReg.FindAllStringIndex(text, -1) {
		raw := text[idx[0]:idx[1]]
		linkType, normalized := classifyDuanjuwLink(raw)
		if linkType == "" || normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}

		context := duanjuwSubstring(text, idx[0]-80, idx[1]+80)
		results = append(results, model.Link{
			Type:     linkType,
			URL:      normalized,
			Password: matchDuanjuwPassword(context),
		})
		seen[normalized] = struct{}{}
	}

	return results
}

func classifyDuanjuwLink(raw string) (string, string) {
	value := strings.TrimSpace(raw)
	for _, pattern := range duanjuwLinkPatterns {
		if matched := pattern.reg.FindString(value); matched != "" {
			return pattern.typ, matched
		}
	}
	return "", ""
}

func extractDuanjuwPassword(node *goquery.Selection) string {
	if href, ok := node.Attr("href"); ok {
		if matches := duanjuwPwdURLRegex.FindStringSubmatch(href); len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	candidates := []string{node.Text()}
	if title, ok := node.Attr("title"); ok {
		candidates = append(candidates, title)
	}
	if parent := node.Parent(); parent.Length() > 0 && parent.Find("a[href]").Length() <= 1 {
		candidates = append(candidates, parent.Text())
	}

	for _, text := range candidates {
		if pwd := matchDuanjuwPassword(text); pwd != "" {
			return pwd
		}
	}
	return ""
}

func matchDuanjuwPassword(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	for _, pattern := range duanjuwPasswordPatterns {
		if matches := pattern.FindStringSubmatch(text); len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}
	if matches := duanjuwPwdURLRegex.FindStringSubmatch(text); len(matches) >= 2 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func extractDuanjuwID(detailURL string) string {
	if matches := duanjuwIDRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func cleanDuanjuwDescription(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\u00a0", " "))
	text = duanjuwSpaceReg.ReplaceAllString(text, " ")
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

func normalizeDuanjuwText(text string) string {
	return strings.TrimSpace(duanjuwSpaceReg.ReplaceAllString(text, " "))
}

func normalizeDuanjuwURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "//") {
		return "https:" + value
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "/") {
		return duanjuwBaseURL + value
	}
	return duanjuwBaseURL + "/" + value
}

func duanjuwSubstring(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	if start >= end {
		return ""
	}
	return text[start:end]
}

func setDuanjuwHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", referer)
}

func newDuanjuwHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		MaxConnsPerHost:     16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func doDuanjuwRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < duanjuwMaxRetries; attempt++ {
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		if attempt < duanjuwMaxRetries-1 {
			time.Sleep(duanjuwRetryDelay * time.Duration(1<<attempt))
		}
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", duanjuwMaxRetries, lastErr)
}

func startDuanjuwCacheCleaner() {
	ticker := time.NewTicker(duanjuwCacheCleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		duanjuwDetailCache.Range(func(key, value interface{}) bool {
			entry, ok := value.(duanjuwDetailCacheEntry)
			if !ok || now.After(entry.expiresAt) {
				duanjuwDetailCache.Delete(key)
			}
			return true
		})
	}
}
