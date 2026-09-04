package ys5266

import (
	"bytes"
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
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName     = "5266ys"
	baseURL        = "https://www.5266ys.net"
	searchPath     = "/e/search/index.php"
	requestTimeout = 20 * time.Second
	maxResults     = 50
	maxConcurrency = 8
)

var (
	detailIDRE = regexp.MustCompile(`/([0-9]+)\.(?:htm|html?)$`)
	dateRE     = regexp.MustCompile(`20[0-9]{2}[-/]([0-9]{1,2})[-/]([0-9]{1,2})`)
)

type searchItem struct {
	id        string
	title     string
	detailURL string
	date      time.Time
}

type magnetItem struct {
	url      string
	subtitle string
}

type Plugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

func init() {
	plugin.RegisterGlobalPlugin(NewPlugin())
}

func NewPlugin() *Plugin {
	return &Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter(pluginName, 3, true),
		client:          &http.Client{Timeout: requestTimeout},
	}
}

func (p *Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *Plugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if p.client != nil {
		client = p.client
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	doc, err := p.fetchSearch(client, keyword)
	if err != nil {
		return nil, err
	}
	items := parseSearchItems(doc)
	if len(items) > maxResults {
		items = items[:maxResults]
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	results := make([]model.SearchResult, 0, len(items))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrency)
	for _, item := range items {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			detail, detailText, imageURL := p.fetchDetail(client, item.detailURL)
			if len(detail) == 0 {
				return
			}
			for index, magnet := range detail {
				id := item.id
				if id == "" {
					id = fmt.Sprintf("%x", fnv32(item.detailURL))
				}
				magnetID := magnetHash(magnet.url)
				if magnetID == "" {
					magnetID = fmt.Sprintf("%d", index)
				} else {
					magnetID = fmt.Sprintf("%s-%x", magnetID, fnv32(magnet.url))
				}
				title := item.title
				if title == "" {
					title = "未知影片"
				}
				if magnet.subtitle != "" {
					title += " - " + magnet.subtitle
				}
				content := detailText
				if content == "" {
					content = "来源：66影视"
				}
				result := model.SearchResult{
					UniqueID:  fmt.Sprintf("%s-%s-%s", p.Name(), id, magnetID),
					MessageID: fmt.Sprintf("%s-%s-%s", p.Name(), id, magnetID),
					Title:     title,
					Content:   content,
					Channel:   "",
					Datetime:  item.date,
					Links: []model.Link{{
						Type:      "magnet",
						URL:       magnet.url,
						WorkTitle: item.title,
					}},
				}
				if imageURL != "" {
					result.Images = []string{imageURL}
				}
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *Plugin) fetchSearch(client *http.Client, keyword string) (*goquery.Document, error) {
	encoded, err := encodeGB18030(keyword)
	if err != nil {
		return nil, fmt.Errorf("[%s] 编码搜索关键词失败: %w", p.Name(), err)
	}
	form := "show=title%2Csmalltext&tempid=1&tbname=article&keyboard=" + url.QueryEscape(string(encoded)) + "&submit="
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+searchPath, strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setHeaders(req, baseURL+"/")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索请求返回 HTTP %d", p.Name(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索结果失败: %w", p.Name(), err)
	}
	decoded, err := decodeGB18030(body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解码搜索结果失败: %w", p.Name(), err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}
	return doc, nil
}

func (p *Plugin) fetchDetail(client *http.Client, detailURL string) ([]magnetItem, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, detailURL, nil)
	if err != nil {
		return nil, "", ""
	}
	setHeaders(req, baseURL+"/")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return nil, "", ""
	}
	decoded, err := decodeGB18030(body)
	if err != nil {
		return nil, "", ""
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(decoded))
	if err != nil {
		return nil, "", ""
	}

	links := extractMagnets(doc)
	contentNode := doc.Find(".contentinfo").First()
	if contentNode.Length() == 0 {
		contentNode = doc.Find(".mainleft").First()
	}
	if contentNode.Length() == 0 {
		contentNode = doc.Find("body").First()
	}
	content := cleanText(contentNode.Text())
	if len(content) > 500 {
		content = content[:500] + "..."
	}
	imageURL := firstContentImage(doc)
	return links, content, imageURL
}

