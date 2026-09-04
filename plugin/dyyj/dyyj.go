package dyyj

import (
	"context"
	encodingjson "encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	PluginName     = "dyyj"
	DisplayName    = "电影云集"
	Description    = "电影云集 - 影视资源网盘链接搜索"
	BaseURL        = "https://bbs.dyyjmax.org"
	SearchPath     = "/?q=%s"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults     = 100
	MaxConcurrency = 100
	RequestTimeout = 30 * time.Second

	// HTTP连接池配置（性能优化）
	MaxIdleConns          = 100              // 最大空闲连接数
	MaxIdleConnsPerHost   = 100              // 每个主机的最大空闲连接数
	MaxConnsPerHost       = 100              // 每个主机的最大连接数
	IdleConnTimeout       = 90 * time.Second // 空闲连接超时
	TLSHandshakeTimeout   = 10 * time.Second // TLS握手超时
	ExpectContinueTimeout = 1 * time.Second  // Expect: 100-continue超时
)

// 预编译的正则表达式（性能优化：避免重复编译）
var (
	// 提取文章ID的正则
	postIDRegex = regexp.MustCompile(`/d/(\d+)`)

	// 提取noscript标签的正则
	noscriptRegex = regexp.MustCompile(`<noscript[^>]*id=["']flarum-content["'][^>]*>([\s\S]*?)</noscript>`)

	// 提取li标签内链接的正则
	liLinkRegex = regexp.MustCompile(`<li[^>]*>\s*<a[^>]*href=["']([^"']*\/d\/[^"']*)["'][^>]*>([\s\S]*?)</a>\s*</li>`)

	// 清理HTML标签的正则
	htmlTagRegex = regexp.MustCompile(`<[^>]+>`)

	// 提取链接的正则
	linkHrefRegex = regexp.MustCompile(`href=["']([^"']*\/d\/[^"']*)["']`)

	// 提取发布时间meta标签的正则
	publishTimeRegexes = []*regexp.Regexp{
		regexp.MustCompile(`<meta\s+name=["']article:published_time["']\s+content=["']([^"']+)["']`),
		regexp.MustCompile(`<meta\s+property=["']article:published_time["']\s+content=["']([^"']+)["']`),
		regexp.MustCompile(`<meta\s+name=["']article:updated_time["']\s+content=["']([^"']+)["']`),
		regexp.MustCompile(`<time[^>]*datetime=["']([^"']+)["']`),
	}

	// 网盘链接匹配模式（预编译，性能优化）
	networkDiskPatterns = []struct {
		name    string
		regex   *regexp.Regexp
		urlType string
	}{
		{"夸克网盘", regexp.MustCompile(`<p><strong>夸克[^<]*</strong></p>\s*<p><a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>`), "quark"},
		{"百度网盘", regexp.MustCompile(`<p><strong>百度[^<]*</strong></p>\s*<p><a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>`), "baidu"},
		{"阿里云盘", regexp.MustCompile(`<p><strong>阿里[^<]*</strong></p>\s*<p><a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>`), "aliyun"},
		{"天翼云盘", regexp.MustCompile(`<p><strong>天翼[^<]*</strong></p>\s*<p><a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>`), "tianyi"},
		{"迅雷网盘", regexp.MustCompile(`<p><strong>迅雷[^<]*</strong></p>\s*<p><a[^>]*href\s*=\s*["']([^"']+)["'][^>]*>`), "xunlei"},
		{"通用网盘", regexp.MustCompile(`<a[^>]*href\s*=\s*["'](https?://[^"']*(?:pan|drive|cloud)[^"']*)["'][^>]*>`), "others"},
	}
)

// DyyjPlugin 电影云集插件
type DyyjPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode       bool
	detailCache     sync.Map // 缓存详情页结果
	cacheTTL        time.Duration
	optimizedClient *http.Client // 优化的HTTP客户端（连接池）
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewDyyjPlugin())
}

// NewDyyjPlugin 创建新的电影云集插件实例
func NewDyyjPlugin() *DyyjPlugin {
	debugMode := false // 生产环境关闭调试

	p := &DyyjPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 2), // 质量良好，优先级2
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,            // 详情页缓存30分钟
		optimizedClient: createOptimizedHTTPClient(), // 创建优化的HTTP客户端
	}

	return p
}

// createOptimizedHTTPClient 创建优化的HTTP客户端（连接池配置）
func createOptimizedHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          MaxIdleConns,
		MaxIdleConnsPerHost:   MaxIdleConnsPerHost,
		MaxConnsPerHost:       MaxConnsPerHost,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   TLSHandshakeTimeout,
		ExpectContinueTimeout: ExpectContinueTimeout,
		ForceAttemptHTTP2:     true,  // 启用HTTP/2支持
		DisableKeepAlives:     false, // 启用Keep-Alive连接复用
	}

	return &http.Client{
		Transport: transport,
		Timeout:   RequestTimeout,
	}
}

// Name 插件名称
func (p *DyyjPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *DyyjPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *DyyjPlugin) Description() string {
	return Description
}

