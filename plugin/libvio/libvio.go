package libvio

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"pansou/util"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	BaseURL        = "https://libvio.host"
	SearchPath     = "/search/-------------.html"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxConcurrency = 6 // 详情页最大并发数
	MaxPages       = 1 // 最大搜索页数（暂时只搜索第一页）
)

// 站点自 2026-10 起在入口套了一层 CDN 浏览器验证：纯 HTTP 客户端一律 403，
// 响应头带 x-cdn-challenge: required，正文是一段浏览器端 SHA-256 前导零挑战。
// 解出 nonce 后服务端用 302 + Set-Cookie 下发 __cdn_verified（Max-Age 1800），
// 后续请求带上它才能拿到真正的页面。不走完这套握手，搜索/详情/播放三级全是 403。
const (
	powCookieName   = "__cdn_pow"
	verifiedName    = "__cdn_verified"
	challengeHeader = "X-CDN-Challenge"

	// 服务端 __cdn_verified 的有效期是 1800 秒，提前 5 分钟作废，
	// 免得在边界上拿着刚好过期的 cookie 白撞一次 403 再重来。
	verifiedTTL = 25 * time.Minute

	// 挑战难度是 SHA-256 前 4 个 hex（16 bit 前导零），期望约 6.5 万次尝试。
	// 上限给到 2^22 纯属熔断：正常机器远用不到，只是为了不出现无限循环。
	powMaxAttempts = 1 << 22

	// 判定挑战页时需要检视的正文上限。
	challengeProbeBytes = 64 << 10

	// 读取挑战页正文的上限。
	challengeReadLimit = 1 << 20
)

// LibvioPlugin LIBVIO插件
type LibvioPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	playCache   sync.Map // 缓存播放页结果
	cacheTTL    time.Duration
	cdn         cdnGuard // CDN 浏览器验证握手状态，见 cdn_pow.go
}

// NewLibvioPlugin 创建新的LIBVIO插件实例
func NewLibvioPlugin() *LibvioPlugin {
	// 检查调试模式
	debugMode := false // 开启调试模式

	p := &LibvioPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("libvio", 1, true),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,
	}

	return p
}

// Name 返回插件名称
func (p *LibvioPlugin) Name() string {
	return "libvio"
}

// DisplayName 返回插件显示名称
func (p *LibvioPlugin) DisplayName() string {
	return "LIBVIO"
}

// Description 返回插件描述
func (p *LibvioPlugin) Description() string {
	return "LIBVIO - 影视资源网盘下载"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *LibvioPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *LibvioPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// setRequestHeaders 设置请求头
func (p *LibvioPlugin) setRequestHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// doRequest 发送HTTP请求，命中 CDN 浏览器验证时自动完成握手后重试。
func (p *LibvioPlugin) doRequest(client *http.Client, url string, referer string) (*http.Response, error) {
	used := p.cdn.get()
	resp, err := p.doRequestOnce(client, url, referer, used)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}

	// 403 有两种：CDN 浏览器验证和地域封禁。后者重试多少次都一样。
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, challengeProbeBytes))
	resp.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if !isChallengeBody(resp.Header, body) {
		// 不是验证页：把正文还回去，让调用方按原样处理这个 403。
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}

	// 同一时刻只做一次握手；等锁期间可能已经有别的请求解好了。
	p.cdn.solveMu.Lock()
	defer p.cdn.solveMu.Unlock()

	verified := p.cdn.get()
	if verified == "" || verified == used {
		if verified != "" && verified == used {
			// 手里的 cookie 被服务端否掉了（过期或被吊销），作废重解。
			p.cdn.set("")
		}
		verified, err = p.solveCDNChallenge(client)
		if err != nil {
			return nil, fmt.Errorf("过 CDN 浏览器验证失败: %w", err)
		}
		p.cdn.set(verified)
	}

	return p.doRequestOnce(client, url, referer, verified)
}

