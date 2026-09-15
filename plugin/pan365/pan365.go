package pan365

import (
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName           = "pan365"
	defaultBaseURL       = "https://pan.365wp.top"
	searchTimeout        = 28 * time.Second
	searchPhaseTimeout   = 10 * time.Second
	maxCandidatesPerType = 6
	resolveConcurrency   = 3
	shareCacheTTL        = 30 * time.Minute
	maxCacheEntries      = 256
	maxResponseBytes     = 2 << 20
	requestCooldown      = time.Minute
)

type Pan365Plugin struct {
	*plugin.BaseAsyncPlugin
	baseURL      string
	resolveSlots chan struct{}
	mu           sync.Mutex
	shareCache   map[string]cachedShare
	blockedUntil time.Time
	blockedErr   error
}

type searchItem struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Source   string `json:"source"`
	DiskType string `json:"disk_type"`
}

type searchResponse struct {
	Code int `json:"code"`
	Data *struct {
		List  []searchItem `json:"list"`
		Stats []struct {
			Success bool `json:"success"`
		} `json:"search_stats"`
	} `json:"data"`
}

// 只读取公开分享字段，file_info 包含上游网盘的内部任务信息。
type transferResponse struct {
	Code int `json:"code"`
	Data *struct {
		OriginalURL string `json:"original_url"`
		ShareURL    string `json:"share_url"`
		Passcode    string `json:"passcode"`
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
	plugin.RegisterGlobalPlugin(NewPan365Plugin())
}

func NewPan365Plugin() *Pan365Plugin {
	return &Pan365Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, 3),
		baseURL:         defaultBaseURL,
		resolveSlots:    make(chan struct{}, resolveConcurrency),
		shareCache:      make(map[string]cachedShare),
	}
}

var _ plugin.AsyncSearchPlugin = (*Pan365Plugin)(nil)

func (p *Pan365Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	return result.Results, err
}

func (p *Pan365Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Pan365Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	searchCtx, stopSearch := context.WithTimeout(ctx, searchPhaseTimeout)
	defer stopSearch()

	// 不指定类型时首页几乎全是夸克；分别查询并交替解析，给百度保留名额。
	providers := []string{"quark", "baidu"}
	batches := make([][]searchItem, len(providers))
	searchErrors := make([]error, len(providers))
	var wg sync.WaitGroup
	for i, provider := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batches[i], searchErrors[i] = p.searchProvider(searchCtx, client, keyword, provider)
		}()
	}
	wg.Wait()
	stopSearch()

	entries := make([]searchItem, 0, len(providers)*maxCandidatesPerType)
	for i := 0; i < maxCandidatesPerType; i++ {
		for _, batch := range batches {
			if i < len(batch) {
				entries = append(entries, batch[i])
			}
		}
	}
	refresh, _ := ext["refresh"].(bool)
	shares := make([]resolvedShare, len(entries))
	errs := make([]error, len(entries))
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
				shares[i], errs[i] = p.resolveEntry(ctx, client, entries[i], refresh)
			}
		}()
	}
	wg.Wait()

	var lastErr error
	for _, err := range append(searchErrors, errs...) {
		if err != nil {
			lastErr = err
		}
	}
	results := make([]model.SearchResult, 0, len(entries))
	seen := make(map[string]bool)
	for i, share := range shares {
		if share.link.URL == "" || seen[share.identity] {
			continue
		}
		seen[share.identity] = true
		hash := sha256.Sum256([]byte(share.identity))
		share.link.WorkTitle = entries[i].Title
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%x", pluginName, hash[:16]),
			Channel:  "",
			Title:    entries[i].Title,
			Content:  "来源：365 聚合站 · " + entries[i].Source,
			Datetime: time.Now(),
			Links:    []model.Link{share.link},
		})
	}
	if len(results) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *Pan365Plugin) searchProvider(ctx context.Context, client *http.Client, keyword, provider string) ([]searchItem, error) {
	query := url.Values{"keyword": {keyword}, "disk_type": {provider}, "page": {"1"}, "pageSize": {"20"}}
	var response searchResponse
	if err := p.requestJSON(ctx, client, http.MethodGet, "/api/interface/search?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}
	if response.Code != 200 {
		return nil, p.apiError("搜索", response.Code)
	}
	if response.Data == nil || response.Data.List == nil {
		return nil, fmt.Errorf("[%s] 搜索响应缺少资源列表", pluginName)
	}
	if len(response.Data.List) == 0 && len(response.Data.Stats) > 0 {
		succeeded := false
		for _, stat := range response.Data.Stats {
			succeeded = succeeded || stat.Success
		}
		if !succeeded {
			return nil, fmt.Errorf("[%s] %s 搜索线路全部失败", pluginName, provider)
		}
	}
	entries := make([]searchItem, 0, maxCandidatesPerType)
	seen := make(map[string]bool)
	for _, item := range response.Data.List {
		item.Title = strings.Join(strings.Fields(html.UnescapeString(item.Title)), " ")
		item.URL = strings.TrimSpace(item.URL)
		if item.DiskType != provider || item.URL == "" || len(item.URL) > 8192 || seen[item.URL] || !matchesKeyword(item.Title, keyword) {
			continue
		}
		seen[item.URL] = true
		entries = append(entries, item)
		if len(entries) == maxCandidatesPerType {
			break
		}
	}
	return entries, nil
}

func (p *Pan365Plugin) resolveEntry(ctx context.Context, client *http.Client, item searchItem, refresh bool) (resolvedShare, error) {
	key := item.DiskType + "\x00" + item.URL
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
	if link, ok := normalizeShareLink(item.URL, ""); ok && link.Type == item.DiskType {
		return resolvedShare{link: link, identity: shareIdentity(link)}, nil
	}
	var response transferResponse
	payload := map[string]string{"encrypted_url": item.URL}
	if err := p.requestJSON(ctx, client, http.MethodPost, "/api/transfer-share/transfer-share", payload, &response); err != nil {
		return resolvedShare{}, err
	}
	if response.Code != 200 {
		return resolvedShare{}, p.apiError("链接解析", response.Code)
	}
	if response.Data == nil {
		return resolvedShare{}, fmt.Errorf("[%s] 链接解析响应缺少 data", pluginName)
	}
	link, ok := normalizeShareLink(response.Data.ShareURL, response.Data.Passcode)
	if !ok || link.Type != item.DiskType {
		return resolvedShare{}, fmt.Errorf("[%s] 未返回有效的 %s 分享链接", pluginName, item.DiskType)
	}
	// 转存生成的新链接会变化；优先用原始分享标识资源，避免刷新后重复。
	identity := item.DiskType + "\x00" + item.Source + "\x00" + item.Title
	if original, valid := normalizeShareLink(response.Data.OriginalURL, ""); valid && original.Type == item.DiskType {
		identity = shareIdentity(original)
	}
	share := resolvedShare{link: link, identity: identity}
	p.putCachedShare(key, share)
	return share, nil
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
