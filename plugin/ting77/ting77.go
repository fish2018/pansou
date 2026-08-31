package ting77

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
	pluginName       = "ting77"
	pluginPriority   = 2
	defaultBaseURL   = "https://sou.77ting.top"
	requestTimeout   = 25 * time.Second
	maxResponseBytes = 4 << 20
	maxTokenRequests = 8
	tokenWindow      = 65 * time.Second
	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var errRateLimited = errors.New("链接令牌请求过于频繁")

func init() {
	plugin.RegisterGlobalPlugin(NewTing77Plugin())
}

type Ting77Plugin struct {
	*plugin.BaseAsyncPlugin
	baseURL      string
	linkCache    sync.Map
	tokenMu      sync.Mutex
	tokenRequest []time.Time
}

func NewTing77Plugin() *Ting77Plugin {
	return &Ting77Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, pluginPriority),
		baseURL:         defaultBaseURL,
	}
}

func (p *Ting77Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *Ting77Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Ting77Plugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	searchURL := p.baseURL + "/search?q=" + url.QueryEscape(keyword)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setRequestHeaders(req, p.baseURL+"/", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索页面返回状态码: %d", p.Name(), resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索页面失败: %w", p.Name(), err)
	}
	entries := parseSearchEntries(doc)
	if len(entries) == 0 {
		return nil, nil
	}

	results, resolveErr := p.resolveEntries(ctx, client, entries)
	if len(results) == 0 && resolveErr != nil {
		return nil, fmt.Errorf("[%s] 获取网盘链接失败: %w", p.Name(), resolveErr)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

type searchEntry struct {
	ID          string
	Title       string
	Description string
	Size        string
	Tags        []string
	CloudTypes  []string
	Datetime    time.Time
}

func parseSearchEntries(doc *goquery.Document) []searchEntry {
	entries := make([]searchEntry, 0, 20)
	doc.Find("a.resource-row").Each(func(_ int, selection *goquery.Selection) {
		href, _ := selection.Attr("href")
		resourceID := strings.TrimSpace(path.Base(strings.TrimSpace(href)))
		title := strings.TrimSpace(selection.Find(".row-title").First().Text())
		if resourceID == "" || resourceID == "." || resourceID == "/" || title == "" || !strings.HasPrefix(href, "/resource/") {
			return
		}

		entry := searchEntry{
			ID:          resourceID,
			Title:       title,
			Description: strings.TrimSpace(selection.Find(".row-desc").First().Text()),
			Size:        strings.TrimSpace(selection.Find(".row-size").First().Text()),
			Datetime:    parseDate(strings.TrimSpace(selection.Find(".row-date").First().Text())),
		}
		selection.Find(".row-tag").Each(func(_ int, tag *goquery.Selection) {
			if value := strings.TrimSpace(tag.Text()); value != "" {
				entry.Tags = append(entry.Tags, value)
			}
		})
		cloudSet := make(map[string]struct{})
		selection.Find(".cloud-badge").Each(func(_ int, badge *goquery.Selection) {
			classes, _ := badge.Attr("class")
			for _, className := range strings.Fields(classes) {
				if cloudType := normalizeSiteCloudType(className); cloudType != "" {
					cloudSet[cloudType] = struct{}{}
				}
			}
		})
		for cloudType := range cloudSet {
			entry.CloudTypes = append(entry.CloudTypes, cloudType)
		}
		sort.Slice(entry.CloudTypes, func(i, j int) bool {
			return cloudTypePriority(entry.CloudTypes[i]) < cloudTypePriority(entry.CloudTypes[j])
		})
		if len(entry.CloudTypes) > 0 {
			entries = append(entries, entry)
		}
	})
	return entries
}

func (p *Ting77Plugin) resolveEntries(ctx context.Context, client *http.Client, entries []searchEntry) ([]model.SearchResult, error) {
	results := make([]model.SearchResult, 0, len(entries))
	var lastErr error
	for _, entry := range entries {
		links := make([]model.Link, 0, len(entry.CloudTypes))
		for _, cloudType := range entry.CloudTypes {
			link, err := p.resolveLink(ctx, client, entry, cloudType)
			if err != nil {
				lastErr = err
				if errors.Is(err, errRateLimited) {
					break
				}
				continue
			}
			links = append(links, link)
		}
		if len(links) > 0 {
			results = append(results, model.SearchResult{
				UniqueID: pluginName + "-" + entry.ID,
				Channel:  "",
				Datetime: entry.Datetime,
				Title:    entry.Title,
				Content:  formatContent(entry.Description, entry.Size),
				Tags:     entry.Tags,
				Links:    links,
			})
		}
		if errors.Is(lastErr, errRateLimited) {
			break
		}
	}
	return results, lastErr
}

type tokenResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Token string `json:"token"`
		TS    string `json:"ts"`
	} `json:"data"`
}

