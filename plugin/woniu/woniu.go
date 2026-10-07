// Package woniu 实现「蜗牛 I 4K」（https://wn4k.com）的搜索插件。
//
// 站点是 MacCMS + panlian_dark 模板：搜索结果页给出影片卡片，真正的网盘链接在详情页里，
// 而详情页对游客只放出"锁定预览"——标题和简介可见，链接位置一律是
// https://******（登录后可见），一个真实链接都拿不到。所以插件必须带登录态：
// 搜索本身游客可做，详情必须登录。
//
// 与 panlian 插件的区别：盘链走的是自建 JSON API（/api/videos），蜗牛这里仍是
// MacCMS 的 HTML 页面（/vodsearch/...、/voddetail/{id}/），因此按 HTML 插件实现，
// 但账号管理沿用同一套"多账号 + 专属 hash + Web 管理页"的模式。
package woniu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

// DefaultBaseURL 是站点根地址。声明为变量而非常量，是为了让测试能指向本地假服务器，
// 从而验证搜索、详情、登录与登出语义，而不是只能靠肉眼看代码。
var DefaultBaseURL = "https://wn4k.com"

const (
	PluginName     = "woniu"
	DisplayName    = "蜗牛"
	Description    = "蜗牛 I 4K - 登录后检索影视资源并聚合网盘链接"
	ConfigFileName = "woniu_config.json"

	RequestTimeout = 20 * time.Second
	// LoginTimeout 比普通请求宽：站点在 Cloudflare 后面，登录偶尔要排队。
	LoginTimeout = 30 * time.Second

	// MaxSearchResults 是搜索页最多取用的影片数。站点默认一页 24 条且不提供翻页
	// （实测 /vodsearch/-------------/page/2.html 返回空页），所以只做首页。
	MaxSearchResults = 24
	// MaxConcurrentDetails 限制详情页并发。站点对 8 路并发实测无风控，但详情页是
	// 全量正文，保守取 6，配合发布预算足够在窗口内跑完常见关键词。
	MaxConcurrentDetails = 6
	// MaxLinksPerResult 单条结果的链接上限，防止个别影片挂了几百条分享撑爆响应。
	MaxLinksPerResult = 200

	maxResponseBytes = 8 << 20
)

var (
	storageDir string

	errLoginRequired = errors.New("login required")

	// detailIDRE 从详情页链接里取影片 ID。
	detailIDRE = regexp.MustCompile(`/voddetail/(\d+)/?`)

	// passwordRE 兜底解析"提取码：xxxx"这类写在标题或正文里的密码。
	passwordRE = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|password)\s*[:：=]?\s*([0-9A-Za-z]{3,12})`)

	// panHostTypes 把链接域名映射到 PanSou 的网盘类型标识。
	// 蜗牛主推 115，其次百度，但列表里也会出现夸克/UC/阿里等，一并识别。
	panHostTypes = []struct {
		needle string
		typ    string
	}{
		{"115cdn.com", "115"},
		{"115.com", "115"},
		{"anxia.com", "115"},
		{"pan.baidu.com", "baidu"},
		{"pan.quark.cn", "quark"},
		{"drive.uc.cn", "uc"},
		{"aliyundrive.com", "aliyun"},
		{"alipan.com", "aliyun"},
		{"guangyapan.com", "guangya"},
		{"pan.xunlei.com", "xunlei"},
		{"cloud.189.cn", "tianyi"},
		{"123684.com", "123"},
		{"123685.com", "123"},
		{"123912.com", "123"},
		{"123pan.com", "123"},
		{"123pan.cn", "123"},
		{"123592.com", "123"},
		{"caiyun.139.com", "mobile"},
		{"mypikpak.com", "pikpak"},
		{"share.weiyun.com", "weiyun"},
		{"lanzou", "lanzou"},
	}
)

var (
	_ plugin.PluginWithWebHandler = (*WoniuPlugin)(nil)
	_ plugin.InitializablePlugin  = (*WoniuPlugin)(nil)
)

// WoniuPlugin 蜗牛搜索插件。
type WoniuPlugin struct {
	*plugin.BaseAsyncPlugin
	users       sync.Map // hash -> *User
	mu          sync.RWMutex
	config      PluginConfig
	configMu    sync.RWMutex
	initialized bool
	client      *http.Client
}

// User 是一个被管理的蜗牛账号。Cookie 是唯一真正影响抓取结果的字段。
type User struct {
	Hash         string    `json:"hash"`
	Username     string    `json:"username"`
	Cookie       string    `json:"cookie"`
	Status       string    `json:"status"` // pending / active / expired
	CreatedAt    time.Time `json:"created_at"`
	LoginAt      time.Time `json:"login_at"`
	ExpireAt     time.Time `json:"expire_at"`
	LastAccessAt time.Time `json:"last_access_at"`
	LastCheckAt  time.Time `json:"last_check_at"`
	LastError    string    `json:"last_error,omitempty"`
}

// PluginConfig 是插件级配置（不区分账号）。
type PluginConfig struct {
	BaseURL string `json:"base_url"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewWoniuPlugin())
}

