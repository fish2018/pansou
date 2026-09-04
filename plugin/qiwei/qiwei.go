package qiwei

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	pluginName          = "qiwei"
	defaultPriority     = 3
	searchSuggestLimit  = 100
	maxConcurrent       = 12
	maxFallbackPlayURLs = 6
	searchTimeout       = 8 * time.Second
	detailTimeout       = 8 * time.Second
	cacheTTL            = 45 * time.Minute
	userAgent           = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

var (
	qiweiHosts = []string{
		"https://www.qmp4.com",
		"https://www.gmp4.com",
		"https://www.qwmp4.com",
		"https://www.qwmkv.com",
		"https://www.qwfilm.com",
		"https://www.qwfun.com",
		"https://www.qwshow.com",
		"https://www.qnnull.com",
		"https://www.qnhot.com",
		"https://www.qncool.com",
	}

	whitespaceRegex = regexp.MustCompile(`\s+`)
	yearSuffixRegex = regexp.MustCompile(`\s*\(\d{4}\)\s*$`)
	updateTimeRegex = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`)
	magnetRegex     = regexp.MustCompile(`magnet:\?[^\s"'<>]+`)
	passwordRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:提取码|密码|pwd)[：:\s]*([a-z0-9]{4,8})`),
		regexp.MustCompile(`(?i)\?pwd=([a-z0-9]{4,8})`),
	}
	highValueKeywords         = []string{"杜比", "dolby", "原盘", "高码", "remux", "蓝光", "hdr10+", "hdr10", "hdr", "4k", "2160p", "uhd"}
	verificationScriptRegex   = regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']*huadong_[^"']+\.js[^"']*)`)
	verificationTypeRegex     = regexp.MustCompile(`(?i)type=([a-f0-9]{32})`)
	verificationKeyRegex      = regexp.MustCompile(`(?i)key=["']([a-f0-9]{32})["']`)
	verificationValueRegex    = regexp.MustCompile(`(?i)value=["']([^"']+)["']`)
	verificationEndpointRegex = regexp.MustCompile(`(?i)/([a-z0-9_]+_yanzheng_huadong\.php)\?type=`)
)

type suggestResponse struct {
	Code int           `json:"code"`
	Msg  string        `json:"msg"`
	List []suggestItem `json:"list"`
}

type suggestItem struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Pic  string `json:"pic"`
	En   string `json:"en"`
}

type detailInfo struct {
	Title    string
	Content  string
	Datetime time.Time
	Links    []model.Link
	Images   []string
}

type detailCacheEntry struct {
	Info     detailInfo
	CachedAt time.Time
}

type QiweiPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client

	detailCache sync.Map
	hostMu      sync.RWMutex
	activeHost  string
}

func init() {
	plugin.RegisterGlobalPlugin(NewQiweiPlugin())
}

func NewQiweiPlugin() *QiweiPlugin {
	transport := &http.Transport{
		MaxIdleConns:        120,
		MaxIdleConnsPerHost: 24,
		MaxConnsPerHost:     36,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   true,
	}
	jar, _ := cookiejar.New(nil)

	return &QiweiPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client: &http.Client{
			Timeout:   searchTimeout,
			Transport: transport,
			Jar:       jar,
		},
		activeHost: qiweiHosts[0],
	}
}

