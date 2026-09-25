package sopanya

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	pluginName       = "sopanya"
	defaultPriority  = 3
	requestTimeout   = 28 * time.Second
	maxCandidates    = 24
	maxPerType       = 8
	resolveWorkers   = 6
	statusPolls      = 8
	statusPollDelay  = 300 * time.Millisecond
	maxResponseBytes = 2 << 20
)

// sopanyaBaseURL is a variable so parser/integration tests can use a local server.
var sopanyaBaseURL = "https://sopanya.com"

var shareURLRegex = regexp.MustCompile(`https?://[^\s"'<>]+`)
var passwordRegex = regexp.MustCompile(`(?i)(?:提取码|密码|访问码|访问密码|password|pwd|code)\s*[:：=]?\s*([a-z0-9]{3,16})`)

type SopanyaPlugin struct {
	*plugin.BaseAsyncPlugin
}

type searchEvent struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	IsType      int    `json:"is_type"`
	Type        string `json:"type"`
}

type indexedEvent struct {
	index int
	event searchEvent
}

type resourceData struct {
	Title       string          `json:"title"`
	URL         string          `json:"url"`
	IsType      int             `json:"is_type"`
	Code        string          `json:"code"`
	Content     string          `json:"content"`
	Description string          `json:"description"`
	VODContent  string          `json:"vod_content"`
	UpdateTime  json.RawMessage `json:"update_time"`
	CreateTime  json.RawMessage `json:"create_time"`
}

type saveResponse struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Data    *resourceData `json:"data"`
}

type statusPayload struct {
	Ready    bool          `json:"ready"`
	Resource *resourceData `json:"resource"`
}

type statusResponse struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    *statusPayload `json:"data"`
}

type resolvedEvent struct {
	index int
	data  resourceData
	event searchEvent
}

func init() {
	plugin.RegisterGlobalPlugin(NewSopanyaPlugin())
}

func NewSopanyaPlugin() *SopanyaPlugin {
	return &SopanyaPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority)}
}

var _ plugin.AsyncSearchPlugin = (*SopanyaPlugin)(nil)

