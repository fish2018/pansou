package panzun

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
	"pansou/model"
	"pansou/plugin"
	jsonutil "pansou/util/json"
)

const (
	pluginName      = "panzun"
	baseURL         = "https://www.panzun.cc"
	apiBase         = baseURL + "/api"
	defaultPriority = 2
	defaultTimeout  = 30 * time.Second
	maxPages        = 3
	pageSize        = 20
)

var (
	shortLinkRegex = regexp.MustCompile(`https?://a\.7u9\.cn/s/[A-Za-z0-9]+`)
	realLinkRegex  = regexp.MustCompile(`https?://(?:pan\.quark\.cn/s/[0-9A-Za-z]+|drive\.uc\.cn/s/[0-9A-Za-z]+|pan\.baidu\.com/s/[0-9A-Za-z_\-]+(?:\?pwd=[0-9A-Za-z]+)?|cloud\.189\.cn/t/[0-9A-Za-z]+|(?:www\.)?aliyundrive\.com/s/[0-9A-Za-z]+|(?:www\.)?alipan\.com/s/[0-9A-Za-z]+|115\.com/s/[0-9A-Za-z]+|pan\.xunlei\.com/s/[0-9A-Za-z_\-]+|www\.123684\.com/s/[0-9A-Za-z]+|www\.123865\.com/s/[0-9A-Za-z]+|www\.123912\.com/s/[0-9A-Za-z]+|www\.123pan\.com/s/[0-9A-Za-z]+)`)
)

// SearchResponse Flarum discussions 搜索响应
type SearchResponse struct {
	Links    map[string]string   `json:"links"`
	Data     []Discussion        `json:"data"`
	Included []IncludedResource  `json:"included"`
}

// Discussion 主题
type Discussion struct {
	Type          string                 `json:"type"`
	ID            string                 `json:"id"`
	Attributes    map[string]interface{} `json:"attributes"`
	Relationships map[string]interface{} `json:"relationships"`
}

// IncludedResource 附带资源
type IncludedResource struct {
	Type          string                 `json:"type"`
	ID            string                 `json:"id"`
	Attributes    map[string]interface{} `json:"attributes"`
	Relationships map[string]interface{} `json:"relationships"`
}

// PanzunPlugin 盘尊社区插件
type PanzunPlugin struct {
	*plugin.BaseAsyncPlugin
	scraper *cloudscraper.Scraper
}

var _ plugin.AsyncSearchPlugin = (*PanzunPlugin)(nil)

func init() {
	plugin.RegisterGlobalPlugin(NewPanzunPlugin())
}

func NewPanzunPlugin() *PanzunPlugin {
	scraper, err := cloudscraper.New()
	if err != nil {
		fmt.Printf("[%s] Failed to create cloudscraper: %v\n", pluginName, err)
		return &PanzunPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority)}
	}
	return &PanzunPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		scraper:         scraper,
	}
}

func (p *PanzunPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *PanzunPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *PanzunPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.scraper == nil {
		return nil, fmt.Errorf("cloudscraper not initialized")
	}

	var allResults []model.SearchResult
	seenIDs := make(map[string]bool)

	for page := 1; page <= maxPages; page++ {
		offset := (page - 1) * pageSize
		searchURL := fmt.Sprintf("%s/discussions?filter[q]=%s&page[offset]=%d", apiBase, url.QueryEscape(keyword), offset)

		resp, err := p.scraper.Get(searchURL)
		if err != nil {
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: failed to fetch page %d: %v\n", p.Name(), page, err)
				break
			}
			return nil, fmt.Errorf("[%s] search request failed on page %d: %w", p.Name(), page, err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: unexpected status code %d on page %d\n", p.Name(), resp.StatusCode, page)
				break
			}
			return nil, fmt.Errorf("[%s] unexpected status code: %d on page %d", p.Name(), resp.StatusCode, page)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if len(allResults) > 0 {
				break
			}
			return nil, fmt.Errorf("[%s] failed to read response on page %d: %w", p.Name(), page, err)
		}

		var searchResp SearchResponse
		if err := jsonutil.Unmarshal(body, &searchResp); err != nil {
			if len(allResults) > 0 {
				break
			}
			return nil, fmt.Errorf("[%s] failed to parse response on page %d: %w", p.Name(), page, err)
		}

		includedMap := buildIncludedMap(searchResp.Included)
		pageResults, err := p.convertDiscussionsToResults(client, searchResp.Data, includedMap)
		if err != nil {
			fmt.Printf("[%s] Warning: detail parse failed on page %d: %v\n", p.Name(), page, err)
		}

		for _, result := range pageResults {
			if result.UniqueID == "" || seenIDs[result.UniqueID] {
				continue
			}
			seenIDs[result.UniqueID] = true
			allResults = append(allResults, result)
		}

		if searchResp.Links["next"] == "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	return plugin.FilterResultsByKeyword(allResults, keyword), nil
}

func buildIncludedMap(included []IncludedResource) map[string]IncludedResource {
	m := make(map[string]IncludedResource)
	for _, item := range included {
		if item.Type == "" || item.ID == "" {
			continue
		}
		m[item.Type+":"+item.ID] = item
	}
	return m
}