func (p *QiweiPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *QiweiPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *QiweiPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	forceRefresh := shouldForceRefresh(ext)

	items, host, err := p.searchSuggestWithFallback(client, keyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	items = dedupeSuggestItems(items)
	sort.SliceStable(items, func(i, j int) bool {
		leftScore := scoreTitle(items[i].Name, keyword)
		rightScore := scoreTitle(items[j].Name, keyword)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return len(items[i].Name) < len(items[j].Name)
	})

	results := p.enrichResults(client, host, items, forceRefresh)
	if len(results) == 0 {
		return []model.SearchResult{}, nil
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *QiweiPlugin) searchSuggestWithFallback(client *http.Client, keyword string) ([]suggestItem, string, error) {
	var lastErr error
	for _, host := range p.hostCandidates() {
		items, err := p.searchSuggest(client, host, keyword)
		if err == nil {
			p.setActiveHost(host)
			return items, host, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("[%s] 所有域名均不可用", p.Name())
	}
	return nil, "", lastErr
}

func (p *QiweiPlugin) searchSuggest(client *http.Client, host, keyword string) ([]suggestItem, error) {
	searchURL := fmt.Sprintf("%s/index.php/ajax/suggest?mid=1&limit=%d&wd=%s", host, searchSuggestLimit, url.QueryEscape(keyword))
	body, err := p.fetchBody(client, searchURL, host+"/", searchTimeout)
	if err != nil {
		return nil, err
	}
	if isVerifyPage(body) {
		if err := p.solveVerification(client, searchURL, body); err != nil {
			return nil, fmt.Errorf("[%s] 搜索验证失败: %w", p.Name(), err)
		}
		body, err = p.fetchBody(client, searchURL, host+"/", searchTimeout)
		if err != nil {
			return nil, err
		}
		if isVerifyPage(body) {
			return nil, fmt.Errorf("[%s] 命中验证页: %s", p.Name(), host)
		}
	}

	var resp suggestResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("[%s] suggest 响应解析失败: %w", p.Name(), err)
	}

	if resp.Code != 1 && len(resp.List) == 0 {
		return nil, fmt.Errorf("[%s] suggest 响应异常: host=%s code=%d msg=%s", p.Name(), host, resp.Code, resp.Msg)
	}

	return resp.List, nil
}

func (p *QiweiPlugin) enrichResults(client *http.Client, host string, items []suggestItem, forceRefresh bool) []model.SearchResult {
	results := make([]model.SearchResult, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for i, item := range items {
		wg.Add(1)
		go func(idx int, it suggestItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[idx] = p.buildResult(client, host, it, forceRefresh)
		}(i, item)
	}

	wg.Wait()

	finalResults := make([]model.SearchResult, 0, len(results))
	for _, result := range results {
		if result.UniqueID != "" {
			finalResults = append(finalResults, result)
		}
	}
	return finalResults
}

func (p *QiweiPlugin) buildResult(client *http.Client, host string, item suggestItem, forceRefresh bool) model.SearchResult {
	detailURL := fmt.Sprintf("%s/mv/%d.html", host, item.ID)
	baseTitle := cleanText(item.Name)
	cover := normalizeURL(detailURL, item.Pic)

	result := model.SearchResult{
		MessageID: fmt.Sprintf("%s-%d", p.Name(), item.ID),
		UniqueID:  fmt.Sprintf("%s-%d", p.Name(), item.ID),
		Channel:   "",
		Datetime:  time.Now(),
		Title:     baseTitle,
		Content:   "来源：七味",
		Links: []model.Link{{
			Type:      "others",
			URL:       detailURL,
			WorkTitle: baseTitle,
		}},
	}

	if cover != "" {
		result.Images = []string{cover}
	}

	info, err := p.getDetailInfo(client, detailURL, baseTitle, cover, forceRefresh)
	if err != nil {
		return result
	}

	if info.Title != "" {
		result.Title = info.Title
		if len(result.Links) > 0 && result.Links[0].WorkTitle == baseTitle {
			result.Links[0].WorkTitle = info.Title
		}
	}
	if info.Content != "" {
		result.Content = info.Content
	}
	if !info.Datetime.IsZero() {
		result.Datetime = info.Datetime
	}
	if len(info.Images) > 0 {
		result.Images = info.Images
	}
	if len(info.Links) > 0 {
		result.Links = info.Links
	}

	return result
}

func (p *QiweiPlugin) getDetailInfo(client *http.Client, detailURL, fallbackTitle, fallbackPic string, forceRefresh bool) (detailInfo, error) {
	if forceRefresh {
		p.detailCache.Delete(detailURL)
	} else if cached, ok := p.detailCache.Load(detailURL); ok {
		if entry, ok := cached.(detailCacheEntry); ok {
			if time.Since(entry.CachedAt) < cacheTTL {
				return entry.Info, nil
			}
			p.detailCache.Delete(detailURL)
		}
	}

	var lastErr error
	for _, candidateURL := range p.detailURLCandidates(detailURL) {
		body, err := p.fetchBody(client, candidateURL, candidateURL, detailTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		if isVerifyPage(body) {
			if verifyErr := p.solveVerification(client, candidateURL, body); verifyErr != nil {
				lastErr = fmt.Errorf("[%s] 详情页验证失败: %w", p.Name(), verifyErr)
				continue
			}
			body, err = p.fetchBody(client, candidateURL, candidateURL, detailTimeout)
			if err != nil {
				lastErr = err
				continue
			}
			if isVerifyPage(body) {
				lastErr = fmt.Errorf("[%s] 详情页验证未通过: %s", p.Name(), candidateURL)
				continue
			}
		}

		info, err := p.parseDetail(candidateURL, body, fallbackTitle, fallbackPic)
		if err != nil {
			lastErr = err
			continue
		}

		p.detailCache.Store(detailURL, detailCacheEntry{Info: info, CachedAt: time.Now()})
		return info, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("[%s] 获取详情失败: %s", p.Name(), detailURL)
	}
	return detailInfo{}, lastErr
}

// solveVerification completes the site's deterministic slider challenge. The
// challenge is session-bound, so the caller and this method must share a
// cookie jar on the same http.Client.
func (p *QiweiPlugin) solveVerification(client *http.Client, pageURL, verifyHTML string) error {
	scriptMatch := verificationScriptRegex.FindStringSubmatch(verifyHTML)
	if len(scriptMatch) < 2 {
		return fmt.Errorf("未找到滑动验证脚本")
	}
	scriptURL := normalizeURL(pageURL, scriptMatch[1])
	jsBody, err := p.fetchBody(client, scriptURL, pageURL, detailTimeout)
	if err != nil {
		return fmt.Errorf("获取验证脚本失败: %w", err)
	}

	typeMatch := verificationTypeRegex.FindStringSubmatch(jsBody)
	keyMatch := verificationKeyRegex.FindStringSubmatch(jsBody)
	valueMatch := verificationValueRegex.FindStringSubmatch(jsBody)
	if len(typeMatch) < 2 || len(keyMatch) < 2 || len(valueMatch) < 2 {
		return fmt.Errorf("验证脚本参数不完整")
	}

	encodedValue := md5StringToHex(valueMatch[1])
	endpointPath := "/a20be899_96a6_40b2_88ba_32f1f75f1552_yanzheng_huadong.php"
	if endpointMatch := verificationEndpointRegex.FindStringSubmatch(jsBody); len(endpointMatch) > 1 {
		endpointPath = "/" + endpointMatch[1]
	}
	parsedPage, err := url.Parse(pageURL)
	if err != nil {
		return fmt.Errorf("验证页面地址无效: %w", err)
	}
	verifyURL := (&url.URL{Scheme: parsedPage.Scheme, Host: parsedPage.Host, Path: endpointPath}).String()
	query := url.Values{}
	query.Set("type", typeMatch[1])
	query.Set("key", keyMatch[1])
	query.Set("value", encodedValue)
	verifyURL += "?" + query.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), detailTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		return fmt.Errorf("创建验证请求失败: %w", err)
	}
	p.setHeaders(req, pageURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return fmt.Errorf("提交验证失败: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取验证响应失败: %w", err)
	}
	if isVerifyPage(string(responseBody)) {
		return fmt.Errorf("站点未接受验证参数")
	}
	return nil
}

func md5StringToHex(value string) string {
	var builder strings.Builder
	for _, r := range value {
		builder.WriteString(fmt.Sprintf("%d", r+1))
	}
	sum := md5.Sum([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func (p *QiweiPlugin) parseDetail(detailURL, body, fallbackTitle, fallbackPic string) (detailInfo, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return detailInfo{}, fmt.Errorf("[%s] 解析详情页失败: %w", p.Name(), err)
	}

	title := cleanText(doc.Find(".main-ui-meta h1").First().Text())
	title = yearSuffixRegex.ReplaceAllString(title, "")
	if title == "" {
		title = fallbackTitle
	}

	cover := normalizeURL(detailURL, attrOrEmpty(doc.Find(`meta[property="og:image"]`).First(), "content"))
	if cover == "" {
		cover = normalizeURL(detailURL, attrOrEmpty(doc.Find(".main-left .img img").First(), "src"))
	}
	if cover == "" {
		cover = fallbackPic
	}

	contentParts := make([]string, 0, 3)
	if meta := cleanText(doc.Find(".main-ui-meta .otherbox").First().Text()); meta != "" {
		contentParts = append(contentParts, meta)
	}
	intro := cleanText(doc.Find(".movie-introduce .sqjj_a").First().Text())
	if intro == "" {
		intro = cleanText(doc.Find(".movie-introduce .zkjj_a").First().Text())
	}
	if intro != "" {
		contentParts = append(contentParts, trimRunes(intro, 320))
	}
	content := strings.Join(contentParts, " | ")
	if content == "" {
		content = "来源：七味"
	}

	updateTime := extractUpdateTime(doc.Text())
	if updateTime.IsZero() {
		updateTime = time.Now()
	}

	links := p.extractLinksFromDetail(doc, body, detailURL, title)
	if len(links) == 0 {
		links = []model.Link{{Type: "others", URL: detailURL, WorkTitle: title}}
	}

	images := []string{}
	if cover != "" {
		images = append(images, cover)
	}

	return detailInfo{
		Title:    title,
		Content:  content,
		Datetime: updateTime,
		Links:    links,
		Images:   images,
	}, nil
}

func (p *QiweiPlugin) extractLinksFromDetail(doc *goquery.Document, rawHTML, detailURL, title string) []model.Link {
	seen := make(map[string]struct{}, 16)
	panLinks := make([]model.Link, 0, 16)
	magnetLinks := make([]model.Link, 0, 32)
	playLinks := make([]model.Link, 0, 8)

	addLink := func(link model.Link) {
		if link.URL == "" {
			return
		}
		if _, ok := seen[link.URL]; ok {
			return
		}
		seen[link.URL] = struct{}{}
		if link.Type == "magnet" || link.Type == "ed2k" {
			magnetLinks = append(magnetLinks, link)
			return
		}
		if isPanOrSpecialLinkType(link.Type) {
			panLinks = append(panLinks, link)
		}
	}

	// 优先按下载区结构提取，拿到更完整的资源标题和更多链接。
	doc.Find(".down-list .content li.down-list2").Each(func(_ int, li *goquery.Selection) {
		folderAnchor := li.Find("a.folder").First()
		folderURL := normalizeURL(detailURL, attrOrEmpty(folderAnchor, "href"))
		folderTitle := cleanText(attrOrEmpty(folderAnchor, "title"))
		if folderTitle == "" {
			folderTitle = cleanText(folderAnchor.Text())
		}

		downloadAnchor := li.Find("span a[href]").Last()
		downloadURL := normalizeURL(detailURL, attrOrEmpty(downloadAnchor, "href"))
		if downloadURL == "" {
			downloadURL = folderURL
		}

		linkType := determineCloudType(downloadURL)
		if !isPanOrSpecialLinkType(linkType) {
			return
		}

		addLink(model.Link{
			Type:      linkType,
			URL:       downloadURL,
			Password:  extractPassword(downloadURL, folderTitle, cleanText(li.Text())),
			WorkTitle: buildWorkTitle(title, folderTitle),
		})
	})

	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(attrOrEmpty(s, "href"))
		if href == "" {
			return
		}

		text := cleanText(s.Text())
		resolved := href
		if !strings.HasPrefix(href, "magnet:") && !strings.HasPrefix(href, "ed2k://") {
			resolved = normalizeURL(detailURL, href)
		}

		linkType := determineCloudType(resolved)
		switch {
		case isPanOrSpecialLinkType(linkType):
			addLink(model.Link{
				Type:      linkType,
				URL:       resolved,
				Password:  extractPassword(resolved, text, cleanText(s.Parent().Text())),
				WorkTitle: buildWorkTitle(title, text),
			})
		case isPlayPageURL(resolved):
			if _, ok := seen[resolved]; ok {
				return
			}
			seen[resolved] = struct{}{}
			playLinks = append(playLinks, model.Link{
				Type:      "others",
				URL:       resolved,
				WorkTitle: buildWorkTitle(title, text),
			})
		}
	})

	for _, match := range magnetRegex.FindAllString(rawHTML, -1) {
		magnetURL := htmlEntityDecode(match)
		addLink(model.Link{
			Type:      "magnet",
			URL:       magnetURL,
			WorkTitle: title,
		})
	}

	if len(playLinks) > maxFallbackPlayURLs {
		playLinks = playLinks[len(playLinks)-maxFallbackPlayURLs:]
	}

	result := make([]model.Link, 0, len(panLinks)+len(magnetLinks)+len(playLinks))
	result = append(result, panLinks...)
	result = append(result, magnetLinks...)
	if len(result) == 0 {
		result = append(result, playLinks...)
	}
	return result
}

func (p *QiweiPlugin) detailURLCandidates(detailURL string) []string {
	parsed, err := url.Parse(detailURL)
	if err != nil {
		return []string{detailURL}
	}

	baseHost := parsed.Scheme + "://" + parsed.Host
	path := parsed.RequestURI()
	seen := make(map[string]struct{}, len(qiweiHosts)+1)
	urls := make([]string, 0, len(qiweiHosts)+1)

	appendURL := func(host string) {
		candidate := host + path
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		urls = append(urls, candidate)
	}

	appendURL(baseHost)
	for _, host := range p.hostCandidates() {
		appendURL(host)
	}
	return urls
}

func (p *QiweiPlugin) fetchBody(client *http.Client, requestURL, referer string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}
	p.setHeaders(req, referer)

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("[%s] HTTP状态码异常: %d url=%s", p.Name(), resp.StatusCode, requestURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	return normalizeResponseBody(string(body)), nil
}

func (p *QiweiPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	const maxRetries = 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(time.Duration(1<<uint(i-1)) * 200 * time.Millisecond)
		}

		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("unknown request error")
	}
	return nil, fmt.Errorf("[%s] 重试 %d 次后仍失败: %w", p.Name(), maxRetries, lastErr)
}