func (p *SopanyaPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *SopanyaPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *SopanyaPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	client = extendClientTimeout(client)

	searchURL := strings.TrimRight(sopanyaBaseURL, "/") + "/api/other/web_search?title=" + url.QueryEscape(keyword) + "&is_type=-1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] create search request failed: %w", p.Name(), err)
	}
	setHeaders(req, true)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] search request failed: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("[%s] search returned HTTP %d", p.Name(), resp.StatusCode)
	}

	events := make(chan indexedEvent, maxCandidates)
	resolved := make(chan resolvedEvent, maxCandidates)
	var workers sync.WaitGroup
	for i := 0; i < resolveWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for item := range events {
				data, ok := p.resolveEvent(ctx, client, item.event)
				if ok {
					resolved <- resolvedEvent{index: item.index, data: data, event: item.event}
				}
			}
		}()
	}

	counts := make(map[int]int)
	seenTokens := make(map[string]struct{})
	accepted := 0
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxResponseBytes))
	scanner.Buffer(make([]byte, 4096), 256*1024)
	var scanErr error
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			if payload == "[DONE]" {
				break
			}
			continue
		}
		var event searchEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.URL == "" || event.Title == "" || event.Type != "" {
			continue
		}
		if accepted >= maxCandidates || counts[event.IsType] >= maxPerType {
			if accepted >= maxCandidates {
				break
			}
			continue
		}
		if _, exists := seenTokens[event.URL]; exists {
			continue
		}
		seenTokens[event.URL] = struct{}{}
		counts[event.IsType]++
		select {
		case events <- indexedEvent{index: accepted, event: event}:
			accepted++
		case <-ctx.Done():
			scanErr = ctx.Err()
			break
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	close(events)
	workers.Wait()
	close(resolved)

	items := make([]resolvedEvent, 0, len(resolved))
	for item := range resolved {
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })

	results := make([]model.SearchResult, 0, len(items))
	seenURLs := make(map[string]struct{})
	for _, item := range items {
		result := buildResult(item.event, item.data)
		if len(result.Links) == 0 {
			continue
		}
		allSeen := true
		for _, link := range result.Links {
			key := link.URL + "\x00" + link.Password
			if _, exists := seenURLs[key]; !exists {
				seenURLs[key] = struct{}{}
				allSeen = false
			}
		}
		if allSeen {
			continue
		}
		results = append(results, result)
	}
	if len(results) == 0 && scanErr != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("[%s] read search stream failed: %w", p.Name(), scanErr)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *SopanyaPlugin) resolveEvent(ctx context.Context, client *http.Client, event searchEvent) (resourceData, bool) {
	if directURLs := extractShareURLs(event.URL); len(directURLs) > 0 {
		return resourceData{
			Title:       event.Title,
			URL:         directURLs[0],
			IsType:      event.IsType,
			Content:     strings.Join(directURLs[1:], "\n"),
			Description: event.Description,
		}, true
	}

	payload := map[string]string{
		"url":         url.QueryEscape(event.URL),
		"title":       event.Title,
		"stoken":      "",
		"description": event.Description,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return resourceData{}, false
	}

	first, err := p.callSaveEndpoint(ctx, client, "/api/other/save_url", body)
	if err != nil {
		return resourceData{}, false
	}
	data := first.Data
	if data == nil || len(extractShareURLs(data.URL)) == 0 {
		if isPermanentSaveError(first.Message) {
			return resourceData{}, false
		}
		for i := 0; i < statusPolls; i++ {
			select {
			case <-ctx.Done():
				return resourceData{}, false
			case <-time.After(statusPollDelay):
			}
			status, statusErr := p.callStatusEndpoint(ctx, client, body)
			if statusErr != nil || status.Data == nil || !status.Data.Ready || status.Data.Resource == nil {
				continue
			}
			data = status.Data.Resource
			if len(extractShareURLs(data.URL)) > 0 {
				break
			}
		}
	}
	if data == nil || len(collectDataURLs(*data)) == 0 {
		return resourceData{}, false
	}
	return *data, true
}

func (p *SopanyaPlugin) callSaveEndpoint(ctx context.Context, client *http.Client, path string, body []byte) (saveResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sopanyaBaseURL, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return saveResponse{}, err
	}
	setHeaders(req, false)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return saveResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return saveResponse{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return saveResponse{}, err
	}
	var result saveResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return saveResponse{}, err
	}
	return result, nil
}

func (p *SopanyaPlugin) callStatusEndpoint(ctx context.Context, client *http.Client, body []byte) (statusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sopanyaBaseURL, "/")+"/api/other/save_status", strings.NewReader(string(body)))
	if err != nil {
		return statusResponse{}, err
	}
	setHeaders(req, false)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return statusResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusResponse{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return statusResponse{}, err
	}
	var result statusResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return statusResponse{}, err
	}
	return result, nil
}

func buildResult(event searchEvent, data resourceData) model.SearchResult {
	urls := collectDataURLs(data)
	links := make([]model.Link, 0, len(urls))
	password := extractPassword(data.Code, data.URL, data.Content, data.Description, event.Description)
	linkTime := parseSourceTime(data.UpdateTime)
	if linkTime.IsZero() {
		linkTime = parseSourceTime(data.CreateTime)
	}
	for _, rawURL := range urls {
		linkType := classifyShareURL(rawURL)
		if linkType == "" {
			continue
		}
		linkPassword := passwordFromURL(rawURL)
		if linkPassword == "" {
			linkPassword = password
		}
		links = append(links, model.Link{Type: linkType, URL: rawURL, Password: linkPassword, Datetime: linkTime})
	}
	title := strings.TrimSpace(data.Title)
	if title == "" {
		title = strings.TrimSpace(event.Title)
	}
	content := strings.TrimSpace(data.Description)
	if content == "" {
		content = strings.TrimSpace(event.Description)
	}
	if content == "" {
		content = strings.TrimSpace(data.Content)
	}
	identity := title
	if len(links) > 0 {
		identity = links[0].URL + "\x00" + links[0].Password
	}
	hash := sha256.Sum256([]byte(identity))
	when := linkTime
	if when.IsZero() {
		when = time.Now()
	}
	return model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%x", pluginName, hash[:8]),
		Channel:  "",
		Datetime: when,
		Title:    title,
		Content:  content,
		Links:    links,
	}
}

