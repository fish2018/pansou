package panta

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"sort"
)

// 预编译的正则表达式
var (
	// 提取topicId的正则表达式
	topicIDRegex = regexp.MustCompile(`topicId=(\d+)`)
	
	// 从标题中提取年份的正则表达式
	yearRegex = regexp.MustCompile(`\(([0-9]{4})\)`)
	
	// 从文本中提取发表时间的正则表达式
	postTimeRegex = regexp.MustCompile(`发表时间：(.+)`)
	
	// 从URL中提取提取码的正则表达式
	pwdParamRegex = regexp.MustCompile(`[?&]pwd=([0-9a-zA-Z]+)`)
	
	// 提取码匹配模式
	pwdPatterns = []*regexp.Regexp{
		regexp.MustCompile(`提取码[：:]\s*([0-9a-zA-Z]+)`),
		regexp.MustCompile(`密码[：:]\s*([0-9a-zA-Z]+)`),
		regexp.MustCompile(`pwd[=:：]\s*([0-9a-zA-Z]+)`),
	}
	
	// 网盘链接的正则表达式
	netDiskPatterns = []*regexp.Regexp{
		// 百度网盘链接格式
		regexp.MustCompile(`https?://pan\.baidu\.com/s/[0-9a-zA-Z_\-]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		// 夸克网盘链接格式
		regexp.MustCompile(`https?://pan\.quark\.cn/s/[0-9a-zA-Z]+`),
		// 阿里云盘链接格式
		regexp.MustCompile(`https?://www\.aliyundrive\.com/s/[0-9a-zA-Z]+`),
		regexp.MustCompile(`https?://alipan\.com/s/[0-9a-zA-Z]+`),
		// 迅雷网盘链接格式 - 修正以支持任意长度的提取码和特殊字符
		regexp.MustCompile(`https?://pan\.xunlei\.com/s/[0-9a-zA-Z_\-]+(?:\?pwd=[0-9a-zA-Z]+)?[#]?`),
		// 天翼云盘链接格式
		regexp.MustCompile(`https?://cloud\.189\.cn/t/[0-9a-zA-Z]+`),
		// 移动云盘链接格式
		regexp.MustCompile(`https?://caiyun\.139\.com/m/i\?[0-9a-zA-Z]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		regexp.MustCompile(`https?://www\.caiyun\.139\.com/m/i\?[0-9a-zA-Z]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		regexp.MustCompile(`https?://caiyun\.139\.com/w/i\?[0-9a-zA-Z]+(?:\?pwd=[0-9a-zA-Z]+)?`),
		regexp.MustCompile(`https?://www\.caiyun\.139\.com/w/i\?[0-9a-zA-Z]+(?:\?pwd=[0-9a-zA-Z]+)?`),
	}
	
	// 提取码相关关键词
	pwdKeywords = []string{"提取码", "密码", "pwd", "验证码", "口令"}
	
	// 网盘域名列表，用于快速检查URL是否为网盘链接
	netDiskDomains = []string{
		"pan.baidu.com",
		"pan.quark.cn",
		"aliyundrive.com",
		"alipan.com",
		"pan.xunlei.com",
		"cloud.189.cn",
		"caiyun.139.com",
		"www.caiyun.139.com",
		"drive.uc.cn",
		"115.com",
		"mypikpak.com",
	}
	
	// 缓存相关
	isNetDiskLinkCache     = sync.Map{} // 缓存URL是否为网盘链接的结果
	determineLinkTypeCache = sync.Map{} // 缓存URL的链接类型
	extractPasswordCache   = sync.Map{} // 缓存提取码提取结果
	
	// 新增缓存，用于存储已解析的topicId
	topicIDCache = sync.Map{}
	
	// 新增缓存，用于存储已解析的发布时间
	postTimeCache = sync.Map{}
	
	// 新增缓存，用于存储已解析的年份
	yearCache = sync.Map{}
	
	// 链接提取结果缓存
	linkExtractCache = sync.Map{} // 缓存从文本中提取的链接结果
	
	// 线程链接缓存
	threadLinksCache = sync.Map{} // 缓存帖子详情页中的链接
)

// 缓存键结构，用于extractPassword函数
type passwordCacheKey struct {
	content string
	url     string
}

// 常量定义
const (
	// 插件名称
	pluginName = "panta"
	
	// 搜索URL模板
	searchURLTemplate = "https://www.91panta.cn/search?keyword=%s"
	
	// 帖子URL模板
	threadURLTemplate = "https://www.91panta.cn/thread?topicId=%s"
	
	// 默认优先级
	defaultPriority = 4
	
	// 默认超时时间（秒）
	defaultTimeout = 6
	
	// 默认并发数
	defaultConcurrency = 30
	
	// 最大重试次数
	maxRetries = 2
	
	// 最小并发数
	minConcurrency = 5
	
	// 最大并发数
	maxConcurrency = 50
	
	// 响应时间阈值（毫秒），超过此值则减少并发数
	responseTimeThreshold = 500
	
	// 并发调整步长
	concurrencyStep = 5
	
	// 并发调整间隔（秒）
	concurrencyAdjustInterval = 30
	
	// 指数退避基数（毫秒）
	backoffBase = 100
	
	// 最大退避时间（毫秒）
	maxBackoff = 5000
)

// PantaPlugin 是PanTa网站的搜索插件实现
type PantaPlugin struct {
	// HTTP客户端，用于发送请求
	client *http.Client
	
	// 并发控制
	maxConcurrency int
	
	// 自适应并发控制
	currentConcurrency int
	responseTimes      []time.Duration
	responseTimesMutex sync.Mutex
	lastAdjustTime     time.Time
}

// 确保PantaPlugin实现了SearchPlugin接口
var _ plugin.SearchPlugin = (*PantaPlugin)(nil)

// 在包初始化时注册插件
func init() {
	// 创建并注册插件实例
	plugin.RegisterGlobalPlugin(NewPantaPlugin())
}

// NewPantaPlugin 创建一个新的PanTa插件实例
func NewPantaPlugin() *PantaPlugin {
	// 创建一个带有更多配置的HTTP传输层
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 60 * time.Second, // 增加保持活动时间
			DualStack: true,             // 启用IPv4/IPv6双栈
		}).DialContext,
		MaxIdleConns:          200,  // 增加最大空闲连接数
		IdleConnTimeout:       120 * time.Second, // 增加空闲连接超时
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   50,   // 增加每个主机的最大空闲连接数
		MaxConnsPerHost:       100,  // 限制每个主机的最大连接数
		DisableKeepAlives:     false, // 确保启用长连接
		ForceAttemptHTTP2:     true,  // 尝试使用HTTP/2
		WriteBufferSize:       16 * 1024, // 增加写缓冲区大小
		ReadBufferSize:        16 * 1024, // 增加读缓冲区大小
	}
	
	// 创建HTTP客户端
	client := &http.Client{
		Timeout:   time.Duration(defaultTimeout) * time.Second,
		Transport: transport,
		// 禁用重定向，因为我们只关心初始响应
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	
	// 启动定期清理缓存的goroutine
	go startCacheCleaner()
	
	// 创建插件实例
	plugin := &PantaPlugin{
		client:             client,
		maxConcurrency:     defaultConcurrency,
		currentConcurrency: defaultConcurrency,
		responseTimes:      make([]time.Duration, 0, 100),
		lastAdjustTime:     time.Now(),
	}
	
	// 启动自适应并发控制
	go plugin.startConcurrencyAdjuster()
	
	return plugin
}

