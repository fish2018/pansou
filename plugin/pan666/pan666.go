package pan666

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 在init函数中注册插件
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewPan666AsyncPlugin())
}

const (
	// API基础URL
	BaseURL = "https://pan666.net/api/discussions"
	
	// 默认参数
	PageSize = 50 // 符合API实际返回数量
	MaxRetries = 2
)

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.107 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1.2 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:90.0) Gecko/20100101 Firefox/90.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.114 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.107 Safari/537.36",
}

// Pan666AsyncPlugin pan666网盘搜索异步插件
type Pan666AsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	retries int
}

// NewPan666AsyncPlugin 创建新的pan666异步插件
func NewPan666AsyncPlugin() *Pan666AsyncPlugin {
	return &Pan666AsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("pan666", 3),
		retries:         MaxRetries,
	}
}

// Search 执行搜索并返回结果
func (p *Pan666AsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 使用保存的主缓存键，传递ext参数但不使用
	return p.AsyncSearch(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *Pan666AsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 初始化随机数种子
	rand.Seed(time.Now().UnixNano())
	
	// 只并发请求2个页面（0-1页）
	allResults, _, err := p.fetchBatch(client, keyword, 0, 2)
	if err != nil {
		return nil, err
	}
	
	// 去重
	uniqueResults := p.deduplicateResults(allResults)
	
	// 使用过滤功能过滤结果
	filteredResults := p.FilterResultsByKeyword(uniqueResults, keyword)
	
	return filteredResults, nil
}

// fetchBatch 获取一批页面的数据
func (p *Pan666AsyncPlugin) fetchBatch(client *http.Client, keyword string, startOffset, pageCount int) ([]model.SearchResult, bool, error) {
	var wg sync.WaitGroup
	resultChan := make(chan struct{
		offset  int
		results []model.SearchResult
		hasMore bool
		err     error
	}, pageCount)
	
	// 并发请求多个页面，但每个请求之间添加随机延迟
	for i := 0; i < pageCount; i++ {
		offset := (startOffset + i) * PageSize
		wg.Add(1)
		
		go func(offset int, index int) {
			defer wg.Done()
			
			// 第一个请求立即执行，后续请求添加随机延迟
			if index > 0 {
				// 随机等待0-1秒
				randomDelay := time.Duration(100 + rand.Intn(900)) * time.Millisecond
				time.Sleep(randomDelay)
			}
			
			// 请求特定页面
			results, hasMore, err := p.fetchPage(client, keyword, offset)
			
			resultChan <- struct{
				offset  int
				results []model.SearchResult
				hasMore bool
				err     error
			}{
				offset:  offset,
				results: results,
				hasMore: hasMore,
				err:     err,
			}
		}(offset, i)
	}
	
	// 等待所有请求完成
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// 收集结果
	var allResults []model.SearchResult
	hasMore := false
	
	for result := range resultChan {
		if result.err != nil {
			return nil, false, result.err
		}
		
		allResults = append(allResults, result.results...)
		hasMore = hasMore || result.hasMore
	}
	
	return allResults, hasMore, nil
}

// deduplicateResults 去除重复结果
func (p *Pan666AsyncPlugin) deduplicateResults(results []model.SearchResult) []model.SearchResult {
	seen := make(map[string]bool)
	unique := make([]model.SearchResult, 0, len(results))
	
	for _, result := range results {
		if !seen[result.UniqueID] {
			seen[result.UniqueID] = true
			unique = append(unique, result)
		}
	}
	
	// 按时间降序排序
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Datetime.After(unique[j].Datetime)
	})
	
	return unique
}

