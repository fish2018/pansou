package xiaokupan

import (
	"context"
	"crypto/sha256"
	stdjson "encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
)

const (
	pluginName              = "xiaokupan"
	defaultBaseURL          = "https://xiaokupan.com"
	defaultServerFunctionID = "ffb7ba806a267ced7478dc27716e79ea729a98a801af2ac9c3647bdaca91af78"
	defaultPriority         = 2
	requestTimeout          = 30 * time.Second
	maxSearchResponseSize   = 4 << 20
	maxDiscoveryBodySize    = 8 << 20
)

var (
	indexAssetPattern = regexp.MustCompile(`/assets/index-[A-Za-z0-9_-]+\.js`)
	hashPattern       = regexp.MustCompile(`[a-f0-9]{64}`)
	whitespacePattern = regexp.MustCompile(`\s+`)
)

type XiaokupanPlugin struct {
	*plugin.BaseAsyncPlugin
	baseURL string

	serverFunctionMu sync.RWMutex
	serverFunctionID string
}

type serovalNode struct {
	Type   int                `json:"t"`
	ID     *int               `json:"i,omitempty"`
	Scalar stdjson.RawMessage `json:"s,omitempty"`
	Props  *serovalProps      `json:"p,omitempty"`
	Array  []*serovalNode     `json:"a,omitempty"`
}

type serovalProps struct {
	Keys   []string       `json:"k"`
	Values []*serovalNode `json:"v"`
}

type serovalDecoder struct {
	references map[int]*serovalNode
}

func init() {
	plugin.RegisterGlobalPlugin(NewXiaokupanPlugin())
}

func NewXiaokupanPlugin() *XiaokupanPlugin {
	return &XiaokupanPlugin{
		BaseAsyncPlugin:  plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		baseURL:          defaultBaseURL,
		serverFunctionID: defaultServerFunctionID,
	}
}

func (p *XiaokupanPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *XiaokupanPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *XiaokupanPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = cleanText(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	functionID := p.currentServerFunctionID()
	results, err := p.searchWithFunctionID(client, keyword, functionID)
	if err != nil {
		refreshedID, refreshErr := p.refreshServerFunctionID(client, functionID)
		if refreshErr != nil {
			return nil, fmt.Errorf("[%s] 搜索失败: %v；刷新接口标识失败: %w", p.Name(), err, refreshErr)
		}
		results, err = p.searchWithFunctionID(client, keyword, refreshedID)
	}
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索失败: %w", p.Name(), err)
	}
	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *XiaokupanPlugin) searchWithFunctionID(client *http.Client, keyword, functionID string) ([]model.SearchResult, error) {
	payload, err := buildSearchPayload(keyword)
	if err != nil {
		return nil, fmt.Errorf("构造搜索参数失败: %w", err)
	}

	endpoint := fmt.Sprintf("%s/_serverFn/%s", strings.TrimRight(p.baseURL, "/"), functionID)
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("解析搜索地址失败: %w", err)
	}
	query := parsedEndpoint.Query()
	query.Set("payload", string(payload))
	parsedEndpoint.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedEndpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("创建搜索请求失败: %w", err)
	}
	p.setSearchHeaders(req, keyword)

	body, err := doLimitedRequest(client, req, maxSearchResponseSize)
	if err != nil {
		return nil, err
	}
	return parseSearchResponse(body)
}

func buildSearchPayload(keyword string) ([]byte, error) {
	payload := map[string]interface{}{
		"t": map[string]interface{}{
			"t": 10,
			"i": 0,
			"p": map[string]interface{}{
				"k": []string{"data"},
				"v": []interface{}{map[string]interface{}{
					"t": 10,
					"i": 1,
					"p": map[string]interface{}{
						"k": []string{"query"},
						"v": []interface{}{map[string]interface{}{
							"t": 1,
							"s": keyword,
						}},
					},
					"o": 0,
				}},
			},
			"o": 0,
		},
		"f": 63,
		"m": []interface{}{},
	}
	return stdjson.Marshal(payload)
}

