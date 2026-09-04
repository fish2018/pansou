package jupansou

import (
	"bufio"
	"context"
	"crypto/md5"
	stdjson "encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

const (
	jupansouPluginName      = "jupansou"
	jupansouBaseURL         = "https://dyuzi.com"
	jupansouAPIURL          = jupansouBaseURL + "/api/search/stream?keyword=%s&type=all"
	jupansouDefaultPriority = 3
	jupansouTimeout         = 20 * time.Second
	jupansouStreamTimeout   = 3 * time.Second
	jupansouMaxRetries      = 3
)

type JuPansouPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type juPansouStreamItem struct {
	Title    string `json:"title"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	DiskType string `json:"disk_type"`
	IsType   int    `json:"is_type"`
}

type juPansouTransferResponse struct {
	Success bool `json:"success"`
	Data    struct {
		ShareURL string `json:"share_url"`
		Password string `json:"pwd"`
		FileName string `json:"file_name"`
	} `json:"data"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewJuPansouPlugin())
}

func NewJuPansouPlugin() *JuPansouPlugin {
	return &JuPansouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(jupansouPluginName, jupansouDefaultPriority),
		client: &http.Client{
			// The site issues a search_token cookie during session bootstrap.
			// Keep it on the client for the subsequent stream and transfer calls.
			Jar: func() http.CookieJar {
				jar, _ := cookiejar.New(nil)
				return jar
			}(),
			Timeout: jupansouTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				MaxConnsPerHost:     16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *JuPansouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *JuPansouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *JuPansouPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	searchURL := fmt.Sprintf(jupansouAPIURL, url.QueryEscape(keyword))
	if err := p.ensureSearchSession(client); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), jupansouStreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", jupansouBaseURL+"/")
	req.Header.Set("Origin", jupansouBaseURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := doJuPansouRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 接口返回状态码: %d", p.Name(), resp.StatusCode)
	}

	items := make([]juPansouStreamItem, 0)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var item juPansouStreamItem
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			continue
		}
		item.Title = strings.TrimSpace(item.Title)
		item.Name = strings.TrimSpace(item.Name)
		item.URL = strings.TrimSpace(item.URL)
		if item.Title == "" || item.URL == "" {
			continue
		}
		items = append(items, item)
	}

	if err := scanner.Err(); err != nil && len(items) == 0 {
		return nil, fmt.Errorf("[%s] 读取流式结果失败: %w", p.Name(), err)
	}

	// The stream may contain broad third-party lines. Filter by title before
	// exchanging encrypted URLs to avoid unnecessary transfer requests.
	keywordLower := strings.ToLower(strings.TrimSpace(keyword))
	filteredItems := items[:0]
	for _, item := range items {
		if keywordLower == "" || strings.Contains(strings.ToLower(item.Title), keywordLower) {
			filteredItems = append(filteredItems, item)
		}
	}

	results := p.exchangeItems(client, filteredItems)
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *JuPansouPlugin) ensureSearchSession(client *http.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), jupansouTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jupansouBaseURL+"/api/search/session", nil)
	if err != nil {
		return fmt.Errorf("[%s] 创建搜索会话请求失败: %w", p.Name(), err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", jupansouBaseURL+"/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("[%s] 搜索会话请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("[%s] 搜索会话返回状态码: %d", p.Name(), resp.StatusCode)
	}
	return nil
}

func (p *JuPansouPlugin) exchangeItems(client *http.Client, items []juPansouStreamItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	seen := make(map[string]struct{})
	for _, item := range items {
		item := item
		if item.URL == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			shareURL, password := p.exchangeURL(client, item)
			if shareURL == "" {
				return
			}
			linkType := mapJuPansouLinkType(item.IsType, item.DiskType, shareURL)
			sum := md5.Sum([]byte(shareURL))
			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%x", p.Name(), sum),
				Title:    item.Title,
				Content:  "来源: 聚盘搜",
				Channel:  "",
				Datetime: time.Now(),
				Tags:     []string{linkType},
				Links:    []model.Link{{Type: linkType, URL: shareURL, Password: password}},
			}
			mu.Lock()
			if _, exists := seen[shareURL]; !exists {
				seen[shareURL] = struct{}{}
				results = append(results, result)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func (p *JuPansouPlugin) exchangeURL(client *http.Client, item juPansouStreamItem) (string, string) {
	if strings.HasPrefix(item.URL, "http://") || strings.HasPrefix(item.URL, "https://") {
		return item.URL, extractJuPansouPassword(item.URL)
	}
	var body strings.Builder
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("link", item.URL)
	if item.DiskType != "" {
		_ = writer.WriteField("type", item.DiskType)
	}
	_ = writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), jupansouTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, jupansouBaseURL+"/api/transfer", strings.NewReader(body.String()))
	if err != nil {
		return "", ""
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", jupansouBaseURL)
	req.Header.Set("Referer", jupansouBaseURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	var payload juPansouTransferResponse
	if err := stdjson.NewDecoder(resp.Body).Decode(&payload); err != nil || !payload.Success {
		return "", ""
	}
	return strings.TrimSpace(payload.Data.ShareURL), strings.TrimSpace(payload.Data.Password)
}

func mapJuPansouLinkType(isType int, diskType, rawURL string) string {
	if diskType != "" {
		switch strings.ToLower(strings.TrimSpace(diskType)) {
		case "quark", "baidu", "aliyun", "uc", "xunlei", "tianyi", "115", "123", "mobile", "pikpak", "magnet", "ed2k":
			return strings.ToLower(strings.TrimSpace(diskType))
		}
	}
	switch isType {
	case 0:
		return "quark"
	case 1:
		return "aliyun"
	case 2:
		return "baidu"
	case 3:
		return "uc"
	case 4:
		return "xunlei"
	default:
		urlValue := strings.ToLower(rawURL)
		switch {
		case strings.Contains(urlValue, "pan.quark.cn"):
			return "quark"
		case strings.Contains(urlValue, "pan.baidu.com"):
			return "baidu"
		case strings.Contains(urlValue, "alipan.com"), strings.Contains(urlValue, "aliyundrive.com"):
			return "aliyun"
		case strings.Contains(urlValue, "drive.uc.cn"):
			return "uc"
		case strings.Contains(urlValue, "pan.xunlei.com"):
			return "xunlei"
		default:
			return "others"
		}
	}
}

func extractJuPansouPassword(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, key := range []string{"pwd", "code", "passcode"} {
		if value := strings.TrimSpace(u.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func doJuPansouRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < jupansouMaxRetries; attempt++ {
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		if lastErr == nil {
			lastErr = fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
		}
		if attempt < jupansouMaxRetries-1 {
			time.Sleep(200 * time.Millisecond * time.Duration(1<<attempt))
		}
	}
	return nil, fmt.Errorf("重试 %d 次后失败: %w", jupansouMaxRetries, lastErr)
}