// fetchPage 获取指定页的搜索结果
func (p *Pan666AsyncPlugin) fetchPage(client *http.Client, keyword string, offset int) ([]model.SearchResult, bool, error) {
	// 构建API URL
	apiURL := fmt.Sprintf("%s?filter[q]=%s&include=mostRelevantPost&page[offset]=%d&page[limit]=%d",
		BaseURL, url.QueryEscape(keyword), offset, PageSize)
	
	// 创建请求
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("创建请求失败: %w", err)
	}
	
	// 设置请求头
	req.Header.Set("User-Agent", getRandomUA())
	req.Header.Set("X-Forwarded-For", generateRandomIP())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	
	var resp *http.Response
	var responseBody []byte
	
	// 重试逻辑
	for i := 0; i <= p.retries; i++ {
		// 发送请求
		resp, err = client.Do(req)
		if err != nil {
			if i == p.retries {
				return nil, false, fmt.Errorf("请求失败: %w", err)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		
		defer resp.Body.Close()
		
		// 读取响应体
		responseBody, err = io.ReadAll(resp.Body)
		if err != nil {
			if i == p.retries {
				return nil, false, fmt.Errorf("读取响应失败: %w", err)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		
		// 状态码检查
		if resp.StatusCode != http.StatusOK {
			if i == p.retries {
				return nil, false, fmt.Errorf("API返回非200状态码: %d", resp.StatusCode)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		
		// 请求成功，跳出重试循环
		break
	}
	
	// 解析响应
	var apiResp Pan666Response
	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return nil, false, fmt.Errorf("解析响应失败: %w", err)
	}
	
	// 处理结果
	results := make([]model.SearchResult, 0, len(apiResp.Data))
	postMap := make(map[string]Pan666Post)
	
	// 创建帖子ID到帖子内容的映射
	for _, post := range apiResp.Included {
		postMap[post.ID] = post
	}
	
	// 遍历搜索结果
	for _, discussion := range apiResp.Data {
		// 获取相关帖子
		postID := discussion.Relationships.MostRelevantPost.Data.ID
		post, ok := postMap[postID]
		if !ok {
			continue
		}
		
		// 清理HTML内容
		cleanedHTML := cleanHTML(post.Attributes.ContentHTML)
		
		// 提取链接
		links := extractLinksFromText(cleanedHTML)
		if len(links) == 0 {
			links = extractLinks(cleanedHTML)
		}
		
		// 如果没有找到链接，跳过该结果
		if len(links) == 0 {
			continue
		}
		
		// 解析时间
		createdTime, err := time.Parse(time.RFC3339, discussion.Attributes.CreatedAt)
		if err != nil {
			createdTime = time.Now() // 如果解析失败，使用当前时间
		}
		
		// 创建唯一ID：插件名-帖子ID
		uniqueID := fmt.Sprintf("pan666-%s", discussion.ID)
		
		// 创建搜索结果
		result := model.SearchResult{
			UniqueID:  uniqueID,
			Title:     discussion.Attributes.Title,
			Datetime:  createdTime,
			Links:     links,
		}
		
		results = append(results, result)
	}
	
	// 判断是否有更多结果
	hasMore := apiResp.Links.Next != ""
	
	return results, hasMore, nil
}

// 生成随机IP
func generateRandomIP() string {
	return fmt.Sprintf("%d.%d.%d.%d", 
		rand.Intn(223)+1,  // 避免0和255
		rand.Intn(255),
		rand.Intn(255),
		rand.Intn(254)+1)  // 避免0
}

// 获取随机UA
func getRandomUA() string {
	return userAgents[rand.Intn(len(userAgents))]
}

// 从文本提取链接
func extractLinks(content string) []model.Link {
	var allLinks []model.Link
	
	// 提取百度网盘链接
	baiduLinks := extractLinksByPattern(content, "链接: https://pan.baidu.com", "提取码:", "baidu")
	allLinks = append(allLinks, baiduLinks...)
	
	// 提取蓝奏云链接
	lanzouLinks := extractLinksByPattern(content, "https://[a-zA-Z0-9-]+.lanzou", "密码:", "lanzou")
	allLinks = append(allLinks, lanzouLinks...)
	
	// 提取阿里云盘链接
	aliyunLinks := extractLinksByPattern(content, "https://www.aliyundrive.com/s/", "提取码:", "aliyun")
	allLinks = append(allLinks, aliyunLinks...)
	
	// 提取天翼云盘链接
	tianyiLinks := extractLinksByPattern(content, "https://cloud.189.cn", "访问码:", "tianyi")
	allLinks = append(allLinks, tianyiLinks...)
	
	return allLinks
}

// 根据模式提取链接
func extractLinksByPattern(content, pattern, altPattern, linkType string) []model.Link {
	var links []model.Link
	
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, pattern) {
			link := extractLinkFromLine(line, pattern)
			
			// 如果在当前行找不到密码，尝试在下一行查找
			if link.Password == "" && i+1 < len(lines) && strings.Contains(lines[i+1], altPattern) {
				passwordLine := lines[i+1]
				start := strings.Index(passwordLine, altPattern) + len(altPattern)
				if start < len(passwordLine) {
					end := len(passwordLine)
					// 提取密码（移除前后空格）
					password := strings.TrimSpace(passwordLine[start:end])
					link.Password = password
				}
			}
			
			link.Type = linkType
			links = append(links, link)
		}
	}
	
	return links
}

// 从行中提取链接
func extractLinkFromLine(line, prefix string) model.Link {
	var link model.Link
	
	start := strings.Index(line, prefix)
	if start < 0 {
		return link
	}
	
	// 查找URL的结束位置
	end := len(line)
	possibleEnds := []string{" ", "提取码", "密码", "访问码"}
	for _, endStr := range possibleEnds {
		pos := strings.Index(line[start:], endStr)
		if pos > 0 && start+pos < end {
			end = start + pos
		}
	}
	
	// 提取URL
	url := strings.TrimSpace(line[start:end])
	link.URL = url
	
	// 尝试从同一行提取密码
	passwordKeywords := []string{"提取码:", "密码:", "访问码:"}
	for _, keyword := range passwordKeywords {
		passwordStart := strings.Index(line, keyword)
		if passwordStart >= 0 {
			passwordStart += len(keyword)
			passwordEnd := len(line)
			password := strings.TrimSpace(line[passwordStart:passwordEnd])
			link.Password = password
			break
		}
	}
	
	// 尝试从URL中提取密码
	if link.Password == "" {
		link.Password = extractPasswordFromURL(url)
	}
	
	return link
}

// 清理HTML内容
func cleanHTML(html string) string {
	// 移除<br>标签
	html = strings.ReplaceAll(html, "<br>", "\n")
	html = strings.ReplaceAll(html, "<br/>", "\n")
	html = strings.ReplaceAll(html, "<br />", "\n")
	
	// 移除其他HTML标签
	var result strings.Builder
	inTag := false
	
	for _, r := range html {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}
	
	// 处理HTML实体
	output := result.String()
	output = strings.ReplaceAll(output, "&amp;", "&")
	output = strings.ReplaceAll(output, "&lt;", "<")
	output = strings.ReplaceAll(output, "&gt;", ">")
	output = strings.ReplaceAll(output, "&quot;", "\"")
	output = strings.ReplaceAll(output, "&apos;", "'")
	output = strings.ReplaceAll(output, "&#39;", "'")
	output = strings.ReplaceAll(output, "&nbsp;", " ")
	
	// 处理多行空白
	lines := strings.Split(output, "\n")
	var cleanedLines []string
	
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleanedLines = append(cleanedLines, trimmed)
		}
	}
	
	return strings.Join(cleanedLines, "\n")
}

