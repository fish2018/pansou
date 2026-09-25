package discourse

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
	"regexp"
	"strings"
	"time"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
)

// 预编译的正则表达式 - 用于从blurb中提取网盘链接
var (
	// 网盘链接正则表达式
	quarkRegex  = regexp.MustCompile(`https://pan\.quark\.cn/s/[0-9a-zA-Z]+`)
	baiduRegex  = regexp.MustCompile(`https://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+(?:\?pwd=([0-9a-zA-Z]+))?`)
	aliyunRegex = regexp.MustCompile(`https://(?:www\.)?aliyundrive\.com/s/[0-9a-zA-Z]+`)
	xunleiRegex = regexp.MustCompile(`https://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+(?:\?pwd=([0-9a-zA-Z]+))?`)
	tianyiRegex = regexp.MustCompile(`https://cloud\.189\.cn/t/[0-9a-zA-Z]+`)
	ucRegex     = regexp.MustCompile(`https://drive\.uc\.cn/s/[0-9a-zA-Z]+`)
	pan115Regex = regexp.MustCompile(`https://115\.com/s/[0-9a-zA-Z]+`)

	// 百度网盘提取码 (出现在文本中)
	baiduPwdRegex = regexp.MustCompile(`(?:提取码|密码|pwd)[：:]\s*([0-9a-zA-Z]{4})`)
)

// 常量定义
const (
	pluginName = "discourse"
	// searchURLTemplate = "https://linux.do/search.json?q=%s%%20%%23resource%%3Acloud-asset%%20in%%3Atitle&page=%d"
	searchURLTemplate = "https://linux.do/search.json?q=%s%%20in%%3Atitle%%20%%23resource&page=%d"
	detailURLTemplate = "https://linux.do/t/%d.json?track_visit=true&forceLoad=true"
	defaultPriority   = 2
	defaultTimeout    = 30 * time.Second

	// 多页获取配置
	defaultMaxPages  = 1                      // 默认最多获取1页
	maxAllowedPages  = 10                     // 最多允许获取10页
	pageRequestDelay = 500 * time.Millisecond // 每页请求间隔
)

// DiscourseAsyncPlugin 是 Discourse 论坛的异步搜索插件实现
type DiscourseAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	scraper *cloudscraper.Scraper
}

// SearchResponse 搜索API响应结构
type SearchResponse struct {
	Posts               []Post              `json:"posts"`
	Topics              []Topic             `json:"topics"`
	GroupedSearchResult GroupedSearchResult `json:"grouped_search_result"`
}

// Post 帖子信息
type Post struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	LikeCount int    `json:"like_count"`
	Blurb     string `json:"blurb"`
	TopicID   int    `json:"topic_id"`
}

// Topic 主题信息
type Topic struct {
	ID         int      `json:"id"`
	Title      string   `json:"title"`
	FancyTitle string   `json:"fancy_title"`
	Tags       []string `json:"tags"`
	PostsCount int      `json:"posts_count"`
	CreatedAt  string   `json:"created_at"`
	CategoryID int      `json:"category_id"`
}

// GroupedSearchResult 搜索元数据
type GroupedSearchResult struct {
	Term        string `json:"term"`
	PostIDs     []int  `json:"post_ids"`
	MoreResults bool   `json:"more_full_page_results"`
}

// DetailResponse 详情API响应结构
type DetailResponse struct {
	PostStream PostStream `json:"post_stream"`
	ID         int        `json:"id"`
	Title      string     `json:"title"`
	Tags       []string   `json:"tags"`
}

// PostStream 帖子流
type PostStream struct {
	Posts []DetailPost `json:"posts"`
}

// DetailPost 详情帖子
type DetailPost struct {
	ID         int         `json:"id"`
	Username   string      `json:"username"`
	CreatedAt  string      `json:"created_at"`
	Cooked     string      `json:"cooked"`
	TopicID    int         `json:"topic_id"`
	LinkCounts []LinkCount `json:"link_counts"`
}

// LinkCount 链接统计
type LinkCount struct {
	URL        string `json:"url"`
	Internal   bool   `json:"internal"`
	Reflection bool   `json:"reflection"`
	Clicks     int    `json:"clicks"`
}

// 确保 DiscourseAsyncPlugin 实现了 AsyncSearchPlugin 接口
var _ plugin.AsyncSearchPlugin = (*DiscourseAsyncPlugin)(nil)

