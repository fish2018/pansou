package run488

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

const (
	pluginName        = "488run"
	defaultPriority   = 2
	defaultBaseURL    = "https://488.run"
	defaultTheme      = "wechat-cinehunt"
	searchLimit       = 24
	requestTimeout    = 10 * time.Second
	cookieTTL         = 6 * time.Hour
	defaultPollDelay  = 420 * time.Millisecond
	defaultMaxPolls   = 6
	rateLimitCooldown = 20 * time.Second
	maxResponseBytes  = 4 * 1024 * 1024
	userAgent         = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
)

var (
	cstZone = time.FixedZone("CST", 8*3600)

	errRateLimited  = errors.New("488run rate limited")
	errUnauthorized = errors.New("488run session unauthorized")

	urlPattern      = regexp.MustCompile(`https?://[a-zA-Z0-9._~:/?#\[\]@!$&'*+,;=%-]+`)
	magnetPattern   = regexp.MustCompile(`magnet:\?xt=urn:btih:[a-zA-Z0-9]+[^\s<>"']*`)
	ed2kPattern     = regexp.MustCompile(`ed2k://\|file\|[^\s<>"']+\|/`)
	pwdTokenPattern = regexp.MustCompile(`^[a-zA-Z0-9]{4,8}`)

	baidupwdPattern      = regexp.MustCompile(`(?i)(?:提取码|密码|访问码|验证码|口令|pwd|password|passcode)\s*[:：=]\s*([a-zA-Z0-9]{4})\b`)
	trailingNoisePattern = regexp.MustCompile(`\s*提取码\s*[:：]?\s*(?:\d+\s*(?:天|小时|分钟|秒|个月|周|年)前)?\s*$`)
	hashIDPattern        = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
)

// Run488Plugin 488.run (夸搜盘) 搜索插件
type Run488Plugin struct {
	*plugin.BaseAsyncPlugin
	baseURL       string
	pollDelay     time.Duration
	maxPolls      int
	cookieMu      sync.Mutex
	cookie        string
	cookieAt      time.Time
	cookieGroup   singleflight.Group
	cooldownUntil atomic.Int64
}

type apiEnvelope struct {
	Code    int        `json:"code"`
	Message string     `json:"message"`
	Data    searchData `json:"data"`
}

type searchData struct {
	Complete    bool           `json:"complete"`
	Counts      map[string]int `json:"counts"`
	Items       []resourceItem `json:"items"`
	List        []resourceItem `json:"list"`
	SearchID    string         `json:"search_id"`
	SearchIDAlt string         `json:"searchId"`
	Total       int            `json:"total"`
	Blocked     bool           `json:"blocked"`
	BlockedWord string         `json:"blocked_word"`
}

