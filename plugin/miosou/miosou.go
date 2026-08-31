package miosou

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	pluginName       = "miosou"
	pluginPriority   = 2
	baseURL          = "https://miosou.cc"
	apiBaseURL       = baseURL + "/miov1"
	requestTimeout   = 35 * time.Second
	detailTimeout    = 20 * time.Second
	gateTimeout      = 30 * time.Second
	maxDetailWorkers = 4
	maxShareRefs     = 80
	anubisPassPath   = "/.within.website/x/cmd/anubis/api/pass-challenge"
	browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

var passwordPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[?&](?:pwd|password|code|passcode)=([^&#\s]+)`),
	regexp.MustCompile(`(?i)(?:提取码|密码|访问码|取件码)\s*[:：]?\s*([0-9a-zA-Z]{2,12})`),
}

var anubisChallengePattern = regexp.MustCompile(`(?s)<script id="anubis_challenge" type="application/json">\s*(.*?)\s*</script>`)

func init() {
	plugin.RegisterGlobalPlugin(NewMiosouPlugin())
}

// MiosouPlugin searches the miosou.cc aggregated public share index.
type MiosouPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client

	gateMu    sync.Mutex
	gateReady bool
}

func NewMiosouPlugin() *MiosouPlugin {
	jar, _ := cookiejar.New(nil)
	return &MiosouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, pluginPriority),
		client: &http.Client{
			Jar:       jar,
			Timeout:   requestTimeout,
			Transport: &http.Transport{MaxIdleConns: 32, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 16, IdleConnTimeout: 90 * time.Second},
		},
	}
}

func (p *MiosouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *MiosouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *MiosouPlugin) searchImpl(_ *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}
	if err := p.ensureGate(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/search?keyword="+url.QueryEscape(keyword), nil)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
		}
		setHeaders(req, "text/event-stream")
		resp, err := p.client.Do(req)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
		}
		if isAnubisGateResponse(resp) {
			resp.Body.Close()
			cancel()
			p.invalidateGate()
			if err := p.ensureGate(); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			return nil, fmt.Errorf("[%s] 搜索接口返回状态码: %d", p.Name(), resp.StatusCode)
		}
		groups, err := parseSearchStream(resp.Body)
		resp.Body.Close()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("[%s] 解析搜索流失败: %w", p.Name(), err)
		}
		results := p.convertGroups(ctx, groups, keyword)
		cancel()
		return results, nil
	}
	return nil, fmt.Errorf("[%s] 人机验证会话失效", p.Name())
}

func (p *MiosouPlugin) ensureGate() error {
	p.gateMu.Lock()
	defer p.gateMu.Unlock()
	if p.gateReady {
		return nil
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), gateTimeout)
		err := p.completeAnubisChallenge(ctx)
		cancel()
		if err == nil {
			p.gateReady = true
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("[%s] 人机验证未通过: %w", p.Name(), lastErr)
}

func (p *MiosouPlugin) completeAnubisChallenge(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return fmt.Errorf("创建 Anubis 验证请求失败: %w", err)
	}
	setPageHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("获取 Anubis 验证题目失败: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("读取 Anubis 验证题目失败: %w", readErr)
	}
	challenge, found, err := parseAnubisChallenge(body)
	if err != nil {
		return err
	}
	if !found {
		if resp.StatusCode == http.StatusOK && !isAnubisGateResponse(resp) {
			return nil
		}
		return fmt.Errorf("Anubis 验证页返回状态码 %d", resp.StatusCode)
	}

	startedAt := time.Now()
	hash, nonce, err := solveAnubisChallenge(ctx, challenge.Challenge.RandomData, challenge.Rules.Difficulty)
	if err != nil {
		return err
	}
	elapsed := time.Since(startedAt).Milliseconds()
	if elapsed < 1 {
		elapsed = 1
	}
	query := url.Values{
		"id":          []string{challenge.Challenge.ID},
		"response":    []string{hash},
		"nonce":       []string{strconv.FormatUint(nonce, 10)},
		"redir":       []string{baseURL + "/"},
		"elapsedTime": []string{strconv.FormatInt(elapsed, 10)},
	}
	passReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+anubisPassPath+"?"+query.Encode(), nil)
	if err != nil {
		return fmt.Errorf("创建 Anubis 验证提交请求失败: %w", err)
	}
	setPageHeaders(passReq)
	passReq.Header.Set("Referer", resp.Request.URL.String())
	passResp, err := p.client.Do(passReq)
	if err != nil {
		return fmt.Errorf("提交 Anubis 验证结果失败: %w", err)
	}
	io.Copy(io.Discard, io.LimitReader(passResp.Body, 1<<20))
	passResp.Body.Close()
	if passResp.StatusCode != http.StatusOK || isAnubisGateResponse(passResp) {
		return fmt.Errorf("Anubis 验证结果返回状态码 %d", passResp.StatusCode)
	}
	return nil
}