// init 在包初始化时注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewDiscourseAsyncPlugin())
}

// NewDiscourseAsyncPlugin 创建一个新的 Discourse 异步插件实例
func NewDiscourseAsyncPlugin() *DiscourseAsyncPlugin {
	// 创建 cloudscraper 实例
	scraper, err := cloudscraper.New()
	if err != nil {
		// 如果创建失败，记录错误但不阻止插件注册
		fmt.Printf("[%s] Failed to create cloudscraper: %v\n", pluginName, err)
		return &DiscourseAsyncPlugin{
			BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		}
	}

	return &DiscourseAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(pluginName, defaultPriority),
		scraper:         scraper,
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *DiscourseAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *DiscourseAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	// 使用BaseAsyncPlugin的异步搜索能力
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现具体的搜索逻辑
func (p *DiscourseAsyncPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 检查 cloudscraper 是否初始化成功
	if p.scraper == nil {
		return nil, fmt.Errorf("cloudscraper not initialized")
	}

	// 提取 max_pages 参数（最多获取多少页）
	maxPages := defaultMaxPages
	if maxPagesVal, ok := ext["max_pages"]; ok {
		if maxPagesInt, ok := maxPagesVal.(int); ok {
			maxPages = maxPagesInt
		} else if maxPagesFloat, ok := maxPagesVal.(float64); ok {
			maxPages = int(maxPagesFloat)
		}
	}

	// 限制最大页数
	if maxPages > maxAllowedPages {
		maxPages = maxAllowedPages
	}
	if maxPages < 1 {
		maxPages = 1
	}

	// 提取起始page参数（默认为1）
	startPage := 1
	if pageVal, ok := ext["page"]; ok {
		if pageInt, ok := pageVal.(int); ok {
			startPage = pageInt
		}
	}

	// URL编码关键词
	encodedKeyword := url.QueryEscape(keyword)

	// 存储所有结果
	var allResults []model.SearchResult
	seenPostIDs := make(map[int]bool) // 用于去重
	fetchedPages := 0                 // 实际获取的页数

	// 循环获取多页
	for currentPage := startPage; currentPage < startPage+maxPages; currentPage++ {
		fetchedPages++
		// 如果不是第一页，添加延迟避免请求过快
		if currentPage > startPage {
			time.Sleep(pageRequestDelay)
		}

		searchURL := fmt.Sprintf(searchURLTemplate, encodedKeyword, currentPage)

		// 发送搜索请求
		resp, err := p.scraper.Get(searchURL)
		if err != nil {
			// 如果已经获取到一些结果，返回已有结果而不是报错
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: failed to fetch page %d: %v\n", p.Name(), currentPage, err)
				break
			}
			return nil, fmt.Errorf("[%s] search request failed on page %d: %w", p.Name(), currentPage, err)
		}

		// 检查HTTP状态码
		if resp.StatusCode != 200 {
			resp.Body.Close()
			// 如果已经获取到一些结果，返回已有结果
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: unexpected status code %d on page %d\n", p.Name(), resp.StatusCode, currentPage)
				break
			}
			return nil, fmt.Errorf("[%s] unexpected status code: %d on page %d", p.Name(), resp.StatusCode, currentPage)
		}

		// 读取响应体
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: failed to read page %d: %v\n", p.Name(), currentPage, err)
				break
			}
			return nil, fmt.Errorf("[%s] read response failed on page %d: %w", p.Name(), currentPage, err)
		}

		// 解析JSON响应
		var searchResp SearchResponse
		if err := json.Unmarshal(body, &searchResp); err != nil {
			if len(allResults) > 0 {
				fmt.Printf("[%s] Warning: failed to parse page %d: %v\n", p.Name(), currentPage, err)
				break
			}
			return nil, fmt.Errorf("[%s] parse json failed on page %d: %w", p.Name(), currentPage, err)
		}

		// 如果没有帖子了，停止获取
		if len(searchResp.Posts) == 0 {
			break
		}

		// 转换为SearchResult并去重
		pageResults := p.convertToSearchResults(searchResp)

		// 添加结果（去重）
		for _, result := range pageResults {
			// 从 UniqueID 中提取帖子ID
			var postID int
			fmt.Sscanf(result.UniqueID, "discourse-%d", &postID)

			if !seenPostIDs[postID] {
				seenPostIDs[postID] = true
				allResults = append(allResults, result)
			}
		}

		// 如果 API 返回没有更多结果了，停止获取
		if !searchResp.GroupedSearchResult.MoreResults {
			break
		}

		// 如果这一页没有新的结果，也停止
		if len(pageResults) == 0 {
			break
		}
	}

	// 如果启用了多页获取，在日志中显示获取的总结果数
	if maxPages > 1 && len(allResults) > 0 {
		fmt.Printf("[%s] Fetched %d unique results from %d pages for keyword: %s\n",
			p.Name(), len(allResults), fetchedPages, keyword)
	}

	return allResults, nil
}