// NewWoniuPlugin 创建蜗牛插件。
//
// 优先级取 2：站点资源以 115 分享为主、更新及时，但链接要登录才可见，
// 没配账号时插件会静默返回空结果，所以不给 1。
func NewWoniuPlugin() *WoniuPlugin {
	return &WoniuPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 2),
		config:          PluginConfig{BaseURL: DefaultBaseURL},
	}
}

func (p *WoniuPlugin) DisplayName() string { return DisplayName }
func (p *WoniuPlugin) Description() string { return Description }

// Initialize 实现 InitializablePlugin：准备存储目录、恢复账号、拉起后台保活。
func (p *WoniuPlugin) Initialize() error {
	if p.initialized {
		return nil
	}

	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	storageDir = filepath.Join(cachePath, "woniu_users")

	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return fmt.Errorf("创建存储目录失败: %w", err)
	}
	if err := p.loadConfig(); err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	p.loadAllUsers()
	p.initialized = true

	// 站点 Cookie 有效期约 30 天，但被顶号/改密会提前失效。低频校验一次，
	// 避免每个请求都去撞 /user/index.html。
	go p.startSessionKeepAlive()

	return nil
}

// RegisterWebRoutes 注册管理路由，统一挂在 /woniu 前缀下，与 panlian/qqpd 等一致。
func (p *WoniuPlugin) RegisterWebRoutes(router *gin.RouterGroup) {
	group := router.Group("/woniu")
	group.GET("/:param", p.handleManagePage)
	group.POST("/:param", p.handleManagePagePOST)
}

// ============================================================
// 搜索入口
// ============================================================

func (p *WoniuPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *WoniuPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *WoniuPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = p.httpClient()
	}

	// 从搜索开始算发布预算：框架到点就把已发布的结果交出去，超时的整批丢弃。
	deadline := time.Now().Add(plugin.PublishBudget())

	user := p.pickUser()
	if user == nil {
		// 没有账号时详情页只会返回"已锁定"的占位链接。与其把一堆假链接交给
		// 调用方，不如明确报错——部署方看到这条日志就知道该去配账号了。
		return nil, fmt.Errorf("[%s] 未配置可用账号：站点详情页需登录才能查看网盘链接，请在 /woniu/{用户名} 登录后再试", p.Name())
	}

	items, err := p.fetchSearch(client, user, keyword)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}
	if len(items) > MaxSearchResults {
		items = items[:MaxSearchResults]
	}

	results := p.fetchDetails(client, user, items, deadline)
	if len(results) == 0 {
		// 搜索页列出了影片却一条详情都没解析出链接：要么会话失效，要么站点改版。
		// 不静默返回 0 条，否则和"关键词确实没资源"完全分不清。
		return nil, fmt.Errorf("[%s] %d 个搜索结果均未取到网盘链接，可能是登录态失效或站点改版", p.Name(), len(items))
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

// fetchDetails 并发抓取详情页。到点就停止派发新请求，把已拿到的先交出去。
func (p *WoniuPlugin) fetchDetails(client *http.Client, user *User, items []searchItem, deadline time.Time) []model.SearchResult {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		results  = make([]model.SearchResult, 0, len(items))
		failed   int
		firstErr error
		skipped  int
	)
	sem := make(chan struct{}, MaxConcurrentDetails)

	for _, item := range items {
		if time.Now().After(deadline) {
			skipped++
			continue
		}
		wg.Add(1)
		go func(it searchItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := p.fetchDetail(client, user, it)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			results = append(results, result)
		}(item)
	}
	wg.Wait()

	if skipped > 0 && len(results) == 0 {
		// 一条都没赶在预算内，值得记一笔：多半是站点变慢而不是没资源。
		fmt.Printf("[%s] %d 个详情页因超出发布预算被跳过，未取得结果\n", p.Name(), skipped)
	}
	if failed > 0 {
		fmt.Printf("[%s] %d/%d 个详情页抓取失败，首个原因: %v\n", p.Name(), failed, len(items), firstErr)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Title < results[j].Title
	})
	return results
}

// searchItem 是搜索结果页上的一张影片卡片。
type searchItem struct {
	ID    string
	Title string
	URL   string
	Pic   string
	Note  string
	Meta  string
}