// doRequestOnce 发送单次请求，verified 非空时附加 CDN 验证 cookie。
func (p *LibvioPlugin) doRequestOnce(client *http.Client, url string, referer string, verified string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	p.setRequestHeaders(req, referer)
	if verified != "" {
		req.Header.Set("Cookie", verifiedName+"="+verified)
	}

	if p.debugMode {
		log.Printf("[Libvio] 发送请求: %s (已过验证=%v)", url, verified != "")
	}

	// 不借用 client 的 cookie jar：验证 cookie 完全由插件自己维护，
	// 让 jar 再追加一份只会产生重复的 Cookie 头。
	reqClient := *client
	reqClient.Jar = nil

	resp, err := reqClient.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 请求失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[Libvio] 响应状态: %d", resp.StatusCode)
	}

	return resp, nil
}

// cdnGuard 在插件内共享 CDN 验证 cookie。
// 详情页并发是 6，没有它每个被拦的请求都要重解一遍 PoW。
type cdnGuard struct {
	mu       sync.Mutex // 保护 cookie/expireAt
	solveMu  sync.Mutex // 串行化握手，避免并发解同一道题
	cookie   string
	expireAt time.Time
}

// get 返回当前可用的验证 cookie，没有或已过期时返回空串。
func (g *cdnGuard) get() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cookie == "" || time.Now().After(g.expireAt) {
		return ""
	}
	return g.cookie
}

func (g *cdnGuard) set(cookie string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cookie = cookie
	g.expireAt = time.Now().Add(verifiedTTL)
}

// isChallengeBody 判断这个 403 是不是 CDN 浏览器验证。
// 403 在本站有两种：浏览器验证和地域封禁，后者重试多少次都一样，必须先分开。
// 响应头是快判据，但实测部分节点只给正文，所以用正文特征兜底。
func isChallengeBody(header http.Header, body []byte) bool {
	if strings.EqualFold(header.Get(challengeHeader), "required") {
		return true
	}
	return bytes.Contains(body, []byte(powCookieName))
}

// solveCDNChallenge 走完一次 CDN 握手，返回 __cdn_verified 的值。
func (p *LibvioPlugin) solveCDNChallenge(client *http.Client) (string, error) {
	// 挑战参数由服务端按请求注入，每次都不一样，必须先取一次挑战页。
	challengeResp, err := p.doRequestOnce(client, BaseURL+"/", BaseURL, "")
	if err != nil {
		return "", fmt.Errorf("获取验证页失败: %w", err)
	}
	defer challengeResp.Body.Close()

	reader, err := p.getResponseReader(challengeResp)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(reader, challengeReadLimit))
	if err != nil {
		return "", fmt.Errorf("读取验证页失败: %w", err)
	}

	params := powParamRegex.FindSubmatch(body)
	if params == nil {
		return "", fmt.Errorf("验证页未包含 PoW 参数 (HTTP %d)", challengeResp.StatusCode)
	}
	ts, sig, diff, mode := string(params[1]), string(params[2]), string(params[3]), string(params[4])

	nonce, err := solvePowNonce(sig, diff)
	if err != nil {
		return "", err
	}

	powCookie := fmt.Sprintf("%s=%s_%s_%d_%s", powCookieName, ts, mode, nonce, sig)

	// 答案在 302 响应的 Set-Cookie 里，所以这一跳必须自己读、不能跟随重定向。
	submitClient := *client
	submitClient.Jar = nil
	submitClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	submitReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, BaseURL+"/", nil)
	if err != nil {
		return "", err
	}
	p.setRequestHeaders(submitReq, BaseURL)
	submitReq.Header.Set("Cookie", powCookie)

	submitResp, err := submitClient.Do(submitReq)
	if err != nil {
		return "", fmt.Errorf("提交 PoW 失败: %w", err)
	}
	defer submitResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(submitResp.Body, challengeProbeBytes))

	for _, cookie := range submitResp.Cookies() {
		if cookie.Name == verifiedName && cookie.Value != "" {
			return cookie.Value, nil
		}
	}
	return "", fmt.Errorf("提交 PoW 后未收到 %s (HTTP %d)", verifiedName, submitResp.StatusCode)
}