// startCacheCleaner 启动一个定期清理缓存的goroutine
func startCacheCleaner() {
	// 每小时清理一次缓存
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	
	for range ticker.C {
		// 清空所有缓存
		isNetDiskLinkCache = sync.Map{}
		determineLinkTypeCache = sync.Map{}
		extractPasswordCache = sync.Map{}
		topicIDCache = sync.Map{}
		postTimeCache = sync.Map{}
		yearCache = sync.Map{}
		linkExtractCache = sync.Map{}
		threadLinksCache = sync.Map{}
	}
}

// Name 返回插件名称
func (p *PantaPlugin) Name() string {
	return pluginName
}

// Priority 返回插件优先级
func (p *PantaPlugin) Priority() int {
	return defaultPriority
}

// Search 执行搜索并返回结果
func (p *PantaPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 对关键词进行URL编码
	encodedKeyword := url.QueryEscape(keyword)
	
	// 构建搜索URL
	searchURL := fmt.Sprintf(searchURLTemplate, encodedKeyword)
	
	// 创建一个带有超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(defaultTimeout)*time.Second)
	defer cancel()
	
	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}
	
	// 设置User-Agent和Referer
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Referer", "https://www.91panta.cn/index")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	
	// 使用带重试的请求方法发送HTTP请求
	resp, err := p.doRequestWithRetry(req)
	if err != nil {
		return nil, fmt.Errorf("请求PanTa搜索页面失败: %v", err)
	}
	defer resp.Body.Close()
	
	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求PanTa搜索页面失败，状态码: %d", resp.StatusCode)
	}
	
	// 使用goquery解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %v", err)
	}
	
	// 解析搜索结果
	results, err := p.parseSearchResults(doc)
	if err != nil {
		return nil, err
	}
	
	// 使用过滤功能过滤结果
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)
	
	return filteredResults, nil
}

