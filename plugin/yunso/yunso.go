package yunso

import (
	"context"
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
	"pansou/util"
	jsonutil "pansou/util/json"
)

const (
	yunsoSearchAPI       = "https://www.yunso.net/api/Core/search2"
	yunsoSearchPage      = "https://www.yunso.net/index/user/s"
	yunsoDecryptKey      = "pWz1vnL1fTkOvTMW3f9M1jJWfneUIh50"
	yunsoDefaultMode     = "90002"
	yunsoDefaultScope    = "0"
	yunsoDefaultPageSize = 20
	yunsoDefaultMaxPages = 3
	yunsoDefaultTimeout  = 30 * time.Second
)

var (
	yunsoDatetimeRegex = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
	yunsoTypeCodeRegex = regexp.MustCompile(`/assets/xyso/(\d+)\.png`)
	yunsoDecryptBytes  = []byte(yunsoDecryptKey)
)

func init() {
	plugin.RegisterGlobalPlugin(NewYunsoAsyncPlugin())
}

// YunsoAsyncPlugin 小云搜索异步插件
type YunsoAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// YunsoAPIResponse 小云搜索接口响应
type YunsoAPIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data string `json:"data"`
}

// YunsoItem 小云搜索结果项
type YunsoItem struct {
	FullID       string
	QID          string
	Title        string
	EncryptedURL string
	URL          string
	Password     string
	TypeCode     string
	TypeName     string
	Preview      string
	FileSummary  string
	Datetime     time.Time
	Badges       []string
}

// NewYunsoAsyncPlugin 创建新的小云搜索插件
func NewYunsoAsyncPlugin() *YunsoAsyncPlugin {
	return &YunsoAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("yunso", 3),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *YunsoAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含 IsFinal 标记的结果
func (p *YunsoAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际搜索实现
func (p *YunsoAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	resultChan := make(chan []YunsoItem, yunsoDefaultMaxPages)
	errChan := make(chan error, yunsoDefaultMaxPages)

	var wg sync.WaitGroup
	for page := 1; page <= yunsoDefaultMaxPages; page++ {
		wg.Add(1)
		go func(pageNum int) {
			defer wg.Done()

			items, err := p.searchPage(client, keyword, pageNum)
			if err != nil {
				errChan <- fmt.Errorf("page %d search failed: %w", pageNum, err)
				return
			}
			resultChan <- items
		}(page)
	}

	go func() {
		wg.Wait()
		close(resultChan)
		close(errChan)
	}()

	var allItems []YunsoItem
	for items := range resultChan {
		allItems = append(allItems, items...)
	}

	var errs []error
	for err := range errChan {
		errs = append(errs, err)
	}

	if len(allItems) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}

	uniqueItems := p.deduplicateItems(allItems)
	results := p.convertResults(uniqueItems)
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *YunsoAsyncPlugin) searchPage(client *http.Client, keyword string, page int) ([]YunsoItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), yunsoDefaultTimeout)
	defer cancel()

	params := url.Values{}
	params.Set("requestID", "")
	params.Set("mode", yunsoDefaultMode)
	params.Set("scope_content", yunsoDefaultScope)
	params.Set("stype", "")
	params.Set("wd", keyword)
	params.Set("uk", "")
	params.Set("page", strconv.Itoa(page))
	params.Set("limit", strconv.Itoa(yunsoDefaultPageSize))
	params.Set("screen_filetype", "")

	searchURL := yunsoSearchAPI + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}

	referer := yunsoSearchPage + "?wd=" + url.QueryEscape(keyword)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", "https://www.yunso.net")
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	var apiResp YunsoAPIResponse
	if err := jsonutil.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	if apiResp.Code != 0 {
		return nil, fmt.Errorf("api returned error: %s", apiResp.Msg)
	}

	return p.parseItems(apiResp.Data)
}

