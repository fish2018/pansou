package lingjisp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

const (
	lingjiPluginName      = "lingjisp"
	lingjiAPIBase         = "https://web5.mukaku.com/prod/api/v1/"
	lingjiAppID           = "83768d9ad4"
	lingjiIdentity        = "23734adac0301bccdcb107c4aa21f96c"
	lingjiDefaultPriority = 2
	lingjiSearchTimeout   = 20 * time.Second
	lingjiDetailTimeout   = 12 * time.Second
	lingjiMaxRetries      = 3
	lingjiMaxConcurrency  = 6
)

var (
	lingjiPwdURLRegex = regexp.MustCompile(`[?&](?:pwd|code|passcode)=([0-9A-Za-z]+)`)
)

type LingjiPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

type lingjiSearchResponse struct {
	Success bool `json:"success"`
	Code    int  `json:"code"`
	Data    struct {
		Data []lingjiVideoItem `json:"data"`
		List []lingjiVideoItem `json:"list"`
	} `json:"data"`
}

type lingjiDetailResponse struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Data    lingjiVideoItem `json:"data"`
}

type lingjiVideoItem struct {
	ID               int                           `json:"id"`
	DoubID           int                           `json:"doub_id"`
	Title            string                        `json:"title"`
	Image            string                        `json:"image"`
	Years            string                        `json:"years"`
	Abstract         string                        `json:"abstract"`
	Performer        string                        `json:"performer"`
	Director         string                        `json:"director"`
	ProductionArea   string                        `json:"production_area"`
	Ejs              string                        `json:"ejs"`
	Zqxd             string                        `json:"zqxd"`
	Class            string                        `json:"class"`
	SeedUpdatedAt    string                        `json:"seed_updated_at"`
	UpdatedAt        string                        `json:"updated_at"`
	MoviesOnlineSeed map[string][]lingjiOnlineSeed `json:"movies_online_seed"`
	AllSeeds         []lingjiMagnetSeed            `json:"all_seeds"`
	Ecca             map[string][]lingjiMagnetSeed `json:"ecca"`
}

type lingjiOnlineSeed struct {
	SeedName string `json:"seed_name"`
	Link     string `json:"link"`
	Type     string `json:"type"`
	Code     string `json:"code"`
	Nickname string `json:"nickname"`
}