// 提取文本中的链接
func extractLinksFromText(content string) []model.Link {
	var allLinks []model.Link
	
	lines := strings.Split(content, "\n")
	
	// 收集所有可能的链接信息
	var linkInfos []struct {
		link     model.Link
		position int
		category string
	}
	
	// 收集所有可能的密码信息
	var passwordInfos []struct {
		keyword   string
		position  int
		password  string
	}
	
	// 第一遍：查找所有的链接和密码
	for i, line := range lines {
		// 检查链接
		line = strings.TrimSpace(line)
		
		// 检查百度网盘
		if strings.Contains(line, "pan.baidu.com") {
			url := extractURLFromText(line)
			if url != "" {
				linkInfos = append(linkInfos, struct {
					link     model.Link
					position int
					category string
				}{
					link:     model.Link{URL: url, Type: "baidu"},
					position: i,
					category: "baidu",
				})
			}
		}
		
		// 检查阿里云盘
		if strings.Contains(line, "aliyundrive.com") {
			url := extractURLFromText(line)
			if url != "" {
				linkInfos = append(linkInfos, struct {
					link     model.Link
					position int
					category string
				}{
					link:     model.Link{URL: url, Type: "aliyun"},
					position: i,
					category: "aliyun",
				})
			}
		}
		
		// 检查蓝奏云
		if strings.Contains(line, "lanzou") {
			url := extractURLFromText(line)
			if url != "" {
				linkInfos = append(linkInfos, struct {
					link     model.Link
					position int
					category string
				}{
					link:     model.Link{URL: url, Type: "lanzou"},
					position: i,
					category: "lanzou",
				})
			}
		}
		
		// 检查天翼云盘
		if strings.Contains(line, "cloud.189.cn") {
			url := extractURLFromText(line)
			if url != "" {
				linkInfos = append(linkInfos, struct {
					link     model.Link
					position int
					category string
				}{
					link:     model.Link{URL: url, Type: "tianyi"},
					position: i,
					category: "tianyi",
				})
			}
		}
		
		// 检查提取码/密码/访问码
		passwordKeywords := []string{"提取码", "密码", "访问码"}
		for _, keyword := range passwordKeywords {
			if strings.Contains(line, keyword) {
				// 寻找冒号后面的内容
				colonPos := strings.Index(line, ":")
				if colonPos == -1 {
					colonPos = strings.Index(line, "：")
				}
				
				if colonPos != -1 && colonPos+1 < len(line) {
					password := strings.TrimSpace(line[colonPos+1:])
					// 如果密码长度超过10个字符，可能不是密码
					if len(password) <= 10 {
						passwordInfos = append(passwordInfos, struct {
							keyword   string
							position  int
							password  string
						}{
							keyword:   keyword,
							position:  i,
							password:  password,
						})
					}
				}
			}
		}
	}
	
	// 第二遍：将密码与链接匹配
	for i := range linkInfos {
		// 检查链接自身是否包含密码
		password := extractPasswordFromURL(linkInfos[i].link.URL)
		if password != "" {
			linkInfos[i].link.Password = password
			continue
		}
		
		// 查找最近的密码
		minDistance := 1000000
		var closestPassword string
		
		for _, pwInfo := range passwordInfos {
			// 根据链接类型和密码关键词进行匹配
			match := false
			
			if linkInfos[i].category == "baidu" && (pwInfo.keyword == "提取码" || pwInfo.keyword == "密码") {
				match = true
			} else if linkInfos[i].category == "aliyun" && (pwInfo.keyword == "提取码" || pwInfo.keyword == "密码") {
				match = true
			} else if linkInfos[i].category == "lanzou" && pwInfo.keyword == "密码" {
				match = true
			} else if linkInfos[i].category == "tianyi" && (pwInfo.keyword == "访问码" || pwInfo.keyword == "密码") {
				match = true
			}
			
			if match {
				distance := abs(pwInfo.position - linkInfos[i].position)
				if distance < minDistance {
					minDistance = distance
					closestPassword = pwInfo.password
				}
			}
		}
		
		// 只有当距离较近时才认为是匹配的密码
		if minDistance <= 3 {
			linkInfos[i].link.Password = closestPassword
		}
	}
	
	// 收集所有有效链接
	for _, info := range linkInfos {
		allLinks = append(allLinks, info.link)
	}
	
	return allLinks
}