// fetchSearch 请求搜索页。站点搜索对游客开放，但详情页不开，所以这里照样带 Cookie，
// 让站点侧的行为（以及将来可能的个性化排序）与浏览器一致。
func (p *WoniuPlugin) fetchSearch(client *http.Client, user *User, keyword string) ([]searchItem, error) {
	searchURL := p.getBaseURL() + "/vodsearch/-------------/?wd=" + url.QueryEscape(keyword)

	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造搜索请求失败: %w", err)
	}
	p.setHeaders(req, p.getBaseURL()+"/", cookieOf(user))

	doc, err := p.loadDocument(ctx, client, req)
	if err != nil {
		return nil, err
	}
	return parseSearchItems(doc, p.getBaseURL()), nil
}

// fetchDetail 抓取详情页并解析网盘链接。
func (p *WoniuPlugin) fetchDetail(client *http.Client, user *User, item searchItem) (model.SearchResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil)
	if err != nil {
		return model.SearchResult{}, fmt.Errorf("构造详情请求失败: %w", err)
	}
	p.setHeaders(req, p.getBaseURL()+"/", cookieOf(user))

	doc, err := p.loadDocument(ctx, client, req)
	if err != nil {
		return model.SearchResult{}, err
	}

	if isLockedPage(doc) {
		return model.SearchResult{}, errLoginRequired
	}

	links := parsePanLinks(doc, item.Title)
	if len(links) == 0 {
		return model.SearchResult{}, fmt.Errorf("详情页无可用网盘链接")
	}
	if len(links) > MaxLinksPerResult {
		links = links[:MaxLinksPerResult]
	}

	title := firstNonEmpty(strings.TrimSpace(doc.Find("h1.premium-title").First().Text()), item.Title)
	result := model.SearchResult{
		UniqueID:  fmt.Sprintf("%s-%s", p.Name(), item.ID),
		MessageID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Channel:   "",
		Title:     title,
		Content:   buildContent(doc, item),
		Links:     links,
		Datetime:  parseDetailTime(doc),
	}
	if pic := firstNonEmpty(item.Pic, attrOr(doc.Find(".premium-poster img").First(), "src")); pic != "" {
		result.Images = []string{pic}
	}
	if tags := parseTags(doc); len(tags) > 0 {
		result.Tags = tags
	}
	return result, nil
}

// loadDocument 发一次请求并把响应体解析成文档。
//
// 不依赖调用方 client 的重定向策略（部署里可能被设成不跟随），遇到 3xx 就自己按
// Location 再取一次；状态码原样带进错误文本，排查时能直接看出是超时还是被拒。
func (p *WoniuPlugin) loadDocument(ctx context.Context, client *http.Client, req *http.Request) (*goquery.Document, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if isRedirectStatus(resp.StatusCode) {
		location := strings.TrimSpace(resp.Header.Get("Location"))
		resp.Body.Close()
		if location == "" {
			return nil, fmt.Errorf("HTTP %d 但响应缺少 Location", resp.StatusCode)
		}
		next, err := http.NewRequestWithContext(ctx, http.MethodGet, resolveLocation(req.URL, location), nil)
		if err != nil {
			return nil, err
		}
		p.setHeaders(next, p.getBaseURL()+"/", req.Header.Get("Cookie"))
		if resp, err = client.Do(next); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxResponseBytes))
}

// ============================================================
// HTML 解析
// ============================================================

// parseSearchItems 解析搜索结果页的影片卡片。
func parseSearchItems(doc *goquery.Document, baseURL string) []searchItem {
	items := make([]searchItem, 0, MaxSearchResults)
	seen := make(map[string]struct{})

	doc.Find("a.video-card").Each(func(_ int, s *goquery.Selection) {
		href := strings.TrimSpace(s.AttrOr("href", ""))
		match := detailIDRE.FindStringSubmatch(href)
		if len(match) < 2 {
			return
		}
		id := match[1]
		if _, ok := seen[id]; ok {
			return
		}
		title := firstNonEmpty(strings.TrimSpace(s.AttrOr("title", "")),
			strings.TrimSpace(s.Find(".video-title").First().Text()))
		if title == "" {
			return
		}
		seen[id] = struct{}{}
		items = append(items, searchItem{
			ID:    id,
			Title: title,
			URL:   absoluteURL(baseURL, href),
			Pic:   absoluteURL(baseURL, strings.TrimSpace(s.Find(".video-poster img").First().AttrOr("src", ""))),
			Note:  strings.TrimSpace(s.Find(".video-episode").First().Text()),
			Meta:  cleanText(s.Find(".video-meta").First().Text()),
		})
	})
	return items
}

// isLockedPage 判断详情页是不是"未登录"的锁定视图。
func isLockedPage(doc *goquery.Document) bool {
	if doc.Find(".pan-lock-box").Length() > 0 {
		return true
	}
	// 兜底：锁定态把链接写成 https://******（登录后可见）。
	text := doc.Text()
	return strings.Contains(text, "登录后可见") && strings.Contains(text, "******")
}