// solvePowNonce 求满足 SHA-256(SIG + nonce) 以 diff 为前缀的最小 nonce。
func solvePowNonce(sig, diff string) (int, error) {
	sigLen := len(sig)
	buf := make([]byte, sigLen, sigLen+20)
	copy(buf, sig)

	for nonce := 0; nonce < powMaxAttempts; nonce++ {
		buf = strconv.AppendInt(buf[:sigLen], int64(nonce), 10)
		sum := sha256.Sum256(buf)
		if strings.HasPrefix(hex.EncodeToString(sum[:]), diff) {
			return nonce, nil
		}
	}
	return 0, fmt.Errorf("PoW 未在 %d 次尝试内解出 (难度 %s)", powMaxAttempts, diff)
}

// searchImpl 实际的搜索实现
func (p *LibvioPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s%s?wd=%s&submit=", BaseURL, SearchPath, url.QueryEscape(keyword))

	if p.debugMode {
		log.Printf("[Libvio] 开始搜索: %s", keyword)
		log.Printf("[Libvio] 搜索URL: %s", searchURL)
	}

	// 发送搜索请求
	resp, err := p.doRequest(client, searchURL, BaseURL)
	if err != nil {
		return nil, fmt.Errorf("发送搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索响应状态码异常: %d", resp.StatusCode)
	}

	// 处理响应体（可能是gzip压缩的）
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil, err
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	results := p.extractSearchResults(doc, keyword)

	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个搜索结果", len(results))
	}

	// 并发获取详情页的下载链接
	results = p.enrichWithDetailLinks(client, results, keyword)

	if p.debugMode {
		// 统计链接数量
		totalLinks := 0
		for i, r := range results {
			log.Printf("[Libvio] 结果 %d: %s, 链接数: %d", i+1, r.Title, len(r.Links))
			totalLinks += len(r.Links)
		}
		log.Printf("[Libvio] 总计: %d 个结果，%d 个链接", len(results), totalLinks)
	}

	// 过滤结果
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	if p.debugMode {
		log.Printf("[Libvio] 过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// getResponseReader 获取响应读取器（处理gzip压缩）
func (p *LibvioPlugin) getResponseReader(resp *http.Response) (io.Reader, error) {
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if p.debugMode {
		log.Printf("[Libvio] Content-Encoding: %s", contentEncoding)
	}

	// 如果是gzip压缩，手动解压
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip reader失败: %w", err)
		}
		// 注意：不要在这里关闭gzReader，它需要在外部使用
		reader = util.NewCappedReader(gzReader, util.MaxDecompressedBytes)
	}

	return reader, nil
}

// extractSearchResults 从HTML中提取搜索结果
func (p *LibvioPlugin) extractSearchResults(doc *goquery.Document, keyword string) []model.SearchResult {
	var results []model.SearchResult

	// 选择所有搜索结果项
	doc.Find("ul.stui-vodlist li").Each(func(i int, s *goquery.Selection) {
		// 提取标题和详情页链接
		titleElem := s.Find(".stui-vodlist__detail h4 a")
		title := strings.TrimSpace(titleElem.Text())
		if title == "" {
			title, _ = titleElem.Attr("title")
		}

		detailPath, _ := titleElem.Attr("href")
		if detailPath == "" {
			// 尝试从缩略图链接获取
			thumbLink := s.Find("a.stui-vodlist__thumb")
			detailPath, _ = thumbLink.Attr("href")
		}

		if title == "" || detailPath == "" {
			return
		}

		// 构建完整的详情页URL
		detailURL := BaseURL + detailPath

		// 提取其他信息
		episodeInfo := strings.TrimSpace(s.Find(".pic-text").Text())
		rating := strings.TrimSpace(s.Find(".pic-tag").Text())

		// 从详情页路径提取ID（如：/detail/4095.html -> 4095）
		idMatch := libvioRe1.FindStringSubmatch(detailPath)
		resourceID := ""
		if len(idMatch) > 1 {
			resourceID = idMatch[1]
		} else {
			resourceID = fmt.Sprintf("%d", time.Now().UnixNano())
		}

		if p.debugMode {
			log.Printf("[Libvio] 提取结果 %d: %s, URL: %s", i+1, title, detailURL)
		}

		// 构建内容描述
		content := ""
		if episodeInfo != "" {
			content = episodeInfo
		}
		if rating != "" {
			if content != "" {
				content += " | "
			}
			content += "评分: " + rating
		}

		result := model.SearchResult{
			Title:     title,
			Content:   content,
			Channel:   "",
			MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
			Datetime:  time.Now(),
			Links:     []model.Link{}, // 稍后填充
		}

		// 将详情页URL存储在Tags中供后续使用
		result.Tags = []string{detailURL}

		results = append(results, result)
	})

	return results
}