func (p *XiaokupanPlugin) setSearchHeaders(req *http.Request, keyword string) {
	base := strings.TrimRight(p.baseURL, "/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/x-tss-framed, application/x-ndjson, application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/s/"+url.PathEscape(keyword))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("x-tsr-serverFn", "true")
}

func doLimitedRequest(client *http.Client, req *http.Request, limit int64) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("响应超过 %d 字节", limit)
	}
	return body, nil
}

func (p *XiaokupanPlugin) currentServerFunctionID() string {
	p.serverFunctionMu.RLock()
	defer p.serverFunctionMu.RUnlock()
	return p.serverFunctionID
}

func (p *XiaokupanPlugin) refreshServerFunctionID(client *http.Client, staleID string) (string, error) {
	p.serverFunctionMu.Lock()
	defer p.serverFunctionMu.Unlock()
	if p.serverFunctionID != "" && p.serverFunctionID != staleID {
		return p.serverFunctionID, nil
	}

	discoveredID, err := p.discoverServerFunctionID(client)
	if err != nil {
		return "", err
	}
	p.serverFunctionID = discoveredID
	return discoveredID, nil
}

func (p *XiaokupanPlugin) discoverServerFunctionID(client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	homeURL := strings.TrimRight(p.baseURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, homeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	homeBody, err := doLimitedRequest(client, req, maxDiscoveryBodySize)
	if err != nil {
		return "", fmt.Errorf("读取首页失败: %w", err)
	}

	assetPath := string(indexAssetPattern.Find(homeBody))
	if assetPath == "" {
		return "", fmt.Errorf("首页未找到入口脚本")
	}
	assetReq, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.baseURL, "/")+assetPath, nil)
	if err != nil {
		return "", err
	}
	assetReq.Header.Set("User-Agent", req.Header.Get("User-Agent"))
	assetReq.Header.Set("Referer", homeURL)
	assetBody, err := doLimitedRequest(client, assetReq, maxDiscoveryBodySize)
	if err != nil {
		return "", fmt.Errorf("读取入口脚本失败: %w", err)
	}

	routeIndex := strings.Index(string(assetBody), "/s/$query")
	if routeIndex < 0 {
		return "", fmt.Errorf("入口脚本未找到搜索路由")
	}
	windowStart := max(0, routeIndex-2048)
	hashes := hashPattern.FindAll(assetBody[windowStart:routeIndex], -1)
	if len(hashes) == 0 {
		return "", fmt.Errorf("入口脚本未找到搜索接口标识")
	}
	return string(hashes[len(hashes)-1]), nil
}