func (p *YunsoAsyncPlugin) parseItems(fragment string) ([]YunsoItem, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div id="yunso-root">` + fragment + `</div>`))
	if err != nil {
		return nil, fmt.Errorf("parse html failed: %w", err)
	}

	items := make([]YunsoItem, 0, 16)
	doc.Find("div.layui-card[data-qid]").Each(func(_ int, card *goquery.Selection) {
		anchor := card.Find(`a[onclick*="open_sid"]`).First()
		if anchor.Length() == 0 {
			return
		}

		title := cleanYunsoText(anchor.Text())
		if title == "" {
			return
		}

		encryptedURL, _ := anchor.Attr("url")
		decryptedURL := ""
		if encryptedURL != "" {
			if decoded, err := decryptYunsoURL(encryptedURL); err == nil {
				decryptedURL = decoded
			}
		}

		fileSummary := cleanYunsoText(card.Find(".layui-card-body span").First().Text())
		if strings.Contains(strings.ToUpper(fileSummary), "N/A") {
			fileSummary = ""
		}

		item := YunsoItem{
			Title:        title,
			EncryptedURL: strings.TrimSpace(encryptedURL),
			URL:          strings.TrimSpace(decryptedURL),
			Preview:      cleanYunsoText(card.Find("p.result.container.p").First().Text()),
			FileSummary:  fileSummary,
			Datetime:     parseYunsoDatetime(card.Find(".layui-card-header").Text()),
			TypeName:     cleanYunsoText(card.Find(`img[src*="/assets/xyso/"]`).First().AttrOr("alt", "")),
			TypeCode:     extractYunsoTypeCode(card.Find(`img[src*="/assets/xyso/"]`).First().AttrOr("src", "")),
			Password:     cleanYunsoText(anchor.AttrOr("pa", "")),
			FullID:       strings.TrimSpace(anchor.AttrOr("id", "")),
			QID:          strings.TrimSpace(card.AttrOr("data-qid", "")),
			Badges:       extractYunsoBadges(card),
		}

		if item.Password == "" {
			item.Password = extractYunsoPassword(item.URL)
		}

		items = append(items, item)
	})

	return items, nil
}

func (p *YunsoAsyncPlugin) deduplicateItems(items []YunsoItem) []YunsoItem {
	uniqueMap := make(map[string]YunsoItem)

	for _, item := range items {
		key := item.URL
		if key == "" {
			key = item.FullID
		}
		if key == "" {
			key = item.QID + "|" + item.Title
		}

		existing, exists := uniqueMap[key]
		if !exists || scoreYunsoItem(item) > scoreYunsoItem(existing) {
			uniqueMap[key] = item
		}
	}

	result := make([]YunsoItem, 0, len(uniqueMap))
	for _, item := range uniqueMap {
		result = append(result, item)
	}
	return result
}

func scoreYunsoItem(item YunsoItem) int {
	score := 0
	if item.URL != "" {
		score += 8
	}
	if item.Password != "" {
		score += 5
	}
	if !item.Datetime.IsZero() {
		score += 3
	}
	if item.FileSummary != "" {
		score += 2
	}
	if item.Preview != "" {
		score += 2
	}
	if item.TypeCode != "" {
		score++
	}
	return score
}

func (p *YunsoAsyncPlugin) convertResults(items []YunsoItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for i, item := range items {
		if strings.TrimSpace(item.URL) == "" {
			continue
		}

		contentParts := make([]string, 0, 3)
		if item.Preview != "" {
			contentParts = append(contentParts, item.Preview)
		}
		if item.FileSummary != "" {
			contentParts = append(contentParts, item.FileSummary)
		}
		if item.TypeName != "" {
			contentParts = append(contentParts, "网盘: "+item.TypeName)
		}

		tags := uniqueYunsoStrings(append([]string{item.TypeName}, item.Badges...))
		if len(tags) == 0 {
			tags = nil
		}

		uniqueID := fmt.Sprintf("yunso-%s", item.FullID)
		if item.FullID == "" {
			uniqueID = fmt.Sprintf("yunso-%s-%d", item.QID, i)
		}

		results = append(results, model.SearchResult{
			UniqueID: uniqueID,
			Channel:  "",
			Datetime: item.Datetime,
			Title:    item.Title,
			Content:  strings.Join(contentParts, "\n"),
			Tags:     tags,
			Links: []model.Link{
				{
					URL:      item.URL,
					Type:     p.mapDiskType(item.TypeCode, item.URL),
					Password: item.Password,
					Datetime: item.Datetime,
				},
			},
		})
	}

	return results
}

func (p *YunsoAsyncPlugin) mapDiskType(typeCode string, rawURL string) string {
	switch strings.TrimSpace(typeCode) {
	case "1":
		return "baidu"
	case "20100":
		return "aliyun"
	case "20500":
		return "quark"
	case "20000":
		return "tianyi"
	case "20300":
		return "mobile"
	case "20400":
		return "xunlei"
	case "20501":
		return "uc"
	case "20600":
		return "lanzou"
	}

	lowerURL := strings.ToLower(strings.TrimSpace(rawURL))
	if strings.Contains(lowerURL, "fast.uc.cn") || strings.Contains(lowerURL, "uc.cn") {
		return "uc"
	}
	return util.GetLinkType(lowerURL)
}

func decryptYunsoURL(value string) (string, error) {
	value = strings.TrimSpace(html.UnescapeString(value))
	if value == "" {
		return "", fmt.Errorf("empty encrypted url")
	}
	// Newer responses expose the share URL directly. Keep the legacy
	// base64/XOR path below for older results.
	if strings.HasPrefix(strings.ToLower(value), "http://") || strings.HasPrefix(strings.ToLower(value), "https://") {
		return value, nil
	}

	decoded, err := decodeYunsoBase64(value)
	if err != nil {
		return "", err
	}

	rawText := strings.TrimSpace(string(decoded))
	if strings.HasPrefix(rawText, "http://") || strings.HasPrefix(rawText, "https://") {
		return rawText, nil
	}

	result := make([]byte, len(decoded))
	for i := range decoded {
		result[i] = decoded[i] ^ yunsoDecryptBytes[i%len(yunsoDecryptBytes)]
	}

	return strings.TrimSpace(string(result)), nil
}

func decodeYunsoBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty encrypted url")
	}

	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}

	if mod := len(value) % 4; mod != 0 {
		value += strings.Repeat("=", 4-mod)
	}
	return base64.StdEncoding.DecodeString(value)
}

func extractYunsoTypeCode(iconSrc string) string {
	matches := yunsoTypeCodeRegex.FindStringSubmatch(iconSrc)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func extractYunsoBadges(card *goquery.Selection) []string {
	badges := make([]string, 0, 2)
	card.Find(".badge").Each(func(_ int, badge *goquery.Selection) {
		text := cleanYunsoText(badge.Text())
		if text != "" {
			badges = append(badges, text)
		}
	})
	return uniqueYunsoStrings(badges)
}

func extractYunsoPassword(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	for _, key := range []string{"pwd", "pass", "password"} {
		if value := strings.TrimSpace(parsedURL.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func parseYunsoDatetime(text string) time.Time {
	match := yunsoDatetimeRegex.FindString(text)
	if match == "" {
		return time.Time{}
	}

	parsedTime, err := time.Parse("2006-01-02 15:04:05", match)
	if err != nil {
		return time.Time{}
	}
	return parsedTime
}

func cleanYunsoText(value string) string {
	value = html.UnescapeString(value)
	value = strings.Join(strings.Fields(value), " ")
	return strings.TrimSpace(value)
}

func uniqueYunsoStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = cleanYunsoText(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