// enrichWithDetailLinks 并发获取详情页的下载链接
func (p *LibvioPlugin) enrichWithDetailLinks(client *http.Client, results []model.SearchResult, keyword string) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	if p.debugMode {
		log.Printf("[Libvio] 开始获取 %d 个详情页的下载链接", len(results))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, MaxConcurrency)

	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加小延迟避免请求过快
			time.Sleep(time.Duration(idx*50) * time.Millisecond)

			// 从Tags中获取详情页URL
			if len(results[idx].Tags) > 0 {
				detailURL := results[idx].Tags[0]
				links := p.fetchDetailPageLinks(client, detailURL, results[idx].Title)

				mu.Lock()
				results[idx].Links = links
				// 清空Tags，避免返回给用户
				results[idx].Tags = nil
				mu.Unlock()

				if p.debugMode {
					log.Printf("[Libvio] 详情页 %d/%d 获取到 %d 个链接", idx+1, len(results), len(links))
				}
			}
		}(i)
	}

	wg.Wait()

	validResults := results[:0]
	for _, result := range results {
		if len(result.Links) > 0 {
			validResults = append(validResults, result)
		}
	}
	return validResults
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *LibvioPlugin) fetchDetailPageLinks(client *http.Client, detailURL string, workTitle string) []model.Link {
	if p.debugMode {
		log.Printf("[Libvio] 开始获取详情页: %s", detailURL)
	}

	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[Libvio] 使用缓存的详情页结果: %s, 链接数: %d", detailURL, len(links))
			}
			return links
		}
	}

	// 访问详情页
	resp, err := p.doRequest(client, detailURL, BaseURL)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 获取详情页失败: %s, 错误: %v", detailURL, err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Libvio] 详情页响应状态码异常: %s, 状态码: %d", detailURL, resp.StatusCode)
		}
		return nil
	}

	// 处理响应体
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	// 新版详情页直接暴露网盘链接，无需再访问播放页。
	links := p.extractDirectPanLinks(doc, workTitle)
	if len(links) > 0 {
		p.cacheDetailLinks(detailURL, links)
		return links
	}

	// 兼容旧版详情页：从下载播放页的 player_aaaa 中提取链接。
	playLinks := p.extractDownloadPlayLinks(doc)

	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个下载播放页链接", len(playLinks))
	}

	if len(playLinks) == 0 {
		if p.debugMode {
			log.Printf("[Libvio] 未找到下载链接")
		}
		return nil
	}

	// 获取网盘链接
	links = make([]model.Link, 0, len(playLinks))
	for _, playLink := range playLinks {
		if p.debugMode {
			log.Printf("[Libvio] 获取网盘链接: %s", playLink.URL)
		}
		panLink := p.fetchPanLink(client, playLink.URL, detailURL)
		if panLink != nil {
			panLink.WorkTitle = workTitle
			if panLink.Password == "" {
				panLink.Password = extractPassword(panLink.URL)
			}
			links = append(links, *panLink)
		} else if p.debugMode {
			log.Printf("[Libvio] 未能获取网盘链接: %s", playLink.URL)
		}
	}

	if p.debugMode {
		log.Printf("[Libvio] 详情页 %s 最终获取到 %d 个网盘链接", detailURL, len(links))
	}

	p.cacheDetailLinks(detailURL, links)
	return links
}

