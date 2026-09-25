package gaoqing888

import (
	"context"
	"fmt"
	"io"
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
)

const (
	pluginName      = "gaoqing888"
	baseURL         = "https://www.gaoqing888.com"
	searchURL       = baseURL + "/search?kw=%s"
	defaultPriority = 3
	searchTimeout   = 15 * time.Second
	detailTimeout   = 10 * time.Second
	maxRetries      = 3
	maxConcurrency  = 8
)

var (
	detailIDRegex = regexp.MustCompile(`/(\d+)/detail`)
	magnetRegex   = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9A-Za-z]+[^"' <]*`)
	spaceRegex    = regexp.MustCompile(`\s+`)
)

type Gaoqing888Plugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type articleItem struct {
	ID        string
	Title     string
	DetailURL string
	ImageURL  string
	Content   string
}

func init() {
	plugin.RegisterGlobalPlugin(NewGaoqing888Plugin())
}

func NewGaoqing888Plugin() *Gaoqing888Plugin {
	return &Gaoqing888Plugin{
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

func (p *Gaoqing888Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *Gaoqing888Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Gaoqing888Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
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

			links, content, tags := p.fetchDetail(client, item.DetailURL)
			if len(links) == 0 {
				return
			}

			if content == "" {
				content = item.Content
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
				Title:    item.Title,
				Content:  content,
				Links:    links,
				Tags:     tags,
				Images:   dedupeStrings([]string{item.ImageURL}),
				Channel:  "",
				Datetime: time.Now(),
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *Gaoqing888Plugin) fetchSearchResults(client *http.Client, keyword string) ([]articleItem, error) {
	requestURL := fmt.Sprintf(searchURL, url.QueryEscape(keyword))
	doc, err := fetchDocument(client, requestURL, searchTimeout, baseURL+"/")
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	items := make([]articleItem, 0)
	doc.Find("div.wp-list.search-list div.video-row").Each(func(_ int, s *goquery.Selection) {
		titleLink := s.Find("a.title-link").First()
		title := cleanText(titleLink.Text())
		detailURL, ok := titleLink.Attr("href")
		if !ok || title == "" || detailURL == "" {
			return
		}

		id := extractDetailID(detailURL)
		if id == "" {
			return
		}

		imageURL := strings.TrimSpace(s.Find("a.cover-link img.cover").AttrOr("src", ""))
		content := cleanText(strings.Join([]string{
			s.Find("div.meta").Eq(0).Text(),
			s.Find("div.meta").Eq(1).Text(),
		}, " | "))

		items = append(items, articleItem{
			ID:        id,
			Title:     title,
			DetailURL: detailURL,
			ImageURL:  imageURL,
			Content:   content,
		})
	})

	return items, nil
}

func (p *Gaoqing888Plugin) fetchDetail(client *http.Client, detailURL string) ([]model.Link, string, []string) {
	doc, err := fetchDocument(client, detailURL, detailTimeout, baseURL+"/")
	if err != nil {
		return nil, "", nil
	}

	links := make([]model.Link, 0)
	seen := make(map[string]struct{})
	add := func(link model.Link) {
		if link.URL == "" {
			return
		}
		key := link.Type + "|" + link.URL
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		links = append(links, link)
	}

	doc.Find("ul.playlist a[href], div.wp-download a[href]").Each(func(_ int, s *goquery.Selection) {
		href, ok := s.Attr("href")
		if !ok {
			return
		}
		link := parseGaoqingLink(href)
		if link.URL == "" {
			return
		}
		add(link)
	})

	html, _ := doc.Html()
	for _, magnet := range magnetRegex.FindAllString(html, -1) {
		add(model.Link{Type: "magnet", URL: magnet})
	}

	if len(links) == 0 {
		return nil, "", nil
	}

	content := cleanText(doc.Find("div.wp-content.video-detail p").First().Text())
	if content == "" {
		content = cleanText(doc.Find("meta[name='description']").AttrOr("content", ""))
	}

	tags := []string{}
	if typ := cleanText(doc.Find("div.info ul li").Eq(3).Text()); typ != "" {
		tags = append(tags, typ)
	}

	return links, content, tags
}

func parseGaoqingLink(raw string) model.Link {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return model.Link{}
	}

	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		return model.Link{Type: "magnet", URL: raw}
	}

	if strings.Contains(raw, "/go/play?") {
		u, err := url.Parse(raw)
		if err == nil {
			target := strings.TrimSpace(u.Query().Get("url"))
			if target != "" {
				raw = target
			}
		}
	}

	lower := strings.ToLower(raw)
	switch {
	case strings.Contains(lower, "pan.quark.cn"):
		return model.Link{Type: "quark", URL: raw, Password: extractURLPassword(raw)}
	case strings.Contains(lower, "pan.baidu.com"):
		return model.Link{Type: "baidu", URL: raw, Password: extractURLPassword(raw)}
	case strings.Contains(lower, "pan.xunlei.com"):
		return model.Link{Type: "xunlei", URL: raw, Password: extractURLPassword(raw)}
	case strings.Contains(lower, "alipan.com"), strings.Contains(lower, "aliyundrive.com"):
		return model.Link{Type: "aliyun", URL: raw, Password: extractURLPassword(raw)}
	case strings.Contains(lower, "drive.uc.cn"):
		return model.Link{Type: "uc", URL: raw, Password: extractURLPassword(raw)}
	default:
		return model.Link{}
	}
}

func extractDetailID(detailURL string) string {
	if matches := detailIDRegex.FindStringSubmatch(detailURL); len(matches) >= 2 {
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
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Referer", referer)

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			data, readErr := io.ReadAll(resp.Body)
			cancel()
			return data, readErr
		}
		if resp != nil {
			resp.Body.Close()
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		cancel()
		if attempt < maxRetries-1 {
			time.Sleep(200 * time.Millisecond * time.Duration(1<<attempt))
		}
	}
	return nil, lastErr
}