func (p *QiweiPlugin) setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

func (p *QiweiPlugin) hostCandidates() []string {
	p.hostMu.RLock()
	active := p.activeHost
	p.hostMu.RUnlock()

	candidates := make([]string, 0, len(qiweiHosts))
	seen := make(map[string]struct{}, len(qiweiHosts))
	appendHost := func(host string) {
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		candidates = append(candidates, host)
	}

	appendHost(active)
	for _, host := range qiweiHosts {
		appendHost(host)
	}
	return candidates
}

func (p *QiweiPlugin) setActiveHost(host string) {
	p.hostMu.Lock()
	p.activeHost = host
	p.hostMu.Unlock()
}

func dedupeSuggestItems(items []suggestItem) []suggestItem {
	seen := make(map[int]struct{}, len(items))
	result := make([]suggestItem, 0, len(items))
	for _, item := range items {
		if item.ID == 0 || cleanText(item.Name) == "" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		result = append(result, item)
	}
	return result
}

func determineCloudType(link string) string {
	lower := strings.ToLower(link)
	switch {
	case strings.Contains(lower, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lower, "aliyundrive.com") || strings.Contains(lower, "alipan.com"):
		return "aliyun"
	case strings.Contains(lower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lower, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lower, "115.com") || strings.Contains(lower, "115cdn.com") || strings.Contains(lower, "anxia.com"):
		return "115"
	case strings.Contains(lower, "123684.com") || strings.Contains(lower, "123685.com") || strings.Contains(lower, "123912.com") || strings.Contains(lower, "123865.com") || strings.Contains(lower, "123pan.com") || strings.Contains(lower, "123pan.cn") || strings.Contains(lower, "123592.com"):
		return "123"
	case strings.Contains(lower, "content.21cn.com"):
		return "tianyi"
	case strings.Contains(lower, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(lower, "mypikpak.com"):
		return "pikpak"
	case strings.HasPrefix(lower, "magnet:"):
		return "magnet"
	case strings.HasPrefix(lower, "ed2k://"):
		return "ed2k"
	default:
		return "others"
	}
}

func isPanOrSpecialLinkType(linkType string) bool {
	switch linkType {
	case "quark", "uc", "baidu", "aliyun", "xunlei", "tianyi", "115", "123", "mobile", "pikpak", "magnet", "ed2k":
		return true
	default:
		return false
	}
}

func isPlayPageURL(link string) bool {
	return strings.Contains(link, "/py/") && strings.HasSuffix(strings.ToLower(link), ".html")
}

func scoreTitle(title, keyword string) int {
	lowerTitle := strings.ToLower(title)
	lowerKeyword := strings.ToLower(keyword)
	score := 0
	if strings.Contains(lowerTitle, lowerKeyword) {
		score += 1000
	}
	if strings.HasPrefix(lowerTitle, lowerKeyword) {
		score += 200
	}
	for idx, marker := range highValueKeywords {
		if strings.Contains(lowerTitle, strings.ToLower(marker)) {
			score += len(highValueKeywords) - idx
		}
	}
	return score
}

func buildWorkTitle(title, label string) string {
	cleanLabel := cleanText(label)
	if cleanLabel == "" || cleanLabel == title || strings.Contains(cleanLabel, title) {
		return title
	}
	return cleanText(title + " " + cleanLabel)
}

func extractPassword(link string, contexts ...string) string {
	for _, re := range passwordRegexes {
		if matches := re.FindStringSubmatch(link); len(matches) > 1 {
			return matches[1]
		}
	}
	for _, ctx := range contexts {
		text := cleanText(ctx)
		for _, re := range passwordRegexes {
			if matches := re.FindStringSubmatch(text); len(matches) > 1 {
				return matches[1]
			}
		}
	}
	return ""
}

func extractUpdateTime(text string) time.Time {
	match := updateTimeRegex.FindStringSubmatch(text)
	if len(match) < 2 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", match[1], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

func normalizeURL(baseURL, raw string) string {
	raw = strings.TrimSpace(cleanInvisibleRunes(htmlEntityDecode(raw)))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "magnet:") || strings.HasPrefix(raw, "ed2k://") {
		return raw
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return raw
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(ref).String()
}

func cleanText(s string) string {
	s = htmlEntityDecode(s)
	s = whitespaceRegex.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func trimRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

func htmlEntityDecode(s string) string {
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&#38;", "&",
		"&quot;", `"`,
		"&#34;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&lt;", "<",
		"&gt;", ">",
	)
	return replacer.Replace(s)
}

func normalizeResponseBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var decoded string
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			decoded = strings.TrimSpace(decoded)
			if strings.Contains(decoded, "<html") || strings.Contains(decoded, "<!DOCTYPE html") {
				return decoded
			}
		}
	}
	return body
}

func cleanInvisibleRunes(s string) string {
	replacer := strings.NewReplacer(
		"\u200b", "",
		"\u200c", "",
		"\u200d", "",
		"\ufeff", "",
	)
	return replacer.Replace(s)
}

func shouldForceRefresh(ext map[string]interface{}) bool {
	if ext == nil {
		return false
	}
	value, ok := ext["refresh"]
	if !ok {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	default:
		return false
	}
}

func attrOrEmpty(s *goquery.Selection, name string) string {
	if s == nil {
		return ""
	}
	value, _ := s.Attr(name)
	return value
}

func isVerifyPage(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "系统安全验证") ||
		strings.Contains(lower, "verify_check") ||
		strings.Contains(lower, "mac_verify_img") ||
		strings.Contains(lower, "请输入验证码") ||
		strings.Contains(lower, "/_guard/html.js?js=easy_click_html")
}