// Search 搜索接口
func (p *DyyjPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *DyyjPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现
func (p *DyyjPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[DYYJ] 开始搜索: %s", keyword)
	}

	// 第一步：执行搜索获取结果列表
	// 使用优化的客户端（连接池）而不是传入的client
	searchResults, err := p.executeSearch(p.optimizedClient, keyword)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 执行搜索失败: %v", err)
		}
		return nil, fmt.Errorf("[%s] 执行搜索失败: %w", p.Name(), err)
	}

	if p.debugMode {
		log.Printf("[DYYJ] 搜索获取到 %d 个结果", len(searchResults))
	}
	// The Flarum API response already includes each relevant post's content and
	// extracted links. Do not fall back to HTML detail pages, which are protected
	// by a Cloudflare challenge.
	if hasInlineLinks(searchResults) {
		return plugin.FilterResultsByKeyword(searchResults, keyword), nil
	}

	// 第二步：先对标题进行关键词过滤，只处理包含关键词的结果（避免不必要的详情页请求）
	titleFilteredResults := p.filterByTitleKeyword(searchResults, keyword)
	if p.debugMode {
		log.Printf("[DYYJ] 标题关键词过滤后剩余 %d 个结果（将只对这些结果获取详情页）", len(titleFilteredResults))
	}

	// 第三步：并发获取详情页链接（只对标题包含关键词的结果）
	// 使用优化的客户端（连接池）而不是传入的client
	finalResults := p.fetchDetailLinks(p.optimizedClient, titleFilteredResults, keyword)

	if p.debugMode {
		log.Printf("[DYYJ] 最终获取到 %d 个有效结果", len(finalResults))
	}

	// 第四步：最终关键词过滤（对标题和内容都进行过滤，标准网盘插件需要过滤）
	filteredResults := plugin.FilterResultsByKeyword(finalResults, keyword)

	if p.debugMode {
		log.Printf("[DYYJ] 最终关键词过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

func hasInlineLinks(results []model.SearchResult) bool {
	for _, result := range results {
		if len(result.Links) > 0 {
			return true
		}
	}
	return false
}

// executeSearch 执行搜索请求
func (p *DyyjPlugin) executeSearch(client *http.Client, keyword string) ([]model.SearchResult, error) {
	return p.executeSearchAPI(client, keyword)
}

// executeSearchHTML keeps the legacy HTML parser available for future fallback
// use if the forum API changes or becomes unavailable.
func (p *DyyjPlugin) executeSearchHTML(client *http.Client, keyword string) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("%s%s", BaseURL, fmt.Sprintf(SearchPath, url.QueryEscape(keyword)))

	if p.debugMode {
		log.Printf("[DYYJ] 搜索URL: %s", searchURL)
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 创建搜索请求失败: %v", err)
		}
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", BaseURL+"/")

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 搜索请求失败: %v", err)
		}
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if p.debugMode {
		log.Printf("[DYYJ] 搜索请求响应状态码: %d", resp.StatusCode)
	}

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[DYYJ] 搜索请求HTTP状态错误: %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("[%s] 搜索请求HTTP状态错误: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应体用于调试
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 读取响应体失败: %v", err)
		}
		return nil, fmt.Errorf("[%s] 读取响应体失败: %w", p.Name(), err)
	}

	bodyString := string(bodyBytes)
	if p.debugMode {
		log.Printf("[DYYJ] 响应体大小: %d 字节", len(bodyString))

		// 保存完整HTML到文件用于分析
		filename := fmt.Sprintf("./dyyj_search_%s_%d.html", url.QueryEscape(keyword), time.Now().Unix())
		if err := os.WriteFile(filename, bodyBytes, 0644); err == nil {
			log.Printf("[DYYJ] 完整HTML已保存到: %s", filename)
		} else {
			log.Printf("[DYYJ] 保存HTML文件失败: %v", err)
		}

		// 输出HTML的前2000个字符用于调试
		previewLen := 2000
		if len(bodyString) < previewLen {
			previewLen = len(bodyString)
		}
		log.Printf("[DYYJ] HTML内容预览（前%d字符）:\n%s", previewLen, bodyString[:previewLen])

		// 检查关键元素是否存在
		hasNoscript := strings.Contains(bodyString, "<noscript")
		hasFlarumContent := strings.Contains(bodyString, "flarum-content")
		hasContainer := strings.Contains(bodyString, "container")
		hasUL := strings.Contains(bodyString, "<ul>")
		hasLI := strings.Contains(bodyString, "<li>")
		hasIDFlarumContent := strings.Contains(bodyString, "id=\"flarum-content\"") || strings.Contains(bodyString, "id='flarum-content'")
		log.Printf("[DYYJ] HTML结构检查: noscript=%v, flarum-content=%v, id=flarum-content=%v, container=%v, ul=%v, li=%v",
			hasNoscript, hasFlarumContent, hasIDFlarumContent, hasContainer, hasUL, hasLI)

		// 查找所有noscript标签
		noscriptCount := strings.Count(bodyString, "<noscript")
		log.Printf("[DYYJ] 找到 %d 个noscript标签", noscriptCount)

		// 尝试查找所有包含flarum-content的noscript
		if hasNoscript {
			noscriptIndex := 0
			start := 0
			for {
				noscriptStart := strings.Index(bodyString[start:], "<noscript")
				if noscriptStart < 0 {
					break
				}
				noscriptStart += start
				noscriptEnd := strings.Index(bodyString[noscriptStart:], "</noscript>")
				if noscriptEnd > 0 {
					noscriptContent := bodyString[noscriptStart : noscriptStart+noscriptEnd+10]
					noscriptIndex++

					hasFlarumInNoscript := strings.Contains(noscriptContent, "flarum-content")
					hasULInNoscript := strings.Contains(noscriptContent, "<ul>")
					hasLIInNoscript := strings.Contains(noscriptContent, "<li>")

					previewLen := 1000
					if len(noscriptContent) < previewLen {
						previewLen = len(noscriptContent)
					}
					log.Printf("[DYYJ] noscript标签 #%d 内容预览（前%d字符，flarum-content=%v, ul=%v, li=%v）:\n%s",
						noscriptIndex, previewLen, hasFlarumInNoscript, hasULInNoscript, hasLIInNoscript, noscriptContent[:previewLen])

					start = noscriptStart + noscriptEnd + 10
				} else {
					break
				}
			}
		}

		// 查找所有包含/d/的链接（使用预编译的正则）
		matches := linkHrefRegex.FindAllStringSubmatch(bodyString, -1)
		log.Printf("[DYYJ] 使用正则表达式找到 %d 个包含'/d/'的链接", len(matches))
		for i, match := range matches {
			if i < 10 {
				log.Printf("[DYYJ]   链接 %d: %s", i+1, match[1])
			}
		}
	}

	// 解析HTML提取搜索结果
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(bodyString))
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 解析搜索结果HTML失败: %v", err)
		}
		return nil, fmt.Errorf("[%s] 解析搜索结果HTML失败: %w", p.Name(), err)
	}

	results, err := p.parseSearchResults(doc, bodyString)
	if p.debugMode {
		log.Printf("[DYYJ] 解析搜索结果完成，获取到 %d 个结果", len(results))
	}

	return results, err
}

