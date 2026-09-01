package haitunsou

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"pansou/model"
	"pansou/plugin"
	jsonutil "pansou/util/json"
)

const (
	pluginName      = "haitunsou"
	defaultBaseURL  = "https://www.haitunsou.com"
	defaultPriority = 2
	requestTimeout  = 25 * time.Second
	maxResponseSize = 3 << 20
	timeLayout      = "2006-01-02"
)

var listMarkers = [][]byte{
	[]byte("const list = JSON.parse('"),
	[]byte("let listItems = JSON.parse('"),
}

type HaitunsouPlugin struct {
	*plugin.BaseAsyncPlugin
	baseURL string
}

type searchItem struct {
	ID               int64       `json:"id"`
	SourceCategoryID int64       `json:"source_category_id"`
	Title            string      `json:"title"`
	IsType           int         `json:"is_type"`
	Code             string      `json:"code"`
	URL              string      `json:"url"`
	Name             string      `json:"name"`
	Times            string      `json:"times"`
	Category         interface{} `json:"category"`
}

func init() {
	plugin.RegisterGlobalPlugin(NewHaitunsouPlugin())
}

func NewHaitunsouPlugin() *HaitunsouPlugin {
	return &HaitunsouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		baseURL:         defaultBaseURL,
	}
}

func (p *HaitunsouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *HaitunsouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *HaitunsouPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
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

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索请求返回 HTTP %d", p.Name(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索响应失败: %w", p.Name(), err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("[%s] 搜索响应超过 %d 字节", p.Name(), maxResponseSize)
	}

	items, err := parseEmbeddedList(body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果失败: %w", p.Name(), err)
	}
	results := make([]model.SearchResult, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		result, ok := convertItem(item)
		if !ok {
			continue
		}
		key := result.Links[0].URL + "\x00" + result.Links[0].Password
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		results = append(results, result)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func setRequestHeaders(req *http.Request, baseURL string) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Referer", strings.TrimRight(baseURL, "/")+"/")
}

func parseEmbeddedList(body []byte) ([]searchItem, error) {
	start := -1
	for _, marker := range listMarkers {
		if index := indexBytes(body, marker); index >= 0 {
			start = index + len(marker)
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("未找到页面内嵌结果")
	}

	encoded, err := scanSingleQuotedString(body, start)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJSSingleQuotedString(encoded)
	if err != nil {
		return nil, err
	}
	var items []searchItem
	if err := jsonutil.Unmarshal(decoded, &items); err != nil {
		return nil, fmt.Errorf("解析结果 JSON 失败: %w", err)
	}
	return items, nil
}

func indexBytes(data, marker []byte) int {
	return bytes.Index(data, marker)
}

func scanSingleQuotedString(body []byte, start int) ([]byte, error) {
	escaped := false
	for index := start; index < len(body); index++ {
		if escaped {
			escaped = false
			continue
		}
		switch body[index] {
		case '\\':
			escaped = true
		case '\'':
			return body[start:index], nil
		}
	}
	return nil, fmt.Errorf("内嵌结果字符串未闭合")
}

func decodeJSSingleQuotedString(encoded []byte) ([]byte, error) {
	decoded := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); index++ {
		if encoded[index] != '\\' {
			decoded = append(decoded, encoded[index])
			continue
		}
		index++
		if index >= len(encoded) {
			return nil, fmt.Errorf("字符串以转义符结尾")
		}
		switch encoded[index] {
		case '\\', '\'', '"', '/':
			decoded = append(decoded, encoded[index])
		case 'b':
			decoded = append(decoded, '\b')
		case 'f':
			decoded = append(decoded, '\f')
		case 'n':
			decoded = append(decoded, '\n')
		case 'r':
			decoded = append(decoded, '\r')
		case 't':
			decoded = append(decoded, '\t')
		case 'v':
			decoded = append(decoded, '\v')
		case '0':
			decoded = append(decoded, 0)
		case '\n':
			// JavaScript line continuation.
		case '\r':
			if index+1 < len(encoded) && encoded[index+1] == '\n' {
				index++
			}
		case 'x':
			value, next, err := decodeHexEscape(encoded, index+1, 2)
			if err != nil {
				return nil, err
			}
			decoded = utf8.AppendRune(decoded, rune(value))
			index = next - 1
		case 'u':
			value, next, err := decodeHexEscape(encoded, index+1, 4)
			if err != nil {
				return nil, err
			}
			r := rune(value)
			index = next - 1
			if r >= 0xD800 && r <= 0xDBFF && next+6 <= len(encoded) && encoded[next] == '\\' && encoded[next+1] == 'u' {
				low, lowNext, lowErr := decodeHexEscape(encoded, next+2, 4)
				if lowErr == nil && low >= 0xDC00 && low <= 0xDFFF {
					r = utf16.DecodeRune(r, rune(low))
					index = lowNext - 1
				}
			}
			decoded = utf8.AppendRune(decoded, r)
		default:
			decoded = append(decoded, encoded[index])
		}
	}
	return decoded, nil
}

