package kpkuang

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	utiljson "pansou/util/json"
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
	pluginName           = "kpkuang"
	pluginPriority       = 3
	primaryHost          = "https://www.kpkuang.org"
	betaAPI              = "https://kpdata.flixfiend.top/esearch/index"
	requestTimeout       = 15 * time.Second
	detailTimeout        = 6 * time.Second
	maxResponseBytes     = 8 << 20
	maxSearchItems       = 10
	maxLinksPerMovie     = 24
	maxDetailConcurrency = 4
	maxHosts             = 3
)

var siteHosts = []string{
	primaryHost,
	"https://www.kpkuang.fun",
	"https://www.kpkuang.sbs",
}

var (
	detailIDRE = regexp.MustCompile(`/voddetail/(\d+)(?:/|\.html)?`)
	defCoverRE = regexp.MustCompile(`(?s)var\s+def_cover\s*=\s*["']([^"']+)`)
	passwordRE = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|取件码|password|pwd)\s*[:：]?\s*([0-9a-zA-Z_-]{3,32})`)
)

// KpkuangPlugin searches 看片狂人 and extracts its encrypted cloud/torrent links.
type KpkuangPlugin struct {
	*plugin.BaseAsyncPlugin
}

type betaItem struct {
	ID   utiljson.RawMessage `json:"id"`
	Data struct {
		Name       string `json:"vod_name"`
		Original   string `json:"vod_name_org"`
		Year       string `json:"vod_year"`
		Area       string `json:"vod_area"`
		Picture    string `json:"vod_pic"`
		IMDBPoster string `json:"vod_imdb_poster"`
		Douban     string `json:"vod_douban_cover"`
		Actor      string `json:"vod_actor"`
		Director   string `json:"vod_director"`
		Writer     string `json:"vod_writer"`
		TypeID     int    `json:"type_id"`
	} `json:"data"`
	High struct {
		Name []string `json:"vod_name"`
	} `json:"high"`
}

type betaResponse struct {
	Code int    `json:"code"`
	JS   string `json:"js"`
}

type searchItem struct {
	id        string
	title     string
	detailURL string
	image     string
	content   string
	tags      []string
}

func init() {
	plugin.RegisterGlobalPlugin(NewKpkuangPlugin())
}

func NewKpkuangPlugin() *KpkuangPlugin {
	return &KpkuangPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter(pluginName, pluginPriority, true)}
}

func (p *KpkuangPlugin) Name() string        { return pluginName }
func (p *KpkuangPlugin) DisplayName() string { return "看片狂人" }
func (p *KpkuangPlugin) Description() string {
	return "看片狂人 - 电影、电视剧磁力和网盘资源"
}

func (p *KpkuangPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *KpkuangPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *KpkuangPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	items, betaErr := p.betaSearch(client, keyword)
	if len(items) == 0 {
		items, betaErr = p.normalSearch(client, keyword)
	}
	if len(items) == 0 {
		if betaErr != nil {
			return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), betaErr)
		}
		return []model.SearchResult{}, nil
	}
	if len(items) > maxSearchItems {
		items = items[:maxSearchItems]
	}

	results := make([]model.SearchResult, len(items))
	valid := make([]bool, len(items))
	sem := make(chan struct{}, maxDetailConcurrency)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func(index int, item searchItem) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result, ok := p.fetchMovie(client, item)
			if ok {
				results[index] = result
				valid[index] = true
			}
		}(i, item)
	}
	wg.Wait()

	out := make([]model.SearchResult, 0, len(items))
	for i := range results {
		if valid[i] && len(results[i].Links) > 0 {
			out = append(out, results[i])
		}
	}
	return plugin.FilterResultsByKeyword(out, keyword), nil
}

func (p *KpkuangPlugin) betaSearch(client *http.Client, keyword string) ([]searchItem, error) {
	query := url.Values{}
	query.Set("kw", keyword)
	query.Set("ts", strconv.FormatInt(time.Now().UnixMilli(), 10))
	query.Set("callback", "__kpkuang_cb")
	target := betaAPI + "?" + query.Encode()
	body, err := fetchBody(client, target, primaryHost+"/")
	if err != nil {
		return nil, err
	}
	return parseBetaResponse(body)
}

func parseBetaResponse(body []byte) ([]searchItem, error) {
	start, end := strings.IndexByte(string(body), '{'), strings.LastIndexByte(string(body), '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("无效的 beta JSONP 响应")
	}
	var envelope betaResponse
	if err := utiljson.Unmarshal(body[start:end+1], &envelope); err != nil {
		return nil, err
	}
	if envelope.Code != 1 || strings.TrimSpace(envelope.JS) == "" {
		return []searchItem{}, nil
	}
	decoded, err := decodeBase64(envelope.JS)
	if err != nil {
		return nil, err
	}
	var rawItems []betaItem
	if err := utiljson.Unmarshal(decoded, &rawItems); err != nil {
		return nil, err
	}
	items := make([]searchItem, 0, len(rawItems))
	seen := make(map[string]struct{})
	for _, raw := range rawItems {
		id := rawID(raw.ID)
		if id == "" {
			continue
		}
		title := cleanText(raw.Data.Name)
		if title == "" {
			title = cleanText(raw.Data.Original)
		}
		if title == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		imageURL := firstNonEmpty(raw.Data.Douban, raw.Data.IMDBPoster, raw.Data.Picture)
		content := betaContent(raw.Data.Actor, raw.Data.Director, raw.Data.Writer, raw.Data.Area, raw.Data.Year)
		tags := []string{"看片狂人"}
		if raw.Data.Area != "" {
			tags = append(tags, cleanText(raw.Data.Area))
		}
		if raw.Data.Year != "" {
			tags = append(tags, cleanText(raw.Data.Year))
		}
		items = append(items, searchItem{
			id: id, title: title, detailURL: primaryHost + "/voddetail/" + id + "/",
			image: resolveURL(imageURL, primaryHost), content: content, tags: tags,
		})
	}
	return items, nil
}

func (p *KpkuangPlugin) normalSearch(client *http.Client, keyword string) ([]searchItem, error) {
	var lastErr error
	for _, host := range siteHosts {
		searchURL := host + "/vodsearch/-------------.html?wd=" + url.QueryEscape(keyword) + "&limit=500"
		doc, err := fetchDocument(client, searchURL, host+"/")
		if err != nil {
			lastErr = err
			continue
		}
		items := parseNormalSearch(doc, host)
		if len(items) > 0 {
			return items, nil
		}
	}
	return []searchItem{}, lastErr
}

func (p *KpkuangPlugin) fetchMovie(client *http.Client, item searchItem) (model.SearchResult, bool) {
	hosts := orderedHosts(item.detailURL)
	for _, host := range hosts {
		detailURL := host + "/voddetail/" + item.id + "/"
		doc, err := fetchDocumentWithTimeout(client, detailURL, host+"/", detailTimeout)
		if err != nil {
			continue
		}
		parsed := parseDetail(doc, detailURL, item)
		if len(parsed.links) == 0 {
			continue
		}
		content := parsed.content
		if content == "" {
			content = item.content
		}
		title := item.title
		if title == "" {
			title = parsed.title
		}
		for i := range parsed.links {
			parsed.links[i].WorkTitle = title
		}
		id := item.id
		if id == "" {
			id = shortID(detailURL)
		}
		result := model.SearchResult{
			UniqueID:  p.Name() + "-" + id,
			MessageID: p.Name() + "-" + id,
			Channel:   "", Datetime: time.Now(), Title: title, Content: content,
			Links: parsed.links, Tags: dedupeStrings(append([]string{"看片狂人"}, append(item.tags, parsed.tags...)...)),
		}
		if parsed.image != "" {
			result.Images = []string{parsed.image}
		} else if item.image != "" {
			result.Images = []string{item.image}
		}
		return result, true
	}
	return model.SearchResult{}, false
}

type detailData struct {
	title   string
	content string
	image   string
	tags    []string
	links   []model.Link
}

func parseDetail(doc *goquery.Document, pageURL string, item searchItem) detailData {
	result := detailData{title: item.title, tags: []string{}}
	result.image = resolveURL(doc.Find("#cover_showbox").First().AttrOr("data-original", ""), pageURL)
	if result.image == "" {
		if htmlText, err := doc.Html(); err == nil {
			if match := defCoverRE.FindStringSubmatch(htmlText); len(match) > 1 {
				result.image = resolveURL(match[1], pageURL)
			}
		}
	}
	if result.title == "" {
		result.title = cleanTitle(doc.Find("h1.uk-card-title").First().Text())
	}
	meta := make(map[string]string)
	doc.Find(".vodbox li").Each(func(_ int, s *goquery.Selection) {
		text := cleanText(s.Text())
		for _, label := range []string{"主演", "导演", "编剧", "分类", "地区", "年份", "更新", "TAG", "简介"} {
			prefix := label + "："
			if strings.HasPrefix(text, prefix) {
				meta[label] = cleanText(strings.TrimPrefix(text, prefix))
				return
			}
		}
	})
	result.content = detailContent(meta)
	for _, key := range []string{"分类", "地区", "年份", "TAG"} {
		if value := meta[key]; value != "" {
			result.tags = append(result.tags, value)
		}
	}
	result.links = extractDetailLinks(doc)
	return result
}

func extractDetailLinks(doc *goquery.Document) []model.Link {
	links := make([]model.Link, 0, 8)
	seen := make(map[string]struct{})
	// New pages use td#td-*, while older pages expose the same encrypted
	// values directly on li#magnet-/li#pan-/li#xunlei- rows.
	doc.Find("td[id^='td-'], li[id^='magnet-'], li[id^='pan-'], li[id^='xunlei-']").EachWithBreak(func(_ int, row *goquery.Selection) bool {
		if len(links) >= maxLinksPerMovie {
			return false
		}
		rowText := cleanText(row.Text())
		pendingPassword := ""
		values := make([]string, 0, 3)
		row.Find("[data-pan-url], [data-clipboard-text]").Each(func(_ int, s *goquery.Selection) {
			for _, attr := range []string{"data-pan-url", "data-clipboard-text"} {
				if value, ok := s.Attr(attr); ok && value != "" {
					decoded := decryptMyString(html.UnescapeString(value))
					if decoded == "" || decoded == "@@@encrypted@@@" {
						continue
					}
					if isLinkValue(decoded) {
						values = append(values, decoded)
					} else if pendingPassword == "" && looksLikePassword(decoded) {
						pendingPassword = decoded
					}
				}
			}
		})
		if pendingPassword == "" {
			pendingPassword = extractPassword(rowText)
		}
		for _, raw := range values {
			link := parseLink(raw, pendingPassword)
			if link.URL == "" {
				continue
			}
			key := link.Type + "\x00" + link.URL + "\x00" + link.Password
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			links = append(links, link)
			if len(links) >= maxLinksPerMovie {
				break
			}
		}
		return true
	})
	return links
}

func parseNormalSearch(doc *goquery.Document, host string) []searchItem {
	items := make([]searchItem, 0, maxSearchItems)
	seen := make(map[string]struct{})
	add := func(anchor *goquery.Selection, scope *goquery.Selection) {
		href := resolveURL(anchor.AttrOr("href", ""), host)
		matches := detailIDRE.FindStringSubmatch(href)
		if len(matches) < 2 || matches[1] == "" {
			return
		}
		if _, ok := seen[matches[1]]; ok {
			return
		}
		title := cleanText(anchor.AttrOr("title", ""))
		if title == "" {
			title = cleanText(anchor.Text())
		}
		if title == "" {
			return
		}
		image := resolveURL(scope.Find("img").First().AttrOr("data-original", ""), host)
		if image == "" {
			image = resolveURL(scope.Find("img").First().AttrOr("src", ""), host)
		}
		seen[matches[1]] = struct{}{}
		items = append(items, searchItem{id: matches[1], title: title, detailURL: href, image: image, content: cleanText(scope.Text()), tags: []string{"看片狂人"}})
	}
	doc.Find("a.fed-list-pics[href*='/voddetail/']").Each(func(_ int, s *goquery.Selection) { add(s, s.Parent()) })
	if len(items) == 0 {
		doc.Find("a[href*='/voddetail/']").Each(func(_ int, s *goquery.Selection) { add(s, s.Parent()) })
	}
	return items
}

func parseLink(raw, password string) model.Link {
	raw = strings.TrimSpace(html.UnescapeString(raw))
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		return model.Link{Type: "magnet", URL: raw, Password: password}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" {
		return model.Link{}
	}
	if password == "" {
		password = extractURLPassword(u)
	}
	linkType := cloudType(u)
	if linkType == "" {
		return model.Link{}
	}
	return model.Link{Type: linkType, URL: raw, Password: password}
}

func cloudType(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "pan.baidu.com" || host == "yun.baidu.com":
		return "baidu"
	case strings.HasSuffix(host, "alipan.com") || strings.HasSuffix(host, "aliyundrive.com"):
		return "aliyun"
	case host == "pan.quark.cn":
		return "quark"
	case host == "pan.xunlei.com":
		return "xunlei"
	case host == "drive.uc.cn":
		return "uc"
	case host == "cloud.189.cn":
		return "tianyi"
	case host == "caiyun.139.com":
		return "mobile"
	case host == "115.com" || strings.HasSuffix(host, ".115.com") || host == "115cdn.com":
		return "115"
	case host == "123pan.com" || strings.HasSuffix(host, ".123pan.com"):
		return "123"
	case host == "mypikpak.com" || strings.HasSuffix(host, ".mypikpak.com"):
		return "pikpak"
	default:
		return ""
	}
}

func extractURLPassword(u *url.URL) string {
	for _, key := range []string{"pwd", "password", "passcode", "code"} {
		if value := strings.TrimSpace(u.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func extractPassword(value string) string {
	if match := passwordRE.FindStringSubmatch(value); len(match) > 1 {
		return match[1]
	}
	return ""
}

func decryptMyString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "@@@encrypted@@@" {
		return ""
	}
	if !strings.HasPrefix(value, "@@@encrypted@@@") {
		if isLinkValue(value) {
			return sanitizeValue(value)
		}
		decoded, err := decodeBase64(value)
		if err != nil {
			return ""
		}
		return sanitizeValue(string(decoded))
	}
	value = strings.TrimPrefix(value, "@@@encrypted@@@")
	if len(value) < 5 {
		return ""
	}
	value = value[len(value)-5:] + value[:len(value)-5]
	decoded, err := decodeBase64(value)
	if err != nil {
		return ""
	}
	decodedValue := decryptPayload(decoded, []byte("your_secret_key_here"))
	if !usefulDecodedValue(decodedValue) {
		// A few older mirrors used the previous short key. Keep this fallback
		// so archived detail pages remain usable after the site rotates scripts.
		decodedValue = decryptPayload(decoded, []byte("DYWNn"))
	}
	return sanitizeValue(decodedValue)
}

func decryptPayload(value, key []byte) string {
	value = append([]byte(nil), value...)
	for i := range value {
		value[i] ^= key[i%len(key)]
	}
	for i, j := 0, len(value)-1; i < j; i, j = i+1, j-1 {
		value[i], value[j] = value[j], value[i]
	}
	return strings.TrimSpace(string(value))
}

func usefulDecodedValue(value string) bool {
	if isLinkValue(value) || looksLikePassword(value) {
		return true
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return value != ""
}

func sanitizeValue(value string) string {
	value = strings.ReplaceAll(value, "\u200b", "")
	value = strings.ReplaceAll(value, "\u200c", "")
	value = strings.ReplaceAll(value, "\u200d", "")
	value = strings.ReplaceAll(value, "\ufeff", "")
	return strings.TrimSpace(value)
}

func decodeBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(value, "="))
}

func isLinkValue(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "magnet:")
}

func looksLikePassword(value string) bool {
	return len(value) >= 3 && len(value) <= 32 && !strings.ContainsAny(value, " /\\\t\r\n")
}

func fetchDocument(client *http.Client, target, referer string) (*goquery.Document, error) {
	return fetchDocumentWithTimeout(client, target, referer, requestTimeout)
}

func fetchDocumentWithTimeout(client *http.Client, target, referer string, timeout time.Duration) (*goquery.Document, error) {
	body, err := fetchBodyWithTimeout(client, target, referer, timeout)
	if err != nil {
		return nil, err
	}
	return goquery.NewDocumentFromReader(strings.NewReader(string(body)))
}

func fetchBody(client *http.Client, target, referer string) ([]byte, error) {
	return fetchBodyWithTimeout(client, target, referer, requestTimeout)
}

func fetchBodyWithTimeout(client *http.Client, target, referer string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if strings.HasPrefix(target, betaAPI) {
		req.Header.Set("Origin", primaryHost)
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

func orderedHosts(detailURL string) []string {
	result := make([]string, 0, maxHosts)
	if parsed, err := url.Parse(detailURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		incoming := parsed.Scheme + "://" + parsed.Host
		for _, host := range siteHosts[1:] {
			if host != incoming {
				result = append(result, host)
			}
		}
		result = append(result, incoming)
	}
	for _, host := range siteHosts {
		found := false
		for _, existing := range result {
			if existing == host {
				found = true
				break
			}
		}
		if !found {
			result = append(result, host)
		}
	}
	return result
}

func resolveURL(raw, base string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		return parsed.String()
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return raw
	}
	return baseURL.ResolveReference(parsed).String()
}

func rawID(raw utiljson.RawMessage) string {
	var value string
	if utiljson.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	var number utiljson.Number
	if utiljson.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

func betaContent(actor, director, writer, area, year string) string {
	parts := []string{}
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "主演", value: actor},
		{label: "导演", value: director},
		{label: "编剧", value: writer},
		{label: "地区", value: area},
		{label: "年份", value: year},
	} {
		label, value := field.label, field.value
		if value != "" {
			parts = append(parts, label+"："+cleanText(value))
		}
	}
	return strings.Join(parts, " | ")
}

func detailContent(meta map[string]string) string {
	parts := []string{}
	for _, key := range []string{"主演", "导演", "编剧", "分类", "地区", "年份", "TAG"} {
		if value := meta[key]; value != "" {
			parts = append(parts, key+"："+value)
		}
	}
	if value := meta["简介"]; value != "" {
		parts = append(parts, value)
	}
	return strings.Join(parts, " | ")
}

func cleanTitle(value string) string {
	value = cleanText(value)
	value = kpkuangRe1.ReplaceAllString(value, "")
	return strings.TrimSpace(value)
}

func cleanText(value string) string {
	value = strings.ReplaceAll(value, "\u00a0", " ")
	return strings.Join(strings.Fields(value), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(value) != "null" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func dedupeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func shortID(value string) string {
	var hash uint32 = 2166136261
	for i := 0; i < len(value); i++ {
		hash ^= uint32(value[i])
		hash *= 16777619
	}
	return strconv.FormatUint(uint64(hash), 16)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	kpkuangRe1 = regexp.MustCompile(`\s*\(\d{4}\)\s*$`)
)
