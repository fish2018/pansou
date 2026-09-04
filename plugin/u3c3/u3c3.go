package u3c3

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	BaseURL       = "https://u3c3.com"
	legacyBaseURL = "https://u3c3u3c3.u3c3u3c3u3c3.com"
	UserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
	MaxRetries    = 2
	RetryDelay    = 500 * time.Millisecond
)

// U3c3Plugin U3C3插件
type U3c3Plugin struct {
	*plugin.BaseAsyncPlugin
	debugMode bool
	search2   string // 缓存的search2参数
	lastSync  time.Time
	baseMu    sync.RWMutex
	activeURL string
}

func (p *U3c3Plugin) baseCandidates() []string {
	p.baseMu.RLock()
	active := p.activeURL
	p.baseMu.RUnlock()
	result := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, candidate := range []string{active, BaseURL, legacyBaseURL} {
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		result = append(result, candidate)
	}
	return result
}

func (p *U3c3Plugin) getActiveURL() string {
	p.baseMu.RLock()
	defer p.baseMu.RUnlock()
	if p.activeURL == "" {
		return BaseURL
	}
	return p.activeURL
}

func (p *U3c3Plugin) setActiveURL(baseURL string) {
	p.baseMu.Lock()
	p.activeURL = baseURL
	p.baseMu.Unlock()
}