func parseSearchResponse(body []byte) ([]model.SearchResult, error) {
	var root serovalNode
	if err := stdjson.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("解析 Seroval 响应失败: %w", err)
	}
	decoder := newSerovalDecoder(&root)
	resultNode := decoder.objectValue(&root, "result")
	searchResultsNode := decoder.objectValue(resultNode, "searchResults")
	mergedNode := decoder.objectValue(searchResultsNode, "merged_by_type")
	mergedNode = decoder.resolve(mergedNode)
	if mergedNode == nil || mergedNode.Props == nil {
		return nil, fmt.Errorf("响应缺少 merged_by_type")
	}

	results := make([]model.SearchResult, 0)
	seenURLs := make(map[string]struct{})
	for index, linkType := range mergedNode.Props.Keys {
		if index >= len(mergedNode.Props.Values) {
			break
		}
		arrayNode := decoder.resolve(mergedNode.Props.Values[index])
		if arrayNode == nil || arrayNode.Type != 9 {
			continue
		}
		for _, itemNode := range arrayNode.Array {
			resourceURL := strings.TrimSpace(decoder.stringValue(decoder.objectValue(itemNode, "url")))
			if !validResourceURL(resourceURL, linkType) {
				continue
			}
			if _, exists := seenURLs[resourceURL]; exists {
				continue
			}

			note := cleanText(decoder.stringValue(decoder.objectValue(itemNode, "note")))
			if note == "" {
				continue
			}
			password := strings.TrimSpace(decoder.stringValue(decoder.objectValue(itemNode, "password")))
			source := cleanText(decoder.stringValue(decoder.objectValue(itemNode, "source")))
			datetime := parseRFC3339(decoder.stringValue(decoder.objectValue(itemNode, "datetime")))
			images := decoder.stringArray(decoder.objectValue(itemNode, "images"))
			uniqueID := fmt.Sprintf("%s-%x", pluginName, sha256.Sum256([]byte(resourceURL)))
			tags := []string{linkType}
			if source != "" {
				tags = append(tags, source)
			}

			results = append(results, model.SearchResult{
				MessageID: uniqueID,
				UniqueID:  uniqueID,
				Channel:   "",
				Datetime:  datetime,
				Title:     note,
				Content:   note,
				Links: []model.Link{{
					Type:      linkType,
					URL:       resourceURL,
					Password:  password,
					Datetime:  datetime,
					WorkTitle: note,
				}},
				Tags:   tags,
				Images: images,
			})
			seenURLs[resourceURL] = struct{}{}
		}
	}
	return results, nil
}

func newSerovalDecoder(root *serovalNode) *serovalDecoder {
	decoder := &serovalDecoder{references: make(map[int]*serovalNode)}
	decoder.collect(root)
	return decoder
}

func (d *serovalDecoder) collect(node *serovalNode) {
	if node == nil {
		return
	}
	if node.ID != nil {
		d.references[*node.ID] = node
	}
	if node.Props != nil {
		for _, child := range node.Props.Values {
			d.collect(child)
		}
	}
	for _, child := range node.Array {
		d.collect(child)
	}
}

func (d *serovalDecoder) resolve(node *serovalNode) *serovalNode {
	for depth := 0; node != nil && node.Type == 4 && depth < 16; depth++ {
		var referenceID int
		if err := stdjson.Unmarshal(node.Scalar, &referenceID); err != nil {
			return nil
		}
		node = d.references[referenceID]
	}
	return node
}

func (d *serovalDecoder) objectValue(node *serovalNode, key string) *serovalNode {
	node = d.resolve(node)
	if node == nil || node.Props == nil {
		return nil
	}
	for index, candidate := range node.Props.Keys {
		if candidate == key && index < len(node.Props.Values) {
			return d.resolve(node.Props.Values[index])
		}
	}
	return nil
}

func (d *serovalDecoder) stringValue(node *serovalNode) string {
	node = d.resolve(node)
	if node == nil || node.Type != 1 {
		return ""
	}
	var value string
	if err := stdjson.Unmarshal(node.Scalar, &value); err != nil {
		return ""
	}
	return value
}

func (d *serovalDecoder) stringArray(node *serovalNode) []string {
	node = d.resolve(node)
	if node == nil || node.Type != 9 {
		return nil
	}
	values := make([]string, 0, len(node.Array))
	for _, child := range node.Array {
		if value := strings.TrimSpace(d.stringValue(child)); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func validResourceURL(rawURL, linkType string) bool {
	if linkType == "magnet" {
		return strings.HasPrefix(strings.ToLower(rawURL), "magnet:?")
	}
	if linkType == "ed2k" {
		return strings.HasPrefix(strings.ToLower(rawURL), "ed2k://")
	}
	parsed, err := url.Parse(rawURL)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func parseRFC3339(value string) time.Time {
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return time.Now()
}

func cleanText(value string) string {
	value = whitespacePattern.ReplaceAllString(strings.TrimSpace(value), " ")
	for _, suffix := range []string{"投稿链接:", "投稿链接：", "链接:", "链接："} {
		value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
	}
	return value
}
