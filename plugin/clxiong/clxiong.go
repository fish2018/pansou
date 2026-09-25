package clxiong

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
	BaseURL    = "https://www.cilixiong.org"
	SearchURL  = "https://www.cilixiong.org/e/search/index.php"
	UserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"
	MaxRetries = 3
	RetryDelay = 2 * time.Second
	MaxResults = 30
)

// DetailPageInfo 详情页信息结构体
type DetailPageInfo struct {
	MagnetLinks []model.Link
	UpdateTime  time.Time
	Title       string
	FileNames   []string // 所有文件的名称，与磁力链接对应
}

// ClxiongPlugin 磁力熊插件
type ClxiongPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode bool
}

func init() {
	p := &ClxiongPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("clxiong", 2, true),
		debugMode:       false, // 开启调试模式检查磁力链接提取问题
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 搜索接口实现
func (p *ClxiongPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 搜索并返回详细结果
func (p *ClxiongPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (*model.PluginSearchResult, error) {
	if p.debugMode {
		log.Printf("[CLXIONG] 开始搜索: %s", keyword)
	}

	// 第一步：POST搜索获取searchid
	searchID, err := p.getSearchID(keyword)
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 获取searchid失败: %v", err)
		}
		return nil, fmt.Errorf("获取searchid失败: %v", err)
	}

	// 第二步：GET搜索结果
	results, err := p.getSearchResults(searchID, keyword)
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 获取搜索结果失败: %v", err)
		}
		return nil, err
	}

	// 第三步：同步获取详情页磁力链接
	results = p.fetchDetailLinksSync(results)

	if p.debugMode {
		log.Printf("[CLXIONG] 搜索完成，获得 %d 个结果", len(results))
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

// getSearchID 第一步：POST搜索获取searchid
func (p *ClxiongPlugin) getSearchID(keyword string) (string, error) {
	if p.debugMode {
		log.Printf("[CLXIONG] 正在获取searchid...")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 不自动跟随重定向，我们需要手动处理
			return http.ErrUseLastResponse
		},
	}

	// 准备POST数据
	formData := url.Values{}
	formData.Set("classid", "1,2")    // 1=电影，2=剧集
	formData.Set("show", "title")     // 搜索字段
	formData.Set("tempid", "1")       // 模板ID
	formData.Set("keyboard", keyword) // 搜索关键词

	req, err := http.NewRequest("POST", SearchURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")

	var resp *http.Response
	var lastErr error

	// 重试机制
	for i := 0; i < MaxRetries; i++ {
		resp, lastErr = client.Do(req)
		if lastErr == nil && (resp.StatusCode == 302 || resp.StatusCode == 301) {
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
		return "", lastErr
	}
	defer resp.Body.Close()

	// 检查重定向响应
	if resp.StatusCode != 302 && resp.StatusCode != 301 {
		return "", fmt.Errorf("期望302重定向，但得到状态码: %d", resp.StatusCode)
	}

	// 从Location头部提取searchid
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("重定向响应中没有Location头部")
	}

	// 解析searchid
	searchID := p.extractSearchIDFromLocation(location)
	if searchID == "" {
		return "", fmt.Errorf("无法从Location中提取searchid: %s", location)
	}

	if p.debugMode {
		log.Printf("[CLXIONG] 获取到searchid: %s", searchID)
	}

	return searchID, nil
}

// extractSearchIDFromLocation 从Location头部提取searchid
func (p *ClxiongPlugin) extractSearchIDFromLocation(location string) string {
	// location格式: "result/?searchid=7549"
	re := clxiongRe1
	matches := re.FindStringSubmatch(location)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// getSearchResults 第二步：GET搜索结果
func (p *ClxiongPlugin) getSearchResults(searchID, keyword string) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[CLXIONG] 正在获取搜索结果，searchid: %s", searchID)
	}

	// 构建结果页URL
	resultURL := fmt.Sprintf("%s/e/search/result/?searchid=%s", BaseURL, searchID)

	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequest("GET", resultURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", BaseURL+"/")
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
		return nil, fmt.Errorf("搜索结果请求失败，状态码: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return p.parseSearchResults(string(body))
}

// parseSearchResults 解析搜索结果页面
func (p *ClxiongPlugin) parseSearchResults(html string) ([]model.SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}

	var results []model.SearchResult

	// 查找搜索结果项
	doc.Find(".row.row-cols-2.row-cols-lg-4 .col").Each(func(i int, s *goquery.Selection) {
		if i >= MaxResults {
			return // 限制结果数量
		}

		// 提取详情页链接
		linkEl := s.Find("a[href*='/drama/'], a[href*='/movie/']")
		if linkEl.Length() == 0 {
			return // 跳过无链接的项
		}

		detailPath, exists := linkEl.Attr("href")
		if !exists || detailPath == "" {
			return
		}

		// 构建完整的详情页URL
		detailURL := BaseURL + detailPath

		// 提取标题
		title := strings.TrimSpace(linkEl.Find("h2.h4").Text())
		if title == "" {
			return // 跳过无标题的项
		}

		// 提取评分
		rating := strings.TrimSpace(s.Find(".rank").Text())

		// 提取年份
		year := strings.TrimSpace(s.Find(".small").Last().Text())

		// 提取海报图片
		poster := ""
		cardImg := s.Find(".card-img")
		if cardImg.Length() > 0 {
			if style, exists := cardImg.Attr("style"); exists {
				poster = p.extractImageFromStyle(style)
			}
		}

		// 构建内容信息
		var contentParts []string
		if rating != "" {
			contentParts = append(contentParts, "评分: "+rating)
		}
		if year != "" {
			contentParts = append(contentParts, "年份: "+year)
		}
		if poster != "" {
			contentParts = append(contentParts, "海报: "+poster)
		}
		// 添加详情页链接到content中，供后续提取磁力链接使用
		contentParts = append(contentParts, "详情页: "+detailURL)

		content := strings.Join(contentParts, " | ")

		// 生成唯一ID
		uniqueID := p.generateUniqueID(detailPath)

		result := model.SearchResult{
			Title:    title,
			Content:  content,
			Channel:  "", // 插件搜索结果必须为空
			Tags:     []string{"磁力链接", "影视"},
			Datetime: time.Now(),     // 搜索时间
			Links:    []model.Link{}, // 初始为空，后续异步获取
			UniqueID: uniqueID,
		}

		results = append(results, result)
	})

	if p.debugMode {
		log.Printf("[CLXIONG] 解析到 %d 个搜索结果", len(results))
	}

	return results, nil
}

// extractImageFromStyle 从style属性中提取背景图片URL
func (p *ClxiongPlugin) extractImageFromStyle(style string) string {
	// style格式: "background-image: url('https://i.nacloud.cc/2024/12154.webp');"
	re := clxiongRe2
	matches := re.FindStringSubmatch(style)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// fetchDetailLinksSync 同步获取详情页磁力链接
func (p *ClxiongPlugin) fetchDetailLinksSync(results []model.SearchResult) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	if p.debugMode {
		log.Printf("[CLXIONG] 开始同步获取 %d 个详情页的磁力链接", len(results))
	}

	// 使用WaitGroup确保所有请求完成后再返回
	var wg sync.WaitGroup
	var mu sync.Mutex                          // 保护results切片的互斥锁
	var additionalResults []model.SearchResult // 存储额外创建的搜索结果

	// 限制并发数，避免过多请求
	semaphore := make(chan struct{}, 5) // 最多5个并发请求

	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			detailURL := p.extractDetailURLFromContent(results[index].Content)
			if detailURL != "" {
				detailInfo := p.fetchDetailPageInfo(detailURL, results[index].Title)
				if detailInfo != nil && len(detailInfo.MagnetLinks) > 0 {
					// 为每个磁力链接创建独立的搜索结果，这样每个链接都有自己的note
					baseResult := results[index]

					// 第一个链接更新原结果
					if len(detailInfo.FileNames) > 0 {
						results[index].Title = fmt.Sprintf("%s-%s", baseResult.Title, detailInfo.FileNames[0])
					}
					results[index].Links = []model.Link{detailInfo.MagnetLinks[0]}
					if !detailInfo.UpdateTime.IsZero() {
						results[index].Datetime = detailInfo.UpdateTime
					}

					// 其他链接创建新的搜索结果
					var newResults []model.SearchResult
					for i := 1; i < len(detailInfo.MagnetLinks); i++ {
						newResult := model.SearchResult{
							MessageID: fmt.Sprintf("%s-%d", baseResult.MessageID, i+1),
							UniqueID:  fmt.Sprintf("%s-%d", baseResult.UniqueID, i+1),
							Channel:   baseResult.Channel,
							Content:   baseResult.Content,
							Tags:      baseResult.Tags,
							Images:    baseResult.Images,
							Links:     []model.Link{detailInfo.MagnetLinks[i]},
						}

						// 设置独特的标题和时间
						if i < len(detailInfo.FileNames) {
							newResult.Title = fmt.Sprintf("%s-%s", baseResult.Title, detailInfo.FileNames[i])
						} else {
							newResult.Title = baseResult.Title
						}

						if !detailInfo.UpdateTime.IsZero() {
							newResult.Datetime = detailInfo.UpdateTime
						} else {
							newResult.Datetime = baseResult.Datetime
						}

						newResults = append(newResults, newResult)
					}

					// 使用锁保护切片的修改
					if len(newResults) > 0 {
						mu.Lock()
						additionalResults = append(additionalResults, newResults...)
						mu.Unlock()
					}

					if p.debugMode {
						log.Printf("[CLXIONG] 为结果 %d 获取到 %d 个磁力链接，创建了 %d 个搜索结果", index+1, len(detailInfo.MagnetLinks), len(detailInfo.MagnetLinks))
					}
				}
			}
		}(i)
	}

	// 等待所有goroutine完成
	wg.Wait()

	// 合并额外创建的搜索结果
	results = append(results, additionalResults...)

	if p.debugMode {
		totalLinks := 0
		for _, result := range results {
			totalLinks += len(result.Links)
		}
		log.Printf("[CLXIONG] 所有磁力链接获取完成，共获得 %d 个磁力链接，总搜索结果 %d 个", totalLinks, len(results))
	}

	return results
}