// parseSearchResults 使用goquery解析搜索结果
func (p *PantaPlugin) parseSearchResults(doc *goquery.Document) ([]model.SearchResult, error) {
	var results []model.SearchResult
	
	// 创建信号量控制并发数，使用自适应并发数
	semaphore := make(chan struct{}, p.currentConcurrency)
	
	// 创建结果通道和错误通道
	resultChan := make(chan model.SearchResult, 100)
	errorChan := make(chan error, 100)
	
	// 创建等待组
	var wg sync.WaitGroup
	
	// 预先收集所有需要处理的话题项
	var topicItems []*goquery.Selection
	doc.Find("div.topicItem").Each(func(i int, s *goquery.Selection) {
		topicItems = append(topicItems, s)
	})
	
	// 如果没有找到任何话题，直接返回空结果
	if len(topicItems) == 0 {
		return results, nil
	}
	
	// 记录开始时间，用于计算总处理时间
	startTime := time.Now()
	
	// 批量处理所有话题项
	for i, s := range topicItems {
		wg.Add(1)
		
		// 为每个话题创建一个goroutine
		go func(index int, s *goquery.Selection) {
			defer wg.Done()
			
			// 获取信号量，限制并发数
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			
			// 记录处理开始时间
			itemStartTime := time.Now()
			
			// 提取话题ID
			topicLink := s.Find("a[href^='thread?topicId=']")
			href, exists := topicLink.Attr("href")
			if !exists {
				return
			}
			
			// 从href中提取topicId - 使用缓存
			var topicID string
			if cachedID, ok := topicIDCache.Load(href); ok {
				topicID = cachedID.(string)
			} else {
				match := topicIDRegex.FindStringSubmatch(href)
				if len(match) < 2 {
					return
				}
				topicID = match[1]
				topicIDCache.Store(href, topicID)
			}
			
			// 提取标题
			title := strings.TrimSpace(topicLink.Text())
			
			// 提取摘要
			summary := strings.TrimSpace(s.Find("h2.summary").Text())
			
			// 提取发布时间
			postTimeText := s.Find("span.postTime").Text()
			var postTime time.Time
			
			// 使用缓存提取发布时间
			if cachedTime, ok := postTimeCache.Load(postTimeText); ok {
				postTime = cachedTime.(time.Time)
			} else {
				timeMatch := postTimeRegex.FindStringSubmatch(postTimeText)
				if len(timeMatch) >= 2 {
					timeStr := strings.TrimSpace(timeMatch[1])
					parsedTime, err := time.Parse("2006-01-02 15:04:05", timeStr)
					if err == nil {
						postTime = parsedTime
					} else {
						postTime = time.Now()
					}
				} else {
					postTime = time.Now()
				}
				postTimeCache.Store(postTimeText, postTime)
			}
			
			// 从标题中提取年份作为可能的提取码
			var yearFromTitle string
			if cachedYear, ok := yearCache.Load(title); ok {
				yearFromTitle = cachedYear.(string)
			} else {
				yearMatch := yearRegex.FindStringSubmatch(title)
				if len(yearMatch) >= 2 {
					yearFromTitle = yearMatch[1]
				}
				yearCache.Store(title, yearFromTitle)
			}
			
			// 尝试从摘要中提取链接
			var links []model.Link
			
			// 首先尝试从当前元素中提取链接
			foundLinks := p.extractLinksFromElement(s, yearFromTitle)
			
			// 如果没有找到链接，尝试获取帖子详情
			if len(foundLinks) == 0 {
				// 添加重试机制
				for retry := 0; retry <= maxRetries; retry++ {
					if retry > 0 {
						// 重试前等待一段时间，使用指数退避
						backoffTime := time.Duration(min(backoffBase*1<<uint(retry-1), maxBackoff)) * time.Millisecond
						time.Sleep(backoffTime)
					}
					
					threadLinks, err := p.fetchThreadLinks(topicID)
					if err == nil && len(threadLinks) > 0 {
						foundLinks = threadLinks
						break
					}
				}
			}
			
			// 处理找到的链接
			for _, link := range foundLinks {
				links = append(links, link)
			}
			
			// 只有包含链接的结果才添加到结果中
			if len(links) > 0 {
				result := model.SearchResult{
					UniqueID: "panta_" + topicID,
					Channel:  pluginName,
					Datetime: postTime,
					Title:    title,
					Content:  summary,
					Links:    links,
					Tags:     []string{"panta"},
				}
				
				resultChan <- result
			}
			
			// 记录处理时间
			itemProcessTime := time.Since(itemStartTime)
			p.recordResponseTime(itemProcessTime)
			
		}(i, s)
	}
	
	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultChan)
		close(errorChan)
	}()
	
	// 收集所有结果
	for result := range resultChan {
		results = append(results, result)
	}
	
	// 检查是否有错误
	for err := range errorChan {
		if err != nil {
			return results, err
		}
	}
	
	// 记录总处理时间
	totalProcessTime := time.Since(startTime)
	// 调整并发数
	if totalProcessTime > time.Duration(defaultTimeout/2)*time.Second {
		// 如果处理时间过长，减少并发数
		p.currentConcurrency = max(p.currentConcurrency-concurrencyStep, minConcurrency)
	}
	
	return results, nil
}