// composeWorkTitle 把影片名与站点给的资源备注拼成链接的 WorkTitle。
//
// 这里必须带上影片名：Service 层合并链接（mergeResultsByType）在 WorkTitle 非空时
// 就只拿它做关键词过滤，不再回退到 result.Title。而蜗牛的资源备注经常只是规格描述
// ——实测"庆余年"的备注是 "S02 4K WEB-DL DV HiveWeb-173.26GB"，一个"庆余年"都没有，
// 于是两条链接全被过滤掉，表现为"results 里有结果、merged_by_type 却是空的"。
// 带上影片名后，关键词过滤和"同一影片多条线路的区分"两件事都成立。
func composeWorkTitle(movieTitle, note string) string {
	movieTitle = strings.TrimSpace(movieTitle)
	note = strings.TrimSpace(note)

	switch {
	case movieTitle == "":
		return note
	case note == "" || note == movieTitle:
		return movieTitle
	case strings.Contains(note, movieTitle):
		return note
	default:
		return movieTitle + " " + note
	}
}

// parsePanLinks 从详情页的"网盘资源"面板解析链接。
//
// 站点结构（panlian_dark 模板）：
//
//	<div class="pan-group">
//	  <div class="pan-group-title">115离线下载</div>
//	  <div class="pan-link-item" data-pan-item>
//	    <div class="pan-link-title">影片名 4K</div>
//	    <div class="pan-link-meta">https://115cdn.com/s/xxx?password=abcd</div>
//	  </div>
//	</div>
//
// 链接可能落在 data-pan-item 属性、pan-link-meta 文本或"打开"按钮的 href 上，
// 三处都取一遍再统一校验，避免站点小改一处就整批抓不到。
//
// workTitle 是影片名，用来兜住链接的 WorkTitle（见 composeWorkTitle）。
func parsePanLinks(doc *goquery.Document, workTitle string) []model.Link {
	links := make([]model.Link, 0, 8)
	seen := make(map[string]struct{})

	doc.Find(".pan-link-item").Each(func(_ int, item *goquery.Selection) {
		note := cleanText(firstNonEmpty(
			item.Find(".pan-link-title").First().Text(),
			item.Find(".pan-link-name").First().Text(),
		))
		title := composeWorkTitle(workTitle, note)

		candidates := make([]string, 0, 4)
		if raw := strings.TrimSpace(item.AttrOr("data-pan-item", "")); raw != "" {
			candidates = append(candidates, raw)
		}
		if href := strings.TrimSpace(item.Find("a.pan-link-btn").First().AttrOr("href", "")); href != "" {
			candidates = append(candidates, href)
		}
		if copyText := strings.TrimSpace(item.Find("[data-copy]").First().AttrOr("data-copy", "")); copyText != "" {
			candidates = append(candidates, copyText)
		}
		item.Find(".pan-link-meta").Each(func(_ int, meta *goquery.Selection) {
			candidates = append(candidates, strings.TrimSpace(meta.Text()))
		})

		for _, candidate := range candidates {
			link, ok := normalizePanLink(candidate, title)
			if !ok {
				continue
			}
			if _, dup := seen[link.URL]; dup {
				continue
			}
			seen[link.URL] = struct{}{}
			links = append(links, link)
			break // 一条记录只取一个链接，避免同一链接被三处来源重复计入
		}
	})

	return links
}

// normalizePanLink 把一个候选字符串规整成合法的 model.Link。
func normalizePanLink(raw, workTitle string) (model.Link, bool) {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" || strings.Contains(raw, "******") {
		return model.Link{}, false
	}
	// 链接后面常跟着中文说明，按空白/引号截断。
	if idx := strings.IndexAny(raw, " \t\r\n\"'<>"); idx > 0 {
		raw = raw[:idx]
	}
	raw = strings.TrimRight(raw, ".,;，。；")

	linkType, ok := detectPanType(raw)
	if !ok {
		return model.Link{}, false
	}

	// 密码优先取 URL 查询参数，其次从原串里找"提取码：xxxx"。
	password := ""
	if parsed, err := url.Parse(raw); err == nil {
		query := parsed.Query()
		for _, key := range []string{"pwd", "password", "passcode", "code"} {
			if value := strings.TrimSpace(query.Get(key)); value != "" {
				password = value
				break
			}
		}
	}
	if password == "" {
		if match := passwordRE.FindStringSubmatch(raw); len(match) > 1 {
			password = match[1]
		}
	}

	return model.Link{
		Type:      linkType,
		URL:       raw,
		Password:  password,
		WorkTitle: workTitle,
	}, true
}

