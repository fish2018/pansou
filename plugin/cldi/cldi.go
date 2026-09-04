package cldi

import (
	"context"
	"fmt"
	"io"
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

type CldiPlugin struct {
	*plugin.BaseAsyncPlugin
}

const (
	// 并发数限制
	MaxConcurrency = 10

	// 最大搜索页数
	MaxPages = 5
)

var (
	// 广告清理正则表达式
	adRegex = regexp.MustCompile(`【[^】]*】`)

	// 文件大小和名称分离正则
	fileSizeRegex = regexp.MustCompile(`^(.+?)&nbsp;<span class="lightColor">([^<]+)</span>$`)

	// 各种数字提取正则
	numberRegex   = regexp.MustCompile(`\d+`)
	hashPathRegex = regexp.MustCompile(`(?i)/hash/([a-f0-9]{40})\.html`)
)

const baseURL = "https://cm7jll1f.1122137.xyz"

func init() {
	p := &CldiPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("cldi", 3, true), // 磁力搜索插件，跳过Service层过滤
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果
func (p *CldiPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *CldiPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *CldiPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 首先搜索第一页
	firstPageResults, err := p.searchPage(client, keyword, 1)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索第一页失败: %w", p.Name(), err)
	}

	// 存储所有结果
	var allResults []model.SearchResult
	allResults = append(allResults, firstPageResults...)

	// 2. 并发搜索其他页面（第2页到第5页）
	if MaxPages > 1 {
		var wg sync.WaitGroup
		var mu sync.Mutex

		// 使用信号量控制并发数
		semaphore := make(chan struct{}, MaxConcurrency)

		// 存储每页结果
		pageResults := make(map[int][]model.SearchResult)

		for page := 2; page <= MaxPages; page++ {
			wg.Add(1)
			go func(pageNum int) {
				defer wg.Done()

				// 获取信号量
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				// 添加小延迟避免过于频繁的请求
				time.Sleep(time.Duration(pageNum%3) * 100 * time.Millisecond)

				currentPageResults, err := p.searchPage(client, keyword, pageNum)
				if err == nil && len(currentPageResults) > 0 {
					mu.Lock()
					pageResults[pageNum] = currentPageResults
					mu.Unlock()
				}
			}(page)
		}

		wg.Wait()

		// 按页码顺序合并所有页面的结果
		for page := 2; page <= MaxPages; page++ {
			if results, exists := pageResults[page]; exists {
				allResults = append(allResults, results...)
			}
		}
	}

	// 3. 关键词过滤
	return plugin.FilterResultsByKeyword(allResults, keyword), nil
}

// searchPage 搜索指定页面
func (p *CldiPlugin) searchPage(client *http.Client, keyword string, page int) ([]model.SearchResult, error) {
	// 构建搜索URL (分类=0全部, 排序=2按添加时间)
	searchURL := fmt.Sprintf("%s/search-%s-0-2-%d.html", baseURL, url.QueryEscape(keyword), page)

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	p.setRequestHeaders(req)

	// 发送请求
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	// 提取搜索结果
	return p.extractSearchResults(doc), nil
}

// setRequestHeaders 设置请求头
func (p *CldiPlugin) setRequestHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", baseURL+"/")
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *CldiPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

		// 克隆请求
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err == nil && resp.StatusCode == 200 {
			return resp, nil
		}

		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
	}

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// extractSearchResults 提取搜索结果
func (p *CldiPlugin) extractSearchResults(doc *goquery.Document) []model.SearchResult {
	var results []model.SearchResult

	// New cldi releases use article.resource cards and expose the BT hash in
	// /hash/<40-hex>.html links. The hash itself is a valid magnet identifier,
	// so no browser-only "copy magnet" action is required.
	doc.Find("article.resource").Each(func(_ int, article *goquery.Selection) {
		anchor := article.Find("h2 a[href]").First()
		href, _ := anchor.Attr("href")
		href = strings.TrimSpace(href)
		match := hashPathRegex.FindStringSubmatch(href)
		if len(match) < 2 {
			return
		}
		title := p.cleanTitle(anchor.Text())
		if title == "" {
			return
		}
		content := strings.TrimSpace(article.Find(".meta").Text())
		result := model.SearchResult{
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), strings.ToLower(match[1])),
			MessageID: fmt.Sprintf("%s-%s", p.Name(), strings.ToLower(match[1])),
			Channel:   "",
			Datetime:  time.Now(),
			Title:     title,
			Content:   content,
			Links: []model.Link{{
				Type:      "magnet",
				URL:       "magnet:?xt=urn:btih:" + strings.ToLower(match[1]),
				WorkTitle: title,
			}},
		}
		if dateMatch := regexp.MustCompile(`添加时间[:：]\s*(\d{4}-\d{2}-\d{2})`).FindStringSubmatch(content); len(dateMatch) > 1 {
			if parsed, err := time.ParseInLocation("2006-01-02", dateMatch[1], time.Local); err == nil {
				result.Datetime = parsed
			}
		}
		results = append(results, result)
	})
	if len(results) > 0 {
		return results
	}

	// 查找所有搜索结果
	doc.Find(".tbox .ssbox").Each(func(i int, s *goquery.Selection) {
		result := p.parseSearchResult(s)
		if result.Title != "" && len(result.Links) > 0 {
			results = append(results, result)
		}
	})

	return results
}

