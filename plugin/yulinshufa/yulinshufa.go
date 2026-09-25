package yulinshufa

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
)

const (
	pluginName        = "yulinshufa"
	baseURL           = "http://www.yulinshufa.cn"
	searchPath        = "/plus/search.php?q=%s"
	defaultPriority   = 3
	maxSearchResults  = 15
	maxDetailWorkers  = 6
	requestTimeout    = 20 * time.Second
	detailTimeout     = 30 * time.Second
	cacheTTL          = 45 * time.Minute
	maxContentRunes   = 500
	requestMaxRetries = 3
	userAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"
)

var (
	passwordRegex = regexp.MustCompile(`(?i)(提取码|密码)[:：\s]*([0-9a-zA-Z]{3,8})`)
	urlRegex      = regexp.MustCompile(`https?://[^\s<>"']+`)
	detailIDRegex = regexp.MustCompile(`/xz/(\d+)/?`)
)

// contentToken 表示详情正文中的顺序 token
type contentToken struct {
	kind tokenKind
	text string
	href string
}

type tokenKind int

const (
	tokenText tokenKind = iota
	tokenLink
)

// searchItem 记录搜索列表页解析出的基础数据
type searchItem struct {
	ID        string
	Title     string
	Summary   string
	DetailURL string
	Category  string
	Poster    string
	Date      time.Time
}

// cachedDetail 缓存的详情数据
type cachedDetail struct {
	result    model.SearchResult
	timestamp time.Time
}

// YulinshufaPlugin 夕阳小站插件实现
type YulinshufaPlugin struct {
	*plugin.BaseAsyncPlugin
	detailCache      sync.Map
	cacheDuration    time.Duration
	detailHTTPClient *http.Client
	debugMode        bool
}

var _ plugin.AsyncSearchPlugin = (*YulinshufaPlugin)(nil)

func init() {
	plugin.RegisterGlobalPlugin(NewYulinshufaPlugin())
}

// NewYulinshufaPlugin 创建插件实例
func NewYulinshufaPlugin() *YulinshufaPlugin {
	return &YulinshufaPlugin{
		BaseAsyncPlugin:  plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		cacheDuration:    cacheTTL,
		detailHTTPClient: newOptimizedHTTPClient(),
		debugMode:        true,
	}
}

// newOptimizedHTTPClient 构建复用良好的HTTP客户端
func newOptimizedHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               util.ProxyFuncForTransport(),
			MaxIdleConns:        80,
			MaxIdleConnsPerHost: 20,
			MaxConnsPerHost:     40,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: detailTimeout,
	}
}