// detectPanType 按域名识别网盘类型；无法识别时只接受明确可用的分享链接。
func detectPanType(raw string) (string, bool) {
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "magnet:?xt=urn:btih:") {
		return "magnet", true
	}
	if strings.HasPrefix(lower, "ed2k://") {
		return "ed2k", true
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", false
	}
	for _, entry := range panHostTypes {
		if strings.Contains(lower, entry.needle) {
			return entry.typ, true
		}
	}
	// 站点偶尔会挂站外链接，保留但标为 others，交由上层决定是否展示。
	return "others", true
}

// buildContent 组装结果描述：简介 + 备注 + 分类信息。
func buildContent(doc *goquery.Document, item searchItem) string {
	parts := make([]string, 0, 3)

	if item.Note != "" {
		parts = append(parts, "【"+item.Note+"】")
	}
	if item.Meta != "" {
		parts = append(parts, item.Meta)
	}

	plot := cleanText(doc.Find("#detailDescText").First().Text())
	if plot == "" {
		plot = cleanText(doc.Find(".detail-desc-text").First().Text())
	}
	if plot != "" && plot != "暂无简介" {
		if len([]rune(plot)) > 300 {
			plot = string([]rune(plot)[:300]) + "..."
		}
		parts = append(parts, plot)
	}

	return strings.Join(parts, " ")
}

// parseTags 取详情页顶部的年份/地区/类型标签。
func parseTags(doc *goquery.Document) []string {
	tags := make([]string, 0, 6)
	doc.Find(".premium-tags-top .p-tag").Each(func(_ int, s *goquery.Selection) {
		text := cleanText(s.Text())
		if text == "" {
			return
		}
		// 评分标签形如 "★ 8.0"，当标签用没有意义。
		if strings.HasPrefix(text, "★") {
			return
		}
		tags = append(tags, text)
	})
	return tags
}

// parseDetailTime 解析"更新时间"。
func parseDetailTime(doc *goquery.Document) time.Time {
	var parsed time.Time
	doc.Find(".premium-meta-grid .meta-item").Each(func(_ int, s *goquery.Selection) {
		if !parsed.IsZero() {
			return
		}
		label := cleanText(s.Find(".m-label").First().Text())
		if !strings.Contains(label, "更新时间") && !strings.Contains(label, "时间") {
			return
		}
		value := cleanText(s.Find(".m-val").First().Text())
		for _, layout := range []string{"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
				parsed = t
				return
			}
		}
	})
	return parsed
}

// ============================================================
// 账号管理
// ============================================================

// doLogin 用账号密码换取会话 Cookie。
//
// 站点登录是 MacCMS 的 /user/login.html：表单字段 user_name / user_pwd，
// 成功返回 {"code":1,...}，失败返回 {"code":1003,"msg":"获取用户信息失败"}。
// 该响应是纯 JSON，Cookie 通过 Set-Cookie 下发，所以这里用独立 jar 收 Cookie。
func (p *WoniuPlugin) doLogin(username, password string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("账号和密码不能为空")
	}

	jar := newCookieJar()
	client := p.newHTTPClient(LoginTimeout, jar)

	form := url.Values{}
	form.Set("user_name", username)
	form.Set("user_pwd", password)

	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.getBaseURL()+"/user/login.html", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造登录请求失败: %w", err)
	}
	p.setHeaders(req, p.getBaseURL()+"/user/login/", "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("登录请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("读取登录响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("登录请求失败: HTTP %d", resp.StatusCode)
	}

	var loginResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return nil, fmt.Errorf("解析登录响应失败: %w", err)
	}
	if loginResp.Code != 1 {
		message := strings.TrimSpace(loginResp.Msg)
		if message == "" {
			message = "登录失败"
		}
		return nil, errors.New(message)
	}

	cookie := jarCookieString(jar, p.getBaseURL())
	if cookie == "" {
		return nil, errors.New("登录成功但未获取到有效 Cookie")
	}

	now := time.Now()
	return &User{
		Hash:         p.generateHash(username),
		Username:     username,
		Cookie:       cookie,
		Status:       "active",
		CreatedAt:    now,
		LoginAt:      now,
		ExpireAt:     cookieExpiry(jar, p.getBaseURL(), now),
		LastAccessAt: now,
		LastCheckAt:  now,
	}, nil
}

// verifyUser 检查 Cookie 是否仍然有效。
//
// 站点对未登录访问 /user/index.html 会 302 到 /user/login.html，登录态则是 200，
// 所以"最终是否被重定向到登录页"就是最可靠的判据，比解析页面文案稳。
func (p *WoniuPlugin) verifyUser(user *User) bool {
	if user == nil || user.Cookie == "" {
		return false
	}
	client := p.newHTTPClient(RequestTimeout, nil)

	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.getBaseURL()+"/user/index.html", nil)
	if err != nil {
		return false
	}
	p.setHeaders(req, p.getBaseURL()+"/", user.Cookie)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	// 跟随重定向的 client 会直接落到登录页，用最终 URL 判断。
	if resp.Request != nil && resp.Request.URL != nil &&
		strings.Contains(resp.Request.URL.Path, "/user/login") {
		return false
	}
	if isRedirectStatus(resp.StatusCode) {
		return !strings.Contains(resp.Header.Get("Location"), "/user/login")
	}
	return resp.StatusCode == http.StatusOK
}