func (p *LibvioPlugin) extractDirectPanLinks(doc *goquery.Document, workTitle string) []model.Link {
	links := make([]model.Link, 0, 4)
	seen := make(map[string]struct{})
	doc.Find(".netdisk-panel a.netdisk-item[href]").Each(func(_ int, selection *goquery.Selection) {
		rawURL, _ := selection.Attr("href")
		rawURL = strings.TrimSpace(rawURL)
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return
		}
		linkType := p.mapPanType("", rawURL)
		if linkType == "others" {
			return
		}
		password := extractPassword(rawURL)
		key := rawURL + "\x00" + password
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		links = append(links, model.Link{
			URL:       rawURL,
			Type:      linkType,
			Password:  password,
			WorkTitle: workTitle,
		})
	})
	return links
}

func (p *LibvioPlugin) cacheDetailLinks(detailURL string, links []model.Link) {
	p.detailCache.Store(detailURL, links)
	go func() {
		time.Sleep(p.cacheTTL)
		p.detailCache.Delete(detailURL)
	}()
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

// PlayLinkInfo 播放链接信息
type PlayLinkInfo struct {
	URL     string
	PanType string // 网盘类型（从标题提取）
}

// extractDownloadPlayLinks 提取下载播放页链接
func (p *LibvioPlugin) extractDownloadPlayLinks(doc *goquery.Document) []PlayLinkInfo {
	var playLinks []PlayLinkInfo

	// 查找所有播放源
	allHeads := doc.Find(".stui-vodlist__head")
	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个播放源头部", allHeads.Length())
	}

	allHeads.Each(func(i int, s *goquery.Selection) {
		// 获取标题
		title := strings.TrimSpace(s.Find("h3").Text())

		if p.debugMode {
			log.Printf("[Libvio] 播放源 %d 标题: %s", i+1, title)
		}

		// 只处理包含"下载"的源
		if !strings.Contains(title, "下载") {
			if p.debugMode {
				log.Printf("[Libvio] 跳过非下载源: %s", title)
			}
			return
		}

		// 提取网盘类型
		panType := ""
		if strings.Contains(title, "夸克") || strings.Contains(title, "quark") {
			panType = "quark"
		} else if strings.Contains(title, "UC") || strings.Contains(title, "uc") {
			panType = "uc"
		} else if strings.Contains(title, "百度") || strings.Contains(title, "baidu") {
			panType = "baidu"
		}

		// 提取播放页链接
		playlistLinks := s.Find(".stui-content__playlist li a")
		if p.debugMode {
			log.Printf("[Libvio] 播放列表中有 %d 个链接", playlistLinks.Length())
		}

		// 通常只取第一个链接（合集）
		firstLink := playlistLinks.First()
		if firstLink.Length() > 0 {
			href, exists := firstLink.Attr("href")
			if exists && href != "" {
				// 构建完整URL
				playURL := BaseURL + href

				playLinks = append(playLinks, PlayLinkInfo{
					URL:     playURL,
					PanType: panType,
				})

				if p.debugMode {
					linkText := strings.TrimSpace(firstLink.Text())
					log.Printf("[Libvio] 找到下载链接: %s (%s) [%s]", playURL, panType, linkText)
				}
			}
		}
	})

	return playLinks
}

