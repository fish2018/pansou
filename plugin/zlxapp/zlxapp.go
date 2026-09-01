package zlxapp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"pansou/model"
	"pansou/plugin"
	jsonutil "pansou/util/json"
)

const (
	pluginName      = "zlxapp"
	baseURL         = "https://www.zlxapp.top"
	defaultPriority = 3
	requestTimeout  = 20 * time.Second
	maxResponseSize = 2 << 20
	maxRetries      = 2
	timeLayout      = "2006-01-02"
)

var listItemsMarker = []byte("const listItemsData = ")

var whitespaceRegex = regexp.MustCompile(`\s+`)

type ZlxappPlugin struct {
	*plugin.BaseAsyncPlugin
	baseURL string
}

type category struct {
	Name string `json:"name"`
}

type sourceLink struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Code        string `json:"code"`
	IsType      int    `json:"is_type"`
	TargetName  string `json:"target_name"`
	SourceTitle string `json:"source_title"`
	Status      int    `json:"status"`
	IsDelete    int    `json:"is_delete"`
}

type searchItem struct {
	ID          int64        `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	IsType      int          `json:"is_type"`
	Code        string       `json:"code"`
	URL         string       `json:"url"`
	Times       string       `json:"times"`
	Category    category     `json:"category"`
	Tags        []string     `json:"tag_names"`
	Links       []sourceLink `json:"links"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewZlxappPlugin())
}

func NewZlxappPlugin() *ZlxappPlugin {
	return &ZlxappPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		baseURL:         baseURL,
	}
}

func (p *ZlxappPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *ZlxappPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *ZlxappPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = cleanText(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	searchURL := fmt.Sprintf("%s/s/%s.html", strings.TrimRight(p.baseURL, "/"), url.PathEscape(keyword))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}
	setRequestHeaders(req, p.baseURL)

	resp, err := doRequestWithRetry(client, req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索响应失败: %w", p.Name(), err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("[%s] 搜索响应超过 %d 字节", p.Name(), maxResponseSize)
	}

	items, err := parseListItems(body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}

	results := make([]model.SearchResult, 0, len(items))
	for _, item := range items {
		if result, ok := convertItem(item); ok {
			results = append(results, result)
		}
	}
	return plugin.FilterResultsByKeyword(deduplicateResults(results), keyword), nil
}

func setRequestHeaders(req *http.Request, refererBaseURL string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Referer", strings.TrimRight(refererBaseURL, "/")+"/")
}

func doRequestWithRetry(client *http.Client, req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-timer.C:
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			}
		}

		resp, err := client.Do(req.Clone(req.Context()))
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}

		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		resp.Body.Close()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("请求失败")
	}
	return nil, lastErr
}

func parseListItems(body []byte) ([]searchItem, error) {
	markerIndex := bytes.Index(body, listItemsMarker)
	if markerIndex < 0 {
		return nil, fmt.Errorf("未找到 listItemsData")
	}

	arrayStart := markerIndex + len(listItemsMarker)
	for arrayStart < len(body) && body[arrayStart] != '[' {
		arrayStart++
	}
	if arrayStart >= len(body) {
		return nil, fmt.Errorf("listItemsData 缺少数组")
	}

	arrayEnd, err := findJSONArrayEnd(body, arrayStart)
	if err != nil {
		return nil, err
	}

	var items []searchItem
	if err := jsonutil.Unmarshal(body[arrayStart:arrayEnd+1], &items); err != nil {
		return nil, err
	}
	return items, nil
}

func findJSONArrayEnd(data []byte, start int) (int, error) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(data); index++ {
		current := data[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				inString = false
			}
			continue
		}

		switch current {
		case '"':
			inString = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, fmt.Errorf("listItemsData 数组未闭合")
}

func convertItem(item searchItem) (model.SearchResult, bool) {
	title := cleanText(item.Title)
	if title == "" || item.ID <= 0 {
		return model.SearchResult{}, false
	}

	links := make([]model.Link, 0, max(1, len(item.Links)))
	seen := make(map[string]struct{})
	if len(item.Links) == 0 && strings.TrimSpace(item.URL) != "" {
		item.Links = []sourceLink{{
			ID:          item.ID,
			URL:         item.URL,
			Code:        item.Code,
			IsType:      item.IsType,
			SourceTitle: title,
			Status:      1,
		}}
	}

	for _, source := range item.Links {
		if source.IsDelete != 0 || source.Status < 0 {
			continue
		}
		linkType, linkURL, password := normalizeLink(source.URL, source.Code)
		if linkURL == "" {
			continue
		}
		key := linkURL + "\x00" + password
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		workTitle := cleanText(source.TargetName)
		if workTitle == "" {
			workTitle = cleanText(source.SourceTitle)
		}
		if workTitle == "" {
			workTitle = title
		}
		links = append(links, model.Link{
			Type:      linkType,
			URL:       linkURL,
			Password:  password,
			WorkTitle: workTitle,
		})
	}
	if len(links) == 0 {
		return model.SearchResult{}, false
	}

	datetime := time.Now()
	if parsed, err := time.ParseInLocation(timeLayout, strings.TrimSpace(item.Times), time.Local); err == nil {
		datetime = parsed
	}

	tags := append([]string(nil), item.Tags...)
	if item.Category.Name != "" && !containsString(tags, item.Category.Name) {
		tags = append(tags, item.Category.Name)
	}
	content := cleanText(item.Description)
	if content == "" {
		content = title
	}
	id := fmt.Sprintf("%s-%d", pluginName, item.ID)
	return model.SearchResult{
		MessageID: id,
		UniqueID:  id,
		Channel:   "",
		Datetime:  datetime,
		Title:     title,
		Content:   content,
		Links:     links,
		Tags:      tags,
	}, true
}

func normalizeLink(rawURL string, declaredPassword string) (string, string, string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", ""
	}

	host := strings.ToLower(parsed.Hostname())
	linkType := ""
	switch {
	case host == "pan.quark.cn":
		linkType = "quark"
	case host == "drive.uc.cn":
		linkType = "uc"
	case host == "pan.baidu.com":
		linkType = "baidu"
	case host == "www.aliyundrive.com" || host == "aliyundrive.com" || host == "www.alipan.com" || host == "alipan.com":
		linkType = "aliyun"
	case host == "pan.xunlei.com":
		linkType = "xunlei"
	case host == "guangyapan.com" || strings.HasSuffix(host, ".guangyapan.com"):
		linkType = "guangya"
	default:
		return "", "", ""
	}
	password := strings.TrimSpace(declaredPassword)
	if password == "" {
		for _, key := range []string{"pwd", "password", "code"} {
			if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
				password = value
				break
			}
		}
	}
	return linkType, parsed.String(), password
}

func deduplicateResults(results []model.SearchResult) []model.SearchResult {
	unique := make([]model.SearchResult, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		keyParts := make([]string, 0, len(result.Links))
		for _, link := range result.Links {
			keyParts = append(keyParts, link.URL+"\x00"+link.Password)
		}
		key := strings.Join(keyParts, "\x01")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, result)
	}
	return unique
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cleanText(value string) string {
	return strings.TrimSpace(whitespaceRegex.ReplaceAllString(value, " "))
}