// 从文本中提取URL
func extractURLFromText(text string) string {
	// 查找URL的起始位置
	urlPrefixes := []string{"http://", "https://"}
	start := -1
	
	for _, prefix := range urlPrefixes {
		pos := strings.Index(text, prefix)
		if pos != -1 {
			start = pos
			break
		}
	}
	
	if start == -1 {
		return ""
	}
	
	// 查找URL的结束位置
	end := len(text)
	endChars := []string{" ", "\t", "\n", "\"", "'", "<", ">", ")", "]", "}", ",", ";"}
	
	for _, char := range endChars {
		pos := strings.Index(text[start:], char)
		if pos != -1 && start+pos < end {
			end = start + pos
		}
	}
	
	return text[start:end]
}

// 从URL中提取密码
func extractPasswordFromURL(url string) string {
	// 查找密码参数
	pwdParams := []string{"pwd=", "password=", "passcode=", "code="}
	
	for _, param := range pwdParams {
		pos := strings.Index(url, param)
		if pos != -1 {
			start := pos + len(param)
			end := len(url)
			
			// 查找参数结束位置
			for i := start; i < len(url); i++ {
				if url[i] == '&' || url[i] == '#' {
					end = i
					break
				}
			}
			
			if start < end {
				return url[start:end]
			}
		}
	}
	
	return ""
}

// 绝对值函数
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Pan666Response API响应结构
type Pan666Response struct {
	Links struct {
		First string `json:"first"`
		Next  string `json:"next,omitempty"`
	} `json:"links"`
	Data     []Pan666Discussion `json:"data"`
	Included []Pan666Post       `json:"included"`
}

// Pan666Discussion 讨论信息
type Pan666Discussion struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		Title          string    `json:"title"`
		Slug           string    `json:"slug"`
		CommentCount   int       `json:"commentCount"`
		CreatedAt      string    `json:"createdAt"`
		LastPostedAt   string    `json:"lastPostedAt"`
		LastPostNumber int       `json:"lastPostNumber"`
		IsApproved     bool      `json:"isApproved"`
	} `json:"attributes"`
	Relationships struct {
		MostRelevantPost struct {
			Data struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"data"`
		} `json:"mostRelevantPost"`
	} `json:"relationships"`
}

// Pan666Post 帖子内容
type Pan666Post struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	Attributes struct {
		Number      int    `json:"number"`
		CreatedAt   string `json:"createdAt"`
		ContentType string `json:"contentType"`
		ContentHTML string `json:"contentHtml"`
	} `json:"attributes"`
} 