func (p *WoniuPlugin) logout(user *User) {
	if user == nil || user.Cookie == "" {
		return
	}
	client := p.newHTTPClient(RequestTimeout, nil)
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.getBaseURL()+"/user/logout.html", nil)
	if err != nil {
		return
	}
	p.setHeaders(req, p.getBaseURL()+"/", user.Cookie)
	if resp, err := client.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
	}
}

// pickUser 选一个可用账号。按最近使用时间排序，让请求尽量落在同一个账号上，
// 既省去重复校验，也避免多账号轮流触发站点的异地登录风控。
func (p *WoniuPlugin) pickUser() *User {
	users := p.getActiveUsers()
	if len(users) == 0 {
		return nil
	}
	return users[0]
}

func (p *WoniuPlugin) getUserByHash(hash string) (*User, bool) {
	value, ok := p.users.Load(hash)
	if !ok {
		return nil, false
	}
	return value.(*User), true
}

func (p *WoniuPlugin) getActiveUsers() []*User {
	users := make([]*User, 0)
	p.users.Range(func(_, value interface{}) bool {
		user := value.(*User)
		if user.Status == "active" && user.Cookie != "" {
			users = append(users, user)
		}
		return true
	})
	sort.Slice(users, func(i, j int) bool {
		return users[i].LastAccessAt.After(users[j].LastAccessAt)
	})
	return users
}

func (p *WoniuPlugin) saveUser(user *User) error {
	p.users.Store(user.Hash, user)
	data, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(storageDir, user.Hash+".json"), data, 0o600)
}

// saveUserOrLog 用于请求路径上的落盘：失败只记日志，不影响本次响应。
func (p *WoniuPlugin) saveUserOrLog(user *User) {
	if err := p.saveUser(user); err != nil {
		fmt.Printf("[%s] 保存账号 %s 失败: %v\n", PluginName, user.Hash, err)
	}
}

func (p *WoniuPlugin) deleteUser(hash string) error {
	p.users.Delete(hash)
	err := os.Remove(filepath.Join(storageDir, hash+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (p *WoniuPlugin) loadAllUsers() {
	entries, err := os.ReadDir(storageDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() == ConfigFileName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(storageDir, entry.Name()))
		if err != nil {
			continue
		}
		var user User
		if err := json.Unmarshal(data, &user); err != nil {
			continue
		}
		if user.Hash == "" {
			continue
		}
		p.users.Store(user.Hash, &user)
	}
}

func (p *WoniuPlugin) configPath() string {
	return filepath.Join(storageDir, ConfigFileName)
}

func (p *WoniuPlugin) loadConfig() error {
	data, err := os.ReadFile(p.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg PluginConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	p.configMu.Lock()
	defer p.configMu.Unlock()
	if strings.TrimSpace(cfg.BaseURL) != "" {
		p.config.BaseURL = normalizeBaseURL(cfg.BaseURL)
	}
	return nil
}

func (p *WoniuPlugin) saveConfig() error {
	p.configMu.RLock()
	cfg := p.config
	p.configMu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.configPath(), data, 0o644)
}

// getBaseURL 返回当前站点地址（去掉尾部斜杠）。
func (p *WoniuPlugin) getBaseURL() string {
	p.configMu.RLock()
	defer p.configMu.RUnlock()
	if strings.TrimSpace(p.config.BaseURL) == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(p.config.BaseURL, "/")
}

func (p *WoniuPlugin) updateBaseURL(raw string) (string, error) {
	normalized := normalizeBaseURL(raw)
	if normalized == "" {
		return "", errors.New("站点地址不能为空")
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return "", errors.New("站点地址格式不正确，请填写完整地址，例如 https://wn4k.com")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("站点地址必须以 http:// 或 https:// 开头")
	}

	p.configMu.Lock()
	p.config.BaseURL = normalized
	p.configMu.Unlock()

	if err := p.saveConfig(); err != nil {
		return "", err
	}
	// 换站后旧 Cookie 不再适用，全部标为待重新登录。
	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		user.Status = "expired"
		user.LastError = "站点地址已变更，请重新登录"
		p.saveUserOrLog(user)
		return true
	})
	return normalized, nil
}

func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	return strings.TrimRight(raw, "/")
}

// ============================================================
// 后台保活
// ============================================================