// extractLinksFromElement 从元素中提取链接
func (p *PantaPlugin) extractLinksFromElement(s *goquery.Selection, yearFromTitle string) []model.Link {
	// 创建缓存键
	html, _ := s.Html()
	cacheKey := fmt.Sprintf("%s_%s", html, yearFromTitle)
	
	// 检查缓存中是否已有结果
	if cachedLinks, ok := linkExtractCache.Load(cacheKey); ok {
		return cachedLinks.([]model.Link)
	}
	
	var links []model.Link
	var foundURLs = make(map[string]bool) // 用于去重
	
	// 一次性获取所有链接
	var allHrefs []string
	var allTexts []string
	
	s.Find("a[href^='http']").Each(func(i int, a *goquery.Selection) {
		href, exists := a.Attr("href")
		if !exists {
			return
		}
		
		// 快速过滤非网盘链接
		isNetDisk := false
		for _, domain := range netDiskDomains {
			if strings.Contains(strings.ToLower(href), domain) {
				isNetDisk = true
				break
			}
		}
		
		if isNetDisk {
			allHrefs = append(allHrefs, href)
			
			// 获取周围文本，用于检查提取码相关信息
			surroundingText := a.Text()
			if surroundingText == "" {
				surroundingText = a.Parent().Text()
			}
			allTexts = append(allTexts, surroundingText)
		}
	})
	
	// 批量处理所有链接
	for i, href := range allHrefs {
		// 如果链接已存在，跳过
		if foundURLs[href] {
			continue
		}
		foundURLs[href] = true
		
		// 获取周围文本
		surroundingText := allTexts[i]
		if surroundingText == "" {
			surroundingText = s.Text()
		}
		
		// 确定链接类型
		linkType := determineLinkType(href)
		
		// 提取密码
		password := extractPassword(surroundingText, href)
		
		// 根据链接类型进行特殊处理
		switch linkType {
		case "quark":
			// 夸克网盘链接，只有在明确需要提取码的情况下才添加
			if password != "" {
				// 检查周围文本是否包含提取码相关关键词
				hasPasswordHint := false
				for _, keyword := range pwdKeywords {
					if strings.Contains(surroundingText, keyword) {
						hasPasswordHint = true
						break
					}
				}
				
				// 如果没有提取码相关关键词，则不添加提取码
				if !hasPasswordHint {
					password = ""
				}
			}
		case "mobile":
			// 移动云盘链接，只有在明确指定提取码的情况下才使用
			// 检查是否明确包含提取码信息
			hasExplicitPassword := false
			for _, pattern := range pwdPatterns {
				if matches := pattern.FindStringSubmatch(surroundingText); len(matches) >= 2 {
					// 使用明确指定的提取码
					password = matches[1]
					hasExplicitPassword = true
					break
				}
			}
			
			// 如果没有明确的提取码信息，则不使用提取码
			if !hasExplicitPassword && !strings.Contains(href, "pwd=") {
				password = ""
			}
		}
		
		// 添加链接
		links = append(links, model.Link{
			Type:     linkType,
			URL:      href,
			Password: password,
		})
	}
	
	// 缓存结果
	linkExtractCache.Store(cacheKey, links)
	
	return links
}

