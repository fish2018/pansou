package yingso

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"pansou/model"
	"pansou/plugin"
	jsonutil "pansou/util/json"
)

const (
	pluginName        = "yingso"
	defaultAPIBaseURL = "https://ysapi.yingso.fun"
	websiteURL        = "https://yingso.fun"
	bootstrapKey      = "12345678"
	defaultPriority   = 3
	pageSize          = 30
	requestTimeout    = 25 * time.Second
	maxKeyConcurrency = 8
	maxResponseSize   = 1 << 20
)

type YingsoPlugin struct {
	*plugin.BaseAsyncPlugin
	apiBaseURL string
}

type apiEnvelope[T any] struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
}

type apiConfig struct {
	URLVersion string `json:"url_version"`
	UserID     string `json:"user_id"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
}

type searchPayload struct {
	PageSize int    `json:"pageSize"`
	PageNum  int    `json:"pageNum"`
	Title    string `json:"title"`
	Root     int    `json:"root"`
	Category string `json:"cat"`
	UserID   string `json:"userId"`
}

type getKeyPayload struct {
	ID     int64  `json:"id"`
	UserID string `json:"userId"`
}

type encryptedPayload struct {
	No   string `json:"no"`
	Info string `json:"info"`
}

type searchItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Root  int    `json:"root"`
}

type resolvedItem struct {
	index  int
	result model.SearchResult
	err    error
}

func init() {
	plugin.RegisterGlobalPlugin(NewYingsoPlugin())
}

func NewYingsoPlugin() *YingsoPlugin {
	return &YingsoPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		apiBaseURL:      defaultAPIBaseURL,
	}
}

func (p *YingsoPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *YingsoPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

func (p *YingsoPlugin) searchImpl(client *http.Client, keyword string, _ map[string]interface{}) ([]model.SearchResult, error) {
	keyword = cleanText(keyword)
	if keyword == "" {
		return []model.SearchResult{}, nil
	}
	if client == nil {
		client = http.DefaultClient
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	config, err := p.fetchConfig(ctx, client)
	if err != nil {
		return nil, err
	}

	items, err := p.search(ctx, client, config, keyword)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return []model.SearchResult{}, nil
	}

	resolved := make([]model.SearchResult, len(items))
	valid := make([]bool, len(items))
	resultCh := make(chan resolvedItem, len(items))
	sem := make(chan struct{}, maxKeyConcurrency)
	var wg sync.WaitGroup

	for index, item := range items {
		wg.Add(1)
		go func(index int, item searchItem) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				resultCh <- resolvedItem{index: index, err: ctx.Err()}
				return
			}

			result, resolveErr := p.resolveItem(ctx, client, config, item)
			resultCh <- resolvedItem{index: index, result: result, err: resolveErr}
		}(index, item)
	}

	wg.Wait()
	close(resultCh)

	var firstErr error
	resolvedCount := 0
	for item := range resultCh {
		if item.err != nil {
			if firstErr == nil {
				firstErr = item.err
			}
			continue
		}
		resolved[item.index] = item.result
		valid[item.index] = true
		resolvedCount++
	}
	if resolvedCount == 0 && firstErr != nil {
		return nil, fmt.Errorf("[%s] 获取分享链接失败: %w", p.Name(), firstErr)
	}

	results := make([]model.SearchResult, 0, resolvedCount)
	seen := make(map[string]struct{}, resolvedCount)
	for index, result := range resolved {
		if !valid[index] || len(result.Links) == 0 {
			continue
		}
		linkKey := result.Links[0].URL + "\x00" + result.Links[0].Password
		if _, exists := seen[linkKey]; exists {
			continue
		}
		seen[linkKey] = struct{}{}
		results = append(results, result)
	}

	return plugin.FilterResultsByKeyword(results, keyword), nil
}

func (p *YingsoPlugin) fetchConfig(ctx context.Context, client *http.Client) (apiConfig, error) {
	var response apiEnvelope[string]
	if err := p.requestJSON(ctx, client, http.MethodGet, p.apiBaseURL+"/test", nil, &response); err != nil {
		return apiConfig{}, fmt.Errorf("[%s] 获取接口配置失败: %w", p.Name(), err)
	}
	if response.Code != http.StatusOK || response.Data == "" {
		return apiConfig{}, fmt.Errorf("[%s] 接口配置异常: code=%d msg=%s", p.Name(), response.Code, response.Msg)
	}

	decoded := xorUTF16(response.Data, bootstrapKey)
	var config apiConfig
	if err := jsonutil.UnmarshalString(decoded, &config); err != nil {
		return apiConfig{}, fmt.Errorf("[%s] 解析接口配置失败: %w", p.Name(), err)
	}
	if config.URLVersion == "" || config.UserID == "" || config.Start < 0 || config.End <= config.Start || config.End > 24 {
		return apiConfig{}, fmt.Errorf("[%s] 接口配置缺少必要字段", p.Name())
	}
	return config, nil
}

func (p *YingsoPlugin) search(ctx context.Context, client *http.Client, config apiConfig, keyword string) ([]searchItem, error) {
	payload := searchPayload{
		PageSize: pageSize,
		PageNum:  1,
		Title:    keyword,
		Root:     0,
		Category: "all",
		UserID:   config.UserID,
	}
	encrypted, err := encryptPayload(payload, config)
	if err != nil {
		return nil, fmt.Errorf("[%s] 生成搜索参数失败: %w", p.Name(), err)
	}

	var response apiEnvelope[[]searchItem]
	endpoint := fmt.Sprintf("%s/%s/search", p.apiBaseURL, url.PathEscape(config.URLVersion))
	if err := p.requestJSON(ctx, client, http.MethodPost, endpoint, encrypted, &response); err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	if response.Code != http.StatusOK {
		return nil, fmt.Errorf("[%s] 搜索接口异常: code=%d msg=%s", p.Name(), response.Code, response.Msg)
	}
	return response.Data, nil
}

func (p *YingsoPlugin) resolveItem(ctx context.Context, client *http.Client, config apiConfig, item searchItem) (model.SearchResult, error) {
	payload := getKeyPayload{ID: item.ID, UserID: config.UserID}
	encrypted, err := encryptPayload(payload, config)
	if err != nil {
		return model.SearchResult{}, err
	}

	var response apiEnvelope[string]
	endpoint := fmt.Sprintf("%s/%s/getKey", p.apiBaseURL, url.PathEscape(config.URLVersion))
	if err := p.requestJSON(ctx, client, http.MethodPost, endpoint, encrypted, &response); err != nil {
		return model.SearchResult{}, err
	}
	if response.Code != http.StatusOK || strings.TrimSpace(response.Data) == "" {
		return model.SearchResult{}, fmt.Errorf("getKey id=%d code=%d msg=%s", item.ID, response.Code, response.Msg)
	}

	linkType, linkURL, password := buildLink(item.Root, response.Data)
	if linkURL == "" {
		return model.SearchResult{}, fmt.Errorf("不支持的网盘类型 root=%d id=%d", item.Root, item.ID)
	}

	title := cleanText(item.Title)
	if title == "" {
		title = fmt.Sprintf("影搜资源 %d", item.ID)
	}
	id := fmt.Sprintf("%s-%d", pluginName, item.ID)
	now := time.Now()
	return model.SearchResult{
		MessageID: id,
		UniqueID:  id,
		Channel:   "",
		Datetime:  now,
		Title:     title,
		Content:   title,
		Links: []model.Link{{
			Type:      linkType,
			URL:       linkURL,
			Password:  password,
			Datetime:  now,
			WorkTitle: title,
		}},
	}, nil
}

func (p *YingsoPlugin) requestJSON(ctx context.Context, client *http.Client, method string, endpoint string, payload interface{}, target interface{}) error {
	var body io.Reader
	if payload != nil {
		encoded, err := jsonutil.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Origin", websiteURL)
	req.Header.Set("Referer", websiteURL+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0.0.0 Safari/537.36")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseSize {
		return fmt.Errorf("响应超过 %d 字节", maxResponseSize)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := jsonutil.Unmarshal(data, target); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	return nil
}

func encryptPayload(payload interface{}, config apiConfig) (encryptedPayload, error) {
	no, err := randomToken(24)
	if err != nil {
		return encryptedPayload{}, err
	}
	if config.Start < 0 || config.End <= config.Start || config.End > len(no) {
		return encryptedPayload{}, fmt.Errorf("无效的加密区间 %d:%d", config.Start, config.End)
	}

	encoded, err := jsonutil.MarshalString(payload)
	if err != nil {
		return encryptedPayload{}, err
	}
	return encryptedPayload{
		No:   no,
		Info: xorUTF16(encoded, no[config.Start:config.End]),
	}, nil
}

func randomToken(length int) (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	for index := range data {
		data[index] = alphabet[int(data[index])%len(alphabet)]
	}
	return string(data), nil
}

func xorUTF16(input string, key string) string {
	if key == "" {
		return ""
	}
	inputUnits := utf16.Encode([]rune(input))
	keyUnits := utf16.Encode([]rune(key))
	output := make([]uint16, len(inputUnits))
	for index, unit := range inputUnits {
		output[index] = unit ^ keyUnits[index%len(keyUnits)]
	}
	return string(utf16.Decode(output))
}

func buildLink(root int, rawKey string) (string, string, string) {
	shareKey := strings.TrimSpace(rawKey)
	if shareKey == "" {
		return "", "", ""
	}

	linkType := ""
	prefix := ""
	switch root {
	case 1:
		linkType, prefix = "aliyun", "https://www.aliyundrive.com/s/"
	case 2:
		linkType, prefix = "quark", "https://pan.quark.cn/s/"
	case 3:
		linkType, prefix = "xunlei", "https://pan.xunlei.com/s/"
	case 4:
		linkType, prefix = "baidu", "https://pan.baidu.com/s/"
	case 5:
		linkType, prefix = "uc", "https://drive.uc.cn/s/"
	default:
		return "", "", ""
	}

	linkURL := shareKey
	if !strings.HasPrefix(linkURL, "http://") && !strings.HasPrefix(linkURL, "https://") {
		linkURL = prefix + strings.TrimLeft(linkURL, "/")
	}
	parsed, err := url.Parse(linkURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", ""
	}

	password := ""
	for _, field := range []string{"pwd", "password", "code"} {
		if value := strings.TrimSpace(parsed.Query().Get(field)); value != "" {
			password = value
			break
		}
	}
	return linkType, parsed.String(), password
}

func cleanText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
