package xiaoyu

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName         = "xiaoyu"
	baseURL            = "https://xykmovie.com"
	searchURL          = baseURL + "/s/%d/%s"
	defaultPriority    = 3
	requestTimeout     = 15 * time.Second
	maxRetries         = 2
	maxPages           = 10
	maxPageConcurrency = 5
	maxResponseSize    = 2 << 20
)

var (
	spaceRegex = regexp.MustCompile(`\s+`)
	urlRegex   = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

type XiaoyuPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type pageResult struct {
	Results    []model.SearchResult
	TotalPages int
}

func init() {
	plugin.RegisterGlobalPlugin(NewXiaoyuPlugin())
}

func NewXiaoyuPlugin() *XiaoyuPlugin {
	return &XiaoyuPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		client: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				MaxConnsPerHost:     maxPageConcurrency,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *XiaoyuPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *XiaoyuPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *XiaoyuPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	keyword = cleanText(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if p.client != nil {
		client = p.client
	}

	firstPage, err := p.fetchPage(client, keyword, 1)
	if err != nil {
		return nil, err
	}

	totalPages := firstPage.TotalPages
	if totalPages < 1 {
		totalPages = 1
	}
	if totalPages > maxPages {
		totalPages = maxPages
	}

	pages := make([][]model.SearchResult, totalPages)
	pages[0] = firstPage.Results
	if totalPages > 1 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxPageConcurrency)

		for page := 2; page <= totalPages; page++ {
			wg.Add(1)
			go func(page int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				result, pageErr := p.fetchPage(client, keyword, page)
				if pageErr == nil {
					pages[page-1] = result.Results
				}
			}(page)
		}
		wg.Wait()
	}

	results := make([]model.SearchResult, 0)
	seen := make(map[string]struct{})
	for _, page := range pages {
		for _, result := range page {
			if len(result.Links) == 0 {
				continue
			}
			key := result.Links[0].URL + "\x00" + result.Links[0].Password
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			results = append(results, result)
		}
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *XiaoyuPlugin) fetchPage(client *http.Client, keyword string, page int) (pageResult, error) {
	requestURL := fmt.Sprintf(searchURL, page, url.PathEscape(keyword))
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return pageResult{}, fmt.Errorf("[%s] 创建第%d页请求失败: %w", p.Name(), page, err)
	}
	setRequestHeaders(req)

	resp, err := doRequestWithRetry(client, req)
	if err != nil {
		return pageResult{}, fmt.Errorf("[%s] 第%d页搜索请求失败: %w", p.Name(), page, err)
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return pageResult{}, fmt.Errorf("[%s] 第%d页解析失败: %w", p.Name(), page, err)
	}

	return parsePage(doc), nil
}

func setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", baseURL+"/")
}

func doRequestWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}

		resp, err := client.Do(req.Clone(req.Context()))
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		lastErr = fmt.Errorf("状态码 %d", resp.StatusCode)
		resp.Body.Close()
	}
	return nil, lastErr
}

func parsePage(doc *goquery.Document) pageResult {
	result := pageResult{TotalPages: parseTotalPages(doc)}
	doc.Find(".search-list .item").Each(func(_ int, item *goquery.Selection) {
		parsed, ok := parseItem(item)
		if ok {
			result.Results = append(result.Results, parsed)
		}
	})
	return result
}

func parseTotalPages(doc *goquery.Document) int {
	pageText := cleanText(doc.Find(".count strong").Eq(1).Text())
	parts := strings.Split(pageText, "/")
	if len(parts) != 2 {
		return 1
	}
	total, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || total < 1 {
		return 1
	}
	return total
}

func parseItem(item *goquery.Selection) (model.SearchResult, bool) {
	openLink := item.Find(".name a.open").First()
	title := cleanText(openLink.Text())
	if title == "" {
		return model.SearchResult{}, false
	}

	rawURL := decodeDataURL(openLink.AttrOr("data-url", ""))
	copyText := html.UnescapeString(item.Find("a.copy[data-code]").First().AttrOr("data-code", ""))
	if rawURL == "" {
		rawURL = urlRegex.FindString(copyText)
	}
	linkType, canonicalURL := normalizeLink(rawURL)
	if canonicalURL == "" {
		return model.SearchResult{}, false
	}

	password := cleanPassword(openLink.AttrOr("data-code", ""))
	if password == "" {
		password = extractPassword(canonicalURL, copyText)
	}

	id := cleanText(openLink.AttrOr("data-id", ""))
	if id == "" {
		hash := sha256.Sum256([]byte(title + "\x00" + canonicalURL))
		id = fmt.Sprintf("%x", hash[:8])
	}

	datetime := time.Now()
	if value := cleanText(item.Find(".atips").First().Text()); value != "" {
		if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, time.Local); err == nil {
			datetime = parsed
		}
	}

	content := cleanText(item.Find(".atips").Eq(1).Text())
	content = strings.TrimSpace(strings.TrimPrefix(content, "【内容】："))

	return model.SearchResult{
		MessageID: fmt.Sprintf("%s-%s", pluginName, id),
		UniqueID:  fmt.Sprintf("%s-%s", pluginName, id),
		Channel:   "",
		Datetime:  datetime,
		Title:     title,
		Content:   content,
		Links: []model.Link{{
			Type:      linkType,
			URL:       canonicalURL,
			Password:  password,
			WorkTitle: title,
		}},
	}, true
}

func decodeDataURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(value)
		if err != nil {
			return ""
		}
	}

	result := strings.TrimSpace(string(decoded))
	if result != "" && !strings.Contains(result, "://") {
		result = "https://" + strings.TrimLeft(result, "/")
	}
	return result
}

func normalizeLink(raw string) (string, string) {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if raw == "" {
		return "", ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", ""
	}

	host := strings.ToLower(parsed.Hostname())
	switch {
	case host == "pan.quark.cn":
		return "quark", parsed.String()
	case host == "pan.baidu.com":
		return "baidu", parsed.String()
	case host == "pan.xunlei.com":
		return "xunlei", parsed.String()
	case host == "drive.uc.cn":
		return "uc", parsed.String()
	case host == "www.alipan.com" || host == "alipan.com" || host == "www.aliyundrive.com" || host == "aliyundrive.com":
		return "aliyun", parsed.String()
	case host == "cloud.189.cn":
		return "tianyi", parsed.String()
	case host == "115.com" || host == "115cdn.com":
		return "115", parsed.String()
	case host == "www.123pan.com" || host == "123pan.com" || host == "www.123684.com" || host == "123684.com":
		return "123", parsed.String()
	case host == "caiyun.139.com":
		return "mobile", parsed.String()
	case host == "mypikpak.com":
		return "pikpak", parsed.String()
	default:
		return "", ""
	}
}

func extractPassword(linkURL string, nearbyText string) string {
	if parsed, err := url.Parse(linkURL); err == nil {
		for _, key := range []string{"pwd", "password", "code"} {
			if value := cleanPassword(parsed.Query().Get(key)); value != "" {
				return value
			}
		}
	}

	for _, marker := range []string{"提取码:", "提取码：", "访问码:", "访问码：", "密码:", "密码："} {
		if index := strings.Index(nearbyText, marker); index >= 0 {
			value := nearbyText[index+len(marker):]
			if fields := strings.Fields(value); len(fields) > 0 {
				return cleanPassword(fields[0])
			}
		}
	}
	return ""
}

func cleanPassword(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.Trim(value, "#，,。.;；:：()（）[]【】")
}

func cleanText(value string) string {
	value = html.UnescapeString(value)
	return strings.TrimSpace(spaceRegex.ReplaceAllString(value, " "))
}