func (p *Ting77Plugin) resolveLink(ctx context.Context, client *http.Client, entry searchEntry, cloudType string) (model.Link, error) {
	cacheKey := entry.ID + "\x00" + cloudType
	if cached, ok := p.linkCache.Load(cacheKey); ok {
		link := cached.(model.Link)
		link.Datetime = entry.Datetime
		link.WorkTitle = entry.Title
		return link, nil
	}
	if err := p.reserveTokenRequest(); err != nil {
		return model.Link{}, err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		token, ts, err := p.fetchLinkToken(ctx, client, entry.ID, cloudType)
		if err != nil {
			if errors.Is(err, errRateLimited) {
				return model.Link{}, err
			}
			lastErr = err
			continue
		}
		query := url.Values{
			"id":    []string{entry.ID},
			"type":  []string{cloudType},
			"token": []string{token},
			"ts":    []string{ts},
		}
		goURL := p.baseURL + "/go?" + query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, goURL, nil)
		if err != nil {
			return model.Link{}, err
		}
		setRequestHeaders(req, p.baseURL+"/resource/"+entry.ID, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		redirectClient := &http.Client{
			Timeout:   client.Timeout,
			Transport: client.Transport,
			Jar:       client.Jar,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := redirectClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 512<<10))
		resp.Body.Close()
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || location == "" {
			lastErr = fmt.Errorf("跳转接口返回状态码 %d", resp.StatusCode)
			continue
		}
		resolved, err := url.Parse(location)
		if err != nil || resolved.Scheme == "" || resolved.Host == "" {
			lastErr = fmt.Errorf("跳转接口返回无效链接")
			continue
		}
		linkType := detectLinkType(location)
		if linkType == "others" {
			lastErr = fmt.Errorf("跳转接口返回不支持的链接: %s", resolved.Host)
			continue
		}
		link := model.Link{
			Type:      linkType,
			URL:       location,
			Password:  extractPassword(location),
			Datetime:  entry.Datetime,
			WorkTitle: entry.Title,
		}
		p.linkCache.Store(cacheKey, link)
		return link, nil
	}
	return model.Link{}, lastErr
}

func (p *Ting77Plugin) reserveTokenRequest() error {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-tokenWindow)
	kept := p.tokenRequest[:0]
	for _, requestedAt := range p.tokenRequest {
		if requestedAt.After(cutoff) {
			kept = append(kept, requestedAt)
		}
	}
	p.tokenRequest = kept
	if len(p.tokenRequest) >= maxTokenRequests {
		return errRateLimited
	}
	p.tokenRequest = append(p.tokenRequest, now)
	return nil
}

func (p *Ting77Plugin) fetchLinkToken(ctx context.Context, client *http.Client, resourceID, cloudType string) (string, string, error) {
	query := url.Values{"id": []string{resourceID}, "type": []string{cloudType}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/link/token?"+query.Encode(), nil)
	if err != nil {
		return "", "", err
	}
	setRequestHeaders(req, p.baseURL+"/resource/"+resourceID, "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return "", "", errRateLimited
		}
		return "", "", fmt.Errorf("令牌接口返回状态码 %d", resp.StatusCode)
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return "", "", fmt.Errorf("解析链接令牌失败: %w", err)
	}
	if token.Code != 0 || token.Data.Token == "" || token.Data.TS == "" {
		if token.Code == http.StatusTooManyRequests {
			return "", "", errRateLimited
		}
		return "", "", fmt.Errorf("链接令牌无效: %s", token.Message)
	}
	return token.Data.Token, token.Data.TS, nil
}

func setRequestHeaders(req *http.Request, referer, accept string) {
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

func normalizeSiteCloudType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "quark":
		return "quark"
	case "ali":
		return "ali"
	case "baidu":
		return "baidu"
	default:
		return ""
	}
}

func cloudTypePriority(value string) int {
	switch value {
	case "quark":
		return 0
	case "ali":
		return 1
	case "baidu":
		return 2
	default:
		return 3
	}
}

func detectLinkType(rawURL string) string {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lower, "aliyundrive.com"), strings.Contains(lower, "alipan.com"):
		return "aliyun"
	default:
		return "others"
	}
}

func extractPassword(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, key := range []string{"pwd", "password", "code"} {
		if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func parseDate(value string) time.Time {
	if parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local); err == nil {
		return parsed
	}
	return time.Now()
}

func formatContent(description, size string) string {
	parts := make([]string, 0, 2)
	if description = strings.TrimSpace(description); description != "" {
		parts = append(parts, description)
	}
	if size = strings.TrimSpace(size); size != "" {
		parts = append(parts, "大小: "+size)
	}
	return strings.Join(parts, " | ")
}