func (p *MiosouPlugin) invalidateGate() {
	p.gateMu.Lock()
	p.gateReady = false
	p.gateMu.Unlock()
}

func setHeaders(req *http.Request, accept string) {
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Referer", baseURL+"/")
	origin := baseURL
	req.Header.Set("Origin", origin)
}

func setPageHeaders(req *http.Request) {
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
}

func isAnubisGateResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusUnauthorized || (resp.Request != nil && resp.Request.URL != nil && strings.HasPrefix(resp.Request.URL.Path, "/.within.website/"))
}

type anubisChallengeDocument struct {
	Rules struct {
		Algorithm  string `json:"algorithm"`
		Difficulty int    `json:"difficulty"`
	} `json:"rules"`
	Challenge struct {
		ID         string `json:"id"`
		RandomData string `json:"randomData"`
	} `json:"challenge"`
}

func parseAnubisChallenge(body []byte) (anubisChallengeDocument, bool, error) {
	match := anubisChallengePattern.FindSubmatch(body)
	if len(match) != 2 {
		return anubisChallengeDocument{}, false, nil
	}
	var challenge anubisChallengeDocument
	if err := json.Unmarshal([]byte(html.UnescapeString(string(match[1]))), &challenge); err != nil {
		return anubisChallengeDocument{}, true, fmt.Errorf("解析 Anubis 验证题目失败: %w", err)
	}
	if challenge.Challenge.ID == "" || challenge.Challenge.RandomData == "" || challenge.Rules.Difficulty < 1 || challenge.Rules.Difficulty > 7 {
		return anubisChallengeDocument{}, true, fmt.Errorf("Anubis 验证题目无效")
	}
	if challenge.Rules.Algorithm != "fast" && challenge.Rules.Algorithm != "slow" {
		return anubisChallengeDocument{}, true, fmt.Errorf("不支持的 Anubis 验证算法: %s", challenge.Rules.Algorithm)
	}
	return challenge, true, nil
}

func solveAnubisChallenge(ctx context.Context, randomData string, difficulty int) (string, uint64, error) {
	prefix := []byte(randomData)
	buffer := make([]byte, len(prefix), len(prefix)+20)
	copy(buffer, prefix)
	for nonce := uint64(0); ; nonce++ {
		if nonce&4095 == 0 {
			select {
			case <-ctx.Done():
				return "", 0, fmt.Errorf("计算 Anubis 工作量证明失败: %w", ctx.Err())
			default:
			}
		}
		candidate := strconv.AppendUint(buffer[:len(prefix)], nonce, 10)
		hash := sha256.Sum256(candidate)
		if hasLeadingZeroNibbles(hash[:], difficulty) {
			return hex.EncodeToString(hash[:]), nonce, nil
		}
	}
}

func hasLeadingZeroNibbles(hash []byte, difficulty int) bool {
	for i := 0; i < difficulty/2; i++ {
		if hash[i] != 0 {
			return false
		}
	}
	return difficulty%2 == 0 || hash[difficulty/2]>>4 == 0
}