// fetchThreadLinks 获取帖子详情页中的链接
func (p *PantaPlugin) fetchThreadLinks(topicID string) ([]model.Link, error) {
	// 检查缓存中是否已有结果
	if cachedLinks, ok := threadLinksCache.Load(topicID); ok {
		return cachedLinks.([]model.Link), nil
	}
	
	// 构建帖子URL
	threadURL := fmt.Sprintf(threadURLTemplate, topicID)
	
	// 创建一个带有超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(defaultTimeout)*time.Second)
	defer cancel()
	
	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", threadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}
	
	// 设置User-Agent和Referer
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Referer", "https://www.91panta.cn/index")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	
	// 使用带重试的请求方法发送HTTP请求
	resp, err := p.doRequestWithRetry(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("请求帖子详情页失败，状态码: %d", resp.StatusCode)
	}
	
	// 使用goquery解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %v", err)
	}
	
	// 提取标题
	title := strings.TrimSpace(doc.Find("div.title").Text())
	
	// 从标题中提取年份作为可能的提取码
	var yearFromTitle string
	yearMatch := yearRegex.FindStringSubmatch(title)
	if len(yearMatch) >= 2 {
		yearFromTitle = yearMatch[1]
	}
	
	// 提取帖子内容区域
	var links []model.Link
	var foundURLs = make(map[string]bool) // 用于去重
	
	// 找到帖子内容区域
	doc.Find("div.topicContent").Each(func(i int, content *goquery.Selection) {
		// 提取所有链接
		content.Find("a[href^='http']").Each(func(i int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			if !exists {
				return
			}
			
			// 检查是否为网盘链接
			if isNetDiskLink(href) {
				// 如果链接已存在，跳过
				if foundURLs[href] {
					return
				}
				foundURLs[href] = true
				
				// 获取周围文本，用于检查提取码相关信息
				surroundingText := a.Text()
				if surroundingText == "" {
					surroundingText = a.Parent().Text()
				}
				if surroundingText == "" {
					surroundingText = content.Text()
				}
				
				// 确定链接类型
				linkType := determineLinkType(href)
				
				// 提取密码
				password := extractPassword(surroundingText, href)
				
				// 根据链接类型进行特殊处理
				switch linkType {
				case "quark":
					// 夸克网盘链接，只有在明确需要提取码的情况下才添加
					if password != "" {
						// 检查周围文本是否包含提取码相关关键词
						hasPasswordHint := false
						for _, keyword := range pwdKeywords {
							if strings.Contains(surroundingText, keyword) {
								hasPasswordHint = true
								break
							}
						}
						
						// 如果没有提取码相关关键词，则不添加提取码
						if !hasPasswordHint {
							password = ""
						}
					}
				case "mobile":
					// 移动云盘链接，只有在明确指定提取码的情况下才使用
					// 检查是否明确包含提取码信息
					hasExplicitPassword := false
					for _, pattern := range pwdPatterns {
						if matches := pattern.FindStringSubmatch(surroundingText); len(matches) >= 2 {
							// 使用明确指定的提取码
							password = matches[1]
							hasExplicitPassword = true
							break
						}
					}
					
					// 如果没有明确的提取码信息，则不使用提取码
					if !hasExplicitPassword && !strings.Contains(href, "pwd=") {
						password = ""
					}
				}
				
				// 添加链接
				links = append(links, model.Link{
					Type:     linkType,
					URL:      href,
					Password: password,
				})
			}
		})
		
		// 尝试从文本中提取可能的网盘链接
		htmlContent, _ := content.Html()
		textLinks := extractTextLinks(htmlContent, yearFromTitle)
		for _, link := range textLinks {
			if !foundURLs[link.URL] {
				foundURLs[link.URL] = true
				links = append(links, link)
			}
		}
	})
	
	// 缓存结果
	threadLinksCache.Store(topicID, links)
	
	return links, nil
}