// parseSearchResult 解析单个搜索结果
func (p *CldiPlugin) parseSearchResult(s *goquery.Selection) model.SearchResult {
	result := model.SearchResult{
		Channel:  "", // 插件搜索结果必须为空字符串
		Datetime: time.Now(),
	}

	// 提取标题和分类
	titleSection := s.Find(".title h3")

	// 提取分类
	category := strings.TrimSpace(titleSection.Find("span").First().Text())
	if category != "" {
		result.Tags = []string{p.mapCategory(category)}
	}

	// 提取标题
	titleLink := titleSection.Find("a")
	title := strings.TrimSpace(titleLink.Text())
	result.Title = p.cleanTitle(title)

	// 提取磁力链接和元数据
	p.extractMagnetInfo(s, &result)

	// 提取文件列表作为内容
	p.extractFileList(s, &result)

	// 生成唯一ID
	result.UniqueID = fmt.Sprintf("%s-%d", p.Name(), time.Now().UnixNano())

	return result
}

// extractMagnetInfo 提取磁力链接和元数据
func (p *CldiPlugin) extractMagnetInfo(s *goquery.Selection, result *model.SearchResult) {
	sbar := s.Find(".sbar")

	// 提取磁力链接
	magnetLink, exists := sbar.Find("a[href^='magnet:']").Attr("href")
	if exists && magnetLink != "" {
		result.Links = []model.Link{{
			Type: "magnet",
			URL:  magnetLink,
		}}
	}

	// 提取添加时间
	sbar.Find("span").Each(func(i int, span *goquery.Selection) {
		text := span.Text()
		if strings.Contains(text, "添加时间:") {
			timeStr := strings.TrimSpace(span.Find("b").Text())
			if timeStr != "" {
				if parsedTime, err := time.Parse("2006-01-02", timeStr); err == nil {
					result.Datetime = parsedTime
				}
			}
		}
	})
}

// extractFileList 提取文件列表
func (p *CldiPlugin) extractFileList(s *goquery.Selection, result *model.SearchResult) {
	var fileList []string

	s.Find(".slist ul li").Each(func(i int, li *goquery.Selection) {
		// 获取原始HTML以解析文件名和大小
		html, _ := li.Html()

		// 使用正则表达式分离文件名和大小
		if matches := fileSizeRegex.FindStringSubmatch(html); len(matches) == 3 {
			fileName := strings.TrimSpace(matches[1])
			fileSize := strings.TrimSpace(matches[2])
			if fileName != "" && fileSize != "" {
				fileList = append(fileList, fmt.Sprintf("%s (%s)", fileName, fileSize))
			}
		} else {
			// 回退方案：直接使用文本内容
			text := strings.TrimSpace(li.Text())
			if text != "" {
				fileList = append(fileList, text)
			}
		}
	})

	if len(fileList) > 0 {
		result.Content = strings.Join(fileList, "\n")
	}
}

// mapCategory 映射分类
func (p *CldiPlugin) mapCategory(category string) string {
	// 移除方括号
	category = strings.Trim(category, "[]")

	switch category {
	case "影视":
		return "影视"
	case "音乐":
		return "音乐"
	case "图像":
		return "图像"
	case "文档书籍":
		return "文档"
	case "压缩文件":
		return "压缩包"
	case "安装包":
		return "软件"
	case "其他":
		return "其他"
	default:
		return "其他"
	}
}

// cleanTitle 清理标题中的广告内容
func (p *CldiPlugin) cleanTitle(title string) string {
	// 移除【】内的广告内容
	cleaned := adRegex.ReplaceAllString(title, "")

	// 清理多余的空格
	cleaned = strings.TrimSpace(cleaned)
	cleaned = regexp.MustCompile(`\s+`).ReplaceAllString(cleaned, " ")

	return cleaned
}