type searchItem struct {
	URL       string `json:"url"`
	ShareRef  string `json:"share_ref"`
	Password  string `json:"password"`
	Note      string `json:"note"`
	Datetime  string `json:"datetime"`
	Source    string `json:"source"`
	Keyword   string `json:"keyword"`
	StreamKey string `json:"stream_key"`
}

type searchSnapshot struct {
	MergedByType map[string][]searchItem `json:"merged_by_type"`
}

func parseSearchStream(r io.Reader) (map[string][]searchItem, error) {
	groups := make(map[string][]searchItem)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var event string
	var data strings.Builder
	var streamErr error
	flush := func() {
		if data.Len() == 0 {
			return
		}
		if event == "snapshot" || event == "" {
			var snapshot searchSnapshot
			if json.Unmarshal([]byte(data.String()), &snapshot) == nil {
				for cloud, items := range snapshot.MergedByType {
					groups[cloud] = mergeItems(groups[cloud], items)
				}
			}
		} else if event == "error" {
			var payload struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(data.String()), &payload) == nil && payload.Message != "" {
				streamErr = fmt.Errorf("%s", payload.Message)
			} else {
				streamErr = fmt.Errorf("%s", strings.TrimSpace(data.String()))
			}
		}
		event = ""
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if streamErr != nil {
		return nil, streamErr
	}
	return groups, nil
}

func mergeItems(existing, incoming []searchItem) []searchItem {
	index := make(map[string]int, len(existing)+len(incoming))
	key := func(item searchItem) string {
		if item.StreamKey != "" {
			return "stream:" + item.StreamKey
		}
		if item.ShareRef != "" {
			return "ref:" + item.ShareRef
		}
		return "url:" + item.URL
	}
	for i, item := range existing {
		index[key(item)] = i
	}
	for _, item := range incoming {
		if item.StreamKey == "" && item.ShareRef == "" && item.URL == "" {
			continue
		}
		if i, ok := index[key(item)]; ok {
			previous := existing[i]
			if item.URL == "" {
				item.URL = previous.URL
			}
			if item.ShareRef == "" {
				item.ShareRef = previous.ShareRef
			}
			if item.Password == "" {
				item.Password = previous.Password
			}
			if item.Note == "" {
				item.Note = previous.Note
			}
			if item.Keyword == "" {
				item.Keyword = previous.Keyword
			}
			if item.Datetime == "" {
				item.Datetime = previous.Datetime
			}
			if item.Source == "" {
				item.Source = previous.Source
			}
			existing[i] = item
		} else {
			index[key(item)] = len(existing)
			existing = append(existing, item)
		}
	}
	return existing
}

func (p *MiosouPlugin) convertGroups(parent context.Context, groups map[string][]searchItem, keyword string) []model.SearchResult {
	items := make([]searchItem, 0)
	directItems := make([]searchItem, 0)
	refItems := make([]searchItem, 0)
	clouds := make([]string, 0, len(groups))
	for cloud := range groups {
		clouds = append(clouds, cloud)
	}
	sort.Strings(clouds)
	for _, cloud := range clouds {
		for _, item := range groups[cloud] {
			if strings.TrimSpace(item.URL) != "" {
				directItems = append(directItems, item)
			} else {
				refItems = append(refItems, item)
			}
		}
	}
	items = append(items, directItems...)
	if len(items) < maxShareRefs {
		remaining := maxShareRefs - len(items)
		if remaining > len(refItems) {
			remaining = len(refItems)
		}
		items = append(items, refItems[:remaining]...)
	}
	if len(items) > maxShareRefs {
		items = items[:maxShareRefs]
	}

	results := make([]model.SearchResult, 0, len(items))
	sem := make(chan struct{}, maxDetailWorkers)
	resultCh := make(chan model.SearchResult, len(items))
	var wg sync.WaitGroup
	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-parent.Done():
				return
			}
			defer func() { <-sem }()
			if result, ok := p.convertItem(parent, item); ok {
				resultCh <- result
			}
		}()
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		results = append(results, result)
	}
	return plugin.FilterResultsByKeyword(results, keyword)
}