func collectDataURLs(data resourceData) []string {
	seen := make(map[string]struct{})
	urls := make([]string, 0, 2)
	for _, text := range []string{data.URL, data.Content, data.VODContent} {
		for _, rawURL := range extractShareURLs(text) {
			if _, exists := seen[rawURL]; exists {
				continue
			}
			seen[rawURL] = struct{}{}
			urls = append(urls, rawURL)
		}
	}
	return urls
}

func extractShareURLs(text string) []string {
	text = html.UnescapeString(strings.ReplaceAll(text, `\/`, `/`))
	matches := shareURLRegex.FindAllString(text, -1)
	urls := make([]string, 0, len(matches))
	seen := make(map[string]struct{})
	for _, match := range matches {
		match = strings.TrimRight(match, ".,;:!?)]}>。，；：！？")
		if classifyShareURL(match) == "" {
			continue
		}
		if _, exists := seen[match]; exists {
			continue
		}
		seen[match] = struct{}{}
		urls = append(urls, match)
	}
	return urls
}

func classifyShareURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "pan.quark.cn" || strings.HasSuffix(host, ".quark.cn"):
		return "quark"
	case host == "aliyundrive.com" || strings.HasSuffix(host, ".aliyundrive.com") || host == "alipan.com" || strings.HasSuffix(host, ".alipan.com"):
		return "aliyun"
	case host == "pan.baidu.com" || strings.HasSuffix(host, ".baidu.com") && strings.Contains(host, "pan"):
		return "baidu"
	case host == "drive.uc.cn" || host == "pan.uc.cn" || strings.HasSuffix(host, ".uc.cn"):
		return "uc"
	case host == "pan.xunlei.com" || strings.HasSuffix(host, ".xunlei.com"):
		return "xunlei"
	case host == "cloud.189.cn" || strings.HasSuffix(host, ".189.cn"):
		return "tianyi"
	case host == "115.com" || strings.HasSuffix(host, ".115.com"):
		return "115"
	case host == "123pan.com" || strings.HasSuffix(host, ".123pan.com"):
		return "123"
	case host == "mypikpak.com" || strings.HasSuffix(host, ".mypikpak.com"):
		return "pikpak"
	default:
		return ""
	}
}

func passwordFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for key, values := range u.Query() {
		lowerKey := strings.ToLower(key)
		if lowerKey == "pwd" || lowerKey == "password" || lowerKey == "code" || lowerKey == "passcode" {
			if len(values) > 0 {
				return strings.TrimSpace(values[0])
			}
		}
	}
	return ""
}

func extractPassword(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if password := passwordFromURL(value); password != "" {
			return password
		}
		if match := passwordRegex.FindStringSubmatch(value); len(match) > 1 {
			return strings.TrimSpace(match[1])
		}
	}
	return ""
}

func parseSourceTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil && number > 0 {
		if number > 1e12 {
			number /= 1000
		}
		return time.Unix(int64(number), 0)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}
	}
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func isPermanentSaveError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"封禁", "登录状态异常", "登录异常", "链接受限", "无权限", "forbidden", "unauthorized"} {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

func setHeaders(req *http.Request, stream bool) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", strings.TrimRight(sopanyaBaseURL, "/"))
	req.Header.Set("Referer", strings.TrimRight(sopanyaBaseURL, "/")+"/")
	if stream {
		req.Header.Set("Accept", "text/event-stream, application/json, text/plain, */*")
	} else {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
}

func extendClientTimeout(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Timeout: requestTimeout}
	}
	copy := *client
	if copy.Timeout < requestTimeout {
		copy.Timeout = requestTimeout
	}
	return &copy
}
