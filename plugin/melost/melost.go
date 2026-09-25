package melost

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"
)

var (
	debugEnabled = false
	htmlTagRegex = regexp.MustCompile(`<[^>]+>`)
)

func debugLog(format string, args ...interface{}) {
	if debugEnabled {
		log.Printf("[melost DEBUG] "+format, args...)
	}
}

func init() {
	plugin.RegisterGlobalPlugin(NewMelostAsyncPlugin())
}

const (
	MelostSearchAPI  = "https://www.melost.cn/v1/search/disk"
	DefaultPageSize  = 30
	DefaultMaxPages  = 3
	DefaultReferer   = "https://www.melost.cn/search"
	DefaultTimeout   = 30 * time.Second
	DefaultAutomated = "0"
)

// MelostAsyncPlugin 影盘社搜索异步插件
type MelostAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewMelostAsyncPlugin 创建新的影盘社搜索插件
func NewMelostAsyncPlugin() *MelostAsyncPlugin {
	return &MelostAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("melost", 3),
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *MelostAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含 IsFinal 标记的结果
func (p *MelostAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际搜索实现
func (p *MelostAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	debugLog("开始搜索，关键词: %s", keyword)

	resultChan := make(chan []MelostItem, DefaultMaxPages)
	errChan := make(chan error, DefaultMaxPages)

	var wg sync.WaitGroup
	for page := 1; page <= DefaultMaxPages; page++ {
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

	var allItems []MelostItem
	for items := range resultChan {
		allItems = append(allItems, items...)
	}

	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	debugLog("收集到 %d 条原始结果，%d 个错误", len(allItems), len(errors))

	if len(allItems) == 0 && len(errors) > 0 {
		return nil, errors[0]
	}

	uniqueItems := p.deduplicateItems(allItems)
	results := p.convertResults(uniqueItems)
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	debugLog("去重后 %d 条，过滤后 %d 条", len(uniqueItems), len(filteredResults))
	return filteredResults, nil
}

func (p *MelostAsyncPlugin) searchPage(client *http.Client, keyword string, pageNum int) ([]MelostItem, error) {
	reqBody := map[string]interface{}{
		"page":          pageNum,
		"q":             keyword,
		"user":          "",
		"exact":         false,
		"user_distinct": false,
		"format":        []string{},
		"share_time":    "",
		"share_year":    "",
		"size":          DefaultPageSize,
		"order":         "",
		"type":          "",
		"search_ticket": "",
		"exclude_user":  []string{},
		"adv_params": map[string]interface{}{
			"wechat_pwd":  "",
			"search_code": "",
			"platform":    "pc",
			"fp_data":     "",
			"automated":   DefaultAutomated,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", MelostSearchAPI, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Origin", "https://www.melost.cn")
	req.Header.Set("Referer", DefaultReferer)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	respBody, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	var apiResp MelostResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	if apiResp.Code != 200 {
		return nil, fmt.Errorf("api returned error: %s", apiResp.Msg)
	}

	return apiResp.Data.List, nil
}

func (p *MelostAsyncPlugin) deduplicateItems(items []MelostItem) []MelostItem {
	uniqueMap := make(map[string]MelostItem)

	for _, item := range items {
		key := item.DiskID
		if key == "" {
			key = item.Link
		}
		if key == "" {
			key = item.DiskName + "|" + item.DiskType
		}

		existing, exists := uniqueMap[key]
		if !exists {
			uniqueMap[key] = item
			continue
		}

		existingScore := scoreItem(existing)
		newScore := scoreItem(item)
		if newScore > existingScore {
			uniqueMap[key] = item
		}
	}

	result := make([]MelostItem, 0, len(uniqueMap))
	for _, item := range uniqueMap {
		result = append(result, item)
	}

	return result
}

func scoreItem(item MelostItem) int {
	score := len(item.Files)
	if strings.TrimSpace(item.DiskPass) != "" {
		score += 5
	}
	if strings.TrimSpace(item.SharedTime) != "" {
		score += 3
	}
	if strings.TrimSpace(item.ShareUser) != "" {
		score += 2
	}
	if item.Tags != nil {
		score += 2
	}
	return score
}

func (p *MelostAsyncPlugin) convertResults(items []MelostItem) []model.SearchResult {
	results := make([]model.SearchResult, 0, len(items))

	for i, item := range items {
		if strings.TrimSpace(item.Link) == "" {
			continue
		}

		linkType := p.convertDiskType(item.DiskType, item.Link)
		password := strings.TrimSpace(item.DiskPass)
		tags := p.processTags(item.Tags)
		title := cleanHTML(item.DiskName)

		contentParts := make([]string, 0, 3)
		if files := cleanHTML(item.Files); files != "" {
			contentParts = append(contentParts, files)
		}
		if shareUser := strings.TrimSpace(item.ShareUser); shareUser != "" {
			contentParts = append(contentParts, "分享用户: "+shareUser)
		}
		if len(tags) > 0 {
			contentParts = append(contentParts, "标签: "+strings.Join(tags, "、"))
		}

		uniqueID := fmt.Sprintf("melost-%s", item.DiskID)
		if item.DiskID == "" {
			uniqueID = fmt.Sprintf("melost-%d-%d", time.Now().UnixNano(), i)
		}

		result := model.SearchResult{
			UniqueID: uniqueID,
			Channel:  "",
			Title:    title,
			Content:  strings.Join(contentParts, "\n"),
			Datetime: parseDatetime(item.SharedTime),
			Tags:     tags,
			Links: []model.Link{
				{
					URL:      strings.TrimSpace(item.Link),
					Type:     linkType,
					Password: password,
				},
			},
		}

		results = append(results, result)
	}

	return results
}

func (p *MelostAsyncPlugin) convertDiskType(diskType string, rawURL string) string {
	switch strings.ToUpper(strings.TrimSpace(diskType)) {
	case "BDY", "BAIDU":
		return "baidu"
	case "ALY", "ALIYUN":
		return "aliyun"
	case "QUARK":
		return "quark"
	case "TIANYI":
		return "tianyi"
	case "UC":
		return "uc"
	case "CAIYUN", "MOBILE":
		return "mobile"
	case "115":
		return "115"
	case "XUNLEI":
		return "xunlei"
	case "123", "123PAN":
		return "123"
	case "PIKPAK":
		return "pikpak"
	case "LANZOU":
		return "lanzou"
	default:
		return util.GetLinkType(strings.TrimSpace(rawURL))
	}
}

func (p *MelostAsyncPlugin) processTags(tags interface{}) []string {
	if tags == nil {
		return nil
	}

	tagArray, ok := tags.([]interface{})
	if !ok {
		return nil
	}

	result := make([]string, 0, len(tagArray))
	for _, tag := range tagArray {
		tagStr, ok := tag.(string)
		if !ok {
			continue
		}

		tagStr = strings.TrimSpace(tagStr)
		if tagStr == "" {
			continue
		}

		result = append(result, tagStr)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

func parseDatetime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}

	parsedTime, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return time.Time{}
	}

	return parsedTime
}

func cleanHTML(value string) string {
	if value == "" {
		return ""
	}

	value = html.UnescapeString(value)
	value = htmlTagRegex.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)

	return value
}

// MelostResponse 影盘社搜索接口响应
type MelostResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Total            int          `json:"total"`
		PerSize          int          `json:"per_size"`
		Took             int          `json:"took"`
		SearchResultText string       `json:"search_result_text"`
		List             []MelostItem `json:"list"`
	} `json:"data"`
}

// MelostItem 影盘社搜索结果项
type MelostItem struct {
	DiskID      string      `json:"disk_id"`
	DiskName    string      `json:"disk_name"`
	DiskPass    string      `json:"disk_pass"`
	DiskType    string      `json:"disk_type"`
	Files       string      `json:"files"`
	DocID       string      `json:"doc_id"`
	ShareUser   string      `json:"share_user"`
	ShareUserID string      `json:"share_user_id"`
	SharedTime  string      `json:"shared_time"`
	RelMovie    string      `json:"rel_movie"`
	IsMine      bool        `json:"is_mine"`
	Tags        interface{} `json:"tags"`
	Link        string      `json:"link"`
	Enabled     bool        `json:"enabled"`
	Weight      int         `json:"weight"`
	Status      int         `json:"status"`
}