// extractTextLinks 从文本中提取网盘链接
func extractTextLinks(text string, yearFromTitle string) []model.Link {
	// 存储最终的链接结果
	var links []model.Link
	
	// 预处理：检查文本是否包含网盘域名和提取码关键词，快速过滤
	hasNetDiskDomain := false
	hasPasswordKeyword := false
	
	// 一次性检查所有域名和关键词
	for _, domain := range netDiskDomains {
		if strings.Contains(text, domain) {
			hasNetDiskDomain = true
			break
		}
	}
	
	// 如果文本中不包含任何网盘域名，直接返回空结果
	if !hasNetDiskDomain {
		return links
	}
	
	// 检查是否包含提取码关键词
	for _, keyword := range pwdKeywords {
		if strings.Contains(text, keyword) {
			hasPasswordKeyword = true
			break
		}
	}
	
	// 按顺序存储所有找到的网盘链接和提取码
	type linkInfo struct {
		url      string // 完整URL
		baseURL  string // 不含提取码的基本URL
		position int    // 在文本中的位置
		endPos   int    // 链接结束位置
		linkType string // 链接类型
		password string // 从URL中提取的提取码
	}
	
	type passwordInfo struct {
		password string // 提取码
		position int    // 在文本中的位置
		endPos   int    // 提取码结束位置
	}
	
	// 合并所有网盘链接正则表达式为一个大正则，减少多次扫描
	// 由于Go不支持直接合并正则，我们仍然需要多次扫描，但可以优化处理逻辑
	
	// 1. 批量提取所有网盘链接和提取码
	var foundLinks []linkInfo
	var foundPasswords []passwordInfo
	
	// 提取所有网盘链接 - 使用并发处理加速
	var wg sync.WaitGroup
	var linksMutex sync.Mutex
	
	// 限制并发数量
	semaphore := make(chan struct{}, 5) // 最多5个并发
	
	for _, pattern := range netDiskPatterns {
		wg.Add(1)
		go func(pattern *regexp.Regexp) {
			defer wg.Done()
			
			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			
			matches := pattern.FindAllStringIndex(text, -1)
			if len(matches) > 0 {
				linksMutex.Lock()
				defer linksMutex.Unlock()
				
				for _, match := range matches {
					if match != nil && len(match) == 2 {
						url := text[match[0]:match[1]]
						
						// 提取基本URL和提取码
						baseURL := url
						password := ""
						
						// 检查链接本身是否包含密码参数
						pwdMatch := pwdParamRegex.FindStringSubmatch(url)
						if len(pwdMatch) >= 2 {
							password = pwdMatch[1]
							// 移除URL中的密码参数，获取基本链接
							if strings.Contains(url, "?pwd=") {
								baseURL = url[:strings.Index(url, "?pwd=")]
							} else if strings.Contains(url, "&pwd=") {
								baseURL = url[:strings.Index(url, "&pwd=")]
							}
						}
						
						// 移除URL末尾的特殊字符（如#）
						baseURL = strings.TrimRight(baseURL, "#")
						
						// 确定链接类型
						linkType := determineLinkType(baseURL)
						
						// 添加到链接列表
						foundLinks = append(foundLinks, linkInfo{
							url:      url,
							baseURL:  baseURL,
							position: match[0],
							endPos:   match[1],
							linkType: linkType,
							password: password,
						})
					}
				}
			}
		}(pattern)
	}
	
	// 并发提取所有提取码
	if hasPasswordKeyword {
		for _, pattern := range pwdPatterns {
			wg.Add(1)
			go func(pattern *regexp.Regexp) {
				defer wg.Done()
				
				// 获取信号量
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				
				matches := pattern.FindAllSubmatchIndex([]byte(text), -1)
				if len(matches) > 0 {
					linksMutex.Lock()
					defer linksMutex.Unlock()
					
					for _, match := range matches {
						if match != nil && len(match) >= 4 {
							// match[2]和match[3]是第一个捕获组的开始和结束位置
							password := text[match[2]:match[3]]
							position := match[0] // 整个匹配的开始位置
							
							foundPasswords = append(foundPasswords, passwordInfo{
								password: password,
								position: position,
								endPos:   match[1],
							})
						}
					}
				}
			}(pattern)
		}
	}
	
	// 等待所有并发任务完成
	wg.Wait()
	
	// 如果没有找到链接，直接返回空结果
	if len(foundLinks) == 0 {
		return links
	}
	
	// 2. 按照在文本中的位置排序链接和提取码
	sort.Slice(foundLinks, func(i, j int) bool {
		return foundLinks[i].position < foundLinks[j].position
	})
	
	if len(foundPasswords) > 0 {
		sort.Slice(foundPasswords, func(i, j int) bool {
			return foundPasswords[i].position < foundPasswords[j].position
		})
	}
	
	// 3. 优化的关联算法：为每个链接找到最合适的提取码
	// 使用映射存储已处理的链接，避免重复
	processedLinks := make(map[string]bool)
	
	// 为每个链接创建一个提取码候选列表
	type pwdCandidate struct {
		password string
		distance int
		score    int // 分数越高，越可能是正确的提取码
	}
	
	// 创建链接与提取码的关联映射
	linkPasswordMap := make(map[int][]pwdCandidate)
	
	// 预处理：为每个链接计算所有可能的提取码候选
	if len(foundPasswords) > 0 {
		for i, link := range foundLinks {
			var candidates []pwdCandidate
			
			// 链接周围文本的范围（用于检查提取码相关关键词）
			contextStart := link.position - 50
			if contextStart < 0 {
				contextStart = 0
			}
			contextEnd := link.endPos + 100
			if contextEnd > len(text) {
				contextEnd = len(text)
			}
			surroundingText := text[contextStart:contextEnd]
			
			// 检查周围文本是否包含提取码相关关键词
			hasPasswordHint := false
			for _, keyword := range pwdKeywords {
				if strings.Contains(surroundingText, keyword) {
					hasPasswordHint = true
					break
				}
			}
			
			// 如果周围文本没有提取码相关关键词，且不是必须需要提取码的链接类型，则跳过
			if !hasPasswordHint && link.linkType != "baidu" && link.linkType != "xunlei" {
				continue
			}
			
			// 为当前链接收集所有可能的提取码候选
			for _, pwd := range foundPasswords {
				// 只考虑链接后面的提取码
				if pwd.position > link.endPos {
					// 计算距离
					distance := pwd.position - link.endPos
					
					// 基础分数 - 距离越近分数越高
					score := 1000 - distance
					
					// 检查这个提取码是否应该关联到当前链接
					// 如果提取码在下一个链接之后，则不应该关联到当前链接
					for j := i + 1; j < len(foundLinks); j++ {
						if pwd.position > foundLinks[j].position {
							score -= 500 // 大幅降低分数
							break
						}
					}
					
					// 检查提取码是否紧跟在链接后面（50个字符内）
					if distance < 50 {
						score += 300 // 提高分数
					}
					
					// 检查提取码周围是否有关联关键词
					pwdContextStart := pwd.position - 20
					if pwdContextStart < 0 {
						pwdContextStart = 0
					}
					pwdContextEnd := pwd.endPos + 20
					if pwdContextEnd > len(text) {
						pwdContextEnd = len(text)
					}
					pwdSurroundingText := text[pwdContextStart:pwdContextEnd]
					
					for _, keyword := range pwdKeywords {
						if strings.Contains(pwdSurroundingText, keyword) {
							score += 200 // 提高分数
							break
						}
					}
					
					// 添加到候选列表
					candidates = append(candidates, pwdCandidate{
						password: pwd.password,
						distance: distance,
						score:    score,
					})
				}
			}
			
			// 如果有候选提取码，存储到映射中
			if len(candidates) > 0 {
				// 按分数排序（从高到低）
				sort.Slice(candidates, func(i, j int) bool {
					return candidates[i].score > candidates[j].score
				})
				
				linkPasswordMap[i] = candidates
			}
		}
	}
	
	// 4. 批量处理所有链接，生成最终结果
	for i, link := range foundLinks {
		// 如果链接已经处理过，跳过
		if processedLinks[link.baseURL] {
			continue
		}
		processedLinks[link.baseURL] = true
		
		// 如果链接已经有提取码，直接使用
		if link.password != "" {
			links = append(links, model.Link{
				Type:     link.linkType,
				URL:      link.url,
				Password: link.password,
			})
			continue
		}
		
		// 变量用于存储最终的提取码和URL
		var finalPassword string
		var finalURL string = link.baseURL
		
		// 从映射中获取候选提取码
		if candidates, ok := linkPasswordMap[i]; ok && len(candidates) > 0 {
			finalPassword = candidates[0].password
		}
		
		// 特殊处理不同类型的网盘链接
		switch link.linkType {
		case "mobile":
			// 移动云盘链接：不自动在URL中添加提取码
			if strings.Contains(link.url, "pwd=") {
				// 如果链接本身已经包含提取码参数，则保留原始URL
				finalURL = link.url
			} else if finalPassword != "" {
				// 只有在原始HTML明确包含带提取码的链接时才使用带提取码的URL
				// 这里我们不自动构建带提取码的URL
				finalURL = link.baseURL
			}
		case "baidu", "xunlei":
			// 百度网盘和迅雷网盘：支持在URL中包含提取码
			if finalPassword != "" {
				if strings.Contains(finalURL, "?") {
					finalURL = finalURL + "&pwd=" + finalPassword
				} else {
					finalURL = finalURL + "?pwd=" + finalPassword
				}
			}
		}
		
		// 添加处理后的链接
		links = append(links, model.Link{
			Type:     link.linkType,
			URL:      finalURL,
			Password: finalPassword,
		})
	}
	
	return links
}