func init() {
	p := &U3c3Plugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("u3c3", 5, true),
		debugMode:       false,
		activeURL:       BaseURL,
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 搜索接口实现
func (p *U3c3Plugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 搜索并返回详细结果
func (p *U3c3Plugin) SearchWithResult(keyword string, ext map[string]interface{}) (*model.PluginSearchResult, error) {
	if p.debugMode {
		log.Printf("[U3C3] 开始搜索: %s", keyword)
	}

	// 第一步：获取search2参数
	search2, err := p.getSearch2Parameter()
	if err != nil {
		if p.debugMode {
			log.Printf("[U3C3] 获取search2参数失败: %v", err)
		}
		return nil, fmt.Errorf("获取search2参数失败: %v", err)
	}

	// 第二步：执行搜索
	results, err := p.doSearch(keyword, search2)
	if err != nil {
		if p.debugMode {
			log.Printf("[U3C3] 搜索失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[U3C3] 搜索完成，获得 %d 个结果", len(results))
	}

	// 应用关键词过滤
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	return &model.PluginSearchResult{
		Results:   filteredResults,
		IsFinal:   true,
		Timestamp: time.Now(),
		Source:    p.Name(),
		Message:   fmt.Sprintf("找到 %d 个结果", len(filteredResults)),
	}, nil
}

// getSearch2Parameter 获取search2参数
func (p *U3c3Plugin) getSearch2Parameter() (string, error) {
	// 如果缓存有效（1小时内），直接返回
	if p.search2 != "" && time.Since(p.lastSync) < time.Hour {
		return p.search2, nil
	}

	if p.debugMode {
		log.Printf("[U3C3] 正在获取search2参数...")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var search2 string
	var lastErr error
	for _, baseURL := range p.baseCandidates() {
		req, err := http.NewRequest("GET", baseURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

		var resp *http.Response
		for i := 0; i < MaxRetries; i++ {
			resp, lastErr = client.Do(req.Clone(req.Context()))
			if lastErr == nil && resp.StatusCode == 200 {
				break
			}
			if resp != nil {
				resp.Body.Close()
			}
			if i < MaxRetries-1 {
				time.Sleep(RetryDelay)
			}
		}
		if lastErr != nil || resp == nil || resp.StatusCode != 200 {
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		search2 = p.extractSearch2FromHTML(string(body))
		if search2 != "" {
			p.setActiveURL(baseURL)
			break
		}
		lastErr = fmt.Errorf("无法从首页提取search2参数")
	}
	if search2 == "" {
		if lastErr == nil {
			lastErr = fmt.Errorf("u3c3 所有域名均不可用")
		}
		return "", lastErr
	}

	// 缓存参数
	p.search2 = search2
	p.lastSync = time.Now()

	if p.debugMode {
		log.Printf("[U3C3] 获取到search2参数: %s", search2)
	}

	return search2, nil
}

// extractSearch2FromHTML 从HTML中提取search2参数
func (p *U3c3Plugin) extractSearch2FromHTML(html string) string {
	// 按行处理，排除注释行
	lines := strings.Split(html, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)

		// 跳过注释行
		if strings.HasPrefix(line, "//") {
			continue
		}

		// 查找包含nmefafej的行
		if strings.Contains(line, "nmefafej") && strings.Contains(line, `"`) {
			// 使用正则提取引号内的值
			re := regexp.MustCompile(`var\s+nmefafej\s*=\s*"([^"]+)"`)
			matches := re.FindStringSubmatch(line)
			if len(matches) > 1 && len(matches[1]) > 5 {
				if p.debugMode {
					log.Printf("[U3C3] 提取到search2参数: %s (来自行: %s)", matches[1], line)
				}
				return matches[1]
			}

			// 备用方案：直接提取引号内容
			start := strings.Index(line, `"`)
			if start != -1 {
				end := strings.Index(line[start+1:], `"`)
				if end != -1 && end > 5 {
					candidate := line[start+1 : start+1+end]
					if len(candidate) > 5 {
						if p.debugMode {
							log.Printf("[U3C3] 备用方案提取search2: %s (来自行: %s)", candidate, line)
						}
						return candidate
					}
				}
			}
		}
	}

	if p.debugMode {
		log.Printf("[U3C3] 未能找到search2参数")
	}
	return ""
}

// doSearch 执行搜索
func (p *U3c3Plugin) doSearch(keyword, search2 string) ([]model.SearchResult, error) {
	// 构建搜索URL
	encodedKeyword := url.QueryEscape(keyword)
	baseURL := p.getActiveURL()
	searchURL := fmt.Sprintf("%s/?search2=%s&search=%s", baseURL, search2, encodedKeyword)

	if p.debugMode {
		log.Printf("[U3C3] 搜索URL: %s", searchURL)
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", baseURL+"/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")

	var resp *http.Response
	var lastErr error

	// 重试机制
	for i := 0; i < MaxRetries; i++ {
		resp, lastErr = client.Do(req)
		if lastErr == nil && resp.StatusCode == 200 {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if i < MaxRetries-1 {
			time.Sleep(RetryDelay)
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("搜索请求失败，状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return p.parseSearchResults(string(body))
}

// parseSearchResults 解析搜索结果
func (p *U3c3Plugin) parseSearchResults(html string) ([]model.SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	var results []model.SearchResult

	// 查找搜索结果表格行
	doc.Find("tbody tr.default").Each(func(i int, s *goquery.Selection) {
		// 跳过广告行（通常包含置顶标识）
		titleCell := s.Find("td:nth-child(2)")
		titleText := titleCell.Text()
		if strings.Contains(titleText, "[置顶]") {
			return // 跳过置顶广告
		}

		// 提取标题和详情链接
		titleLink := titleCell.Find("a")
		title := strings.TrimSpace(titleLink.Text())
		if title == "" {
			return // 跳过空标题
		}

		// 清理标题中的HTML标签和特殊字符
		title = p.cleanTitle(title)

		// 提取详情页链接（可选，用于后续扩展）
		detailURL, _ := titleLink.Attr("href")
		if detailURL != "" && !strings.HasPrefix(detailURL, "http") {
			detailURL = p.getActiveURL() + detailURL
		}

		// 提取链接信息
		linkCell := s.Find("td:nth-child(3)")
		var links []model.Link

		// 磁力链接
		linkCell.Find("a[href^='magnet:']").Each(func(j int, link *goquery.Selection) {
			href, exists := link.Attr("href")
			if exists && href != "" {
				links = append(links, model.Link{
					URL:  href,
					Type: "magnet",
				})
			}
		})

		// 提取文件大小
		sizeText := strings.TrimSpace(s.Find("td:nth-child(4)").Text())

		// 提取上传时间
		dateText := strings.TrimSpace(s.Find("td:nth-child(5)").Text())

		// 提取分类
		categoryText := s.Find("td:nth-child(1) a").AttrOr("title", "")

		// 构建内容信息
		var contentParts []string
		if categoryText != "" {
			contentParts = append(contentParts, "分类: "+categoryText)
		}
		if sizeText != "" {
			contentParts = append(contentParts, "大小: "+sizeText)
		}
		if dateText != "" {
			contentParts = append(contentParts, "时间: "+dateText)
		}

		content := strings.Join(contentParts, " | ")

		// 生成唯一ID
		uniqueID := p.generateUniqueID(title, sizeText)

		result := model.SearchResult{
			Title:    title,
			Content:  content,
			Channel:  "", // 插件搜索结果必须为空
			Tags:     []string{"种子", "磁力链接"},
			Datetime: p.parseDateTime(dateText),
			Links:    links,
			UniqueID: uniqueID,
		}

		results = append(results, result)
	})

	if p.debugMode {
		log.Printf("[U3C3] 解析到 %d 个搜索结果", len(results))
	}

	return results, nil
}

// cleanTitle 清理标题文本
func (p *U3c3Plugin) cleanTitle(title string) string {
	// 移除HTML标签
	title = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(title, "")
	// 移除多余的空白字符
	title = regexp.MustCompile(`\s+`).ReplaceAllString(title, " ")
	// 移除前后空白
	title = strings.TrimSpace(title)
	return title
}

// parseDateTime 解析日期时间
func (p *U3c3Plugin) parseDateTime(dateStr string) time.Time {
	if dateStr == "" {
		return time.Time{}
	}

	// 尝试解析常见的日期格式
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02",
		"01-02 15:04",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, dateStr); err == nil {
			return t
		}
	}

	// 如果解析失败，返回零值
	return time.Time{}
}

// generateUniqueID 生成唯一ID
func (p *U3c3Plugin) generateUniqueID(title, size string) string {
	// 使用插件名、标题和大小生成唯一ID
	source := fmt.Sprintf("%s-%s-%s", p.Name(), title, size)
	// 简单的哈希处理（实际项目中可使用更复杂的哈希算法）
	hash := 0
	for _, char := range source {
		hash = hash*31 + int(char)
	}
	if hash < 0 {
		hash = -hash
	}
	return fmt.Sprintf("u3c3-%d", hash)
}
