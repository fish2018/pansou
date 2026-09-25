package quarktv

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

const (
	pluginName      = "quarktv"
	baseURL         = "https://www.quarktv.com"
	searchURL       = baseURL + "/?s=%s"
	postKitAPI      = baseURL + "/wp-json/postkit/v1/pan_view?post_id=%s&visitor_id=%s"
	defaultPriority = 2
	searchTimeout   = 15 * time.Second
	detailTimeout   = 10 * time.Second
	maxRetries      = 3
	maxConcurrency  = 10
)

var (
	postIDRegex = regexp.MustCompile(`/(\d+)\.html?$`)
	spaceRegex  = regexp.MustCompile(`\s+`)
)

type QuarkTVPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type articleItem struct {
	ID         string
	Title      string
	DetailURL  string
	ImageURL   string
	Summary    string
	Category   string
	PublishRaw string
}

type postkitResponse struct {
	PostID    int               `json:"post_id"`
	Resources []postkitResource `json:"resources"`
}

type postkitResource struct {
	ID      int    `json:"id"`
	Cls     string `json:"cls"`
	PanName string `json:"pan_name"`
	URL     string `json:"url"`
	Qrcode  string `json:"qrcode"`
	IsBT    bool   `json:"is_bt"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewQuarkTVPlugin())
}

func NewQuarkTVPlugin() *QuarkTVPlugin {
	return &QuarkTVPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client: &http.Client{
			Timeout: searchTimeout,
			Transport: &http.Transport{
				Proxy:               util.ProxyFuncForTransport(),
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				MaxConnsPerHost:     24,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *QuarkTVPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *QuarkTVPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *QuarkTVPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	items, err := p.fetchSearchResults(client, keyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, maxConcurrency)
		results = make([]model.SearchResult, 0, len(items))
	)

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}

		go func(item articleItem) {
			defer wg.Done()
			defer func() { <-sem }()

			resourceLinks, content, images := p.fetchDetailLinks(client, item)
			if len(resourceLinks) == 0 {
				return
			}

			tags := make([]string, 0, 1)
			if item.Category != "" {
				tags = append(tags, item.Category)
			}

			if content == "" {
				content = item.Summary
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
				Title:    item.Title,
				Content:  cleanText(content),
				Links:    resourceLinks,
				Tags:     tags,
				Channel:  "",
				Datetime: parseQuarkTVTime(item.PublishRaw),
				Images:   images,
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *QuarkTVPlugin) fetchSearchResults(client *http.Client, keyword string) ([]articleItem, error) {
	requestURL := fmt.Sprintf(searchURL, url.QueryEscape(keyword))
	doc, err := fetchDocument(client, requestURL, searchTimeout, baseURL+"/")
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	items := make([]articleItem, 0)
	doc.Find("article.excerpt").Each(func(_ int, s *goquery.Selection) {
		titleLink := s.Find("header h2 a").First()
		title := cleanText(titleLink.Text())
		detailURL, ok := titleLink.Attr("href")
		if !ok || title == "" || detailURL == "" {
			return
		}

		id := extractPostID(detailURL)
		if id == "" {
			return
		}

		imageURL := ""
		if img := s.Find("a.focus img").First(); img.Length() > 0 {
			imageURL = strings.TrimSpace(img.AttrOr("data-src", img.AttrOr("src", "")))
		}

		items = append(items, articleItem{
			ID:         id,
			Title:      title,
			DetailURL:  detailURL,
			ImageURL:   imageURL,
			Summary:    cleanText(s.Find("p.note").Text()),
			Category:   cleanText(s.Find("div.meta a.cat").Text()),
			PublishRaw: cleanText(s.Find("div.meta time").First().Text()),
		})
	})

	return items, nil
}

func (p *QuarkTVPlugin) fetchDetailLinks(client *http.Client, item articleItem) ([]model.Link, string, []string) {
	doc, err := fetchDocument(client, item.DetailURL, detailTimeout, baseURL+"/")
	if err != nil {
		return nil, "", nil
	}

	postkit := doc.Find("div.postkit").First()
	postID := strings.TrimSpace(postkit.AttrOr("data-postid", item.ID))
	if postID == "" {
		postID = item.ID
	}

	apiURL := fmt.Sprintf(postKitAPI, postID, generateVisitorID())
	body, err := fetchBody(client, apiURL, detailTimeout, item.DetailURL)
	if err != nil {
		return nil, "", nil
	}

	var apiResp postkitResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, "", nil
	}

	links := make([]model.Link, 0, len(apiResp.Resources))
	images := make([]string, 0, len(apiResp.Resources)+1)
	seen := make(map[string]struct{})

	if item.ImageURL != "" {
		images = append(images, item.ImageURL)
	}

	for _, resource := range apiResp.Resources {
		rawURL := strings.TrimSpace(resource.URL)
		if rawURL == "" {
			continue
		}

		link := model.Link{
			Type:     determineQuarkTVType(resource, rawURL),
			URL:      rawURL,
			Password: extractURLPassword(rawURL),
		}
		key := link.Type + "|" + link.URL
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		links = append(links, link)

		if qr := strings.TrimSpace(resource.Qrcode); qr != "" {
			images = append(images, qr)
		}
	}

	content := cleanText(doc.Find("div.postkit_art div.intro").Text())
	if content == "" {
		content = cleanText(doc.Find("article.article-content").Text())
	}

	return links, content, dedupeStrings(images)
}

func determineQuarkTVType(resource postkitResource, rawURL string) string {
	if resource.IsBT || strings.HasPrefix(strings.ToLower(rawURL), "magnet:") {
		return "magnet"
	}

	switch strings.ToLower(strings.TrimSpace(resource.Cls)) {
	case "quark":
		return "quark"
	case "baidu":
		return "baidu"
	case "uc":
		return "uc"
	case "xunlei":
		return "xunlei"
	case "aliyun", "alipan", "ali":
		return "aliyun"
	}

	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lower, "alipan.com"), strings.Contains(lower, "aliyundrive.com"):
		return "aliyun"
	default:
		return "other"
	}
}

func generateVisitorID() string {
	return fmt.Sprintf("pk_%d", time.Now().UnixNano())
}

func parseQuarkTVTime(raw string) time.Time {
	raw = cleanText(raw)
	if raw == "" {
		return time.Now()
	}

	layouts := []string{
		"2006-01-02",
		"2006-01-02 15:04:05",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed
		}
	}
	return time.Now()
}

func extractPostID(detailURL string) string {
	if matches := postIDRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func dedupeStrings(items []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func cleanText(text string) string {
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	return strings.TrimSpace(spaceRegex.ReplaceAllString(text, " "))
}

func extractURLPassword(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, key := range []string{"pwd", "passcode", "code"} {
		if value := strings.TrimSpace(u.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func fetchDocument(client *http.Client, requestURL string, timeout time.Duration, referer string) (*goquery.Document, error) {
	body, err := fetchBody(client, requestURL, timeout, referer)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(strings.NewReader(string(body)))
}

func fetchBody(client *http.Client, requestURL string, timeout time.Duration, referer string) ([]byte, error) {
	var body []byte

	// 重试逻辑收敛到 util.DoWithRetry。关键语义：原实现每次尝试各自 context.WithTimeout，
	// 即超时按次计算而不是整轮共享——ctx/cancel 必须建在闭包内（defer cancel 随闭包返回释放，
	// 不跨轮累积）。退避仍是 200ms x 2^attempt。
	err := util.DoWithRetry(util.RetryConfig{
		Attempts:   maxRetries,
		BaseDelay:  200 * time.Millisecond,
		Multiplier: 2,
	}, func(_ int) error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json,text/plain,*/*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Referer", referer)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			status := resp.StatusCode
			resp.Body.Close()
			return fmt.Errorf("HTTP %d", status)
		}
		data, readErr := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		body = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}