type lingjiMagnetSeed struct {
	ZName           string `json:"zname"`
	ZLink           string `json:"zlink"`
	ZQXD            string `json:"zqxd"`
	ZSize           string `json:"zsize"`
	EZT             string `json:"ezt"`
	DefinitionGroup string `json:"definition_group"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewLingjiPlugin())
}

func NewLingjiPlugin() *LingjiPlugin {
	return &LingjiPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(lingjiPluginName, lingjiDefaultPriority),
		client: &http.Client{
			Timeout: lingjiSearchTimeout,
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

func (p *LingjiPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *LingjiPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *LingjiPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.client != nil {
		client = p.client
	}

	items, err := p.fetchSearchItems(client, keyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		sem     = make(chan struct{}, lingjiMaxConcurrency)
		results = make([]model.SearchResult, 0, len(items))
	)

	for _, item := range items {
		wg.Add(1)
		sem <- struct{}{}

		go func(base lingjiVideoItem) {
			defer wg.Done()
			defer func() { <-sem }()

			detail, err := p.fetchDetail(client, chooseLingjiID(base))
			if err != nil {
				return
			}

			links := extractLingjiLinks(detail)
			if len(links) == 0 {
				return
			}

			result := model.SearchResult{
				UniqueID: fmt.Sprintf("%s-%d", p.Name(), chooseLingjiID(detail)),
				Title:    strings.TrimSpace(detail.Title),
				Content:  buildLingjiContent(detail),
				Channel:  "",
				Datetime: parseLingjiTime(detail.SeedUpdatedAt, detail.UpdatedAt),
				Links:    links,
				Tags:     buildLingjiTags(detail),
			}
			if detail.Image != "" {
				result.Images = []string{strings.TrimSpace(detail.Image)}
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(item)
	}

	wg.Wait()
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *LingjiPlugin) fetchSearchItems(client *http.Client, keyword string) ([]lingjiVideoItem, error) {
	params := url.Values{}
	params.Set("app_id", lingjiAppID)
	params.Set("identity", lingjiIdentity)
	params.Set("sb", keyword)
	params.Set("page", "1")
	params.Set("limit", "20")

	apiURL := lingjiAPIBase + "getVideoList?" + params.Encode()
	body, err := doLingjiGET(client, apiURL, lingjiSearchTimeout)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}

	var resp lingjiSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索响应失败: %w", p.Name(), err)
	}
	if !resp.Success || resp.Code != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索接口返回异常: success=%v code=%d", p.Name(), resp.Success, resp.Code)
	}

	items := resp.Data.Data
	if len(items) == 0 {
		items = resp.Data.List
	}
	return dedupeLingjiItems(items), nil
}

func (p *LingjiPlugin) fetchDetail(client *http.Client, doubID int) (lingjiVideoItem, error) {
	params := url.Values{}
	params.Set("app_id", lingjiAppID)
	params.Set("identity", lingjiIdentity)
	params.Set("id", fmt.Sprintf("%d", doubID))

	apiURL := lingjiAPIBase + "getVideoDetail?" + params.Encode()
	body, err := doLingjiGET(client, apiURL, lingjiDetailTimeout)
	if err != nil {
		return lingjiVideoItem{}, fmt.Errorf("[%s] 详情请求失败: %w", p.Name(), err)
	}

	var resp lingjiDetailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return lingjiVideoItem{}, fmt.Errorf("[%s] 解析详情响应失败: %w", p.Name(), err)
	}
	if !resp.Success || resp.Code != http.StatusOK {
		return lingjiVideoItem{}, fmt.Errorf("[%s] 详情接口返回异常: success=%v code=%d", p.Name(), resp.Success, resp.Code)
	}
	return resp.Data, nil
}

func doLingjiGET(client *http.Client, requestURL string, timeout time.Duration) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < lingjiMaxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			cancel()
			return nil, err
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json,text/plain,*/*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Referer", lingjiAPIBase)

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			data := make([]byte, 0)
			buffer := make([]byte, 32*1024)
			for {
				n, readErr := resp.Body.Read(buffer)
				if n > 0 {
					data = append(data, buffer[:n]...)
				}
				if readErr != nil {
					if strings.Contains(readErr.Error(), "EOF") {
						cancel()
						return data, nil
					}
					lastErr = readErr
					break
				}
			}
		} else {
			if resp != nil {
				resp.Body.Close()
			}
			lastErr = err
			if lastErr == nil {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		cancel()

		if attempt < lingjiMaxRetries-1 {
			time.Sleep(200 * time.Millisecond * time.Duration(1<<attempt))
		}
	}

	return nil, fmt.Errorf("重试 %d 次后失败: %w", lingjiMaxRetries, lastErr)
}

func dedupeLingjiItems(items []lingjiVideoItem) []lingjiVideoItem {
	seen := make(map[int]struct{})
	results := make([]lingjiVideoItem, 0, len(items))

	for _, item := range items {
		id := chooseLingjiID(item)
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		results = append(results, item)
	}
	return results
}

func chooseLingjiID(item lingjiVideoItem) int {
	if item.DoubID > 0 {
		return item.DoubID
	}
	return item.ID
}

func extractLingjiLinks(item lingjiVideoItem) []model.Link {
	results := make([]model.Link, 0)
	seen := make(map[string]struct{})

	addLink := func(link model.Link) {
		key := link.Type + "|" + link.URL + "|" + link.Password
		if link.URL == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		results = append(results, link)
	}

	for _, groups := range item.Ecca {
		for _, seed := range groups {
			if strings.TrimSpace(seed.ZLink) == "" {
				continue
			}
			addLink(model.Link{
				Type:     detectLingjiLinkType(seed.ZLink, "magnet"),
				URL:      strings.TrimSpace(seed.ZLink),
				Password: extractLingjiPassword(seed.ZLink, ""),
			})
		}
	}

	for _, seed := range item.AllSeeds {
		if strings.TrimSpace(seed.ZLink) == "" {
			continue
		}
		addLink(model.Link{
			Type:     detectLingjiLinkType(seed.ZLink, "magnet"),
			URL:      strings.TrimSpace(seed.ZLink),
			Password: extractLingjiPassword(seed.ZLink, ""),
		})
	}

	for groupType, seeds := range item.MoviesOnlineSeed {
		for _, seed := range seeds {
			if strings.TrimSpace(seed.Link) == "" {
				continue
			}
			addLink(model.Link{
				Type:     detectLingjiLinkType(seed.Link, seed.Type, groupType),
				URL:      strings.TrimSpace(seed.Link),
				Password: extractLingjiPassword(seed.Link, seed.Code),
			})
		}
	}

	return results
}

func detectLingjiLinkType(rawURL string, fallbacks ...string) string {
	value := strings.ToLower(strings.TrimSpace(rawURL))
	switch {
	case strings.HasPrefix(value, "magnet:?xt=urn:btih:"):
		return "magnet"
	case strings.Contains(value, "pan.quark.cn"):
		return "quark"
	case strings.Contains(value, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(value, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(value, "drive.uc.cn"):
		return "uc"
	case strings.Contains(value, "alipan.com"), strings.Contains(value, "aliyundrive.com"):
		return "aliyun"
	case strings.Contains(value, "123pan.com"), strings.Contains(value, "123684.com"), strings.Contains(value, "123865.com"), strings.Contains(value, "123685.com"), strings.Contains(value, "123592.com"), strings.Contains(value, "123912.com"):
		return "123"
	}

	for _, fallback := range fallbacks {
		switch strings.ToLower(strings.TrimSpace(fallback)) {
		case "quark":
			return "quark"
		case "baidu":
			return "baidu"
		case "xunlei":
			return "xunlei"
		case "uc":
			return "uc"
		case "alipan", "aliyun", "aliyundrive", "ali":
			return "aliyun"
		case "123", "123pan", "a123":
			return "123"
		case "magnet":
			return "magnet"
		}
	}
	return "other"
}

func extractLingjiPassword(rawURL, rawCode string) string {
	if value := strings.TrimSpace(rawCode); value != "" {
		return value
	}
	if matches := lingjiPwdURLRegex.FindStringSubmatch(rawURL); len(matches) >= 2 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func buildLingjiTags(item lingjiVideoItem) []string {
	tags := make([]string, 0, 4)
	for _, value := range []string{item.Years, item.Class, item.Zqxd, item.Ejs} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		tags = append(tags, value)
	}
	return tags
}

func buildLingjiContent(item lingjiVideoItem) string {
	parts := make([]string, 0, 5)
	for _, value := range []string{item.Years, item.ProductionArea, item.Director, item.Performer} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parts = append(parts, value)
	}
	if summary := cleanLingjiText(item.Abstract); summary != "" {
		parts = append(parts, summary)
	}

	content := strings.Join(parts, " | ")
	if len(content) > 300 {
		return content[:300] + "..."
	}
	return content
}

func cleanLingjiText(text string) string {
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	return strings.TrimSpace(lingjispRe1.ReplaceAllString(text, " "))
}

func parseLingjiTime(candidates ...string) time.Time {
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}

	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		for _, layout := range layouts {
			if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
				return parsed
			}
		}
	}

	return time.Now()
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	lingjispRe1 = regexp.MustCompile(`\s+`)
)