func (p *PanzunPlugin) convertDiscussionsToResults(client *http.Client, discussions []Discussion, includedMap map[string]IncludedResource) ([]model.SearchResult, error) {
	results := make([]model.SearchResult, 0, len(discussions))
	for _, item := range discussions {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		title, _ := item.Attributes["title"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}

		links, content, tags, createdAt, err := p.fetchDiscussionLinks(client, id)
		if err != nil {
			fmt.Printf("[%s] Warning: fetch discussion %s failed: %v\n", p.Name(), id, err)
			continue
		}
		if len(links) == 0 {
			continue
		}

		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), id),
			Title:    title,
			Content:  content,
			Datetime: createdAt,
			Links:    links,
			Tags:     tags,
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *PanzunPlugin) fetchDiscussionLinks(client *http.Client, discussionID string) ([]model.Link, string, []string, time.Time, error) {
	detailURL := fmt.Sprintf("%s/discussions/%s", apiBase, discussionID)
	resp, err := p.scraper.Get(detailURL)
	if err != nil {
		return nil, "", nil, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", nil, time.Time{}, fmt.Errorf("detail status=%d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", nil, time.Time{}, err
	}

	var detailResp struct {
		Data     Discussion         `json:"data"`
		Included []IncludedResource `json:"included"`
	}
	if err := jsonutil.Unmarshal(body, &detailResp); err != nil {
		return nil, "", nil, time.Time{}, err
	}

	includedMap := buildIncludedMap(detailResp.Included)
	rel := detailResp.Data.Relationships

	var postID string
	if postsRaw, ok := rel["posts"].(map[string]interface{}); ok {
		if dataRaw, ok := postsRaw["data"].([]interface{}); ok && len(dataRaw) > 0 {
			if postRef, ok := dataRaw[0].(map[string]interface{}); ok {
				postID, _ = postRef["id"].(string)
			}
		}
	}

	var contentHTML string
	if postID != "" {
		if post, ok := includedMap["posts:"+postID]; ok {
			contentHTML, _ = post.Attributes["contentHtml"].(string)
		}
	}
	content := cleanHTML(contentHTML)
	links := p.extractAndResolveLinks(contentHTML)
	if len(links) == 0 {
		return nil, content, nil, time.Time{}, nil
	}

	var tags []string
	if tagsRaw, ok := rel["tags"].(map[string]interface{}); ok {
		if dataRaw, ok := tagsRaw["data"].([]interface{}); ok {
			for _, item := range dataRaw {
				if tagRef, ok := item.(map[string]interface{}); ok {
					id, _ := tagRef["id"].(string)
					if tag, ok := includedMap["tags:"+id]; ok {
						if name, _ := tag.Attributes["name"].(string); name != "" {
							tags = append(tags, name)
						}
					}
				}
			}
		}
	}

	createdAt := time.Time{}
	if createdStr, _ := detailResp.Data.Attributes["createdAt"].(string); createdStr != "" {
		if parsed, err := time.Parse(time.RFC3339, createdStr); err == nil {
			createdAt = parsed
		}
	}

	return links, content, tags, createdAt, nil
}

func (p *PanzunPlugin) extractAndResolveLinks(contentHTML string) []model.Link {
	seen := make(map[string]bool)
	var links []model.Link
	matches := shortLinkRegex.FindAllString(contentHTML, -1)
	directs := realLinkRegex.FindAllString(contentHTML, -1)
	all := append(matches, directs...)

	for _, raw := range all {
		realURL := p.resolveShortLink(raw)
		if realURL == "" {
			realURL = raw
		}
		if !realLinkRegex.MatchString(realURL) {
			continue
		}
		if seen[realURL] {
			continue
		}
		seen[realURL] = true
		links = append(links, model.Link{
			Type:      detectLinkType(realURL),
			URL:       realURL,
			WorkTitle: "",
		})
	}
	return links
}

func (p *PanzunPlugin) resolveShortLink(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !shortLinkRegex.MatchString(raw) {
		return raw
	}

	headersSimple := map[string]string{
		"User-Agent":      "python-requests/2.31.0",
		"Accept":          "text/html,*/*",
		"Accept-Encoding": "identity",
	}
	for _, headers := range []map[string]string{headersSimple} {
		location := fetchLocationWithCloudscraper(raw, headers)
		if realLinkRegex.MatchString(location) {
			return location
		}
	}
	return ""
}

func fetchLocationWithCloudscraper(raw string, headers map[string]string) string {
	scraper, err := cloudscraper.New()
	if err != nil || scraper == nil {
		return ""
	}
	resp, err := scraper.Get(raw)
	if err == nil && resp != nil {
		defer resp.Body.Close()
		if loc := strings.TrimSpace(resp.Header.Get("Location")); loc != "" {
			return loc
		}
	}

	// fallback: use plain client with no redirect
	transport := &http.Transport{}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return ""
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp2, err := client.Do(req)
	if err != nil || resp2 == nil {
		return ""
	}
	defer resp2.Body.Close()
	return strings.TrimSpace(resp2.Header.Get("Location"))
}

func detectLinkType(link string) string {
	lower := strings.ToLower(link)
	switch {
	case strings.Contains(lower, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lower, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lower, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lower, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lower, "aliyundrive.com") || strings.Contains(lower, "alipan.com"):
		return "aliyun"
	case strings.Contains(lower, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lower, "115.com"):
		return "115"
	case strings.Contains(lower, "123684.com") || strings.Contains(lower, "123865.com") || strings.Contains(lower, "123912.com") || strings.Contains(lower, "123pan.com"):
		return "123"
	default:
		return "others"
	}
}

func cleanHTML(value string) string {
	replacer := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n")
	text := replacer.Replace(value)
	re := regexp.MustCompile(`<[^>]+>`)
	text = re.ReplaceAllString(text, " ")
	text = strings.TrimSpace(text)
	spaceRe := regexp.MustCompile(`\s+`)
	text = spaceRe.ReplaceAllString(text, " ")
	return text
}