type detailResponse struct {
	DisplayURL string `json:"display_url"`
	Share      struct {
		Title string `json:"title"`
	} `json:"share"`
}

func (p *MiosouPlugin) convertItem(parent context.Context, item searchItem) (model.SearchResult, bool) {
	rawURL := strings.TrimSpace(item.URL)
	title := strings.TrimSpace(item.Note)
	if rawURL == "" && item.ShareRef != "" {
		ctx, cancel := context.WithTimeout(parent, detailTimeout)
		defer cancel()
		// The non-shallow response includes display_url (including any
		// extraction code). A shallow response only exposes file metadata and
		// cannot be converted into a PanSou link.
		query := url.Values{"share_ref": []string{item.ShareRef}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/share/detail?"+query.Encode(), nil)
		if err != nil {
			return model.SearchResult{}, false
		}
		setHeaders(req, "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			return model.SearchResult{}, false
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			return model.SearchResult{}, false
		}
		var detail detailResponse
		if json.Unmarshal(body, &detail) != nil {
			return model.SearchResult{}, false
		}
		rawURL = strings.TrimSpace(detail.DisplayURL)
		if title == "" {
			title = strings.TrimSpace(detail.Share.Title)
		}
	}
	if rawURL == "" {
		return model.SearchResult{}, false
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Scheme == "" {
		return model.SearchResult{}, false
	}
	switch strings.ToLower(parsedURL.Scheme) {
	case "http", "https":
		if parsedURL.Host == "" {
			return model.SearchResult{}, false
		}
	case "magnet", "ed2k":
	default:
		return model.SearchResult{}, false
	}
	linkType := detectLinkType(rawURL)
	password := strings.TrimSpace(item.Password)
	if password == "" {
		password = extractPassword(rawURL)
	}
	if password == "" {
		password = extractPassword(item.Note)
	}
	if title == "" {
		title = rawURL
	}
	datetime := parseDatetime(item.Datetime)
	hash := sha256.Sum256([]byte(rawURL + "\x00" + password))
	return model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), hex.EncodeToString(hash[:8])),
		Channel:  "",
		Datetime: datetime,
		Title:    title,
		Content:  formatContent(item.Source, item.ShareRef, item.Keyword),
		Links: []model.Link{{
			Type:      linkType,
			URL:       rawURL,
			Password:  password,
			WorkTitle: title,
		}},
	}, true
}

func parseDatetime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05.000Z"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Now()
}

func formatContent(source, shareRef, keyword string) string {
	parts := make([]string, 0, 3)
	if source = strings.TrimSpace(source); source != "" {
		parts = append(parts, "来源: "+source)
	}
	if shareRef != "" {
		parts = append(parts, "分享索引: "+shareRef)
	}
	if keyword = strings.TrimSpace(keyword); keyword != "" {
		parts = append(parts, "关键词: "+keyword)
	}
	return strings.Join(parts, " | ")
}

func extractPassword(rawURL string) string {
	for _, pattern := range passwordPatterns {
		matches := pattern.FindStringSubmatch(rawURL)
		if len(matches) > 1 {
			if decoded, err := url.QueryUnescape(matches[1]); err == nil {
				return strings.TrimSpace(decoded)
			}
			return strings.TrimSpace(matches[1])
		}
	}
	return ""
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
	case strings.Contains(lower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lower, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lower, "115.com"), strings.Contains(lower, "115cdn.com"):
		return "115"
	case strings.Contains(lower, "123pan.com"):
		return "123"
	case strings.Contains(lower, "caiyun.feixin.10086.cn"):
		return "mobile"
	case strings.Contains(lower, "mypikpak.com"):
		return "pikpak"
	case strings.HasPrefix(lower, "magnet:"):
		return "magnet"
	case strings.HasPrefix(lower, "ed2k:"):
		return "ed2k"
	default:
		return "others"
	}
}