type resourceItem struct {
	ID          string `json:"id"`
	ResourceID  string `json:"resource_id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Name        string `json:"name"`
	Desc        string `json:"desc"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Year        string `json:"year"`
	Area        string `json:"area"`
	DiskType    string `json:"disk_type"`
	DriveType   string `json:"drive_type"`
	Link        string `json:"link"`
	ShareLink   string `json:"share_link"`
	URL         string `json:"url"`
	Magnet      string `json:"magnet"`
	Poster      string `json:"poster"`
	Cover       string `json:"cover"`
	Source      string `json:"source"`
	SourceLabel string `json:"source_label"`
	Status      string `json:"status"`
	ShareTime   string `json:"share_time"`
	UpdatedAt   string `json:"updated_at"`
	CreatedAt   string `json:"created_at"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewRun488Plugin())
}

// NewRun488Plugin 创建 488.run 搜索插件实例
func NewRun488Plugin() *Run488Plugin {
	p := &Run488Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		baseURL:         defaultBaseURL,
		pollDelay:       defaultPollDelay,
		maxPolls:        defaultMaxPolls,
	}
	if len(os.Args) > 0 && !strings.HasSuffix(os.Args[0], ".test") {
		go func() {
			time.Sleep(2 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			_, _ = p.ensureCookie(ctx, p.GetClient(), "", false)
		}()
	}
	return p
}

// Search 执行搜索并返回结果
func (p *Run488Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行带状态元数据的异步搜索
func (p *Run488Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Run488Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}

	if until := p.cooldownUntil.Load(); until > 0 && time.Now().UnixNano() < until {
		return nil, errRateLimited
	}

	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	ctx := context.Background()
	if ext != nil {
		if c, ok := ext[plugin.ExtContextKey].(context.Context); ok && c != nil {
			ctx = c
		}
	}

	cookie, err := p.ensureCookie(ctx, client, keyword, false)
	if err != nil {
		cookie = ""
	}

	publishDeadline := time.Now().Add(plugin.PublishBudget())

	initial, err := p.fetchSearch(ctx, client, keyword, cookie)
	if errors.Is(err, errUnauthorized) {
		freshCookie, cookieErr := p.ensureCookie(ctx, client, keyword, true)
		if cookieErr == nil && freshCookie != "" {
			cookie = freshCookie
			initial, err = p.fetchSearch(ctx, client, keyword, cookie)
		}
	}
	if err != nil {
		return nil, err
	}

	if initial.Data.Blocked {
		return []model.SearchResult{}, nil
	}

	merger := newItemMerger()
	merger.addSlice(initial.Data.List)
	merger.addSlice(initial.Data.Items)

	complete := initial.Data.Complete
	searchID := strings.TrimSpace(initial.Data.SearchID)
	if searchID == "" {
		searchID = strings.TrimSpace(initial.Data.SearchIDAlt)
	}

	maxPolls := p.maxPolls
	if maxPolls <= 0 {
		maxPolls = defaultMaxPolls
	}
	pollDelay := p.pollDelay
	if pollDelay <= 0 {
		pollDelay = defaultPollDelay
	}

	if !complete && searchID != "" {
		for attempt := 0; attempt < maxPolls; attempt++ {
			remaining := time.Until(publishDeadline)
			if merger.len() > 0 && remaining < 350*time.Millisecond {
				break
			}

			waitDur := pollDelay
			if merger.len() > 0 && remaining > 350*time.Millisecond && waitDur > remaining-300*time.Millisecond {
				candidate := remaining - 300*time.Millisecond
				if candidate >= 150*time.Millisecond {
					waitDur = candidate
				}
			}

			if !sleepWithContext(ctx, waitDur) {
				break
			}

			polled, pollErr := p.fetchPoll(ctx, client, keyword, searchID, cookie)
			if pollErr != nil {
				if merger.len() > 0 {
					break
				}
				return nil, pollErr
			}
			if polled == nil {
				continue
			}

			merger.addSlice(polled.Data.List)
			merger.addSlice(polled.Data.Items)
			if polled.Data.Complete {
				break
			}
		}
	}

	items := merger.items()
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	results := make([]model.SearchResult, 0, len(items))
	seenResultIDs := make(map[string]struct{}, len(items))
	for idx, item := range items {
		res, ok := p.convertItem(item, idx)
		if !ok {
			continue
		}
		if _, exists := seenResultIDs[res.UniqueID]; exists {
			continue
		}
		seenResultIDs[res.UniqueID] = struct{}{}
		results = append(results, res)
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Datetime.After(results[j].Datetime)
	})

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *Run488Plugin) getCachedCookie() string {
	p.cookieMu.Lock()
	defer p.cookieMu.Unlock()
	if p.cookie != "" && time.Since(p.cookieAt) < cookieTTL {
		return p.cookie
	}
	return ""
}

func (p *Run488Plugin) ensureCookie(ctx context.Context, client *http.Client, keyword string, forceRefresh bool) (string, error) {
	if !forceRefresh {
		if cached := p.getCachedCookie(); cached != "" {
			return cached, nil
		}
	}

	val, err, _ := p.cookieGroup.Do("yd_frontend", func() (interface{}, error) {
		p.cookieMu.Lock()
		if !forceRefresh && p.cookie != "" && time.Since(p.cookieAt) < cookieTTL {
			cached := p.cookie
			p.cookieMu.Unlock()
			return cached, nil
		}
		p.cookieMu.Unlock()

		reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()

		base := strings.TrimRight(p.baseURL, "/")
		targetURL := base + "/"
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		_, _ = util.ReadAllLimited(resp.Body, maxResponseBytes)
		_ = resp.Body.Close()

		cookieVal := extractSessionCookie(resp)
		if cookieVal == "" {
			return "", fmt.Errorf("488run: yd_frontend cookie not found (status %d)", resp.StatusCode)
		}

		p.cookieMu.Lock()
		p.cookie = cookieVal
		p.cookieAt = time.Now()
		p.cookieMu.Unlock()

		return cookieVal, nil
	})
	if err != nil {
		return "", err
	}
	cookieStr, _ := val.(string)
	return cookieStr, nil
}

func extractSessionCookie(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	for _, c := range resp.Cookies() {
		if c.Name == "yd_frontend" && strings.TrimSpace(c.Value) != "" {
			return "yd_frontend=" + strings.TrimSpace(c.Value)
		}
	}
	for _, raw := range resp.Header.Values("Set-Cookie") {
		parts := strings.Split(raw, ";")
		if len(parts) > 0 {
			kv := strings.TrimSpace(parts[0])
			if strings.HasPrefix(kv, "yd_frontend=") {
				return kv
			}
		}
	}
	return ""
}

func (p *Run488Plugin) fetchSearch(ctx context.Context, client *http.Client, keyword, cookie string) (*apiEnvelope, error) {
	base := strings.TrimRight(p.baseURL, "/")
	params := url.Values{}
	params.Set("q", keyword)
	params.Set("limit", fmt.Sprintf("%d", searchLimit))
	params.Set("theme", defaultTheme)

	apiURL := fmt.Sprintf("%s/api/frontend/search?%s", base, params.Encode())
	return p.doAPIRequest(ctx, client, apiURL, keyword, cookie)
}

func (p *Run488Plugin) fetchPoll(ctx context.Context, client *http.Client, keyword, searchID, cookie string) (*apiEnvelope, error) {
	base := strings.TrimRight(p.baseURL, "/")
	params := url.Values{}
	params.Set("id", searchID)
	params.Set("limit", fmt.Sprintf("%d", searchLimit))
	params.Set("theme", defaultTheme)

	apiURL := fmt.Sprintf("%s/api/frontend/search/poll?%s", base, params.Encode())
	env, err := p.doAPIRequest(ctx, client, apiURL, keyword, cookie)
	if err != nil {
		return nil, err
	}
	if env != nil && (env.Code == 404 || env.Code == 1004) {
		env.Data.Complete = true
		return env, nil
	}
	return env, nil
}

func (p *Run488Plugin) doAPIRequest(ctx context.Context, client *http.Client, apiURL, keyword, cookie string) (*apiEnvelope, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}

	base := strings.TrimRight(p.baseURL, "/")
	referer := fmt.Sprintf("%s/oeol/%s.htm", base, url.PathEscape(keyword))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", referer)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := util.ReadAllLimited(resp.Body, maxResponseBytes)
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}

	if newCookie := extractSessionCookie(resp); newCookie != "" {
		p.cookieMu.Lock()
		p.cookie = newCookie
		p.cookieAt = time.Now()
		p.cookieMu.Unlock()
	}

	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "访问过于频繁")) {
		p.cooldownUntil.Store(time.Now().Add(rateLimitCooldown).UnixNano())
		return nil, errRateLimited
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errUnauthorized
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("488run api status %d", resp.StatusCode)
	}

	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("488run decode json failed: %w", err)
	}

	if env.Code != 0 && env.Code != 404 && env.Code != 1004 {
		if strings.Contains(env.Message, "频繁") {
			p.cooldownUntil.Store(time.Now().Add(rateLimitCooldown).UnixNano())
			return nil, errRateLimited
		}
		return nil, fmt.Errorf("488run api error code=%d msg=%s", env.Code, env.Message)
	}

	return &env, nil
}

type itemMerger struct {
	order []string
	byKey map[string]resourceItem
}

func newItemMerger() *itemMerger {
	return &itemMerger{
		order: make([]string, 0, 32),
		byKey: make(map[string]resourceItem, 32),
	}
}

func (m *itemMerger) len() int {
	return len(m.order)
}

func (m *itemMerger) addSlice(list []resourceItem) {
	for _, item := range list {
		m.add(item)
	}
}

func (m *itemMerger) add(item resourceItem) {
	if strings.EqualFold(strings.TrimSpace(item.Status), "invalid") {
		return
	}
	key := itemIdentity(item)
	if key == "" {
		return
	}
	existing, exists := m.byKey[key]
	if !exists {
		m.order = append(m.order, key)
		m.byKey[key] = item
		return
	}
	m.byKey[key] = mergeResourceItem(existing, item)
}

func (m *itemMerger) items() []resourceItem {
	out := make([]resourceItem, 0, len(m.order))
	for _, k := range m.order {
		out = append(out, m.byKey[k])
	}
	return out
}

func itemIdentity(item resourceItem) string {
	for _, candidate := range []string{item.ShareLink, item.Link, item.URL, item.Magnet, item.ID, item.ResourceID, item.Slug, item.Title, item.Name} {
		if s := strings.TrimSpace(candidate); s != "" {
			return s
		}
	}
	return ""
}

func mergeResourceItem(base, incoming resourceItem) resourceItem {
	if base.ID == "" {
		base.ID = incoming.ID
	}
	if base.ResourceID == "" {
		base.ResourceID = incoming.ResourceID
	}
	if base.Title == "" {
		base.Title = incoming.Title
	}
	if base.Name == "" {
		base.Name = incoming.Name
	}
	if len(incoming.Description) > len(base.Description) {
		base.Description = incoming.Description
	}
	if len(incoming.Desc) > len(base.Desc) {
		base.Desc = incoming.Desc
	}
	if base.Link == "" {
		base.Link = incoming.Link
	}
	if base.ShareLink == "" {
		base.ShareLink = incoming.ShareLink
	}
	if base.URL == "" {
		base.URL = incoming.URL
	}
	if base.Magnet == "" {
		base.Magnet = incoming.Magnet
	}
	if base.DiskType == "" {
		base.DiskType = incoming.DiskType
	}
	if base.DriveType == "" {
		base.DriveType = incoming.DriveType
	}
	if base.Poster == "" {
		base.Poster = incoming.Poster
	}
	if base.Cover == "" {
		base.Cover = incoming.Cover
	}
	if base.Source == "" {
		base.Source = incoming.Source
	}
	if base.SourceLabel == "" {
		base.SourceLabel = incoming.SourceLabel
	}
	if base.ShareTime == "" {
		base.ShareTime = incoming.ShareTime
	}
	if base.UpdatedAt == "" {
		base.UpdatedAt = incoming.UpdatedAt
	}
	if base.CreatedAt == "" {
		base.CreatedAt = incoming.CreatedAt
	}
	return base
}

func (p *Run488Plugin) convertItem(item resourceItem, idx int) (model.SearchResult, bool) {
	title := firstNonEmpty(item.Title, item.Name)
	title = cleanText(title)
	if title == "" {
		return model.SearchResult{}, false
	}

	desc := firstNonEmpty(item.Description, item.Desc)
	desc = cleanText(desc)

	links := extractLinksFromItem(item, title)
	if len(links) == 0 {
		return model.SearchResult{}, false
	}

	rawID := firstNonEmpty(item.ID, item.ResourceID, item.Slug, links[0].URL, fmt.Sprintf("idx-%d", idx))
	uniqueID := fmt.Sprintf("%s-%s", p.Name(), sanitizeID(rawID))

	dt := parseItemTime(firstNonEmpty(item.ShareTime, item.UpdatedAt, item.CreatedAt))

	tags := buildTags(item)
	images := buildImages(p.baseURL, firstNonEmpty(item.Poster, item.Cover))

	return model.SearchResult{
		UniqueID: uniqueID,
		Title:    title,
		Content:  desc,
		Links:    links,
		Tags:     tags,
		Channel:  "",
		Datetime: dt,
		Images:   images,
	}, true
}

func extractLinksFromItem(item resourceItem, workTitle string) []model.Link {
	hintType := mapDiskType(firstNonEmpty(item.DiskType, item.DriveType))
	contextText := strings.Join([]string{item.Title, item.Description, item.Desc}, " ")

	candidates := make([]string, 0, 6)
	for _, raw := range []string{item.ShareLink, item.Link, item.URL, item.Magnet} {
		if s := strings.TrimSpace(raw); s != "" {
			candidates = append(candidates, s)
		}
	}

	for _, extra := range urlPattern.FindAllString(contextText, -1) {
		candidates = append(candidates, extra)
	}
	for _, mag := range magnetPattern.FindAllString(contextText, -1) {
		candidates = append(candidates, mag)
	}
	for _, ed := range ed2kPattern.FindAllString(contextText, -1) {
		candidates = append(candidates, ed)
	}

	links := make([]model.Link, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))

	for _, raw := range candidates {
		cleaned := cleanLinkURL(raw)
		if cleaned == "" {
			continue
		}

		linkType, normURL, pwd := classifyAndExtractLink(cleaned, hintType, contextText)
		if linkType == "" || normURL == "" {
			continue
		}

		dedupKey := linkType + "|" + strings.ToLower(normURL)
		if _, exists := seen[dedupKey]; exists {
			continue
		}
		seen[dedupKey] = struct{}{}

		links = append(links, model.Link{
			Type:      linkType,
			URL:       normURL,
			Password:  pwd,
			WorkTitle: workTitle,
		})
	}

	return links
}

func classifyAndExtractLink(rawURL, hintType, contextText string) (string, string, string) {
	lower := strings.ToLower(rawURL)

	if strings.HasPrefix(lower, "magnet:?") {
		return "magnet", rawURL, ""
	}
	if strings.HasPrefix(lower, "ed2k://") {
		return "ed2k", rawURL, ""
	}

	linkType := detectLinkTypeByHost(lower)
	if linkType == "" {
		if hintType != "" && hintType != "others" && isLikelyShareURL(lower) {
			linkType = hintType
		} else {
			return "", "", ""
		}
	}

	pwd := extractPasswordFromURL(rawURL)
	if pwd == "" && contextText != "" {
		if m := baidupwdPattern.FindStringSubmatch(contextText); len(m) > 1 {
			pwd = strings.TrimSpace(m[1])
		}
	}

	return linkType, rawURL, pwd
}

func detectLinkTypeByHost(lowerURL string) string {
	switch {
	case strings.Contains(lowerURL, "pan.quark.cn") || strings.Contains(lowerURL, "quark.cn/s/"):
		return "quark"
	case strings.Contains(lowerURL, "pan.baidu.com") || strings.Contains(lowerURL, "yun.baidu.com"):
		return "baidu"
	case strings.Contains(lowerURL, "drive.uc.cn") || strings.Contains(lowerURL, "fast.uc.cn"):
		return "uc"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lowerURL, "alipan.com") || strings.Contains(lowerURL, "aliyundrive.com"):
		return "aliyun"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lowerURL, "115.com/s/") || strings.Contains(lowerURL, "115cdn.com/s/") || strings.Contains(lowerURL, "anxia.com/s/"):
		return "115"
	case strings.Contains(lowerURL, "123pan.com/s/") || strings.Contains(lowerURL, "123pan.cn/s/") ||
		strings.Contains(lowerURL, "123684.com/s/") || strings.Contains(lowerURL, "123685.com/s/") ||
		strings.Contains(lowerURL, "123865.com/s/") || strings.Contains(lowerURL, "123912.com/s/") ||
		strings.Contains(lowerURL, "123592.com/s/"):
		return "123"
	case strings.Contains(lowerURL, "caiyun.139.com") || strings.Contains(lowerURL, "yun.139.com"):
		return "mobile"
	case strings.Contains(lowerURL, "mypikpak.com"):
		return "pikpak"
	case strings.Contains(lowerURL, "guangyapan.com") || strings.Contains(lowerURL, "pan.guangya"):
		return "guangya"
	default:
		return ""
	}
}

func isLikelyShareURL(lowerURL string) bool {
	if !strings.HasPrefix(lowerURL, "http://") && !strings.HasPrefix(lowerURL, "https://") {
		return false
	}
	if strings.Contains(lowerURL, "488.run") || strings.Contains(lowerURL, "b46.cn") || strings.Contains(lowerURL, "weserv.nl") {
		return false
	}
	return strings.Contains(lowerURL, "/s/") || strings.Contains(lowerURL, "/t/") || strings.Contains(lowerURL, "share")
}

func mapDiskType(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "quark", "夸克", "夸克网盘":
		return "quark"
	case "baidu", "bd", "百度", "百度网盘":
		return "baidu"
	case "uc", "uc网盘":
		return "uc"
	case "xunlei", "xl", "迅雷", "迅雷网盘":
		return "xunlei"
	case "aliyun", "ali", "alipan", "阿里", "阿里云盘":
		return "aliyun"
	case "tianyi", "189", "天翼", "天翼云盘":
		return "tianyi"
	case "115", "115网盘":
		return "115"
	case "123", "123pan", "123网盘":
		return "123"
	case "mobile", "139", "caiyun", "移动", "移动云盘":
		return "mobile"
	case "pikpak":
		return "pikpak"
	case "guangya", "光鸭", "光鸭云盘":
		return "guangya"
	case "magnet", "磁力":
		return "magnet"
	case "ed2k", "电驴":
		return "ed2k"
	default:
		return ""
	}
}

func extractPasswordFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	q := parsed.Query()
	for _, key := range []string{"pwd", "password", "passcode", "code"} {
		if val := strings.TrimSpace(q.Get(key)); val != "" {
			if m := pwdTokenPattern.FindString(val); m != "" {
				return m
			}
		}
	}
	return ""
}

func cleanLinkURL(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "http://") || strings.HasPrefix(strings.ToLower(s), "https://") {
		if m := urlPattern.FindString(s); m != "" {
			s = m
		}
	}
	s = strings.TrimRight(s, ".,;，。；!！?？)）】]>\"'")
	return s
}

func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(trailingNoisePattern.ReplaceAllString(s, ""))
	return s
}

func buildTags(item resourceItem) []string {
	candidates := []string{
		item.SourceLabel,
		item.Category,
		item.Year,
		item.Area,
	}
	tags := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		t := strings.TrimSpace(c)
		if t == "" || t == "资源" || t == "全部" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		tags = append(tags, t)
	}
	return tags
}

func buildImages(baseURL, poster string) []string {
	p := strings.TrimSpace(poster)
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "//") {
		return []string{"https:" + p}
	}
	if strings.HasPrefix(p, "/") {
		return []string{strings.TrimRight(baseURL, "/") + p}
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return []string{p}
	}
	return nil
}

func parseItemTime(raw string) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Now()
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, cstZone); err == nil {
			return t
		}
	}
	return time.Now()
}

func sanitizeID(raw string) string {
	cleaned := hashIDPattern.ReplaceAllString(strings.TrimSpace(raw), "_")
	cleaned = strings.Trim(cleaned, "_")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	if cleaned == "" {
		return "item"
	}
	return cleaned
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
