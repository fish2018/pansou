package rrbt

import (
	"context"
	"encoding/base64"
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
)

const (
	pluginName     = "rrbt"
	baseURL        = "https://www.rrbts.org"
	defaultPrio    = 4
	maxSearchItem  = 12
	maxConcurrent  = 4
	requestTimeout = 20 * time.Second
)

var (
	shareDateRE = regexp.MustCompile(`分享[:：]\s*(\d{4}-\d{2}-\d{2})`)
	sizeRE      = regexp.MustCompile(`大小[:：]\s*([^\s]+)`)
)

type RRBTPlugin struct {
	*plugin.BaseAsyncPlugin
}

type searchItem struct {
	title string
	url   string
}

func init() {
	plugin.RegisterGlobalPlugin(NewRRBTPlugin())
}

func NewRRBTPlugin() *RRBTPlugin {
	return &RRBTPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPrio)}
}

func (p *RRBTPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *RRBTPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *RRBTPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	searchURL := fmt.Sprintf("%s/?q=%s&re=1", baseURL, url.QueryEscape(keyword))
	doc, err := p.fetchDocument(client, searchURL, baseURL)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	items := make([]searchItem, 0, maxSearchItem)
	seen := make(map[string]struct{})
	doc.Find("#content .pss").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		anchor := s.Find("h2 a").First()
		title := strings.TrimSpace(anchor.Text())
		href := strings.TrimSpace(anchor.AttrOr("href", ""))
		if title == "" || href == "" {
			return true
		}
		if !strings.HasPrefix(href, "http") {
			href = baseURL + "/" + strings.TrimPrefix(href, "/")
		}
		if _, ok := seen[href]; ok {
			return true
		}
		seen[href] = struct{}{}
		items = append(items, searchItem{title: title, url: href})
		return len(items) < maxSearchItem
	})

	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	resultsCh := make(chan model.SearchResult, len(items))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if result, ok := p.fetchDetail(client, item); ok {
				resultsCh <- result
			}
		}()
	}
	wg.Wait()
	close(resultsCh)

	results := make([]model.SearchResult, 0, len(resultsCh))
	for result := range resultsCh {
		results = append(results, result)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *RRBTPlugin) fetchDocument(client *http.Client, target, referer string) (*goquery.Document, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, referer)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 2<<20))
}

func (p *RRBTPlugin) fetchDetail(client *http.Client, item searchItem) (model.SearchResult, bool) {
	doc, err := p.fetchDocument(client, item.url, baseURL)
	if err != nil {
		return model.SearchResult{}, false
	}
	linkAnchor := doc.Find(".down a.red").First()
	rawLink := strings.TrimSpace(linkAnchor.AttrOr("href", ""))
	if rawLink == "" {
		return model.SearchResult{}, false
	}
	shareURL := decodeShareURL(rawLink)
	if !isBaiduShareURL(shareURL) {
		return model.SearchResult{}, false
	}

	title := strings.TrimSpace(doc.Find(".info h1").First().Text())
	if title == "" {
		title = item.title
	}
	directory := strings.TrimSpace(doc.Find(".info .cc").First().Text())
	metaText := strings.TrimSpace(doc.Find(".info").Text())
	content := "来源: 人人BT"
	if directory != "" {
		content += " | " + directory
	}
	if metaText != "" {
		if match := sizeRE.FindStringSubmatch(metaText); len(match) > 1 {
			content += " | 大小: " + match[1]
		}
	}

	datetime := time.Now()
	if match := shareDateRE.FindStringSubmatch(metaText); len(match) > 1 {
		if parsed, parseErr := time.ParseInLocation("2006-01-02", match[1], time.Local); parseErr == nil {
			datetime = parsed
		}
	}
	password := strings.TrimSpace(doc.Find("#tqm").First().Text())
	return model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", pluginName, detailID(item.url)),
		Title:    title,
		Content:  content,
		Channel:  "",
		Datetime: datetime,
		Tags:     []string{"baidu", "rrbt"},
		Links:    []model.Link{{Type: "baidu", URL: shareURL, Password: password}},
	}, true
}

func setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

func decodeShareURL(raw string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	encoded := strings.TrimSpace(u.Query().Get("gourl"))
	if encoded == "" {
		return ""
	}
	encoded = strings.TrimSuffix(encoded, "_@_")
	if strings.HasPrefix(encoded, "M6") {
		encoded = encoded[2:]
	}
	encoded = strings.TrimRight(encoded, "=")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.ReplaceAll(encoded, "-", "+"))
	}
	if err != nil {
		return ""
	}
	value := string(decoded)
	if strings.HasPrefix(value, "//") {
		return "https:" + value
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return ""
}

func isBaiduShareURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return (host == "pan.baidu.com" || host == "yun.baidu.com") && strings.HasPrefix(u.Path, "/s/")
}

func detailID(value string) string {
	value = strings.TrimSuffix(value, "/")
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		id := strings.TrimSuffix(value[idx+1:], ".html")
		if id != "" {
			return id
		}
	}
	return fmt.Sprintf("%x", len(value))
}