// extractPassword 从文本中提取密码
func extractPassword(content string, url string) string {
	// 创建缓存键
	key := passwordCacheKey{
		content: content,
		url:     url,
	}
	
	// 检查缓存中是否已有结果
	if result, ok := extractPasswordCache.Load(key); ok {
		return result.(string)
	}
	
	// 如果URL已经包含密码参数，直接提取
	pwdMatch := pwdParamRegex.FindStringSubmatch(url)
	if len(pwdMatch) >= 2 {
		// 缓存结果
		extractPasswordCache.Store(key, pwdMatch[1])
		return pwdMatch[1]
	}
	
	// 只有当内容中可能包含提取码时才进行提取
	hasPasswordKeyword := false
	for _, keyword := range pwdKeywords {
		if strings.Contains(content, keyword) {
			hasPasswordKeyword = true
			break
		}
	}
	
	// 如果内容中没有提取码相关关键词，直接返回空
	if !hasPasswordKeyword {
		// 缓存结果
		extractPasswordCache.Store(key, "")
		return ""
	}
	
	// 尝试从文本中提取密码
	for _, pattern := range pwdPatterns {
		matches := pattern.FindStringSubmatch(content)
		
		if len(matches) >= 2 {
			// 缓存结果
			extractPasswordCache.Store(key, matches[1])
			return matches[1]
		}
	}
	
	// 缓存结果
	extractPasswordCache.Store(key, "")
	return ""
}