type dyyjAPIResponse struct {
	Data     []dyyjDiscussion `json:"data"`
	Included []dyyjIncluded   `json:"included"`
}

type dyyjDiscussion struct {
	ID         string `json:"id"`
	Attributes struct {
		Title     string `json:"title"`
		CreatedAt string `json:"createdAt"`
	} `json:"attributes"`
	Relationships struct {
		MostRelevantPost struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"mostRelevantPost"`
	} `json:"relationships"`
}

type dyyjIncluded struct {
	ID         string `json:"id"`
	Attributes struct {
		ContentHTML string `json:"contentHtml"`
		CreatedAt   string `json:"createdAt"`
	} `json:"attributes"`
}

func (p *DyyjPlugin) executeSearchAPI(client *http.Client, keyword string) ([]model.SearchResult, error) {
	params := url.Values{}
	params.Set("filter[q]", keyword)
	params.Set("include", "mostRelevantPost")
	params.Set("page[limit]", fmt.Sprintf("%d", MaxResults))
	apiURL := BaseURL + "/api/discussions?" + params.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建 API 请求失败: %w", p.Name(), err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/vnd.api+json, application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", BaseURL+"/")

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] API 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	var payload dyyjAPIResponse
	if err := encodingjson.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("[%s] 解析 API 响应失败: %w", p.Name(), err)
	}

	posts := make(map[string]dyyjIncluded, len(payload.Included))
	for _, post := range payload.Included {
		posts[post.ID] = post
	}

	results := make([]model.SearchResult, 0, len(payload.Data))
	for _, discussion := range payload.Data {
		post, ok := posts[discussion.Relationships.MostRelevantPost.Data.ID]
		if !ok {
			continue
		}
		links := p.extractAPIContentLinks(post.Attributes.ContentHTML)
		if len(links) == 0 {
			continue
		}
		createdAt, parseErr := time.Parse(time.RFC3339, discussion.Attributes.CreatedAt)
		if parseErr != nil {
			createdAt = time.Now()
		}
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s", p.Name(), discussion.ID),
			Title:    strings.TrimSpace(discussion.Attributes.Title),
			Content:  p.cleanContentHTML(post.Attributes.ContentHTML),
			Datetime: createdAt,
			Links:    links,
			Channel:  "",
		})
	}
	return results, nil
}

func (p *DyyjPlugin) extractAPIContentLinks(contentHTML string) []model.Link {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(contentHTML))
	if err != nil {
		return nil
	}
	links := make([]model.Link, 0)
	seen := make(map[string]bool)
	doc.Find("a[href]").Each(func(_ int, node *goquery.Selection) {
		href := strings.TrimSpace(node.AttrOr("href", ""))
		if href == "" || seen[href] {
			return
		}
		linkType := p.determineCloudType(href)
		if linkType == "others" {
			return
		}
		password := p.extractPasswordFromURL(href)
		if password == "" {
			contextText := node.Text()
			if parent := node.Parent(); parent.Length() > 0 {
				contextText += " " + parent.Text()
			}
			password = p.extractPasswordFromURLText(contextText)
		}
		links = append(links, model.Link{Type: linkType, URL: href, Password: password})
		seen[href] = true
	})
	return links
}

func (p *DyyjPlugin) extractPasswordFromURLText(text string) string {
	for _, pattern := range []string{`提取码[:：]?\\s*([A-Za-z0-9]{4,8})`, `密码[:：]?\\s*([A-Za-z0-9]{4,8})`, `pwd\\s*[=:：]\\s*([A-Za-z0-9]{4,8})`} {
		if match := regexp.MustCompile(pattern).FindStringSubmatch(text); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func (p *DyyjPlugin) cleanContentHTML(contentHTML string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(contentHTML))
	if err != nil {
		return strings.TrimSpace(htmlTagRegex.ReplaceAllString(contentHTML, ""))
	}
	return strings.TrimSpace(doc.Text())
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *DyyjPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			if p.debugMode {
				log.Printf("[DYYJ] 重试请求 (第 %d 次)，等待 %v: %s", i, backoff, req.URL.String())
			}
			time.Sleep(backoff)
		} else if p.debugMode {
			log.Printf("[DYYJ] 发送请求: %s", req.URL.String())
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err == nil && resp.StatusCode == 200 {
			if p.debugMode && i > 0 {
				log.Printf("[DYYJ] 重试成功 (第 %d 次): %s", i+1, req.URL.String())
			}
			return resp, nil
		}

		if resp != nil {
			if p.debugMode {
				log.Printf("[DYYJ] 请求失败，状态码: %d (尝试 %d/%d): %s", resp.StatusCode, i+1, maxRetries, req.URL.String())
			}
			resp.Body.Close()
		} else if err != nil && p.debugMode {
			log.Printf("[DYYJ] 请求失败，错误: %v (尝试 %d/%d): %s", err, i+1, maxRetries, req.URL.String())
		}
		lastErr = err
		if lastErr == nil {
			lastErr = fmt.Errorf("HTTP 状态码 %d", resp.StatusCode)
		}
	}

	return nil, fmt.Errorf("[%s] 重试 %d 次后仍然失败: %w", p.Name(), maxRetries, lastErr)
}

// parseSearchResults 解析搜索结果HTML
func (p *DyyjPlugin) parseSearchResults(doc *goquery.Document, htmlContent string) ([]model.SearchResult, error) {
	var results []model.SearchResult

	// 尝试多个选择器（注意：goquery可能无法正确解析noscript标签，需要特殊处理）
	selectors := []string{
		"noscript#flarum-content .container ul li",
		"noscript#flarum-content ul li",
		"noscript[id='flarum-content'] .container ul li",
		"noscript[id=\"flarum-content\"] .container ul li",
		"noscript .container ul li",
		"noscript ul li",
		"#flarum-content .container ul li",
		".container ul li",
		"ul li",
		"li",
	}

	// 如果goquery无法解析noscript，尝试直接使用正则表达式从HTML中提取
	if p.debugMode {
		log.Printf("[DYYJ] 如果选择器都失败，将使用正则表达式从HTML中提取链接")
	}

	if p.debugMode {
		log.Printf("[DYYJ] 开始解析搜索结果，尝试多个选择器")
	}

	var foundCount int
	var usedSelector string

	for _, selector := range selectors {
		if p.debugMode {
			log.Printf("[DYYJ] 尝试选择器: %s", selector)
		}

		count := 0
		doc.Find(selector).Each(func(i int, s *goquery.Selection) {
			count++
		})

		if p.debugMode {
			log.Printf("[DYYJ] 选择器 '%s' 找到 %d 个元素", selector, count)
		}

		if count > 0 {
			usedSelector = selector
			foundCount = count
			break
		}
	}

	if usedSelector == "" {
		if p.debugMode {
			log.Printf("[DYYJ] 所有选择器都未找到结果，使用正则表达式从HTML中提取链接")
		}
		// 使用正则表达式直接从HTML中提取链接（因为goquery可能无法解析noscript）
		results = p.parseSearchResultsWithRegex(htmlContent)
		if p.debugMode {
			log.Printf("[DYYJ] 正则表达式解析完成，获取到 %d 个结果", len(results))
		}
		return results, nil
	}

	if p.debugMode {
		log.Printf("[DYYJ] 使用选择器: %s，找到 %d 个元素", usedSelector, foundCount)
	}

	// 使用找到的选择器解析结果
	doc.Find(usedSelector).Each(func(i int, s *goquery.Selection) {
		if len(results) >= MaxResults {
			return
		}

		result := p.parseResultItem(s, i+1)
		if result != nil {
			results = append(results, *result)
			if p.debugMode {
				log.Printf("[DYYJ] 解析结果项 %d: %s", i+1, result.Title)
			}
		} else if p.debugMode {
			log.Printf("[DYYJ] 跳过无效结果项 %d", i+1)
		}
	})

	if p.debugMode {
		log.Printf("[DYYJ] 找到 %d 个结果项，成功解析 %d 个", foundCount, len(results))
	}

	return results, nil
}

// parseSearchResultsWithRegex 使用正则表达式从HTML中提取搜索结果
func (p *DyyjPlugin) parseSearchResultsWithRegex(htmlContent string) []model.SearchResult {
	var results []model.SearchResult

	// 首先尝试找到noscript#flarum-content标签内的内容（使用预编译的正则）
	noscriptMatches := noscriptRegex.FindStringSubmatch(htmlContent)

	var searchArea string
	if len(noscriptMatches) > 1 {
		searchArea = noscriptMatches[1]
		if p.debugMode {
			log.Printf("[DYYJ] 找到noscript#flarum-content标签，内容长度: %d 字节", len(searchArea))
		}
	} else {
		// 如果找不到，使用整个HTML
		searchArea = htmlContent
		if p.debugMode {
			log.Printf("[DYYJ] 未找到noscript#flarum-content标签，使用整个HTML")
		}
	}

	// 匹配 <li> 标签内的链接（使用预编译的正则）
	matches := liLinkRegex.FindAllStringSubmatch(searchArea, -1)

	if p.debugMode {
		log.Printf("[DYYJ] 正则表达式找到 %d 个匹配项", len(matches))
	}

	for i, match := range matches {
		if len(results) >= MaxResults {
			break
		}

		if len(match) >= 3 {
			href := match[1]
			title := strings.TrimSpace(match[2])
			// 清理HTML标签（使用预编译的正则）
			title = htmlTagRegex.ReplaceAllString(title, "")
			title = strings.TrimSpace(title)

			if title == "" || !strings.Contains(href, "/d/") {
				continue
			}

			// 确保是完整URL
			if !strings.HasPrefix(href, "http") {
				if strings.HasPrefix(href, "/") {
					href = BaseURL + href
				} else {
					href = BaseURL + "/" + href
				}
			}

			// 从href中提取ID
			postID := p.extractPostID(href)
			if postID == "" {
				postID = fmt.Sprintf("regex-%d", i+1)
			}

			result := model.SearchResult{
				Title:    title,
				Content:  fmt.Sprintf("详情页: %s", href),
				Channel:  "",
				UniqueID: fmt.Sprintf("%s-%s", p.Name(), postID),
				Datetime: time.Time{}, // 初始化为零值，稍后从详情页获取
				Links:    []model.Link{},
				Tags:     []string{},
			}

			results = append(results, result)

			if p.debugMode {
				log.Printf("[DYYJ] 正则解析结果 %d: %s -> %s", i+1, title, href)
			}
		}
	}

	return results
}

// parseResultItem 解析单个搜索结果项
func (p *DyyjPlugin) parseResultItem(s *goquery.Selection, index int) *model.SearchResult {
	// 提取链接
	linkEl := s.Find("a")
	if linkEl.Length() == 0 {
		if p.debugMode {
			log.Printf("[DYYJ] 结果项 %d: 未找到链接元素", index)
		}
		return nil
	}

	// 提取标题
	title := strings.TrimSpace(linkEl.Text())
	if title == "" {
		if p.debugMode {
			log.Printf("[DYYJ] 结果项 %d: 标题为空", index)
		}
		return nil
	}

	// 提取详情页链接
	detailURL, exists := linkEl.Attr("href")
	if !exists || detailURL == "" {
		if p.debugMode {
			log.Printf("[DYYJ] 结果项 %d: 未找到详情页链接，标题: %s", index, title)
		}
		return nil
	}

	// 确保是完整URL
	if !strings.HasPrefix(detailURL, "http") {
		if strings.HasPrefix(detailURL, "/") {
			detailURL = BaseURL + detailURL
		} else {
			detailURL = BaseURL + "/" + detailURL
		}
	}

	// 从URL中提取ID
	postID := p.extractPostID(detailURL)
	if postID == "" {
		postID = fmt.Sprintf("unknown-%d", index)
	}

	// 构建初始结果对象（详情页链接稍后获取）
	result := model.SearchResult{
		Title:    title,
		Content:  fmt.Sprintf("详情页: %s", detailURL),
		Channel:  "", // 插件搜索结果必须为空字符串（按开发指南要求）
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), postID),
		Datetime: time.Time{},    // 初始化为零值，稍后从详情页获取
		Links:    []model.Link{}, // 先为空，详情页处理后添加
		Tags:     []string{},
	}

	return &result
}

// extractPostID 从URL中提取文章ID
func (p *DyyjPlugin) extractPostID(url string) string {
	// 使用预编译的正则表达式
	matches := postIDRegex.FindStringSubmatch(url)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// filterByTitleKeyword 根据标题过滤结果（只保留标题包含关键词的结果）
func (p *DyyjPlugin) filterByTitleKeyword(results []model.SearchResult, keyword string) []model.SearchResult {
	if keyword == "" {
		return results
	}

	lowerKeyword := strings.ToLower(keyword)
	keywords := strings.Fields(lowerKeyword) // 支持多关键词

	filtered := make([]model.SearchResult, 0, len(results))
	for _, result := range results {
		lowerTitle := strings.ToLower(result.Title)

		// 检查每个关键词是否都在标题中
		matched := true
		for _, kw := range keywords {
			if !strings.Contains(lowerTitle, kw) {
				matched = false
				break
			}
		}

		if matched {
			filtered = append(filtered, result)
		} else if p.debugMode {
			log.Printf("[DYYJ] 标题不包含关键词，跳过: %s", result.Title)
		}
	}

	return filtered
}

// fetchDetailLinks 并发获取详情页链接
func (p *DyyjPlugin) fetchDetailLinks(client *http.Client, searchResults []model.SearchResult, keyword string) []model.SearchResult {
	if len(searchResults) == 0 {
		if p.debugMode {
			log.Printf("[DYYJ] 没有搜索结果需要获取详情页")
		}
		return []model.SearchResult{}
	}

	if p.debugMode {
		log.Printf("[DYYJ] 开始并发获取 %d 个详情页链接，最大并发数: %d", len(searchResults), MaxConcurrency)
	}

	// 使用通道控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	resultsChan := make(chan model.SearchResult, len(searchResults))

	for _, result := range searchResults {
		wg.Add(1)
		go func(r model.SearchResult) {
			defer wg.Done()
			semaphore <- struct{}{}        // 获取信号量
			defer func() { <-semaphore }() // 释放信号量

			// 从Content中提取详情页URL
			detailURL := p.extractDetailURLFromContent(r.Content)
			if detailURL == "" {
				if p.debugMode {
					log.Printf("[DYYJ] 跳过无详情页URL的结果: %s", r.Title)
				}
				return
			}

			if p.debugMode {
				log.Printf("[DYYJ] 获取详情页链接: %s (标题: %s)", detailURL, r.Title)
			}

			// 获取详情页链接和时间信息
			links, publishTime := p.fetchDetailPageLinks(client, detailURL)
			if len(links) > 0 {
				r.Links = links
				// 如果获取到了发布时间，更新Datetime
				if !publishTime.IsZero() {
					r.Datetime = publishTime
					if p.debugMode {
						log.Printf("[DYYJ] 更新发布时间: %s -> %s", r.Title, publishTime.Format("2006-01-02 15:04:05"))
					}
				} else {
					// 如果没有获取到时间，使用当前时间作为默认值
					r.Datetime = time.Now()
					if p.debugMode {
						log.Printf("[DYYJ] 未获取到发布时间，使用当前时间: %s", r.Title)
					}
				}
				// 清理Content中的详情页URL
				r.Content = p.cleanContent(r.Content)
				if p.debugMode {
					log.Printf("[DYYJ] 成功获取详情页链接: %s，找到 %d 个网盘链接", r.Title, len(links))
				}
				resultsChan <- r
			} else if p.debugMode {
				log.Printf("[DYYJ] 详情页无有效链接: %s (URL: %s)", r.Title, detailURL)
			}
		}(result)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// 收集结果
	var finalResults []model.SearchResult
	for result := range resultsChan {
		finalResults = append(finalResults, result)
	}

	return finalResults
}

// extractDetailURLFromContent 从Content中提取详情页URL
func (p *DyyjPlugin) extractDetailURLFromContent(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "详情页: ") {
			return strings.TrimPrefix(line, "详情页: ")
		}
	}
	return ""
}

// cleanContent 清理Content，移除详情页URL行
func (p *DyyjPlugin) cleanContent(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "详情页: ") {
			cleanedLines = append(cleanedLines, line)
		}
	}
	return strings.Join(cleanedLines, "\n")
}

// fetchDetailPageLinks 获取详情页的网盘链接和发布时间
func (p *DyyjPlugin) fetchDetailPageLinks(client *http.Client, detailURL string) ([]model.Link, time.Time) {
	// 检查缓存
	if cached, found := p.detailCache.Load(detailURL); found {
		if cacheData, ok := cached.(*cacheItem); ok {
			if time.Since(cacheData.Timestamp) < p.cacheTTL {
				if p.debugMode {
					log.Printf("[DYYJ] 使用缓存的详情页链接: %s (缓存了 %d 个链接)", detailURL, len(cacheData.Links))
				}
				return cacheData.Links, cacheData.PublishTime
			}
		}
	}

	if p.debugMode {
		log.Printf("[DYYJ] 开始获取详情页: %s", detailURL)
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 创建详情页请求失败: %v (URL: %s)", err, detailURL)
		}
		return []model.Link{}, time.Time{}
	}

	// 设置请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Connection", "keep-alive")

	// 使用重试机制
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 详情页请求失败: %v (URL: %s)", err, detailURL)
		}
		return []model.Link{}, time.Time{}
	}
	defer resp.Body.Close()

	if p.debugMode {
		log.Printf("[DYYJ] 详情页响应状态码: %d (URL: %s)", resp.StatusCode, detailURL)
	}

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[DYYJ] 详情页HTTP状态错误: %d (URL: %s)", resp.StatusCode, detailURL)
		}
		return []model.Link{}, time.Time{}
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] 读取详情页响应失败: %v (URL: %s)", err, detailURL)
		}
		return []model.Link{}, time.Time{}
	}

	if p.debugMode {
		log.Printf("[DYYJ] 详情页响应体大小: %d 字节 (URL: %s)", len(body), detailURL)
	}

	// 解析网盘链接
	links := p.parseNetworkDiskLinks(string(body))

	// 提取发布时间
	publishTime := p.extractPublishTime(string(body))

	if p.debugMode {
		log.Printf("[DYYJ] 从详情页提取到 %d 个链接: %s", len(links), detailURL)
		for i, link := range links {
			log.Printf("[DYYJ]   链接 %d: %s (%s, 密码: %s)", i+1, link.URL, link.Type, link.Password)
		}
		if !publishTime.IsZero() {
			log.Printf("[DYYJ] 提取到发布时间: %s", publishTime.Format("2006-01-02 15:04:05"))
		} else {
			log.Printf("[DYYJ] 未提取到发布时间")
		}
	}

	// 缓存结果（即使为空也缓存，避免重复请求）
	p.detailCache.Store(detailURL, &cacheItem{
		Links:       links,
		PublishTime: publishTime,
		Timestamp:   time.Now(),
	})

	return links, publishTime
}

// cacheItem 缓存项
type cacheItem struct {
	Links       []model.Link
	PublishTime time.Time
	Timestamp   time.Time
}

// extractPublishTime 从HTML中提取发布时间
func (p *DyyjPlugin) extractPublishTime(htmlContent string) time.Time {
	// 使用预编译的正则表达式（性能优化）
	for _, re := range publishTimeRegexes {
		matches := re.FindStringSubmatch(htmlContent)
		if len(matches) >= 2 {
			timeStr := strings.TrimSpace(matches[1])
			// 尝试多种时间格式
			timeFormats := []string{
				time.RFC3339,                // 2006-01-02T15:04:05Z07:00
				"2006-01-02T15:04:05+00:00", // 2024-05-05T17:04:11+00:00
				"2006-01-02T15:04:05Z",      // 2006-01-02T15:04:05Z
				"2006-01-02 15:04:05",       // 2006-01-02 15:04:05
				"2006-01-02",                // 2006-01-02
			}

			for _, format := range timeFormats {
				if t, err := time.Parse(format, timeStr); err == nil {
					if p.debugMode {
						log.Printf("[DYYJ] 成功解析时间: %s (格式: %s)", timeStr, format)
					}
					return t
				}
			}

			if p.debugMode {
				log.Printf("[DYYJ] 无法解析时间格式: %s", timeStr)
			}
		}
	}

	return time.Time{}
}

// parseNetworkDiskLinks 解析网盘链接
func (p *DyyjPlugin) parseNetworkDiskLinks(htmlContent string) []model.Link {
	var links []model.Link
	seen := make(map[string]bool) // 用于去重

	if p.debugMode {
		log.Printf("[DYYJ] 开始解析网盘链接，HTML内容长度: %d 字节", len(htmlContent))
	}

	// 使用goquery解析HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		if p.debugMode {
			log.Printf("[DYYJ] goquery解析失败，使用正则表达式备选: %v", err)
		}
		// 如果goquery解析失败，使用正则表达式
		return p.parseNetworkDiskLinksWithRegex(htmlContent)
	}

	// 查找noscript标签中的内容
	selector := "noscript#flarum-content .container article .Post-body"
	if p.debugMode {
		log.Printf("[DYYJ] 使用goquery选择器: %s", selector)
	}

	foundPostBody := false
	doc.Find(selector).Each(func(i int, s *goquery.Selection) {
		foundPostBody = true
		if p.debugMode {
			log.Printf("[DYYJ] 找到Post-body元素 %d", i+1)
		}

		// 查找所有p标签，检查是否包含strong标签（网盘名称）和链接
		pCount := 0
		s.Find("p").Each(func(j int, pEl *goquery.Selection) {
			pCount++
			strongEl := pEl.Find("strong")
			if strongEl.Length() == 0 {
				return
			}

			strongText := strings.TrimSpace(strongEl.Text())

			if p.debugMode {
				log.Printf("[DYYJ]   检查p标签 %d，strong文本: %s", j+1, strongText)
			}

			// 检查是否是网盘名称
			if !p.isNetworkDiskName(strongText) {
				return
			}

			if p.debugMode {
				log.Printf("[DYYJ]   找到网盘名称: %s", strongText)
			}

			// 在当前p标签或下一个p标签中查找链接
			var linkEl *goquery.Selection

			// 先检查当前p标签
			linkEl = pEl.Find("a")
			if linkEl.Length() == 0 {
				// 如果当前p没有链接，查找下一个p标签
				nextP := pEl.Next()
				if nextP.Length() > 0 {
					linkEl = nextP.Find("a")
				}
			}

			if linkEl.Length() > 0 {
				linkURL, exists := linkEl.Attr("href")
				if exists && linkURL != "" {
					// 去重检查
					if seen[linkURL] {
						return
					}
					seen[linkURL] = true

					// 确定网盘类型
					urlType := p.determineCloudType(linkURL)
					if urlType != "others" {
						// 提取密码
						password := p.extractPasswordFromURL(linkURL)

						link := model.Link{
							Type:     urlType,
							URL:      linkURL,
							Password: password,
						}

						if p.debugMode {
							log.Printf("[DYYJ]   找到网盘链接: %s (%s, 密码: %s)", linkURL, urlType, password)
						}

						links = append(links, link)
					} else if p.debugMode {
						log.Printf("[DYYJ]   链接类型为others，跳过: %s", linkURL)
					}
				} else if p.debugMode {
					log.Printf("[DYYJ]   p标签 %d 中未找到链接", j+1)
				}
			}
		})

		if p.debugMode {
			log.Printf("[DYYJ] Post-body %d 中共有 %d 个p标签", i+1, pCount)
		}
	})

	if !foundPostBody && p.debugMode {
		log.Printf("[DYYJ] 未找到Post-body元素，尝试使用正则表达式")
	}

	// 如果goquery没有找到链接，使用正则表达式作为备选
	if len(links) == 0 {
		if p.debugMode {
			log.Printf("[DYYJ] goquery未找到链接，使用正则表达式备选方案")
		}
		links = p.parseNetworkDiskLinksWithRegex(htmlContent)
	}

	if p.debugMode {
		log.Printf("[DYYJ] 解析完成，共找到 %d 个网盘链接", len(links))
	}

	return links
}

// parseNetworkDiskLinksWithRegex 使用正则表达式解析网盘链接（备选方案）
func (p *DyyjPlugin) parseNetworkDiskLinksWithRegex(htmlContent string) []model.Link {
	var links []model.Link

	if p.debugMode {
		log.Printf("[DYYJ] 使用正则表达式解析网盘链接")
	}

	// 去重用的map
	seen := make(map[string]bool)

	// 使用预编译的正则表达式（性能优化）
	for _, pattern := range networkDiskPatterns {
		matches := pattern.regex.FindAllStringSubmatch(htmlContent, -1)

		if p.debugMode {
			log.Printf("[DYYJ] 正则模式 '%s' 找到 %d 个匹配", pattern.name, len(matches))
		}

		for _, match := range matches {
			if len(match) >= 2 {
				linkURL := match[1]

				// 去重
				if seen[linkURL] {
					if p.debugMode {
						log.Printf("[DYYJ] 跳过重复链接: %s", linkURL)
					}
					continue
				}
				seen[linkURL] = true

				// 确定网盘类型
				urlType := p.determineCloudType(linkURL)
				if urlType == "others" {
					urlType = pattern.urlType
				}

				// 只添加有效的网盘链接
				if urlType != "others" {
					// 提取密码
					password := p.extractPasswordFromURL(linkURL)

					link := model.Link{
						Type:     urlType,
						URL:      linkURL,
						Password: password,
					}

					if p.debugMode {
						log.Printf("[DYYJ] 正则找到网盘链接: %s (%s, 密码: %s)", linkURL, urlType, password)
					}

					links = append(links, link)
				} else if p.debugMode {
					log.Printf("[DYYJ] 链接类型为others，跳过: %s", linkURL)
				}
			}
		}
	}

	if p.debugMode {
		log.Printf("[DYYJ] 正则表达式解析完成，共找到 %d 个网盘链接", len(links))
	}

	return links
}

// isNetworkDiskName 检查是否是网盘名称
func (p *DyyjPlugin) isNetworkDiskName(text string) bool {
	networkDiskNames := []string{
		"夸克", "百度", "阿里", "天翼", "迅雷", "115", "123", "蓝奏",
		"夸克网盘", "百度网盘", "阿里云盘", "天翼云盘", "迅雷网盘", "115网盘", "123网盘",
	}

	lowerText := strings.ToLower(text)
	for _, name := range networkDiskNames {
		if strings.Contains(lowerText, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// extractPasswordFromURL 从URL中提取密码
func (p *DyyjPlugin) extractPasswordFromURL(linkURL string) string {
	// 从URL参数中提取密码
	patterns := []string{
		`[?&]pwd=([A-Za-z0-9]{4,8})`,
		`[?&]password=([A-Za-z0-9]{4,8})`,
		`[?&]code=([A-Za-z0-9]{4,8})`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(linkURL)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

// determineCloudType 根据URL自动识别网盘类型（按开发指南完整列表）
func (p *DyyjPlugin) determineCloudType(url string) string {
	switch {
	case strings.Contains(url, "pan.quark.cn"):
		return "quark"
	case strings.Contains(url, "drive.uc.cn"):
		return "uc"
	case strings.Contains(url, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(url, "aliyundrive.com") || strings.Contains(url, "alipan.com"):
		return "aliyun"
	case strings.Contains(url, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(url, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "115.com") || strings.Contains(url, "115cdn.com") || strings.Contains(url, "anxia.com"):
		return "115"
	case strings.Contains(url, "123684.com") || strings.Contains(url, "123685.com") ||
		strings.Contains(url, "123912.com") || strings.Contains(url, "123pan.com") ||
		strings.Contains(url, "123pan.cn") || strings.Contains(url, "123592.com"):
		return "123"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	case strings.Contains(url, "magnet:"):
		return "magnet"
	case strings.Contains(url, "ed2k://"):
		return "ed2k"
	default:
		return "others"
	}
}