// max 返回两个整数中的较大值
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// convertToSearchResults 将搜索响应转换为SearchResult列表
func (p *DiscourseAsyncPlugin) convertToSearchResults(resp SearchResponse) []model.SearchResult {
	var results []model.SearchResult

	// 创建 topic 映射，方便快速查找
	topicMap := make(map[int]Topic)
	for _, topic := range resp.Topics {
		topicMap[topic.ID] = topic
	}

	// 遍历所有帖子
	for _, post := range resp.Posts {
		// 获取对应的主题
		topic, found := topicMap[post.TopicID]
		if !found {
			// 如果找不到主题，使用默认值
			topic = Topic{
				ID:    post.TopicID,
				Title: "未知标题",
				Tags:  []string{},
			}
		}

		// 从blurb中提取网盘链接
		links := p.extractNetDiskLinksFromBlurb(post.Blurb)

		// 如果没有提取到链接，跳过这个结果
		if len(links) == 0 {
			continue
		}

		// 解析时间
		createdAt, _ := time.Parse(time.RFC3339, post.CreatedAt)

		// 构建 SearchResult
		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%d", pluginName, post.ID),
			Title:    topic.Title,
			Content:  p.cleanContent(post.Blurb),
			Links:    links,
			Tags:     topic.Tags,
			Channel:  "", // 插件搜索结果必须为空
			Datetime: createdAt,
		}

		results = append(results, result)
	}

	return results
}

// extractNetDiskLinksFromBlurb 从blurb文本中提取网盘链接
func (p *DiscourseAsyncPlugin) extractNetDiskLinksFromBlurb(blurb string) []model.Link {
	var links []model.Link

	// 提取夸克网盘
	quarkLinks := quarkRegex.FindAllString(blurb, -1)
	for _, linkURL := range quarkLinks {
		links = append(links, model.Link{
			Type: "quark",
			URL:  linkURL,
		})
	}

	// 提取百度网盘（带提取码）
	baiduMatches := baiduRegex.FindAllStringSubmatch(blurb, -1)
	for _, match := range baiduMatches {
		link := model.Link{
			Type: "baidu",
			URL:  match[0],
		}
		// 如果URL中包含pwd参数
		if len(match) > 1 && match[1] != "" {
			link.Password = match[1]
		} else {
			// 尝试从文本中查找提取码
			pwdMatch := baiduPwdRegex.FindStringSubmatch(blurb)
			if len(pwdMatch) > 1 {
				link.Password = pwdMatch[1]
			}
		}
		links = append(links, link)
	}

	// 提取阿里云盘
	aliyunLinks := aliyunRegex.FindAllString(blurb, -1)
	for _, linkURL := range aliyunLinks {
		links = append(links, model.Link{
			Type: "aliyun",
			URL:  linkURL,
		})
	}

	// 提取迅雷网盘（带提取码）
	xunleiMatches := xunleiRegex.FindAllStringSubmatch(blurb, -1)
	for _, match := range xunleiMatches {
		link := model.Link{
			Type: "xunlei",
			URL:  match[0],
		}
		if len(match) > 1 && match[1] != "" {
			link.Password = match[1]
		}
		links = append(links, link)
	}

	// 提取天翼云盘
	tianyiLinks := tianyiRegex.FindAllString(blurb, -1)
	for _, linkURL := range tianyiLinks {
		links = append(links, model.Link{
			Type: "tianyi",
			URL:  linkURL,
		})
	}

	// 提取UC网盘
	ucLinks := ucRegex.FindAllString(blurb, -1)
	for _, linkURL := range ucLinks {
		links = append(links, model.Link{
			Type: "uc",
			URL:  linkURL,
		})
	}

	// 提取115网盘
	pan115Links := pan115Regex.FindAllString(blurb, -1)
	for _, linkURL := range pan115Links {
		links = append(links, model.Link{
			Type: "115",
			URL:  linkURL,
		})
	}

	return links
}