// startSessionKeepAlive 定期校验账号 Cookie。
//
// 站点 Cookie 有效期约 30 天，但被顶号、改密或站点清理会话都会提前失效。
// 每小时抽查一次，发现失效就标记，管理页上能直接看到，不必等到搜索报错。
func (p *WoniuPlugin) startSessionKeepAlive() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// 启动后先等一会儿再查，避免和站点冷启动/首轮搜索抢资源。
	time.Sleep(2 * time.Minute)

	for {
		p.checkAllSessions()
		<-ticker.C
	}
}

func (p *WoniuPlugin) checkAllSessions() {
	p.users.Range(func(_, value interface{}) bool {
		user := value.(*User)
		if user.Cookie == "" {
			return true
		}
		// 刚登录过的账号短期内没必要重复校验。
		if !user.LastCheckAt.IsZero() && time.Since(user.LastCheckAt) < 30*time.Minute {
			return true
		}
		ok := p.verifyUser(user)
		user.LastCheckAt = time.Now()
		if ok {
			if user.Status != "active" {
				user.Status = "active"
				user.LastError = ""
			}
		} else {
			user.Status = "expired"
			user.LastError = "登录态已失效，请重新登录"
		}
		p.saveUserOrLog(user)
		return true
	})
}

// ============================================================
// HTTP 辅助
// ============================================================

func (p *WoniuPlugin) httpClient() *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		p.client = p.newHTTPClient(RequestTimeout, nil)
	}
	return p.client
}

func (p *WoniuPlugin) newHTTPClient(timeout time.Duration, jar http.CookieJar) *http.Client {
	transport := &http.Transport{
		Proxy:               util.ProxyFuncForTransport(),
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		Jar:       jar,
	}
}

func (p *WoniuPlugin) setHeaders(req *http.Request, referer, cookie string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func cookieOf(user *User) string {
	if user == nil {
		return ""
	}
	return user.Cookie
}

func newCookieJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	return jar
}

// jarCookieString 把 jar 里的 Cookie 拼成请求头字符串。
func jarCookieString(jar http.CookieJar, rawBaseURL string) string {
	if jar == nil {
		return ""
	}
	parsed, err := url.Parse(rawBaseURL)
	if err != nil {
		return ""
	}
	cookies := jar.Cookies(parsed)
	if len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || cookie.Name == "" || cookie.Value == "" {
			continue
		}
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// cookieExpiry 取会话里最晚的过期时间；站点不返回 Expires 时按 30 天估算。
func cookieExpiry(jar http.CookieJar, rawBaseURL string, now time.Time) time.Time {
	if jar != nil {
		if parsed, err := url.Parse(rawBaseURL); err == nil {
			latest := time.Time{}
			for _, cookie := range jar.Cookies(parsed) {
				if cookie != nil && !cookie.Expires.IsZero() && cookie.Expires.After(latest) {
					latest = cookie.Expires
				}
			}
			if !latest.IsZero() {
				return latest
			}
		}
	}
	return now.Add(30 * 24 * time.Hour)
}

// ============================================================
// Web 管理接口
// ============================================================

func (p *WoniuPlugin) handleManagePage(c *gin.Context) {
	identifier := c.Param("param")
	if identifier == "" {
		c.String(http.StatusBadRequest, "缺少参数")
		return
	}
	if p.isHexString(identifier) && len(identifier) == 64 {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"hash": identifier}})
		return
	}
	c.Redirect(http.StatusFound, "/woniu/"+p.generateHash(identifier))
}

func (p *WoniuPlugin) handleManagePagePOST(c *gin.Context) {
	hash := c.Param("param")
	if hash == "" || !p.isHexString(hash) {
		respondError(c, "无效的hash参数")
		return
	}

	var reqData map[string]interface{}
	if err := c.ShouldBindJSON(&reqData); err != nil {
		respondError(c, "请求参数格式错误")
		return
	}

	action, _ := reqData["action"].(string)
	if action == "" {
		respondError(c, "缺少action字段")
		return
	}

	switch action {
	case "get_status":
		p.handleGetStatus(c, hash)
	case "login":
		p.handleLogin(c, hash, reqData)
	case "logout":
		p.handleLogout(c, hash)
	case "get_config":
		p.handleGetConfig(c)
	case "update_config":
		p.handleUpdateConfig(c, reqData)
	case "test_search":
		p.handleTestSearch(c, hash, reqData)
	default:
		respondError(c, "未知的操作类型: "+action)
	}
}