// determineLinkType 根据URL确定链接类型
func determineLinkType(url string) string {
	// 检查缓存中是否已有结果
	if result, ok := determineLinkTypeCache.Load(url); ok {
		return result.(string)
	}
	
	lowerURL := strings.ToLower(url)
	var linkType string
	
	switch {
	case strings.Contains(lowerURL, "pan.baidu.com"):
		linkType = "baidu"
	case strings.Contains(lowerURL, "pan.quark.cn"):
		linkType = "quark"
	case strings.Contains(lowerURL, "alipan.com") || strings.Contains(lowerURL, "aliyundrive.com"):
		linkType = "aliyun"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		linkType = "tianyi"
	case strings.Contains(lowerURL, "caiyun.139.com"):
		linkType = "mobile"  // 修改为mobile而不是caiyun
	case strings.Contains(lowerURL, "115.com"):
		linkType = "115"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		linkType = "xunlei"
	case strings.Contains(lowerURL, "mypikpak.com"):
		linkType = "pikpak"
	case strings.Contains(lowerURL, "123"):
		linkType = "123"
	default:
		linkType = "others"
	}
	
	// 缓存结果
	determineLinkTypeCache.Store(url, linkType)
	return linkType
}

// isNetDiskLink 检查链接是否为网盘链接
func isNetDiskLink(url string) bool {
	// 检查缓存中是否已有结果
	if result, ok := isNetDiskLinkCache.Load(url); ok {
		return result.(bool)
	}
	
	lowerURL := strings.ToLower(url)
	
	// 使用预定义的网盘域名列表进行快速检查
	for _, domain := range netDiskDomains {
		if strings.Contains(lowerURL, domain) {
			// 缓存结果
			isNetDiskLinkCache.Store(url, true)
			return true
		}
	}
	
	// 缓存结果
	isNetDiskLinkCache.Store(url, false)
	return false
} 

// startConcurrencyAdjuster 启动一个定期调整并发数的goroutine
func (p *PantaPlugin) startConcurrencyAdjuster() {
	ticker := time.NewTicker(concurrencyAdjustInterval * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		p.adjustConcurrency()
	}
}

// adjustConcurrency 根据响应时间调整并发数
func (p *PantaPlugin) adjustConcurrency() {
	p.responseTimesMutex.Lock()
	defer p.responseTimesMutex.Unlock()
	
	// 如果没有足够的响应时间样本，则不调整
	if len(p.responseTimes) < 5 {
		return
	}
	
	// 计算平均响应时间
	var totalTime time.Duration
	for _, t := range p.responseTimes {
		totalTime += t
	}
	avgTime := totalTime / time.Duration(len(p.responseTimes))
	
	// 根据平均响应时间调整并发数
	if avgTime > responseTimeThreshold*time.Millisecond {
		// 响应时间过长，减少并发数
		p.currentConcurrency = max(p.currentConcurrency-concurrencyStep, minConcurrency)
	} else {
		// 响应时间正常，尝试增加并发数
		p.currentConcurrency = min(p.currentConcurrency+concurrencyStep, maxConcurrency)
	}
	
	// 清空响应时间样本
	p.responseTimes = p.responseTimes[:0]
}

// recordResponseTime 记录请求响应时间
func (p *PantaPlugin) recordResponseTime(d time.Duration) {
	p.responseTimesMutex.Lock()
	defer p.responseTimesMutex.Unlock()
	
	// 限制样本数量
	if len(p.responseTimes) >= 100 {
		// 移除最早的样本
		p.responseTimes = p.responseTimes[1:]
	}
	
	p.responseTimes = append(p.responseTimes, d)
}

// doRequestWithRetry 发送HTTP请求，带重试机制
func (p *PantaPlugin) doRequestWithRetry(req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	var startTime time.Time
	
	// 重试循环
	for retry := 0; retry <= maxRetries; retry++ {
		// 如果不是第一次尝试，则等待一段时间
		if retry > 0 {
			// 使用指数退避算法计算等待时间
			backoffTime := time.Duration(min(backoffBase*1<<uint(retry-1), maxBackoff)) * time.Millisecond
			time.Sleep(backoffTime)
			
			// 创建新的请求，因为原请求可能已经被关闭
			newReq := req.Clone(req.Context())
			req = newReq
		}
		
		// 记录开始时间
		startTime = time.Now()
		
		// 发送请求
		resp, err = p.client.Do(req)
		
		// 记录响应时间
		responseTime := time.Since(startTime)
		p.recordResponseTime(responseTime)
		
		// 如果请求成功，或者是不可重试的错误，则退出重试循环
		if err == nil && resp.StatusCode < 500 {
			break
		}
		
		// 如果请求失败，但响应不为nil，则关闭响应体
		if resp != nil {
			resp.Body.Close()
		}
	}
	
	return resp, err
}

// max 返回两个整数中的较大值
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
} 