// Search 兼容方法
func (p *YulinshufaPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 返回带IsFinal的结果
func (p *YulinshufaPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *YulinshufaPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchKeyword := strings.TrimSpace(keyword)
	if ext != nil {
		if titleEn, ok := ext["title_en"].(string); ok && strings.TrimSpace(titleEn) != "" {
			searchKeyword = strings.TrimSpace(titleEn)
		}
	}
	if searchKeyword == "" {
		return nil, fmt.Errorf("[%s] 搜索关键词不能为空", p.Name())
	}
	p.debugf("开始搜索, keyword=%s", searchKeyword)

	items, err := p.fetchSearchItems(client, searchKeyword)
	if err != nil {
		p.debugf("搜索请求失败: %v", err)
		return nil, err
	}
	if len(items) == 0 {
		p.debugf("搜索结果为空")
		return []model.SearchResult{}, nil
	}
	p.debugf("搜索解析到 %d 条候选结果", len(items))

	results := p.fetchDetailResults(items)
	validResults := make([]model.SearchResult, 0, len(results))
	for _, res := range results {
		if len(res.Links) == 0 {
			p.debugf("结果[%s] 无有效链接, 已跳过", res.UniqueID)
			continue
		}
		validResults = append(validResults, res)
	}
	p.debugf("有效结果数量: %d（原始 %d）", len(validResults), len(results))

	return plugin.FilterResultsByKeyword(validResults, keyword), nil
}

// fetchSearchItems 请求搜索页面并解析列表
func (p *YulinshufaPlugin) fetchSearchItems(client *http.Client, keyword string) ([]searchItem, error) {
	encodedKeyword, err := gbkQueryEscape(keyword)
	if err != nil {
		return nil, fmt.Errorf("[%s] GBK 编码搜索关键词失败: %w", p.Name(), err)
	}
	searchURL := fmt.Sprintf(baseURL+searchPath, encodedKeyword)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	p.debugf("请求搜索URL: %s", searchURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	p.decorateCommonHeaders(req)

	resp, err := p.doRequestWithRetry(client, req, requestMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	doc, err := p.buildGBKDocument(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}

	items := p.parseSearchItems(doc)
	p.debugf("搜索页面解析完毕，获取到 %d 条记录", len(items))
	return items, nil
}

func gbkQueryEscape(s string) (string, error) {
	encoder := simplifiedchinese.GBK.NewEncoder()
	gbkBytes, err := encoder.Bytes([]byte(s))
	if err != nil {
		return "", err
	}

	var escaped strings.Builder
	escaped.Grow(len(gbkBytes) * 3)
	for _, b := range gbkBytes {
		if isURLUnreserved(b) {
			escaped.WriteByte(b)
			continue
		}
		escaped.WriteString(fmt.Sprintf("%%%02X", b))
	}

	return escaped.String(), nil
}

func isURLUnreserved(b byte) bool {
	return (b >= '0' && b <= '9') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z') ||
		b == '-' || b == '_' || b == '.' || b == '~'
}

func (p *YulinshufaPlugin) parseSearchItems(doc *goquery.Document) []searchItem {
	items := make([]searchItem, 0, maxSearchResults)
	doc.Find("div.main-list-con ul.main-list > li").EachWithBreak(func(i int, sel *goquery.Selection) bool {
		if len(items) >= maxSearchResults {
			return false
		}
		linkSel := sel.Find("div.list-con p.s-title a").First()
		detailURL, exists := linkSel.Attr("href")
		if !exists {
			return true
		}
		detailURL = p.normalizeURL(detailURL)
		title := cleanText(linkSel.Text())
		if title == "" || detailURL == "" {
			return true
		}

		item := searchItem{
			ID:        p.extractID(detailURL),
			Title:     title,
			DetailURL: detailURL,
			Summary:   cleanText(sel.Find("div.list-con p.s-desc").Text()),
		}

		if category := cleanText(sel.Find("div.list-con div.s-ext span.item a").First().Text()); category != "" {
			item.Category = category
		}

		dateText := cleanText(sel.Find("div.list-con div.s-ext span.item").Last().Text())
		if parsed := p.parseDateOnly(dateText); !parsed.IsZero() {
			item.Date = parsed
		}

		if poster, ok := sel.Find("div.list-pic img").Attr("src"); ok {
			item.Poster = p.normalizeURL(poster)
		}

		items = append(items, item)
		return true
	})

	return items
}

// fetchDetailResults 并发抓取详情页
func (p *YulinshufaPlugin) fetchDetailResults(items []searchItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxDetailWorkers)

	for _, item := range items {
		itemCopy := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if detail, err := p.getDetailResult(itemCopy); err == nil && detail != nil {
				mu.Lock()
				results = append(results, *detail)
				mu.Unlock()
				p.debugf("详情解析成功: %s, 链接数=%d", detail.UniqueID, len(detail.Links))
			} else if err != nil {
				p.debugf("详情解析失败: %s, err=%v", itemCopy.DetailURL, err)
			}
		}()
	}

	wg.Wait()
	return results
}

func (p *YulinshufaPlugin) getDetailResult(item searchItem) (*model.SearchResult, error) {
	cacheKey := item.DetailURL
	if cached, ok := p.loadFromCache(cacheKey); ok {
		p.debugf("详情缓存命中: %s", cacheKey)
		return &cached, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.DetailURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建详情请求失败: %w", p.Name(), err)
	}
	p.decorateCommonHeaders(req)
	req.Header.Set("Referer", baseURL+"/")
	p.debugf("请求详情: %s", item.DetailURL)

	resp, err := p.doRequestWithRetry(p.detailHTTPClient, req, requestMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("[%s] 请求详情失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	doc, err := p.buildGBKDocument(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析详情失败: %w", p.Name(), err)
	}

	result := p.parseDetailDocument(doc, item)
	if result == nil {
		return nil, fmt.Errorf("[%s] 详情解析为空", p.Name())
	}

	p.storeInCache(cacheKey, *result)
	return result, nil
}

// parseDetailDocument 将详情页转为 SearchResult
func (p *YulinshufaPlugin) parseDetailDocument(doc *goquery.Document, item searchItem) *model.SearchResult {
	contentSel := doc.Find("div.content").First()
	if contentSel.Length() == 0 {
		return nil
	}

	result := model.SearchResult{
		UniqueID:  fmt.Sprintf("%s-%s", p.Name(), item.ID),
		MessageID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Channel:   "",
		Title:     item.Title,
		Tags:      make([]string, 0, 2),
	}

	if detailTitle := cleanText(doc.Find("div.content-tit h1").First().Text()); detailTitle != "" {
		result.Title = detailTitle
	}

	if infoSel := doc.Find("div.content-info span"); infoSel.Length() >= 2 {
		if dt := cleanText(infoSel.Eq(1).Text()); dt != "" {
			if parsed := p.parseFullDatetime(dt); !parsed.IsZero() {
				result.Datetime = parsed
			}
		}
	}
	if result.Datetime.IsZero() && !item.Date.IsZero() {
		result.Datetime = item.Date
	} else if result.Datetime.IsZero() {
		result.Datetime = time.Now()
	}

	result.Content = buildContentSnippet(contentSel.Text(), item.Summary)
	if item.Category != "" {
		result.Tags = append(result.Tags, item.Category)
	}

	if breadcrumb := doc.Find("div.content-loc a").Last().Text(); breadcrumb != "" && breadcrumb != item.Category {
		result.Tags = append(result.Tags, cleanText(breadcrumb))
	}

	result.Images = p.extractImages(contentSel, item.Poster)
	result.Links = p.extractLinks(contentSel)

	return &result
}

func (p *YulinshufaPlugin) extractImages(contentSel *goquery.Selection, fallback string) []string {
	images := make([]string, 0, 3)
	seen := make(map[string]struct{})
	contentSel.Find("img").Each(func(i int, sel *goquery.Selection) {
		if src, ok := sel.Attr("src"); ok {
			normalized := p.normalizeURL(src)
			if normalized == "" {
				return
			}
			if _, exists := seen[normalized]; exists {
				return
			}
			seen[normalized] = struct{}{}
			images = append(images, normalized)
		}
	})

	if len(images) == 0 && fallback != "" {
		images = append(images, fallback)
	}
	return images
}

func (p *YulinshufaPlugin) extractLinks(contentSel *goquery.Selection) []model.Link {
	tokens := p.collectTokens(contentSel)
	links := make([]model.Link, 0, len(tokens))
	seen := make(map[string]struct{})
	pendingPassword := ""
	lastIndex := -1

	for _, token := range tokens {
		switch token.kind {
		case tokenLink:
			linkURL := p.normalizeURL(token.href)
			if linkURL == "" || !p.isValidNetworkDriveURL(linkURL) {
				continue
			}
			if _, exists := seen[linkURL]; exists {
				continue
			}
			seen[linkURL] = struct{}{}
			password := pendingPassword
			pendingPassword = ""
			link := model.Link{
				Type:     p.determineCloudType(linkURL),
				URL:      linkURL,
				Password: password,
			}
			links = append(links, link)
			lastIndex = len(links) - 1
		case tokenText:
			text := strings.TrimSpace(token.text)
			if text == "" {
				continue
			}

			if matches := passwordRegex.FindStringSubmatch(text); len(matches) > 2 {
				code := strings.TrimSpace(matches[2])
				if lastIndex >= 0 && links[lastIndex].Password == "" {
					links[lastIndex].Password = code
				} else {
					pendingPassword = code
				}
			}

			urls := urlRegex.FindAllString(text, -1)
			for _, raw := range urls {
				linkURL := p.normalizeURL(trimTrailingPunctuation(raw))
				if linkURL == "" || !p.isValidNetworkDriveURL(linkURL) {
					continue
				}
				if _, exists := seen[linkURL]; exists {
					continue
				}
				seen[linkURL] = struct{}{}
				password := pendingPassword
				pendingPassword = ""
				link := model.Link{
					Type:     p.determineCloudType(linkURL),
					URL:      linkURL,
					Password: password,
				}
				links = append(links, link)
				lastIndex = len(links) - 1
			}
		}
	}

	if pendingPassword != "" && lastIndex >= 0 && links[lastIndex].Password == "" {
		links[lastIndex].Password = pendingPassword
	}

	return links
}

func (p *YulinshufaPlugin) collectTokens(contentSel *goquery.Selection) []contentToken {
	tokens := make([]contentToken, 0, 32)
	for _, node := range contentSel.Nodes {
		p.walkNode(node, &tokens)
	}
	return tokens
}

func (p *YulinshufaPlugin) walkNode(node *html.Node, tokens *[]contentToken) {
	if node == nil {
		return
	}

	switch node.Type {
	case html.TextNode:
		text := strings.TrimSpace(node.Data)
		if text != "" {
			*tokens = append(*tokens, contentToken{kind: tokenText, text: text})
		}
	case html.ElementNode:
		if strings.EqualFold(node.Data, "a") {
			for _, attr := range node.Attr {
				if strings.EqualFold(attr.Key, "href") {
					*tokens = append(*tokens, contentToken{kind: tokenLink, href: attr.Val})
					break
				}
			}
			// 锚点内的文本可以继续遍历，便于提取提取码
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			p.walkNode(child, tokens)
		}

		if node.Data == "br" || node.Data == "p" || node.Data == "div" || node.Data == "li" {
			*tokens = append(*tokens, contentToken{kind: tokenText, text: "\n"})
		}
	default:
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			p.walkNode(child, tokens)
		}
	}
}