// cleanContent 清理内容，移除HTML标签
func (p *DiscourseAsyncPlugin) cleanContent(content string) string {
	// 移除HTML标签
	content = discourseRe1.ReplaceAllString(content, "")

	// 解码HTML实体
	content = strings.ReplaceAll(content, "&amp;", "&")
	content = strings.ReplaceAll(content, "&lt;", "<")
	content = strings.ReplaceAll(content, "&gt;", ">")
	content = strings.ReplaceAll(content, "&quot;", "\"")
	content = strings.ReplaceAll(content, "&#39;", "'")

	// 移除多余空白
	content = discourseRe2.ReplaceAllString(content, " ")
	content = strings.TrimSpace(content)

	// 限制长度
	if len(content) > 200 {
		content = content[:200] + "..."
	}

	return content
}

// GetTopicDetail 获取主题详情（可选实现，用于获取完整链接）
func (p *DiscourseAsyncPlugin) GetTopicDetail(topicID int) ([]model.Link, error) {
	// 检查 cloudscraper 是否初始化成功
	if p.scraper == nil {
		return nil, fmt.Errorf("cloudscraper not initialized")
	}

	// 构建详情URL
	detailURL := fmt.Sprintf(detailURLTemplate, topicID)

	// 发送详情请求
	resp, err := p.scraper.Get(detailURL)
	if err != nil {
		return nil, fmt.Errorf("detail request failed: %w", err)
	}
	defer resp.Body.Close()

	// 检查HTTP状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response failed: %w", err)
	}

	// 解析JSON响应
	var detailResp DetailResponse
	if err := json.Unmarshal(body, &detailResp); err != nil {
		return nil, fmt.Errorf("parse json failed: %w", err)
	}

	// 提取第一个帖子的链接
	if len(detailResp.PostStream.Posts) == 0 {
		return nil, fmt.Errorf("no posts found")
	}

	mainPost := detailResp.PostStream.Posts[0]

	// 从 link_counts 中提取网盘链接
	var links []model.Link
	for _, linkCount := range mainPost.LinkCounts {
		// 跳过内部链接
		if linkCount.Internal {
			continue
		}

		// 判断是否为网盘链接并解析
		link := p.parseNetDiskLink(linkCount.URL)
		if link != nil {
			links = append(links, *link)
		}
	}

	return links, nil
}

// parseNetDiskLink 解析网盘链接
func (p *DiscourseAsyncPlugin) parseNetDiskLink(linkURL string) *model.Link {
	// 夸克网盘
	if quarkRegex.MatchString(linkURL) {
		return &model.Link{
			Type: "quark",
			URL:  linkURL,
		}
	}

	// 百度网盘
	if baiduRegex.MatchString(linkURL) {
		link := &model.Link{
			Type: "baidu",
			URL:  linkURL,
		}
		// 提取pwd参数
		if matches := baiduRegex.FindStringSubmatch(linkURL); len(matches) > 1 && matches[1] != "" {
			link.Password = matches[1]
		}
		return link
	}

	// 阿里云盘
	if aliyunRegex.MatchString(linkURL) {
		return &model.Link{
			Type: "aliyun",
			URL:  linkURL,
		}
	}

	// 迅雷网盘
	if xunleiRegex.MatchString(linkURL) {
		link := &model.Link{
			Type: "xunlei",
			URL:  linkURL,
		}
		// 提取pwd参数
		if matches := xunleiRegex.FindStringSubmatch(linkURL); len(matches) > 1 && matches[1] != "" {
			link.Password = matches[1]
		}
		return link
	}

	// 天翼云盘
	if tianyiRegex.MatchString(linkURL) {
		return &model.Link{
			Type: "tianyi",
			URL:  linkURL,
		}
	}

	// UC网盘
	if ucRegex.MatchString(linkURL) {
		return &model.Link{
			Type: "uc",
			URL:  linkURL,
		}
	}

	// 115网盘
	if pan115Regex.MatchString(linkURL) {
		return &model.Link{
			Type: "115",
			URL:  linkURL,
		}
	}

	// 不是网盘链接
	return nil
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	discourseRe1 = regexp.MustCompile(`<[^>]+>`)
	discourseRe2 = regexp.MustCompile(`\s+`)
)