func (p *WoniuPlugin) handleGetStatus(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		user = &User{
			Hash:         hash,
			Status:       "pending",
			CreatedAt:    time.Now(),
			LastAccessAt: time.Now(),
		}
		p.saveUserOrLog(user)
	} else {
		user.LastAccessAt = time.Now()
		p.saveUserOrLog(user)
	}

	expiresInDays := 0
	if !user.ExpireAt.IsZero() {
		expiresInDays = int(time.Until(user.ExpireAt).Hours() / 24)
		if expiresInDays < 0 {
			expiresInDays = 0
		}
	}

	respondSuccess(c, "获取成功", gin.H{
		"hash":            hash,
		"logged_in":       user.Status == "active" && user.Cookie != "",
		"status":          user.Status,
		"username":        user.Username,
		"login_time":      formatTime(user.LoginAt),
		"expire_time":     formatTime(user.ExpireAt),
		"expires_in_days": expiresInDays,
		"last_error":      user.LastError,
		"account_count":   len(p.getActiveUsers()),
	})
}

func (p *WoniuPlugin) handleLogin(c *gin.Context, hash string, reqData map[string]interface{}) {
	username, _ := reqData["username"].(string)
	password, _ := reqData["password"].(string)
	if strings.TrimSpace(username) == "" || password == "" {
		respondError(c, "缺少用户名或密码")
		return
	}

	user, err := p.doLogin(username, password)
	if err != nil {
		respondError(c, "登录失败: "+err.Error())
		return
	}

	// 同一用户名重复登录时沿用同一个 hash，覆盖旧会话即可，避免管理页堆出一串僵尸账号。
	user.Hash = hash
	user.LastError = ""
	if err := p.saveUser(user); err != nil {
		respondError(c, "保存登录状态失败: "+err.Error())
		return
	}

	respondSuccess(c, "登录成功", gin.H{
		"hash":       hash,
		"username":   user.Username,
		"status":     user.Status,
		"login_time": formatTime(user.LoginAt),
	})
}

func (p *WoniuPlugin) handleLogout(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "账号不存在")
		return
	}

	// 先通知站点注销，再清本地；站点侧失败也不该拦住本地退出。
	p.logout(user)

	if err := p.deleteUser(hash); err != nil {
		respondError(c, "删除账号失败: "+err.Error())
		return
	}
	respondSuccess(c, "已退出登录", gin.H{"status": "pending"})
}

func (p *WoniuPlugin) handleGetConfig(c *gin.Context) {
	respondSuccess(c, "获取成功", gin.H{
		"base_url":    p.getBaseURL(),
		"default_url": DefaultBaseURL,
	})
}

func (p *WoniuPlugin) handleUpdateConfig(c *gin.Context, reqData map[string]interface{}) {
	baseURL, _ := reqData["base_url"].(string)
	normalized, err := p.updateBaseURL(baseURL)
	if err != nil {
		respondError(c, err.Error())
		return
	}
	respondSuccess(c, "站点地址已更新", gin.H{"base_url": normalized})
}

func (p *WoniuPlugin) handleTestSearch(c *gin.Context, hash string, reqData map[string]interface{}) {
	keyword, _ := reqData["keyword"].(string)
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		respondError(c, "缺少搜索关键词")
		return
	}

	user, exists := p.getUserByHash(hash)
	if !exists || user.Status != "active" || user.Cookie == "" {
		respondError(c, "请先登录账号")
		return
	}

	results, err := p.searchImpl(p.httpClient(), keyword, map[string]interface{}{})
	if err != nil {
		respondError(c, "搜索失败: "+err.Error())
		return
	}

	totalLinks := 0
	for _, result := range results {
		totalLinks += len(result.Links)
	}
	respondSuccess(c, "搜索成功", gin.H{
		"keyword":       keyword,
		"total_results": len(results),
		"total_links":   totalLinks,
		"results":       results,
	})
}

// ============================================================
// 通用辅助
// ============================================================

func (p *WoniuPlugin) generateHash(username string) string {
	salt := os.Getenv("WONIU_HASH_SALT")
	if salt == "" {
		salt = "pansou_woniu_secret_2026"
	}
	sum := sha256.Sum256([]byte(username + salt))
	return hex.EncodeToString(sum[:])
}

func (p *WoniuPlugin) isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func resolveLocation(base *url.URL, location string) string {
	parsed, err := url.Parse(location)
	if err != nil {
		return DefaultBaseURL + "/" + strings.TrimPrefix(location, "/")
	}
	if base == nil {
		return parsed.String()
	}
	return base.ResolveReference(parsed).String()
}

func absoluteURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimPrefix(raw, "/")
}

func attrOr(s *goquery.Selection, name string) string {
	if s == nil || s.Length() == 0 {
		return ""
	}
	return strings.TrimSpace(s.AttrOr(name, ""))
}

func cleanText(raw string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func respondSuccess(c *gin.Context, message string, data interface{}) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": message,
		"data":    data,
	})
}

func respondError(c *gin.Context, message string) {
	c.JSON(http.StatusOK, gin.H{
		"success": false,
		"message": message,
		"data":    nil,
	})
}
