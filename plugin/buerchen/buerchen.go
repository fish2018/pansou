package buerchen

import (
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName           = "buerchen"
	defaultBaseURL       = "https://buerchen.top"
	searchTimeout        = 28 * time.Second
	searchPhaseTimeout   = 10 * time.Second
	maxCandidatesPerType = 6
	maxDailyPerType      = 2
	resolveConcurrency   = 3
	shareCacheTTL        = 30 * time.Minute
	maxCacheEntries      = 256
	maxResponseBytes     = 2 << 20
	requestCooldown      = time.Minute
)

type BuerchenPlugin struct {
	*plugin.BaseAsyncPlugin
	baseURL      string
	resolveSlots chan struct{}
	mu           sync.Mutex
	shareCache   map[string]cachedShare
	blockedUntil time.Time
	blockedErr   error
}

type candidate struct {
	title       string
	url         string
	description string
	provider    int
	daily       bool
}

type dailyResponse struct {
	Success bool `json:"success"`
	Code    int  `json:"code"`
	Data    *struct {
		Rows []struct {
			Title      string `json:"title"`
			QuarkLink  string `json:"quarkLink"`
			BaiduLink  string `json:"baiduLink"`
			XunleiLink string `json:"xunleiLink"`
		} `json:"data"`
	} `json:"data"`
}

type decryptResponse struct {
	Success bool `json:"success"`
	Code    int  `json:"code"`
	Data    *struct {
		URL string `json:"url"`
	} `json:"data"`
}

type saveResponse struct {
	Code int `json:"code"`
	Data *struct {
		URL      string `json:"url"`
		Code     string `json:"code"`
		Password string `json:"password"`
	} `json:"data"`
}

type resolvedShare struct {
	link     model.Link
	identity string
}

type cachedShare struct {
	share     resolvedShare
	expiresAt time.Time
}

func init() {
	plugin.RegisterGlobalPlugin(NewBuerchenPlugin())
}

func NewBuerchenPlugin() *BuerchenPlugin {
	return &BuerchenPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		baseURL:         defaultBaseURL,
		resolveSlots:    make(chan struct{}, resolveConcurrency),
		shareCache:      make(map[string]cachedShare),
	}
}

var _ plugin.AsyncSearchPlugin = (*BuerchenPlugin)(nil)

func (p *BuerchenPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	return result.Results, err
}

func (p *BuerchenPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *BuerchenPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	searchCtx, stopSearch := context.WithTimeout(ctx, searchPhaseTimeout)
	defer stopSearch()

	// 该站 is_type=-1 会返回“暂无可用线路”，须按页面的 0/2/4 分别搜索。
	providers := []int{0, 2, 4}
	web := make([][]candidate, len(providers))
	var daily [3][]candidate
	errs := make([]error, len(providers)+1)
	var wg sync.WaitGroup
	for i, provider := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			web[i], errs[i] = p.searchProvider(searchCtx, client, keyword, provider)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		daily, errs[len(providers)] = p.searchDaily(searchCtx, client, keyword)
	}()
	wg.Wait()
	stopSearch()

	groups := make([][]candidate, len(providers))
	for i := range providers {
		groups[i] = append(daily[i], web[i]...)
		if len(groups[i]) > maxCandidatesPerType {
			groups[i] = groups[i][:maxCandidatesPerType]
		}
	}
	entries := make([]candidate, 0, len(providers)*maxCandidatesPerType)
	for i := 0; i < maxCandidatesPerType; i++ {
		for _, group := range groups {
			if i < len(group) {
				entries = append(entries, group[i])
			}
		}
	}
	shares := make([]resolvedShare, len(entries))
	resolveErrors := make([]error, len(entries))
	refresh, _ := ext["refresh"].(bool)
	jobs := make(chan int, len(entries))
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	for range resolveConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				shares[i], resolveErrors[i] = p.resolveEntry(ctx, client, entries[i], refresh)
			}
		}()
	}
	wg.Wait()

	var lastErr error
	for _, err := range append(errs, resolveErrors...) {
		if err != nil {
			lastErr = err
		}
	}
	results := make([]model.SearchResult, 0, len(entries))
	seenIDs, seenURLs := make(map[string]bool), make(map[string]bool)
	for i, share := range shares {
		urlKey := share.link.URL + "\x00" + share.link.Password
		if share.link.URL == "" || seenIDs[share.identity] || seenURLs[urlKey] {
			continue
		}
		seenIDs[share.identity], seenURLs[urlKey] = true, true
		hash := sha256.Sum256([]byte(share.identity))
		entry := entries[i]
		share.link.WorkTitle = entry.title
		content := "来源：不二资源搜索站"
		if entry.daily {
			content += " · 每日更新"
		}
		if entry.description != "" {
			content += "\n" + entry.description
		}
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%x", pluginName, hash[:16]),
			Channel:  "",
			Title:    entry.title,
			Content:  content,
			Datetime: time.Now(),
			Links:    []model.Link{share.link},
		})
	}
	if len(results) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *BuerchenPlugin) searchDaily(ctx context.Context, client *http.Client, keyword string) ([3][]candidate, error) {
	var groups [3][]candidate
	var response dailyResponse
	// 全量目录超过 2 MiB，必须使用服务端关键词过滤。
	if err := p.requestJSON(ctx, client, http.MethodGet, "/api2/content?"+url.Values{"keyword": {keyword}}.Encode(), nil, &response); err != nil {
		return groups, err
	}
	if !response.Success {
		return groups, p.apiError("每日更新搜索", response.Code)
	}
	if response.Data == nil || response.Data.Rows == nil {
		return groups, fmt.Errorf("[%s] 每日更新响应缺少资源列表", pluginName)
	}
	seen := make(map[string]bool)
	for _, row := range response.Data.Rows {
		title := cleanText(row.Title)
		if !matchesKeyword(title, keyword) {
			continue
		}
		for i, raw := range []string{row.QuarkLink, row.BaiduLink, row.XunleiLink} {
			raw = strings.TrimSpace(raw)
			key := strconv.Itoa(i) + "\x00" + raw
			if raw == "" || len(raw) > 8192 || seen[key] || len(groups[i]) == maxDailyPerType {
				continue
			}
			seen[key] = true
			groups[i] = append(groups[i], candidate{title: title, url: raw, provider: i * 2, daily: true})
		}
	}
	return groups, nil
}