// extractDetailURLFromContent 从content中提取详情页URL
func (p *ClxiongPlugin) extractDetailURLFromContent(content string) string {
	// 查找"详情页: URL"模式
	re := clxiongRe3
	matches := re.FindStringSubmatch(content)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// fetchDetailPageInfo 获取详情页的完整信息
func (p *ClxiongPlugin) fetchDetailPageInfo(detailURL string, movieTitle string) *DetailPageInfo {
	if p.debugMode {
		log.Printf("[CLXIONG] 正在获取详情页信息: %s", detailURL)
	}

	client := &http.Client{Timeout: 20 * time.Second}

	req, err := http.NewRequest("GET", detailURL, nil)
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 创建详情页请求失败: %v", err)
		}
		return nil
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 详情页请求失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[CLXIONG] 详情页HTTP状态错误: %d", resp.StatusCode)
		}
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 读取详情页响应失败: %v", err)
		}
		return nil
	}

	return p.parseDetailPageInfo(string(body), movieTitle)
}

// parseDetailPageInfo 从详情页HTML中解析完整信息
func (p *ClxiongPlugin) parseDetailPageInfo(html string, movieTitle string) *DetailPageInfo {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		if p.debugMode {
			log.Printf("[CLXIONG] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	detailInfo := &DetailPageInfo{
		Title: movieTitle,
	}

	// 解析更新时间
	detailInfo.UpdateTime = p.parseUpdateTimeFromDetail(doc)

	// 解析磁力链接
	magnetLinks, fileNames := p.parseMagnetLinksFromDetailDoc(doc, movieTitle)
	detailInfo.MagnetLinks = magnetLinks
	detailInfo.FileNames = fileNames

	if p.debugMode {
		log.Printf("[CLXIONG] 详情页解析完成: 磁力链接 %d 个，更新时间: %v",
			len(detailInfo.MagnetLinks), detailInfo.UpdateTime)
	}

	return detailInfo
}

// parseUpdateTimeFromDetail 从详情页解析更新时间
func (p *ClxiongPlugin) parseUpdateTimeFromDetail(doc *goquery.Document) time.Time {
	// 查找"最后更新于：2025-08-16"这样的文本
	var updateTime time.Time

	doc.Find(".mv_detail p").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if strings.Contains(text, "最后更新于：") {
			// 提取日期部分
			dateStr := strings.Replace(text, "最后更新于：", "", 1)
			dateStr = strings.TrimSpace(dateStr)

			// 解析日期，支持多种格式
			layouts := []string{
				"2006-01-02",
				"2006-1-2",
				"2006/01/02",
				"2006/1/2",
			}

			for _, layout := range layouts {
				if t, err := time.Parse(layout, dateStr); err == nil {
					updateTime = t
					if p.debugMode {
						log.Printf("[CLXIONG] 解析到更新时间: %s -> %v", dateStr, updateTime)
					}
					return
				}
			}

			if p.debugMode {
				log.Printf("[CLXIONG] 无法解析更新时间: %s", dateStr)
			}
		}
	})

	return updateTime
}