func decodeHexEscape(data []byte, start, length int) (uint64, int, error) {
	end := start + length
	if start < 0 || end > len(data) {
		return 0, start, fmt.Errorf("转义序列不完整")
	}
	value, err := strconv.ParseUint(string(data[start:end]), 16, 16)
	if err != nil {
		return 0, start, fmt.Errorf("无效十六进制转义: %w", err)
	}
	return value, end, nil
}

func convertItem(item searchItem) (model.SearchResult, bool) {
	rawTitle := cleanText(item.Title)
	if rawTitle == "" {
		rawTitle = cleanText(item.Name)
	}
	if rawTitle == "" {
		return model.SearchResult{}, false
	}
	title, description := splitTitleAndDescription(rawTitle)
	if title == "" {
		return model.SearchResult{}, false
	}

	linkType, normalizedURL, password := normalizeLink(item.URL, item.Code, item.IsType)
	if normalizedURL == "" {
		return model.SearchResult{}, false
	}
	datetime := time.Now()
	if parsed, err := time.ParseInLocation(timeLayout, strings.TrimSpace(item.Times), time.Local); err == nil {
		datetime = parsed
	}
	content := cleanText(item.Name)
	if content == "" {
		content = description
	}
	if content == "" {
		content = rawTitle
	}

	uniqueID := ""
	if item.ID > 0 {
		uniqueID = fmt.Sprintf("%s-%d", pluginName, item.ID)
	} else {
		uniqueID = fmt.Sprintf("%s-%x", pluginName, sha256.Sum256([]byte(normalizedURL)))
	}
	return model.SearchResult{
		MessageID: uniqueID,
		UniqueID:  uniqueID,
		Channel:   "",
		Datetime:  datetime,
		Title:     title,
		Content:   content,
		Links: []model.Link{{
			Type:      linkType,
			URL:       normalizedURL,
			Password:  password,
			Datetime:  datetime,
			WorkTitle: title,
		}},
		Tags: []string{linkType},
	}, true
}

func splitTitleAndDescription(value string) (string, string) {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"资源标题：", "资源标题:"} {
		value = strings.TrimSpace(strings.TrimPrefix(value, prefix))
	}
	for _, marker := range []string{"资源描述：", "资源描述:"} {
		if index := strings.Index(value, marker); index >= 0 {
			return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+len(marker):])
		}
	}
	return value, ""
}

func normalizeLink(rawURL, declaredPassword string, declaredType int) (string, string, string) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", ""
	}
	host := strings.ToLower(parsed.Hostname())
	linkType := ""
	switch {
	case host == "pan.quark.cn":
		linkType = "quark"
	case host == "www.aliyundrive.com" || host == "aliyundrive.com" || host == "www.alipan.com" || host == "alipan.com":
		linkType = "aliyun"
	case host == "pan.baidu.com":
		linkType = "baidu"
	case host == "drive.uc.cn":
		linkType = "uc"
	case host == "pan.xunlei.com":
		linkType = "xunlei"
	default:
		return "", "", ""
	}
	declaredTypes := map[int]string{0: "quark", 1: "aliyun", 2: "baidu", 3: "uc", 4: "xunlei"}
	if expected, exists := declaredTypes[declaredType]; exists && expected != linkType {
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

func cleanText(value string) string {
	return strings.Join(strings.Fields(html.UnescapeString(strings.TrimSpace(value))), " ")
}