func (p *BuerchenPlugin) resolveEntry(ctx context.Context, client *http.Client, entry candidate, refresh bool) (resolvedShare, error) {
	key := strconv.Itoa(entry.provider) + "\x00" + strconv.FormatBool(entry.daily) + "\x00" + entry.url
	if !refresh {
		if cached, ok := p.getCachedShare(key); ok {
			return cached, nil
		}
	}
	select {
	case p.resolveSlots <- struct{}{}:
		defer func() { <-p.resolveSlots }()
	case <-ctx.Done():
		return resolvedShare{}, fmt.Errorf("[%s] 等待链接解析: %w", pluginName, ctx.Err())
	}
	if !refresh {
		if cached, ok := p.getCachedShare(key); ok {
			return cached, nil
		}
	}
	originalURL := entry.url
	if entry.daily {
		var decoded decryptResponse
		if err := p.requestJSON(ctx, client, http.MethodPost, "/api2/decrypt", map[string]string{"encryptUrl": entry.url}, &decoded); err != nil {
			return resolvedShare{}, err
		}
		if !decoded.Success {
			return resolvedShare{}, p.apiError("每日更新链接解密", decoded.Code)
		}
		if decoded.Data == nil {
			return resolvedShare{}, fmt.Errorf("[%s] 解密响应缺少 data", pluginName)
		}
		originalURL = decoded.Data.URL
	}
	original, direct := normalizeShareLink(originalURL, entry.description+" "+entry.title)
	if direct && original.Type != providerType(entry.provider) {
		return resolvedShare{}, fmt.Errorf("[%s] 分享类型与搜索线路不符", pluginName)
	}
	if !entry.daily && direct {
		return resolvedShare{link: original, identity: shareIdentity(original)}, nil
	}
	if !direct && (entry.daily || strings.HasPrefix(strings.ToLower(strings.TrimSpace(originalURL)), "http")) {
		return resolvedShare{}, fmt.Errorf("[%s] 原始资源不是有效的网盘分享", pluginName)
	}
	var response saveResponse
	payload := map[string]string{"url": encodeURIComponent(originalURL), "title": entry.title}
	if err := p.requestJSON(ctx, client, http.MethodPost, "/api/other/save_url", payload, &response); err != nil {
		return resolvedShare{}, err
	}
	if response.Code != 200 {
		return resolvedShare{}, p.apiError("链接解析", response.Code)
	}
	if response.Data == nil {
		return resolvedShare{}, fmt.Errorf("[%s] 链接解析响应缺少 data", pluginName)
	}
	password := response.Data.Code
	if password == "" {
		password = response.Data.Password
	}
	link, ok := normalizeShareLink(response.Data.URL, password)
	if !ok || link.Type != providerType(entry.provider) {
		return resolvedShare{}, fmt.Errorf("[%s] 未返回有效的 %s 分享链接", pluginName, providerType(entry.provider))
	}
	// SSE 用原始搜索 token，每日更新用解密后的原始分享；不使用新生成的分享作 ID。
	identity := strconv.Itoa(entry.provider) + "\x00" + entry.url
	if direct {
		identity = shareIdentity(original)
		if identity == shareIdentity(link) && link.Password == "" {
			link.Password = original.Password
		}
	}
	share := resolvedShare{link: link, identity: identity}
	p.putCachedShare(key, share)
	return share, nil
}

func providerType(provider int) string {
	switch provider {
	case 0:
		return "quark"
	case 2:
		return "baidu"
	case 4:
		return "xunlei"
	default:
		return ""
	}
}

// QueryEscape 与浏览器 encodeURIComponent 的空格及 !'()* 编码不同。
func encodeURIComponent(value string) string {
	replacer := strings.NewReplacer("+", "%20", "%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*")
	return replacer.Replace(url.QueryEscape(value))
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(html.UnescapeString(value)), " ")
}

func matchesKeyword(title, keyword string) bool {
	if title == "" {
		return false
	}
	title = strings.ToLower(title)
	for _, word := range strings.Fields(strings.ToLower(keyword)) {
		if !strings.Contains(title, word) {
			return false
		}
	}
	return true
}