// parseMagnetLinksFromDetailDoc 从详情页DOM解析磁力链接
func (p *ClxiongPlugin) parseMagnetLinksFromDetailDoc(doc *goquery.Document, movieTitle string) ([]model.Link, []string) {
	var links []model.Link
	var fileNames []string

	if p.debugMode {
		// 调试：检查是否找到磁力下载区域
		mvDown := doc.Find(".mv_down")
		log.Printf("[CLXIONG] 找到 .mv_down 区域数量: %d", mvDown.Length())

		// 调试：检查磁力链接数量
		magnetLinks := doc.Find(".mv_down a[href^='magnet:']")
		log.Printf("[CLXIONG] 找到磁力链接数量: %d", magnetLinks.Length())

		// 如果没找到，尝试其他可能的选择器
		if magnetLinks.Length() == 0 {
			allMagnetLinks := doc.Find("a[href^='magnet:']")
			log.Printf("[CLXIONG] 页面总磁力链接数量: %d", allMagnetLinks.Length())
		}
	}

	// 查找磁力链接
	doc.Find(".mv_down a[href^='magnet:']").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if exists && href != "" {
			// 获取文件名（链接文本）
			fileName := strings.TrimSpace(s.Text())

			link := model.Link{
				URL:  href,
				Type: "magnet",
			}

			// 磁力链接密码字段设置为空（按用户要求）
			link.Password = ""

			links = append(links, link)
			fileNames = append(fileNames, fileName)

			if p.debugMode {
				log.Printf("[CLXIONG] 找到磁力链接: %s", fileName)
			}
		}
	})

	if p.debugMode {
		log.Printf("[CLXIONG] 详情页共找到 %d 个磁力链接", len(links))
	}

	return links, fileNames
}

// generateUniqueID 生成唯一ID
func (p *ClxiongPlugin) generateUniqueID(detailPath string) string {
	// 从路径中提取ID，如 "/drama/4466.html" -> "4466"
	re := clxiongRe4
	matches := re.FindStringSubmatch(detailPath)
	if len(matches) > 1 {
		return fmt.Sprintf("clxiong-%s", matches[1])
	}

	// 备用方案：使用完整路径生成哈希
	hash := 0
	for _, char := range detailPath {
		hash = hash*31 + int(char)
	}
	if hash < 0 {
		hash = -hash
	}
	return fmt.Sprintf("clxiong-%d", hash)
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	clxiongRe1 = regexp.MustCompile(`searchid=(\d+)`)
	clxiongRe2 = regexp.MustCompile(`url\(['"]?([^'"]+)['"]?\)`)
	clxiongRe3 = regexp.MustCompile(`详情页: (https?://[^\s|]+)`)
	clxiongRe4 = regexp.MustCompile(`/(?:drama|movie)/(\d+)\.html`)
)
