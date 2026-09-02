package panlian

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/gin-gonic/gin"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	PluginName        = "panlian"
	DisplayName       = "盘链"
	Description       = "盘链 - 登录后检索影视资源并聚合网盘链接"
	DefaultBaseURL    = "https://pinglian.lol"
	ConfigFileName    = "panlian_config.json"
	RequestTimeout    = 20 * time.Second
	MaxConcurrentJobs = 4
	MaxVideoResults   = 10
	MaxLinksPerResult = 200
)

var (
	storageDir string

	errLoginRequired = errors.New("login required")

	panOrder = map[string]int{
		"quark":   0,
		"uc":      1,
		"baidu":   2,
		"xunlei":  3,
		"123":     4,
		"tianyi":  5,
		"115":     6,
		"aliyun":  7,
		"guangya": 8,
		"mobile":  9,
		"pikpak":  10,
		"magnet":  11,
		"others":  12,
	}

	extractCodeNoiseRegex = regexp.MustCompile(`(?i)([?？]?\s*(提取码|访问码|密码)[:：]\s*[a-z0-9]{4,8})+$`)
	htmlTagRegex          = regexp.MustCompile(`<[^>]+>`)
	pan123ShareHostRegex  = regexp.MustCompile(`(?i)(^|\.)share\.(?:123684\.com|123865\.com|123912\.com|123pan\.com|123pan\.cn|123592\.com)$`)
	panTokenURLRegexes    = []*regexp.Regexp{
		regexp.MustCompile(`(?i)id=["']jumpBtn["'][^>]*href=["']([^"']+)["']`),
		regexp.MustCompile(`(?i)href=["']([^"']+)["'][^>]*id=["']jumpBtn["']`),
		regexp.MustCompile(`(?i)(?:targetUrl|window\.location\.href|location\.href)\s*=\s*["']([^"']+)["']`),
	}
)

const htmlTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>PanSou 盘链配置</title>
  <style>
    * { box-sizing: border-box; }
    body {
      margin: 0;
      padding: 24px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: linear-gradient(135deg, #eef4ff 0%, #f8efe4 100%);
      color: #1f2937;
    }
    .container {
      max-width: 860px;
      margin: 0 auto;
      background: #fff;
      border-radius: 18px;
      box-shadow: 0 18px 48px rgba(15, 23, 42, 0.14);
      overflow: hidden;
    }
    .header {
      padding: 28px 32px;
      background: linear-gradient(135deg, #1d4ed8 0%, #0f766e 100%);
      color: #fff;
    }
    .header h1 { margin: 0 0 8px; font-size: 28px; }
    .header p { margin: 4px 0; opacity: 0.9; }
    .section {
      padding: 28px 32px;
      border-top: 1px solid #e5e7eb;
    }
    .section h2 {
      margin: 0 0 16px;
      font-size: 18px;
    }
    .section-head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
      margin-bottom: 16px;
    }
    .section-head h2 {
      margin: 0;
    }
    .grid {
      display: grid;
      gap: 16px;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    }
    .card {
      border: 1px solid #e5e7eb;
      border-radius: 12px;
      padding: 16px;
      background: #f8fafc;
    }
    label {
      display: block;
      font-weight: 600;
      margin-bottom: 6px;
    }
    input, textarea {
      width: 100%;
      padding: 10px 12px;
      border: 1px solid #cbd5e1;
      border-radius: 10px;
      font-size: 14px;
    }
    textarea {
      min-height: 120px;
      resize: vertical;
    }
    button {
      border: 0;
      border-radius: 10px;
      padding: 10px 16px;
      font-size: 14px;
      cursor: pointer;
      background: #1d4ed8;
      color: #fff;
      transition: opacity 0.2s ease, transform 0.2s ease;
    }
    button:hover { transform: translateY(-1px); }
    button:disabled {
      cursor: not-allowed;
      opacity: 0.75;
      transform: none;
    }
    button.secondary { background: #334155; }
    button.danger { background: #dc2626; }
    .actions {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      margin-top: 12px;
    }
    .mono {
      font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
      font-size: 13px;
      word-break: break-all;
    }
    pre {
      background: #0f172a;
      color: #dbeafe;
      padding: 14px;
      border-radius: 12px;
      overflow: auto;
      font-size: 12px;
      line-height: 1.5;
    }
    .status {
      display: grid;
      gap: 8px;
    }
    .status-item {
      display: flex;
      justify-content: space-between;
      gap: 16px;
      padding: 8px 0;
      border-bottom: 1px dashed #dbe2ea;
    }
    .status-item:last-child { border-bottom: 0; }
    .hidden { display: none; }
    .search-row {
      display: flex;
      gap: 12px;
      align-items: end;
    }
    .search-row .field {
      flex: 1;
    }
    .loading-text {
      display: inline-flex;
      align-items: center;
      gap: 8px;
    }
    .loading-text::before {
      content: "";
      width: 14px;
      height: 14px;
      border-radius: 50%;
      border: 2px solid rgba(255, 255, 255, 0.35);
      border-top-color: #fff;
      animation: spin 0.8s linear infinite;
    }
    @keyframes spin {
      to { transform: rotate(360deg); }
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="header">
      <h1>盘链</h1>
      <p>登录盘链站点后，PanSou 会直接抓取搜索结果和网盘链接。</p>
      <p class="mono">当前管理地址: HASH_PLACEHOLDER</p>
    </div>

    <div class="section hidden" id="statusSection">
      <div class="section-head">
        <h2>状态</h2>
        <button type="button" class="danger" onclick="logoutUser()">退出登录</button>
      </div>
      <div class="card status" id="statusBox"></div>
    </div>

    <div class="section" id="loginSection">
      <h2>登录</h2>
      <div class="grid">
        <div>
          <label for="username">账号</label>
          <input id="username" autocomplete="username">
        </div>
        <div>
          <label for="password">密码</label>
          <input id="password" type="password" autocomplete="current-password">
        </div>
      </div>
      <div class="actions">
        <button type="button" onclick="login()">登录并保存</button>
      </div>
    </div>

    <div class="section">
      <h2>测试搜索</h2>
      <div class="search-row">
        <div class="field">
          <label for="keyword">关键词</label>
          <input id="keyword" placeholder="例如：遮天">
        </div>
        <button type="button" id="testSearchBtn" onclick="testSearch()">搜索测试</button>
      </div>
    </div>

    <div class="section">
      <h2>返回结果</h2>
      <pre id="result">等待操作...</pre>
    </div>
  </div>

  <script>
    const hash = "HASH_PLACEHOLDER";

    async function postAction(action, extra = {}) {
      const resp = await fetch("/panlian/" + hash, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, ...extra })
      });
      return await resp.json();
    }

    function showResult(data) {
      document.getElementById("result").textContent = JSON.stringify(data, null, 2);
    }

    function showError(error) {
      showResult({
        success: false,
        message: error && error.message ? error.message : String(error)
      });
    }

    function setSearchLoading(loading) {
      const btn = document.getElementById("testSearchBtn");
      if (!btn) return;
      btn.disabled = !!loading;
      btn.innerHTML = loading
        ? '<span class="loading-text">搜索中...</span>'
        : '搜索测试';
    }

    function updatePageState(data) {
      const loggedIn = !!(data && data.logged_in);
      document.getElementById("statusSection").classList.toggle("hidden", !loggedIn);
      document.getElementById("loginSection").classList.toggle("hidden", loggedIn);
    }

    function renderStatus(data) {
      const box = document.getElementById("statusBox");
      const rows = [
        ["状态", data.logged_in ? "已登录" : "未登录"],
        ["用户名", data.username || "-"],
        ["登录时间", data.login_time || "-"],
        ["有效期", data.expire_time || "-"],
        ["剩余天数", String(data.expires_in_days || 0)]
      ];
      box.innerHTML = rows.map(([k, v]) => {
        return '<div class="status-item"><span>' + k + '</span><strong>' + v + '</strong></div>';
      }).join("");
      updatePageState(data);
    }

    async function loadStatus() {
      try {
        const result = await postAction("get_status");
        showResult(result);
        if (result.success && result.data) {
          renderStatus(result.data);
        } else {
          updatePageState(null);
        }
      } catch (error) {
        showError(error);
      }
    }

    async function login() {
      try {
        const username = document.getElementById("username").value.trim();
        const password = document.getElementById("password").value;
        const result = await postAction("login", { username, password, remember: true });
        showResult(result);
        if (result.success) {
          document.getElementById("password").value = "";
          await loadStatus();
        }
      } catch (error) {
        showError(error);
      }
    }

    async function logoutUser() {
      try {
        const result = await postAction("logout");
        showResult(result);
        if (result.success) {
          document.getElementById("username").value = "";
          document.getElementById("password").value = "";
          await loadStatus();
        }
      } catch (error) {
        showError(error);
      }
    }

    async function testSearch() {
      const keyword = document.getElementById("keyword").value.trim();
      if (!keyword) {
        showResult({
          success: false,
          message: "请输入搜索关键词"
        });
        return;
      }

      setSearchLoading(true);
      try {
        const result = await postAction("test_search", { keyword });
        showResult(result);
      } catch (error) {
        showError(error);
      } finally {
        setSearchLoading(false);
      }
    }

    window.onload = loadStatus;
  </script>
</body>
</html>`

type PanlianPlugin struct {
	*plugin.BaseAsyncPlugin
	users       sync.Map
	mu          sync.RWMutex
	config      PluginConfig
	initialized bool
}

type PluginConfig struct {
	BlockedPanTypes []string  `json:"blocked_pan_types"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type User struct {
	Hash              string    `json:"hash"`
	Username          string    `json:"username"`
	EncryptedPassword string    `json:"encrypted_password"`
	Cookie            string    `json:"cookie"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	LoginAt           time.Time `json:"login_at"`
	ExpireAt          time.Time `json:"expire_at"`
	LastAccessAt      time.Time `json:"last_access_at"`
}

type LoginResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	User    struct {
		ID         int    `json:"id"`
		Username   string `json:"username"`
		Email      string `json:"email"`
		VIPLevel   int    `json:"vip_level"`
		InviteCode string `json:"invite_code"`
	} `json:"user"`
}

type VideoSearchResponse struct {
	Code      int         `json:"code"`
	Msg       string      `json:"msg"`
	Page      int         `json:"page"`
	PageCount int         `json:"pagecount"`
	Total     int         `json:"total"`
	List      []VideoItem `json:"list"`
}

type VideoItem struct {
	VodID       int    `json:"vod_id"`
	VodName     string `json:"vod_name"`
	VodPic      string `json:"vod_pic"`
	VodRemarks  string `json:"vod_remarks"`
	VodScore    string `json:"vod_score"`
	VodYear     string `json:"vod_year"`
	VodArea     string `json:"vod_area"`
	VodLang     string `json:"vod_lang"`
	TypeName    string `json:"type_name"`
	VodActor    string `json:"vod_actor"`
	VodDirector string `json:"vod_director"`
	VodContent  string `json:"vod_content"`
}

type PanLinkResponse struct {
	Success bool                `json:"success"`
	Message string              `json:"message"`
	Total   int                 `json:"total"`
	Data    map[string]PanGroup `json:"data"`
}

type PanGroup struct {
	Name  string        `json:"name"`
	Icon  string        `json:"icon"`
	Links []PanLinkItem `json:"links"`
}

type PanLinkItem struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	Password string `json:"password"`
	Type     string `json:"type"`
	Time     string `json:"time"`
	Source   string `json:"source"`
	ID       string `json:"id"`
	UserTier string `json:"user_tier"`
}

type resolveTokenResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

var (
	_ plugin.PluginWithWebHandler = (*PanlianPlugin)(nil)
	_ plugin.InitializablePlugin  = (*PanlianPlugin)(nil)
)

func init() {
	plugin.RegisterGlobalPlugin(NewPanlianPlugin())
}

func NewPanlianPlugin() *PanlianPlugin {
	return &PanlianPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 3),
		config: PluginConfig{
			BlockedPanTypes: []string{},
		},
	}
}

func (p *PanlianPlugin) DisplayName() string {
	return DisplayName
}

func (p *PanlianPlugin) Description() string {
	return Description
}

func (p *PanlianPlugin) Initialize() error {
	if p.initialized {
		return nil
	}

	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	storageDir = filepath.Join(cachePath, "panlian_users")

	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		return fmt.Errorf("创建存储目录失败: %w", err)
	}
	if err := p.loadConfig(); err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	p.loadAllUsers()
	p.initialized = true
	return nil
}

func (p *PanlianPlugin) RegisterWebRoutes(router *gin.RouterGroup) {
	group := router.Group("/panlian")
	group.GET("/:param", p.handleManagePage)
	group.POST("/:param", p.handleManagePagePOST)
}

func (p *PanlianPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *PanlianPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *PanlianPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	users := p.getActiveUsers()
	if len(users) == 0 {
		return []model.SearchResult{}, nil
	}

	var lastErr error
	for _, user := range users {
		results, err := p.searchWithUser(client, user, keyword)
		if err != nil {
			lastErr = err
			continue
		}
		filtered := plugin.FilterResultsByKeyword(results, keyword)
		return filtered, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return []model.SearchResult{}, nil
}

func (p *PanlianPlugin) searchWithUser(client *http.Client, user *User, keyword string) ([]model.SearchResult, error) {
	results, err := p.searchOnce(client, user, keyword)
	if err == nil {
		user.LastAccessAt = time.Now()
		_ = p.saveUser(user)
		return results, nil
	}

	if !errors.Is(err, errLoginRequired) {
		return nil, err
	}
	if user.EncryptedPassword == "" || user.Username == "" {
		user.Status = "expired"
		user.Cookie = ""
		_ = p.saveUser(user)
		return nil, err
	}

	if reloginErr := p.reloginUser(user); reloginErr != nil {
		return nil, reloginErr
	}
	return p.searchOnce(client, user, keyword)
}

func (p *PanlianPlugin) searchOnce(client *http.Client, user *User, keyword string) ([]model.SearchResult, error) {
	if client == nil {
		client = p.GetClient()
	}

	videoResp, err := p.fetchVideos(client, user.Cookie, keyword)
	if err != nil {
		return nil, err
	}
	if len(videoResp.List) == 0 {
		return []model.SearchResult{}, nil
	}

	items := videoResp.List
	if len(items) > MaxVideoResults {
		items = items[:MaxVideoResults]
	}

	sem := make(chan struct{}, MaxConcurrentJobs)
	resultsCh := make(chan model.SearchResult, len(items))
	errCh := make(chan error, len(items))
	var wg sync.WaitGroup

	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result, buildErr := p.buildSearchResult(client, user.Cookie, keyword, item)
			if buildErr != nil {
				errCh <- buildErr
				return
			}
			if len(result.Links) == 0 {
				return
			}
			resultsCh <- result
		}()
	}

	wg.Wait()
	close(resultsCh)
	close(errCh)

	results := make([]model.SearchResult, 0, len(items))
	for result := range resultsCh {
		results = append(results, result)
	}

	for err := range errCh {
		if errors.Is(err, errLoginRequired) {
			return nil, err
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Datetime.After(results[j].Datetime)
	})

	return results, nil
}

func (p *PanlianPlugin) buildSearchResult(client *http.Client, cookie string, keyword string, item VideoItem) (model.SearchResult, error) {
	panResp, err := p.fetchPanLinks(client, cookie, keyword, item.VodID)
	if err != nil {
		return model.SearchResult{}, err
	}

	links, summary, latestTime, truncated := p.flattenPanLinks(panResp.Data)
	if len(links) == 0 {
		return model.SearchResult{}, nil
	}

	content := p.buildResultContent(item, summary, truncated)
	if latestTime.IsZero() {
		latestTime = time.Now()
	}

	result := model.SearchResult{
		MessageID: fmt.Sprintf("%d", item.VodID),
		UniqueID:  fmt.Sprintf("%s-%d", PluginName, item.VodID),
		Channel:   "",
		Datetime:  latestTime,
		Title:     strings.TrimSpace(item.VodName),
		Content:   content,
		Links:     links,
		Tags:      compactStrings([]string{item.TypeName, PluginName}),
	}
	if pic := strings.TrimSpace(item.VodPic); pic != "" {
		result.Images = []string{pic}
	}

	return result, nil
}

func (p *PanlianPlugin) buildResultContent(item VideoItem, summary string, truncated bool) string {
	parts := []string{}

	metaLine := strings.Join(compactStrings([]string{
		item.TypeName,
		item.VodRemarks,
		item.VodYear,
		item.VodArea,
		item.VodLang,
	}), " / ")
	if metaLine != "" {
		parts = append(parts, metaLine)
	}
	if actors := strings.TrimSpace(item.VodActor); actors != "" {
		parts = append(parts, "主演: "+actors)
	}
	if director := strings.TrimSpace(item.VodDirector); director != "" {
		parts = append(parts, "导演: "+director)
	}
	if summary != "" {
		parts = append(parts, "网盘汇总: "+summary)
	}
	if truncated {
		parts = append(parts, fmt.Sprintf("链接已截取最新 %d 条", MaxLinksPerResult))
	}
	if desc := sanitizeText(item.VodContent); desc != "" {
		parts = append(parts, desc)
	}

	return strings.Join(parts, "\n")
}

func (p *PanlianPlugin) flattenPanLinks(groups map[string]PanGroup) ([]model.Link, string, time.Time, bool) {
	if len(groups) == 0 {
		return nil, "", time.Time{}, false
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		if isBlockedPanType(key, groups[key].Name, p.getBlockedPanTypes()) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		pi, okI := panOrder[normalizePanTypeName(keys[i])]
		pj, okJ := panOrder[normalizePanTypeName(keys[j])]
		if !okI {
			pi = len(panOrder) + 1
		}
		if !okJ {
			pj = len(panOrder) + 1
		}
		if pi != pj {
			return pi < pj
		}
		return keys[i] < keys[j]
	})

	var latest time.Time
	links := make([]model.Link, 0, 32)
	summaryParts := make([]string, 0, len(keys))
	seen := make(map[string]struct{})
	truncated := false

	for _, key := range keys {
		group := groups[key]
		groupLinks := normalizePanLinks(key, group)
		if len(groupLinks) == 0 {
			continue
		}

		summaryParts = append(summaryParts, fmt.Sprintf("%s%d条", strings.TrimSpace(group.Name), len(groupLinks)))

		for _, item := range groupLinks {
			if len(links) >= MaxLinksPerResult {
				truncated = true
				break
			}
			dedupeKey := item.URL + "@@" + item.Password
			if _, ok := seen[dedupeKey]; ok {
				continue
			}
			seen[dedupeKey] = struct{}{}

			linkTime := parseLinkTime(item.Time)
			if linkTime.After(latest) {
				latest = linkTime
			}

			linkType := normalizeLinkType(item.Type, item.URL)
			links = append(links, model.Link{
				Type:      linkType,
				URL:       normalizePanURL(item.URL, item.Password, linkType),
				Password:  strings.TrimSpace(item.Password),
				Datetime:  linkTime,
				WorkTitle: strings.TrimSpace(item.Title),
			})
		}
		if truncated {
			break
		}
	}

	return links, strings.Join(summaryParts, " / "), latest, truncated
}

func (p *PanlianPlugin) fetchVideos(client *http.Client, cookie string, keyword string) (*VideoSearchResponse, error) {
	values := url.Values{}
	values.Set("wd", keyword)
	values.Set("pg", "1")

	var resp VideoSearchResponse
	if err := p.doJSONGET(client, cookie, "/api/get_videos.php", values, &resp); err != nil {
		return nil, err
	}
	if resp.Code == -1 && strings.Contains(resp.Msg, "登录") {
		return nil, fmt.Errorf("%w: %s", errLoginRequired, resp.Msg)
	}
	if resp.Code != 1 {
		return nil, fmt.Errorf("盘链列表接口异常: %s", resp.Msg)
	}
	return &resp, nil
}

func (p *PanlianPlugin) fetchPanLinks(client *http.Client, cookie string, keyword string, vodID int) (*PanLinkResponse, error) {
	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("vod_id", fmt.Sprintf("%d", vodID))
	values.Set("_t", fmt.Sprintf("%d", time.Now().UnixMilli()))

	var resp PanLinkResponse
	if err := p.doJSONGET(client, cookie, "/api/search_pan_links.php", values, &resp); err != nil {
		return nil, err
	}
	if !resp.Success && strings.Contains(resp.Message, "登录") {
		return nil, fmt.Errorf("%w: %s", errLoginRequired, resp.Message)
	}
	if !resp.Success {
		return nil, fmt.Errorf("盘链网盘接口异常: %s", resp.Message)
	}
	p.resolvePanLinkTokens(client, cookie, resp.Data)
	return &resp, nil
}

func (p *PanlianPlugin) resolvePanLinkTokens(client *http.Client, cookie string, groups map[string]PanGroup) {
	type tokenJob struct {
		groupKey string
		index    int
		token    string
	}
	type tokenResult struct {
		groupKey string
		index    int
		url      string
	}

	jobs := make([]tokenJob, 0)
	for groupKey, group := range groups {
		for index, item := range group.Links {
			candidate := strings.TrimSpace(item.URL)
			if !isRealPanURL(candidate) {
				candidate = strings.TrimSpace(item.Token)
			}
			if candidate != "" && !isRealPanURL(candidate) {
				jobs = append(jobs, tokenJob{groupKey: groupKey, index: index, token: candidate})
			}
		}
	}
	if len(jobs) == 0 {
		return
	}

	resultCh := make(chan tokenResult, len(jobs))
	sem := make(chan struct{}, MaxConcurrentJobs)
	var wg sync.WaitGroup
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resolvedURL, err := p.resolvePanToken(client, cookie, job.token)
			if err == nil && isRealPanURL(resolvedURL) {
				resultCh <- tokenResult{groupKey: job.groupKey, index: job.index, url: resolvedURL}
			}
		}()
	}
	wg.Wait()
	close(resultCh)

	for result := range resultCh {
		group, ok := groups[result.groupKey]
		if !ok || result.index < 0 || result.index >= len(group.Links) {
			continue
		}
		group.Links[result.index].URL = result.url
		groups[result.groupKey] = group
	}
}

func (p *PanlianPlugin) resolvePanToken(client *http.Client, cookie string, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("盘链 token 为空")
	}
	if isRealPanURL(token) {
		return token, nil
	}
	if client == nil {
		client = p.GetClient()
	}
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}

	payload, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DefaultBaseURL+"/api/resolve_token.php", bytes.NewReader(payload))
	if err == nil {
		p.setPanlianHeaders(req, cookie, DefaultBaseURL+"/pages/video.php")
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := client.Do(req)
		if doErr == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				var resolved resolveTokenResponse
				if json.Unmarshal(body, &resolved) == nil && resolved.Success && isRealPanURL(resolved.URL) {
					cancel()
					return strings.TrimSpace(resolved.URL), nil
				}
			}
		}
	}
	cancel()

	goURL := DefaultBaseURL + "/api/go.php?t=" + url.QueryEscape(token)
	ctx, cancel = context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, goURL, nil)
	if err != nil {
		return "", err
	}
	p.setPanlianHeaders(req, cookie, DefaultBaseURL+"/pages/video.php")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	redirectClient := *client
	redirectClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := redirectClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if location := strings.TrimSpace(resp.Header.Get("Location")); isRealPanURL(location) {
		return decodePanURL(location), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for _, pattern := range panTokenURLRegexes {
		match := pattern.FindSubmatch(body)
		if len(match) > 1 {
			resolvedURL := decodePanURL(string(match[1]))
			if isRealPanURL(resolvedURL) {
				return resolvedURL, nil
			}
		}
	}
	return "", fmt.Errorf("盘链 token 未解析出真实链接")
}

func decodePanURL(value string) string {
	value = html.UnescapeString(strings.TrimSpace(value))
	return strings.ReplaceAll(value, `\/`, `/`)
}

func (p *PanlianPlugin) setPanlianHeaders(req *http.Request, cookie string, referer string) {
	req.Header.Set("User-Agent", browserUserAgent())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-TW,zh;q=0.9,zh-CN;q=0.8,en;q=0.7")
	req.Header.Set("Origin", DefaultBaseURL)
	req.Header.Set("Referer", referer)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func (p *PanlianPlugin) doJSONGET(client *http.Client, cookie string, path string, values url.Values, out interface{}) error {
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}

	targetURL := DefaultBaseURL + path
	if values != nil && len(values) > 0 {
		targetURL += "?" + values.Encode()
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("User-Agent", browserUserAgent())
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Language", "zh-TW,zh;q=0.9,zh-CN;q=0.8,en;q=0.7")
		req.Header.Set("Origin", DefaultBaseURL)
		req.Header.Set("Referer", DefaultBaseURL+"/all-videos.php")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			continue
		}
		if err := json.Unmarshal(body, out); err != nil {
			if bytes.Contains(body, []byte("请先登录")) || bytes.Contains(body, []byte("login")) {
				return fmt.Errorf("%w: %s", errLoginRequired, string(body))
			}
			return fmt.Errorf("解析接口响应失败: %w", err)
		}
		return nil
	}

	return lastErr
}

func (p *PanlianPlugin) doLogin(username string, password string, remember bool) (string, *LoginResponse, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", nil, fmt.Errorf("账号和密码不能为空")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: RequestTimeout,
		Jar:     jar,
	}

	// 站点登录只认预先由公开接口建立的 PHPSESSID。
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DefaultBaseURL+"/api/get_types.php", nil)
	if err != nil {
		cancel()
		return "", nil, err
	}
	p.setPanlianHeaders(req, "", DefaultBaseURL+"/all-videos.php")
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return "", nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cancel()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("获取预登录会话失败: HTTP %d", resp.StatusCode)
	}
	baseURL, _ := url.Parse(DefaultBaseURL)
	preCookie := cookiesToString(jar.Cookies(baseURL))

	form := url.Values{}
	form.Set("username", username)
	form.Set("password", password)
	if remember {
		form.Set("remember", "on")
	}

	ctx, cancel = context.WithTimeout(context.Background(), RequestTimeout)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, DefaultBaseURL+"/api/login.php", strings.NewReader(form.Encode()))
	if err != nil {
		cancel()
		return "", nil, err
	}
	p.setPanlianHeaders(req, preCookie, DefaultBaseURL+"/pages/login.php")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	resp, err = client.Do(req)
	if err != nil {
		cancel()
		return "", nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	cancel()
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("登录请求失败: HTTP %d", resp.StatusCode)
	}

	var loginResp LoginResponse
	if err := json.Unmarshal(respBody, &loginResp); err != nil {
		return "", nil, fmt.Errorf("解析登录响应失败: %w", err)
	}
	if !loginResp.Success {
		return "", nil, errors.New(strings.TrimSpace(loginResp.Message))
	}

	cookieString := cookiesToString(jar.Cookies(baseURL))
	if cookieString == "" {
		return "", nil, fmt.Errorf("登录成功但未获取到有效 Cookie")
	}

	return cookieString, &loginResp, nil
}

func (p *PanlianPlugin) reloginUser(user *User) error {
	password, err := p.decryptPassword(user.EncryptedPassword)
	if err != nil {
		return err
	}
	cookie, _, err := p.doLogin(user.Username, password, true)
	if err != nil {
		user.Status = "expired"
		user.Cookie = ""
		_ = p.saveUser(user)
		return err
	}

	user.Cookie = cookie
	user.Status = "active"
	user.LoginAt = time.Now()
	user.ExpireAt = time.Now().Add(30 * 24 * time.Hour)
	user.LastAccessAt = time.Now()
	return p.saveUser(user)
}

func (p *PanlianPlugin) handleManagePage(c *gin.Context) {
	param := c.Param("param")
	if len(param) == 64 && p.isHexString(param) {
		html := strings.ReplaceAll(htmlTemplate, "HASH_PLACEHOLDER", param)
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
		return
	}
	c.Redirect(http.StatusFound, "/panlian/"+p.generateHash(param))
}

func (p *PanlianPlugin) handleManagePagePOST(c *gin.Context) {
	hash := c.Param("param")

	var reqData map[string]interface{}
	if err := c.ShouldBindJSON(&reqData); err != nil {
		respondError(c, "无效的请求格式: "+err.Error())
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
	case "update_config":
		p.handleUpdateConfig(c, reqData)
	case "test_search":
		p.handleTestSearch(c, hash, reqData)
	default:
		respondError(c, "未知的操作类型: "+action)
	}
}

func (p *PanlianPlugin) handleGetStatus(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		user = &User{
			Hash:         hash,
			Status:       "pending",
			CreatedAt:    time.Now(),
			LastAccessAt: time.Now(),
		}
		_ = p.saveUser(user)
	} else {
		user.LastAccessAt = time.Now()
		_ = p.saveUser(user)
	}

	loggedIn := user.Status == "active" && user.Cookie != ""
	expiresInDays := 0
	if !user.ExpireAt.IsZero() {
		expiresInDays = int(time.Until(user.ExpireAt).Hours() / 24)
		if expiresInDays < 0 {
			expiresInDays = 0
		}
	}

	respondSuccess(c, "获取成功", gin.H{
		"hash":              hash,
		"logged_in":         loggedIn,
		"status":            user.Status,
		"username":          user.Username,
		"login_time":        formatTime(user.LoginAt),
		"expire_time":       formatTime(user.ExpireAt),
		"expires_in_days":   expiresInDays,
		"blocked_pan_types": p.getBlockedPanTypes(),
	})
}

func (p *PanlianPlugin) handleLogin(c *gin.Context, hash string, reqData map[string]interface{}) {
	username, _ := reqData["username"].(string)
	password, _ := reqData["password"].(string)
	remember, _ := reqData["remember"].(bool)
	if strings.TrimSpace(username) == "" || password == "" {
		respondError(c, "缺少用户名或密码")
		return
	}

	cookie, loginResp, err := p.doLogin(username, password, remember || !reqDataHasKey(reqData, "remember"))
	if err != nil {
		respondError(c, "登录失败: "+err.Error())
		return
	}

	encryptedPassword, err := p.encryptPassword(password)
	if err != nil {
		respondError(c, "密码加密失败: "+err.Error())
		return
	}

	user, exists := p.getUserByHash(hash)
	if !exists {
		user = &User{
			Hash:      hash,
			CreatedAt: time.Now(),
		}
	}
	user.Username = strings.TrimSpace(username)
	user.EncryptedPassword = encryptedPassword
	user.Cookie = cookie
	user.Status = "active"
	user.LoginAt = time.Now()
	user.ExpireAt = time.Now().Add(30 * 24 * time.Hour)
	user.LastAccessAt = time.Now()

	if err := p.saveUser(user); err != nil {
		respondError(c, "保存登录信息失败: "+err.Error())
		return
	}

	respondSuccess(c, "登录成功", gin.H{
		"username": loginResp.User.Username,
		"status":   "active",
	})
}

func (p *PanlianPlugin) handleLogout(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}
	user.Cookie = ""
	user.Status = "pending"
	user.LastAccessAt = time.Now()
	if err := p.saveUser(user); err != nil {
		respondError(c, "退出失败")
		return
	}
	respondSuccess(c, "已退出登录", gin.H{"status": user.Status})
}

func (p *PanlianPlugin) handleUpdateConfig(c *gin.Context, reqData map[string]interface{}) {
	raw := reqData["blocked_pan_types"]
	if raw == nil {
		raw = reqData["blockedPanTypes"]
	}

	p.mu.Lock()
	p.config.BlockedPanTypes = normalizeBlockedPanTypes(raw)
	p.config.UpdatedAt = time.Now()
	err := p.saveConfigLocked()
	p.mu.Unlock()
	if err != nil {
		respondError(c, "保存配置失败: "+err.Error())
		return
	}

	respondSuccess(c, "配置已保存", gin.H{
		"blocked_pan_types": p.getBlockedPanTypes(),
	})
}

func (p *PanlianPlugin) handleTestSearch(c *gin.Context, hash string, reqData map[string]interface{}) {
	keyword, _ := reqData["keyword"].(string)
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		respondError(c, "缺少keyword字段")
		return
	}

	user, exists := p.getUserByHash(hash)
	if !exists || user.Cookie == "" || user.Status != "active" {
		respondError(c, "请先登录")
		return
	}

	results, err := p.searchWithUser(&http.Client{Timeout: RequestTimeout}, user, keyword)
	if err != nil {
		respondError(c, "测试搜索失败: "+err.Error())
		return
	}

	frontendResults := make([]gin.H, 0, len(results))
	totalLinks := 0
	for _, result := range results {
		links := make([]gin.H, 0, len(result.Links))
		for _, link := range result.Links {
			links = append(links, gin.H{
				"type":       link.Type,
				"url":        link.URL,
				"password":   link.Password,
				"datetime":   formatTime(link.Datetime),
				"work_title": link.WorkTitle,
			})
		}
		totalLinks += len(result.Links)

		frontendResults = append(frontendResults, gin.H{
			"message_id": result.MessageID,
			"unique_id":  result.UniqueID,
			"channel":    result.Channel,
			"title":      result.Title,
			"content":    result.Content,
			"datetime":   formatTime(result.Datetime),
			"tags":       result.Tags,
			"images":     result.Images,
			"link_count": len(result.Links),
			"links":      links,
		})
	}

	respondSuccess(c, fmt.Sprintf("找到 %d 条结果，共 %d 个链接", len(frontendResults), totalLinks), gin.H{
		"keyword":       keyword,
		"total_results": len(frontendResults),
		"total_links":   totalLinks,
		"results":       frontendResults,
	})
}

func (p *PanlianPlugin) getUserByHash(hash string) (*User, bool) {
	value, ok := p.users.Load(hash)
	if !ok {
		return nil, false
	}
	return value.(*User), true
}

func (p *PanlianPlugin) getActiveUsers() []*User {
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

func (p *PanlianPlugin) saveUser(user *User) error {
	p.users.Store(user.Hash, user)
	data, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(storageDir, user.Hash+".json"), data, 0o644)
}

func (p *PanlianPlugin) loadAllUsers() {
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
		p.users.Store(user.Hash, &user)
	}
}

func (p *PanlianPlugin) configPath() string {
	return filepath.Join(storageDir, ConfigFileName)
}

func (p *PanlianPlugin) loadConfig() error {
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
	cfg.BlockedPanTypes = normalizeBlockedPanTypes(cfg.BlockedPanTypes)
	p.mu.Lock()
	p.config = cfg
	p.mu.Unlock()
	return nil
}

func (p *PanlianPlugin) saveConfigLocked() error {
	data, err := json.MarshalIndent(p.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.configPath(), data, 0o644)
}

func (p *PanlianPlugin) getBlockedPanTypes() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]string, len(p.config.BlockedPanTypes))
	copy(result, p.config.BlockedPanTypes)
	return result
}

func normalizeBlockedPanTypes(value interface{}) []string {
	var raw []string
	switch v := value.(type) {
	case nil:
		return []string{}
	case string:
		raw = strings.FieldsFunc(v, func(r rune) bool {
			return r == '\n' || r == ',' || r == '，'
		})
	case []string:
		raw = v
	case []interface{}:
		raw = make([]string, 0, len(v))
		for _, item := range v {
			raw = append(raw, fmt.Sprintf("%v", item))
		}
	default:
		raw = []string{fmt.Sprintf("%v", v)}
	}

	seen := make(map[string]struct{})
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		normalized := normalizePanTypeName(item)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func normalizePanLinks(groupKey string, group PanGroup) []PanLinkItem {
	items := make([]PanLinkItem, 0, len(group.Links))
	seen := make(map[string]struct{})

	for _, link := range group.Links {
		linkType := normalizeLinkType(firstNonEmpty(link.Type, groupKey), link.URL)
		linkURL := normalizePanURL(link.URL, link.Password, linkType)
		if !isRealPanURL(linkURL) {
			continue
		}

		key := linkURL + "@@" + strings.TrimSpace(link.Password)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		items = append(items, PanLinkItem{
			Title:    strings.TrimSpace(link.Title),
			URL:      linkURL,
			Token:    strings.TrimSpace(link.Token),
			Password: strings.TrimSpace(link.Password),
			Type:     linkType,
			Time:     strings.TrimSpace(link.Time),
			Source:   strings.TrimSpace(link.Source),
			ID:       strings.TrimSpace(link.ID),
			UserTier: strings.TrimSpace(link.UserTier),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return parseLinkTime(items[i].Time).After(parseLinkTime(items[j].Time))
	})
	return items
}

func normalizePanTypeName(value string) string {
	text := strings.ToLower(strings.TrimSpace(value))
	switch text {
	case "", "全部":
		return ""
	case "迅雷", "迅雷云盘":
		return "xunlei"
	case "百度", "百度网盘":
		return "baidu"
	case "夸克", "夸克网盘":
		return "quark"
	case "uc", "uc网盘":
		return "uc"
	case "123", "123网盘", "123pan", "a123":
		return "123"
	case "天翼", "天翼云盘", "a189":
		return "tianyi"
	case "115", "115网盘", "a115":
		return "115"
	case "阿里", "阿里云盘", "aliyun", "阿里云", "ali":
		return "aliyun"
	case "光鸭", "光鸭网盘", "guangya", "guangyapan", "gy":
		return "guangya"
	case "中国移动云盘", "移动云盘", "mobile", "a139":
		return "mobile"
	case "pikpak", "pikpak网盘":
		return "pikpak"
	case "磁力", "磁链":
		return "magnet"
	default:
		return text
	}
}

func isBlockedPanType(groupKey string, groupName string, blocked []string) bool {
	key := normalizePanTypeName(groupKey)
	name := normalizePanTypeName(groupName)
	for _, item := range blocked {
		if item == key || item == name {
			return true
		}
	}
	return false
}

func normalizeLinkType(rawType string, rawURL string) string {
	if normalized := normalizePanTypeName(rawType); normalized != "" && normalized != "others" {
		return normalized
	}

	urlLower := strings.ToLower(strings.TrimSpace(rawURL))
	switch {
	case strings.HasPrefix(urlLower, "magnet:"):
		return "magnet"
	case strings.HasPrefix(urlLower, "ed2k://"):
		return "ed2k"
	case strings.Contains(urlLower, "pan.quark.cn"), strings.Contains(urlLower, "pan.qoark.cn"):
		return "quark"
	case strings.Contains(urlLower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(urlLower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(urlLower, "aliyundrive.com"), strings.Contains(urlLower, "alipan.com"):
		return "aliyun"
	case strings.Contains(urlLower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(urlLower, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(urlLower, "115.com"), strings.Contains(urlLower, "115cdn.com"), strings.Contains(urlLower, "anxia.com"):
		return "115"
	case strings.Contains(urlLower, "123pan.com"), strings.Contains(urlLower, "123684.com"), strings.Contains(urlLower, "123685.com"), strings.Contains(urlLower, "123865.com"), strings.Contains(urlLower, "123912.com"), strings.Contains(urlLower, "123592.com"):
		return "123"
	case strings.Contains(urlLower, "caiyun.139.com"), strings.Contains(urlLower, "yun.139.com"):
		return "mobile"
	case strings.Contains(urlLower, "guangyapan"):
		return "guangya"
	case strings.Contains(urlLower, "mypikpak.com"), strings.Contains(urlLower, "mypikpak.net"), strings.Contains(urlLower, "pikpakdrive.com"):
		return "pikpak"
	default:
		return "others"
	}
}

func isRealPanURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(value), "http://") ||
		strings.HasPrefix(strings.ToLower(value), "https://") ||
		strings.HasPrefix(strings.ToLower(value), "magnet:") ||
		strings.HasPrefix(strings.ToLower(value), "ed2k:") ||
		strings.HasPrefix(strings.ToLower(value), "thunder:")
}

func normalizePanShareURL(rawURL string, linkType string) string {
	clean := strings.TrimSpace(rawURL)
	if clean == "" {
		return ""
	}

	parsed, err := url.Parse(clean)
	if err != nil || parsed.Hostname() == "" {
		return clean
	}
	hostname := strings.ToLower(parsed.Hostname())
	shareHost := pan123ShareHostRegex.MatchString(hostname)
	pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if (normalizePanTypeName(linkType) == "123" || shareHost) && shareHost && len(pathParts) == 2 && strings.EqualFold(pathParts[0], "123pan") && pathParts[1] != "" {
		normalized := &url.URL{Scheme: "https", Host: "123865.com", Path: "/s/" + pathParts[1]}
		query := normalized.Query()
		for _, key := range []string{"pwd", "pass", "password", "code"} {
			if value := parsed.Query().Get(key); value != "" {
				query.Set("pwd", value)
				break
			}
		}
		normalized.RawQuery = query.Encode()
		return normalized.String()
	}
	return clean
}

func normalizePanURL(rawURL string, password string, linkType string) string {
	clean := normalizePanShareURL(strings.TrimSpace(rawURL), linkType)
	clean = strings.TrimRight(clean, "#")
	clean = extractCodeNoiseRegex.ReplaceAllString(clean, "")
	clean = strings.TrimSpace(clean)
	if clean == "" {
		return ""
	}

	if password == "" {
		return clean
	}
	lower := strings.ToLower(clean)
	if strings.Contains(lower, "pwd=") || strings.Contains(lower, "password=") || strings.Contains(lower, "passcode=") {
		return clean
	}

	switch linkType {
	case "baidu", "xunlei", "123":
		if strings.Contains(clean, "?") {
			return clean + "&pwd=" + url.QueryEscape(password)
		}
		return clean + "?pwd=" + url.QueryEscape(password)
	case "115":
		if strings.Contains(clean, "?") {
			return clean + "&password=" + url.QueryEscape(password)
		}
		return clean + "?password=" + url.QueryEscape(password)
	default:
		return clean
	}
}

func parseLinkTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

func compactStrings(items []string) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func sanitizeText(value string) string {
	replacer := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n")
	text := replacer.Replace(value)
	text = htmlTagRegex.ReplaceAllString(text, "")
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\t", " ")
	lines := strings.Split(text, "\n")
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(strings.TrimSpace(line)), " ")
		if line != "" {
			trimmed = append(trimmed, line)
		}
	}
	return strings.Join(trimmed, "\n")
}

func browserUserAgent() string {
	return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
}

func cookiesToString(cookies []*http.Cookie) string {
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

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func reqDataHasKey(data map[string]interface{}, key string) bool {
	_, ok := data[key]
	return ok
}

func (p *PanlianPlugin) generateHash(username string) string {
	salt := os.Getenv("PANLIAN_HASH_SALT")
	if salt == "" {
		salt = "pansou_panlian_secret_2026"
	}
	sum := sha256.Sum256([]byte(username + salt))
	return hex.EncodeToString(sum[:])
}

func (p *PanlianPlugin) isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
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

func getEncryptionKey() []byte {
	key := os.Getenv("PANLIAN_ENCRYPTION_KEY")
	if key == "" {
		key = "default-panlian-encryption-key!!"
	}
	return []byte(key)[:32]
}

func (p *PanlianPlugin) encryptPassword(password string) (string, error) {
	block, err := aes.NewCipher(getEncryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(password), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (p *PanlianPlugin) decryptPassword(encrypted string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(getEncryptionKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("密文长度不足")
	}
	nonce := data[:gcm.NonceSize()]
	ciphertext := data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
