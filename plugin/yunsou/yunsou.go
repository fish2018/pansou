package yunsou

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName        = "yunsou"
	searchURLTemplate = "https://wpys.cc/s/%s.html"
	defaultPriority   = 2
	defaultTimeout    = 30 * time.Second
	maxRetries        = 3
	maxResults        = 100
	maxPages          = 10
	timeLayout        = "2006-01-02"
)

var (
	// 分享链接由 onclick="copyText(...,'url','pwd')" 提供。
	copyTextRegex = regexp.MustCompile(`copyText\([^,]+,\s*'[^']*',\s*'([^']+)',\s*'([^']*)'`)
	pwdParamRegex = regexp.MustCompile(`[?&]pwd=([0-9a-zA-Z]+)`)
)

// YunsouAsyncPlugin 是云搜（现无朋盘搜）异步搜索插件。
type YunsouAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	optimizedClient *http.Client
}

func init() { plugin.RegisterGlobalPlugin(NewYunsouAsyncPlugin()) }

func NewYunsouAsyncPlugin() *YunsouAsyncPlugin {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
	}
	return &YunsouAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		optimizedClient: &http.Client{Transport: transport, Timeout: defaultTimeout},
	}
}

func (p *YunsouAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *YunsouAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *YunsouAsyncPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if p.optimizedClient != nil {
		client = p.optimizedClient
	}
	if client == nil {
		client = http.DefaultClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	doc, err := p.fetchPage(ctx, client, keyword, 1)
	if err != nil {
		return nil, err
	}
	results := p.parseSearchResults(doc)
	total := 0
	if rawTotal, ok := doc.Find("el-pagination").First().Attr(":total"); ok {
		fmt.Sscanf(rawTotal, "%d", &total)
	}
	totalPages := (total + 9) / 10
	if totalPages < 1 {
		totalPages = 1
	}
	if totalPages > maxPages {
		totalPages = maxPages
	}
	for page := 2; page <= totalPages && len(results) < maxResults; page++ {
		pageDoc, pageErr := p.fetchPage(ctx, client, keyword, page)
		if pageErr != nil {
			continue
		}
		results = append(results, p.parseSearchResults(pageDoc)...)
	}
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *YunsouAsyncPlugin) fetchPage(ctx context.Context, client *http.Client, keyword string, page int) (*goquery.Document, error) {
	pathKeyword := url.PathEscape(keyword)
	path := pathKeyword
	if page > 1 {
		path = fmt.Sprintf("%s-%d", pathKeyword, page)
	}
	requestURL := fmt.Sprintf(searchURLTemplate, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建第%d页请求失败: %w", p.Name(), page, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://wpys.cc/")
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 第%d页搜索请求失败: %w", p.Name(), page, err)
	}
	defer resp.Body.Close()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析第%d页失败: %w", p.Name(), page, err)
	}
	return doc, nil
}

func (p *YunsouAsyncPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<(attempt-1)) * 200 * time.Millisecond)
		}
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		if resp != nil {
			lastErr = fmt.Errorf("状态码 %d", resp.StatusCode)
			resp.Body.Close()
		} else {
			lastErr = err
		}
	}
	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

func (p *YunsouAsyncPlugin) parseSearchResults(doc *goquery.Document) []model.SearchResult {
	results := make([]model.SearchResult, 0)
	doc.Find(".list .item").Each(func(_ int, item *goquery.Selection) {
		if len(results) >= maxResults {
			return
		}
		title, _ := item.Attr("data-title")
		if title == "" {
			title = item.Find(".title").First().Text()
		}
		title = cleanText(html.UnescapeString(title))
		if title == "" {
			return
		}

		shareURL, password := "", ""
		item.Find("*").EachWithBreak(func(_ int, button *goquery.Selection) bool {
			onclick, _ := button.Attr("onclick")
			if onclick == "" {
				// Vue 绑定在 HTML 中保留为 @click.stop 属性。
				onclick, _ = button.Attr("@click.stop")
			}
			if onclick == "" {
				return true
			}
			match := copyTextRegex.FindStringSubmatch(html.UnescapeString(onclick))
			if len(match) >= 3 {
				shareURL = strings.TrimSpace(match[1])
				password = strings.TrimSpace(match[2])
			}
			return shareURL == ""
		})
		if shareURL == "" {
			return
		}
		if password == "" {
			password = extractPassword(shareURL)
		}

		result := model.SearchResult{
			MessageID: fmt.Sprintf("%s-%s", p.Name(), url.QueryEscape(shareURL)),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), url.QueryEscape(shareURL)),
			Title:     title,
			Channel:   "",
			Datetime:  parseDate(item.Find(".type.time").First().Text()),
			Links: []model.Link{{
				Type:     diskType(shareURL),
				URL:      shareURL,
				Password: password,
			}},
		}
		if source := cleanText(item.Find(".type").First().Text()); source != "" {
			result.Content = source
		}
		results = append(results, result)
	})
	return results
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func parseDate(value string) time.Time {
	if date, err := time.Parse(timeLayout, strings.TrimSpace(value)); err == nil {
		return date
	}
	return time.Now()
}

func extractPassword(rawURL string) string {
	match := pwdParamRegex.FindStringSubmatch(rawURL)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func diskType(rawURL string) string {
	switch {
	case strings.Contains(rawURL, "pan.quark.cn"):
		return "quark"
	case strings.Contains(rawURL, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(rawURL, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(rawURL, "aliyundrive.com"), strings.Contains(rawURL, "alipan.com"):
		return "aliyun"
	case strings.Contains(rawURL, "drive.uc.cn"):
		return "uc"
	default:
		return "others"
	}
}