// fetchPanLink 获取网盘链接
func (p *LibvioPlugin) fetchPanLink(client *http.Client, playURL string, referer string) *model.Link {
	// 检查缓存
	if cached, ok := p.playCache.Load(playURL); ok {
		if link, ok := cached.(*model.Link); ok {
			if p.debugMode {
				log.Printf("[Libvio] 使用缓存的播放页结果: %s", playURL)
			}
			return link
		}
	}

	// 访问播放页
	resp, err := p.doRequest(client, playURL, referer)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 获取播放页失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Libvio] 播放页响应状态码异常: %d", resp.StatusCode)
		}
		return nil
	}

	// 处理响应体（可能是gzip压缩的）
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	// 读取响应体
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil
	}

	// 提取player_aaaa对象
	playerDataRegex := libvioRe2
	matches := playerDataRegex.FindStringSubmatch(string(body))

	if len(matches) < 2 {
		if p.debugMode {
			log.Printf("[Libvio] 未找到player_aaaa对象")
			// 输出部分body内容用于调试
			bodyStr := string(body)
			if len(bodyStr) > 500 {
				log.Printf("[Libvio] 页面内容前500字符: %s", bodyStr[:500])
			} else {
				log.Printf("[Libvio] 页面内容: %s", bodyStr)
			}
		}
		return nil
	}

	// 解析JSON
	playerJSON := matches[1]
	if p.debugMode {
		log.Printf("[Libvio] 找到player_aaaa: %s", playerJSON)
	}

	// 处理转义字符
	playerJSON = strings.ReplaceAll(playerJSON, `\/`, `/`)

	var playerData map[string]interface{}
	if err := json.Unmarshal([]byte(playerJSON), &playerData); err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 解析player_aaaa失败: %v, JSON: %s", err, playerJSON)
		}
		return nil
	}

	// 提取URL
	panURL, ok := playerData["url"].(string)
	if !ok || panURL == "" {
		if p.debugMode {
			log.Printf("[Libvio] player_aaaa中没有url字段")
		}
		return nil
	}

	// 提取网盘类型
	from, _ := playerData["from"].(string)
	linkType := p.mapPanType(from, panURL)

	link := &model.Link{
		URL:  panURL,
		Type: linkType,
	}

	if p.debugMode {
		log.Printf("[Libvio] 提取到网盘链接: %s (from=%s, type=%s)", panURL, from, linkType)
	}

	// 缓存结果
	p.playCache.Store(playURL, link)

	// 设置缓存过期
	go func() {
		time.Sleep(p.cacheTTL)
		p.playCache.Delete(playURL)
	}()

	return link
}

// mapPanType 映射网盘类型
func (p *LibvioPlugin) mapPanType(from string, url string) string {
	// 首先根据from字段判断
	switch strings.ToLower(from) {
	case "uc":
		return "uc"
	case "quark":
		return "quark"
	case "baidu":
		return "baidu"
	case "aliyun", "alipan":
		return "aliyun"
	case "xunlei", "thunder":
		return "xunlei"
	case "115":
		return "115"
	case "123", "123pan":
		return "123"
	}

	// 如果from字段不明确，根据URL判断
	url = strings.ToLower(url)
	if strings.Contains(url, "drive.uc.cn") {
		return "uc"
	} else if strings.Contains(url, "pan.quark.cn") {
		return "quark"
	} else if strings.Contains(url, "pan.baidu.com") {
		return "baidu"
	} else if strings.Contains(url, "alipan.com") || strings.Contains(url, "aliyundrive.com") {
		return "aliyun"
	} else if strings.Contains(url, "pan.xunlei.com") {
		return "xunlei"
	} else if strings.Contains(url, "115.com") {
		return "115"
	} else if strings.Contains(url, "123pan.com") || strings.Contains(url, "123684.com") {
		return "123"
	} else if strings.Contains(url, "cloud.189.cn") {
		return "tianyi"
	}

	// 默认返回others
	return "others"
}

func init() {
	plugin.RegisterGlobalPlugin(NewLibvioPlugin())
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	libvioRe1 = regexp.MustCompile(`/detail/(\d+)\.html`)
	libvioRe2 = regexp.MustCompile(`var\s+player_aaaa\s*=\s*({[^}]+})`)

	// powParamRegex 匹配 CDN 挑战页注入的四个参数。
	// 真实页面形如：var TS = "1791465351", SIG = "aea3e8...", DIFF = "0000", MODE = "auto";
	powParamRegex = regexp.MustCompile(`var TS = "(\d+)", SIG = "([0-9a-f]+)", DIFF = "([^"]+)", MODE = "([^"]+)"`)
)