// buildContentSnippet 生成简要描述
func buildContentSnippet(raw string, fallback string) string {
	clean := cleanText(raw)
	if clean == "" {
		clean = fallback
	}
	if clean == "" {
		return ""
	}

	runes := []rune(clean)
	if len(runes) > maxContentRunes {
		return string(runes[:maxContentRunes]) + "..."
	}
	return clean
}

func (p *YulinshufaPlugin) extractID(detailURL string) string {
	if matches := detailIDRegex.FindStringSubmatch(detailURL); len(matches) > 1 {
		return matches[1]
	}
	return strings.Trim(strings.ReplaceAll(detailURL, "/", "-"), "-")
}

func (p *YulinshufaPlugin) parseDateOnly(text string) time.Time {
	clean := strings.TrimSpace(text)
	clean = strings.TrimPrefix(strings.TrimPrefix(clean, "日期："), "日期:")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02", clean, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

func (p *YulinshufaPlugin) parseFullDatetime(text string) time.Time {
	clean := strings.TrimSpace(strings.ReplaceAll(text, "日期：", ""))
	clean = strings.TrimSpace(strings.ReplaceAll(clean, "日期:", ""))
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, clean, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (p *YulinshufaPlugin) loadFromCache(key string) (model.SearchResult, bool) {
	if value, ok := p.detailCache.Load(key); ok {
		entry := value.(cachedDetail)
		if time.Since(entry.timestamp) < p.cacheDuration {
			p.debugf("缓存有效: %s", key)
			return cloneSearchResult(entry.result), true
		}
		p.detailCache.Delete(key)
	}
	return model.SearchResult{}, false
}

func (p *YulinshufaPlugin) storeInCache(key string, result model.SearchResult) {
	p.detailCache.Store(key, cachedDetail{
		result:    cloneSearchResult(result),
		timestamp: time.Now(),
	})
}

func cloneSearchResult(src model.SearchResult) model.SearchResult {
	result := src
	if len(src.Links) > 0 {
		links := make([]model.Link, len(src.Links))
		copy(links, src.Links)
		result.Links = links
	}
	if len(src.Tags) > 0 {
		tags := make([]string, len(src.Tags))
		copy(tags, src.Tags)
		result.Tags = tags
	}
	if len(src.Images) > 0 {
		images := make([]string, len(src.Images))
		copy(images, src.Images)
		result.Images = images
	}
	return result
}

func (p *YulinshufaPlugin) buildGBKDocument(reader io.Reader) (*goquery.Document, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	utf8Reader := transform.NewReader(bytes.NewReader(body), simplifiedchinese.GBK.NewDecoder())
	utf8Bytes, err := io.ReadAll(utf8Reader)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(bytes.NewReader(utf8Bytes))
}

func (p *YulinshufaPlugin) normalizeURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	if strings.HasPrefix(value, "//") {
		return "http:" + value
	}
	if strings.HasPrefix(value, "/") {
		return baseURL + value
	}
	if strings.HasPrefix(value, "./") {
		return baseURL + strings.TrimPrefix(value, ".")
	}
	return value
}

func (p *YulinshufaPlugin) determineCloudType(link string) string {
	switch {
	case strings.Contains(link, "pan.quark.cn"):
		return "quark"
	case strings.Contains(link, "drive.uc.cn"):
		return "uc"
	case strings.Contains(link, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(link, "aliyundrive.com") || strings.Contains(link, "alipan.com"):
		return "aliyun"
	case strings.Contains(link, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(link, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(link, "115.com"):
		return "115"
	case strings.Contains(link, "123pan.com") || strings.Contains(link, "123pan.cn") || strings.Contains(link, "123684.com") ||
		strings.Contains(link, "123685.com") || strings.Contains(link, "123912.com") || strings.Contains(link, "123592.com"):
		return "123"
	case strings.Contains(link, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(link, "mypikpak.com"):
		return "pikpak"
	case strings.HasPrefix(strings.ToLower(link), "magnet:"):
		return "magnet"
	case strings.HasPrefix(strings.ToLower(link), "ed2k://"):
		return "ed2k"
	default:
		return "others"
	}
}

func (p *YulinshufaPlugin) isValidNetworkDriveURL(link string) bool {
	if link == "" {
		return false
	}
	lower := strings.ToLower(link)
	if strings.HasPrefix(lower, "magnet:") || strings.HasPrefix(lower, "ed2k://") {
		return true
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	knownDomains := []string{
		"pan.quark.cn", "drive.uc.cn", "pan.baidu.com", "aliyundrive.com",
		"alipan.com", "pan.xunlei.com", "cloud.189.cn", "115.com",
		"123pan.com", "123pan.cn", "123684.com", "123685.com",
		"123912.com", "123592.com", "caiyun.139.com", "mypikpak.com",
	}
	for _, domain := range knownDomains {
		if strings.Contains(lower, domain) {
			return true
		}
	}
	return false
}

func trimTrailingPunctuation(link string) string {
	trimmed := strings.TrimSpace(link)
	for len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if strings.ContainsRune(")];>）】。；，,》", rune(last)) {
			trimmed = trimmed[:len(trimmed)-1]
			continue
		}
		break
	}
	return trimmed
}

func cleanText(text string) string {
	replacer := strings.NewReplacer("\u00A0", " ", "\n", " ", "\r", " ", "\t", " ")
	clean := replacer.Replace(text)
	clean = strings.TrimSpace(clean)
	for strings.Contains(clean, "  ") {
		clean = strings.ReplaceAll(clean, "  ", " ")
	}
	return clean
}

func (p *YulinshufaPlugin) decorateCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", baseURL+"/")
}

func (p *YulinshufaPlugin) doRequestWithRetry(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
			p.debugf("重试第 %d 次, url=%s", attempt+1, req.URL.String())
		}

		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("状态码: %d", resp.StatusCode)
		} else {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("请求重试 %d 次仍失败: %w", maxRetries, lastErr)
}

func parseDebugFlag() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("YULINSHUFA_DEBUG")))
	if value == "" {
		return false
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func (p *YulinshufaPlugin) debugf(format string, args ...interface{}) {
	if p.debugMode {
		log.Printf("[YULINSHUFA] "+format, args...)
	}
}