func parseSearchItems(doc *goquery.Document) []searchItem {
	items := make([]searchItem, 0)
	seen := make(map[string]struct{})
	doc.Find(".listBox li").Each(func(_ int, s *goquery.Selection) {
		anchor := s.Find("h3 a").First()
		if anchor.Length() == 0 {
			anchor = s.Find("a[href]").First()
		}
		item := makeSearchItem(anchor, s.Text())
		if item.detailURL == "" || item.title == "" {
			return
		}
		if _, ok := seen[item.detailURL]; ok {
			return
		}
		seen[item.detailURL] = struct{}{}
		items = append(items, item)
	})
	return items
}

func makeSearchItem(anchor *goquery.Selection, text string) searchItem {
	if anchor == nil || anchor.Length() == 0 {
		return searchItem{}
	}
	title := cleanText(anchor.Text())
	href := strings.TrimSpace(anchor.AttrOr("href", ""))
	if href == "" {
		return searchItem{}
	}
	fullURL := absoluteURL(href)
	id := ""
	if match := detailIDRE.FindStringSubmatch(fullURL); len(match) > 1 {
		id = match[1]
	}
	date := time.Now()
	if match := dateRE.FindStringSubmatch(text); len(match) > 0 {
		if parsed, err := time.ParseInLocation("2006-1-2", strings.ReplaceAll(match[0], "/", "-"), time.Local); err == nil {
			date = parsed
		}
	}
	return searchItem{id: id, title: title, detailURL: fullURL, date: date}
}

func extractMagnets(doc *goquery.Document) []magnetItem {
	links := make([]magnetItem, 0)
	seen := make(map[string]struct{})
	doc.Find("a[href]").Each(func(_ int, a *goquery.Selection) {
		href := strings.TrimSpace(a.AttrOr("href", ""))
		if !strings.HasPrefix(strings.ToLower(href), "magnet:?") {
			return
		}
		if _, ok := seen[href]; ok {
			return
		}
		seen[href] = struct{}{}
		subtitle := cleanText(a.Text())
		if subtitle == "" {
			if parsed, err := url.Parse(href); err == nil {
				subtitle = cleanText(parsed.Query().Get("dn"))
			}
		}
		links = append(links, magnetItem{url: href, subtitle: subtitle})
	})
	return links
}

func magnetHash(raw string) string {
	match := regexp.MustCompile(`(?i)btih:([a-z0-9]+)`).FindStringSubmatch(raw)
	if len(match) > 1 {
		return strings.ToLower(match[1])
	}
	return ""
}

func encodeGB18030(value string) ([]byte, error) {
	reader := transform.NewReader(strings.NewReader(value), simplifiedchinese.GB18030.NewEncoder())
	return io.ReadAll(reader)
}

func decodeGB18030(value []byte) (string, error) {
	reader := transform.NewReader(bytes.NewReader(value), simplifiedchinese.GB18030.NewDecoder())
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func setHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", referer)
}

func absoluteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "magnet:") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return baseURL + "/" + strings.TrimPrefix(raw, "/")
}

func cleanText(value string) string {
	value = strings.ReplaceAll(value, "\u00a0", " ")
	value = strings.Join(strings.Fields(value), " ")
	return strings.TrimSpace(value)
}

func firstContentImage(doc *goquery.Document) string {
	var imageURL string
	doc.Find("#text img, .contentinfo img, #dede_content img, img").EachWithBreak(func(_ int, image *goquery.Selection) bool {
		src := absoluteURL(image.AttrOr("src", ""))
		if src == "" || strings.Contains(strings.ToLower(src), "logo") {
			return true
		}
		imageURL = src
		return false
	})
	return imageURL
}

func fnv32(value string) uint32 {
	var hash uint32 = 2166136261
	for i := 0; i < len(value); i++ {
		hash ^= uint32(value[i])
		hash *= 16777619
	}
	return hash
}
