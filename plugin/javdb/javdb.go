package javdb

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
)

const (
	PluginName     = "javdb"
	DisplayName    = "JavDB"
	Description    = "JavDB - 影片数据库，专门提供磁力链接搜索"
	BaseURL        = "https://javdb.com"
	SearchPath     = "/search?q=%s&f=all"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults     = 50
	MaxConcurrency = 10

	// 429限流重试配置
	MaxRetryOnRateLimit = 0 // 遇到429时的最大重试次数，设为0则不重试
	MinRetryDelay       = 4 // 最小延迟秒数
	MaxRetryDelay       = 8 // 最大延迟秒数
)

// JavdbPlugin JavDB插件
type JavdbPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode      bool
	detailCache    sync.Map // 缓存详情页结果
	cacheTTL       time.Duration
	rateLimited    int32 // 429限流标志位，使用atomic操作
	rateLimitCount int32 // 429错误计数
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewJavdbPlugin())
}

// NewJavdbPlugin 创建新的JavDB插件实例
func NewJavdbPlugin() *JavdbPlugin {
	debugMode := false

	// 初始化随机种子
	rand.Seed(time.Now().UnixNano())

	p := &JavdbPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter(PluginName, 5, true),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute, // 详情页缓存30分钟
	}

	return p
}

// Name 插件名称
func (p *JavdbPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *JavdbPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *JavdbPlugin) Description() string {
	return Description
}

// SkipServiceFilter 磁力搜索插件，跳过Service层过滤
func (p *JavdbPlugin) SkipServiceFilter() bool {
	return true // 磁力搜索，跳过网盘服务过滤
}

// Search 搜索接口（兼容性方法）
func (p *JavdbPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *JavdbPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 搜索实现
func (p *JavdbPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[JAVDB] 开始搜索: %s", keyword)
	}

	if p.debugMode {
		log.Printf("[JAVDB] 开始搜索，客户端超时: %v", client.Timeout)
	}

	// 第一步：执行搜索获取结果列表
	searchResults, err, isRateLimited := p.executeSearchWithRateLimit(client, keyword)
	if err != nil && !isRateLimited {
		return nil, fmt.Errorf("[%s] 执行搜索失败: %w", p.Name(), err)
	}

	if p.debugMode {
		if isRateLimited {
			log.Printf("[JAVDB] ⚡ 遇到429限流，但继续处理已获取的 %d 个结果", len(searchResults))
		} else {
			log.Printf("[JAVDB] 搜索获取到 %d 个结果", len(searchResults))
		}
	}

	// 如果没有搜索结果，直接返回
	if len(searchResults) == 0 {
		if p.debugMode {
			log.Printf("[JAVDB] 无搜索结果，直接返回")
		}
		return []model.SearchResult{}, nil
	}

	// 第二步：并发获取详情页磁力链接（设定合理超时）
	finalResults := p.fetchDetailMagnetLinks(client, searchResults, keyword)

	if p.debugMode {
		log.Printf("[JAVDB] 最终获取到 %d 个有效结果", len(finalResults))
		if isRateLimited {
			log.Printf("[JAVDB] ⚡ 由于429限流，结果可能不完整，系统将在后台继续获取")
		}
	}

	return finalResults, nil
}

// executeSearchWithRateLimit 执行搜索请求，支持限流检测
func (p *JavdbPlugin) executeSearchWithRateLimit(client *http.Client, keyword string) ([]model.SearchResult, error, bool) {
	// 重置限流状态，每次新搜索都重新尝试
	atomic.StoreInt32(&p.rateLimited, 0)

	// 构建搜索URL
	searchURL := fmt.Sprintf("%s%s", BaseURL, fmt.Sprintf(SearchPath, url.QueryEscape(keyword)))

	if p.debugMode {
		log.Printf("[JAVDB] 搜索URL: %s", searchURL)
		// 显示重试配置信息
		if MaxRetryOnRateLimit > 0 {
			log.Printf("[JAVDB] 429重试配置: 最大%d次，延迟%d-%d秒", MaxRetryOnRateLimit, MinRetryDelay, MaxRetryDelay)
		} else {
			log.Printf("[JAVDB] 429重试配置: 禁用重试")
		}
		// 如果之前有限流，显示统计信息
		if count := atomic.LoadInt32(&p.rateLimitCount); count > 0 {
			log.Printf("[JAVDB] 历史429限流次数: %d", count)
		}
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err), false
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", BaseURL+"/")

	if p.debugMode {
		log.Printf("[JAVDB] 发送搜索请求...")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err), false
	}
	defer resp.Body.Close()

	if p.debugMode {
		log.Printf("[JAVDB] 搜索请求响应状态: %d", resp.StatusCode)
	}

	// 检测429限流 - 立即返回，不延迟
	if resp.StatusCode == 429 {
		atomic.StoreInt32(&p.rateLimited, 1)
		atomic.AddInt32(&p.rateLimitCount, 1)
		if p.debugMode {
			log.Printf("[JAVDB] ⚡ 检测到429限流，立即返回空结果")
		}
		return []model.SearchResult{}, nil, true // 返回空结果和限流标志
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求HTTP状态错误: %d", p.Name(), resp.StatusCode), false
	}

	// 读取响应体用于调试
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取搜索结果失败: %w", p.Name(), err), false
	}

	if p.debugMode {
		bodyStr := string(bodyBytes)
		log.Printf("[JAVDB] 响应体长度: %d", len(bodyStr))
		// 输出前500个字符用于调试
		if len(bodyStr) > 500 {
			log.Printf("[JAVDB] 响应体前500字符: %s", bodyStr[:500])
		} else {
			log.Printf("[JAVDB] 完整响应体: %s", bodyStr)
		}
	}

	// 解析HTML提取搜索结果
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(bodyBytes)))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果HTML失败: %w", p.Name(), err), false
	}

	results, err := p.parseSearchResults(doc)
	return results, err, false
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *JavdbPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			// 指数退避重试
			backoff := time.Duration(1<<uint(i-1)) * 200 * time.Millisecond
			time.Sleep(backoff)
		}

		// 克隆请求避免并发问题
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

	return nil, fmt.Errorf("[%s] 重试 %d 次后仍然失败: %w", p.Name(), maxRetries, lastErr)
}

// doRequestWithRateLimitRetry 带429重试机制的HTTP请求
func (p *JavdbPlugin) doRequestWithRateLimitRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= MaxRetryOnRateLimit; attempt++ {
		if attempt > 0 {
			// 随机延迟，避免同时重试造成更大压力
			delaySeconds := rand.Intn(MaxRetryDelay-MinRetryDelay+1) + MinRetryDelay
			if p.debugMode {
				log.Printf("[JAVDB] 429重试 %d/%d，随机延迟 %d 秒", attempt, MaxRetryOnRateLimit, delaySeconds)
			}
			time.Sleep(time.Duration(delaySeconds) * time.Second)
		}

		// 克隆请求避免并发问题
		reqClone := req.Clone(req.Context())

		resp, err := client.Do(reqClone)
		if err != nil {
			lastErr = err
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		// 如果不是429，直接返回（无论成功还是其他错误）
		if resp.StatusCode != 429 {
			return resp, nil
		}

		// 遇到429
		atomic.AddInt32(&p.rateLimitCount, 1)
		if p.debugMode {
			log.Printf("[JAVDB] 遇到429限流，尝试 %d/%d", attempt+1, MaxRetryOnRateLimit+1)
		}

		// 如果不允许重试或已达到最大重试次数
		if MaxRetryOnRateLimit == 0 || attempt >= MaxRetryOnRateLimit {
			atomic.StoreInt32(&p.rateLimited, 1)
			resp.Body.Close()
			return nil, fmt.Errorf("[%s] 429限流，%s", p.Name(),
				func() string {
					if MaxRetryOnRateLimit == 0 {
						return "不重试"
					}
					return fmt.Sprintf("重试%d次后仍然限流", MaxRetryOnRateLimit)
				}())
		}

		resp.Body.Close()
		lastErr = fmt.Errorf("429 Too Many Requests")
	}

	return nil, lastErr
}

// parseSearchResults 解析搜索结果HTML
func (p *JavdbPlugin) parseSearchResults(doc *goquery.Document) ([]model.SearchResult, error) {
	var results []model.SearchResult

	if p.debugMode {
		// 检查是否找到了.movie-list元素
		movieListEl := doc.Find(".movie-list")
		log.Printf("[JAVDB] 找到.movie-list元素数量: %d", movieListEl.Length())

		// 检查是否找到了.item元素
		itemEls := doc.Find(".movie-list .item")
		log.Printf("[JAVDB] 找到.movie-list .item元素数量: %d", itemEls.Length())

		// 如果没有找到预期元素，尝试其他可能的选择器
		if itemEls.Length() == 0 {
			log.Printf("[JAVDB] 尝试查找其他可能的结果元素...")

			// 尝试其他可能的选择器
			altSelectors := []string{
				".movie-list > div",
				".movie-list div.item",
				"[class*='movie'] [class*='item']",
				".video-list .item",
				".search-results .item",
			}

			for _, selector := range altSelectors {
				altEls := doc.Find(selector)
				if altEls.Length() > 0 {
					log.Printf("[JAVDB] 找到替代选择器 '%s' 的元素数量: %d", selector, altEls.Length())
				}
			}

			// 输出页面的主要结构用于调试
			doc.Find("div[class*='movie'], div[class*='video'], div[class*='search'], div[class*='result']").Each(func(i int, s *goquery.Selection) {
				className, _ := s.Attr("class")
				log.Printf("[JAVDB] 找到可能相关的div元素: class='%s'", className)
			})
		}
	}

	// 查找搜索结果项: .movie-list .item
	doc.Find(".movie-list .item").Each(func(i int, s *goquery.Selection) {
		if len(results) >= MaxResults {
			return
		}

		if p.debugMode {
			log.Printf("[JAVDB] 开始解析第 %d 个结果项", i+1)
		}

		result := p.parseResultItem(s, i+1)
		if result != nil {
			results = append(results, *result)
			if p.debugMode {
				log.Printf("[JAVDB] 成功解析第 %d 个结果项: %s", i+1, result.Title)
			}
		} else if p.debugMode {
			log.Printf("[JAVDB] 第 %d 个结果项解析失败", i+1)
		}
	})

	if p.debugMode {
		log.Printf("[JAVDB] 解析到 %d 个原始结果", len(results))
	}

	return results, nil
}

// parseResultItem 解析单个搜索结果项
func (p *JavdbPlugin) parseResultItem(s *goquery.Selection, index int) *model.SearchResult {
	if p.debugMode {
		// 输出当前结果项的HTML结构用于调试
		itemHTML, _ := s.Html()
		if len(itemHTML) > 300 {
			log.Printf("[JAVDB] 结果项 %d HTML前300字符: %s", index, itemHTML[:300])
		} else {
			log.Printf("[JAVDB] 结果项 %d 完整HTML: %s", index, itemHTML)
		}
	}

	// 提取详情页链接
	linkEl := s.Find("a.box")
	if p.debugMode {
		log.Printf("[JAVDB] 结果项 %d 找到a.box元素数量: %d", index, linkEl.Length())

		// 如果没有找到a.box，尝试其他可能的链接选择器
		if linkEl.Length() == 0 {
			altLinkSelectors := []string{"a", "a[href*='/v/']", ".box", "[href*='/v/']"}
			for _, selector := range altLinkSelectors {
				altLinks := s.Find(selector)
				if altLinks.Length() > 0 {
					log.Printf("[JAVDB] 结果项 %d 找到替代链接选择器 '%s' 的元素数量: %d", index, selector, altLinks.Length())
				}
			}
		}
	}

	if linkEl.Length() == 0 {
		if p.debugMode {
			log.Printf("[JAVDB] 跳过无链接的结果")
		}
		return nil
	}

	detailURL, _ := linkEl.Attr("href")
	title, _ := linkEl.Attr("title")

	if p.debugMode {
		log.Printf("[JAVDB] 结果项 %d 详情页URL: %s", index, detailURL)
		log.Printf("[JAVDB] 结果项 %d 标题: %s", index, title)
	}

	if detailURL == "" || title == "" {
		if p.debugMode {
			log.Printf("[JAVDB] 跳过无效链接或标题的结果")
		}
		return nil
	}

	// 处理相对路径
	if strings.HasPrefix(detailURL, "/") {
		detailURL = BaseURL + detailURL
	}

	// 提取番号和标题
	videoNumber, _ := p.extractVideoInfo(s)

	// 提取评分
	rating := p.extractRating(s)

	// 提取发布日期
	releaseDate := p.extractReleaseDate(s)

	// 提取标签
	tags := p.extractTags(s)

	// 构建内容
	var contentParts []string
	if videoNumber != "" {
		contentParts = append(contentParts, fmt.Sprintf("番號：%s", videoNumber))
	}
	if rating != "" {
		contentParts = append(contentParts, fmt.Sprintf("評分：%s", rating))
	}
	if releaseDate != "" {
		contentParts = append(contentParts, fmt.Sprintf("發布日期：%s", releaseDate))
	}
	if len(tags) > 0 {
		contentParts = append(contentParts, fmt.Sprintf("標籤：%s", strings.Join(tags, " ")))
	}

	content := strings.Join(contentParts, "\n")

	// 解析时间
	datetime := p.parseTime(releaseDate)

	// 构建初始结果对象（磁力链接稍后获取）
	result := model.SearchResult{
		Title:     p.cleanTitle(title),
		Content:   content,
		Channel:   "", // 插件搜索结果必须为空字符串
		MessageID: fmt.Sprintf("%s-%d-%d", p.Name(), index, time.Now().Unix()),
		UniqueID:  fmt.Sprintf("%s-%d", p.Name(), index),
		Datetime:  datetime,
		Links:     []model.Link{}, // 先为空，详情页处理后添加
		Tags:      tags,
	}

	// 添加详情页URL到临时字段（用于后续处理）
	result.Content += fmt.Sprintf("\n详情页URL: %s", detailURL)

	if p.debugMode {
		log.Printf("[JAVDB] 解析结果: %s (%s)", title, videoNumber)
	}

	return &result
}

// extractVideoInfo 提取番号和标题信息
func (p *JavdbPlugin) extractVideoInfo(s *goquery.Selection) (videoNumber, videoTitle string) {
	videoTitleEl := s.Find(".video-title")
	if videoTitleEl.Length() > 0 {
		fullTitle := strings.TrimSpace(videoTitleEl.Text())

		// 提取番号 (在<strong>标签中)
		strongEl := videoTitleEl.Find("strong")
		if strongEl.Length() > 0 {
			videoNumber = strings.TrimSpace(strongEl.Text())
			// 从完整标题中移除番号，得到作品标题
			videoTitle = strings.TrimSpace(strings.Replace(fullTitle, videoNumber, "", 1))
		} else {
			videoTitle = fullTitle
		}
	}
	return videoNumber, videoTitle
}

// extractRating 提取评分
func (p *JavdbPlugin) extractRating(s *goquery.Selection) string {
	ratingEl := s.Find(".score .value")
	if ratingEl.Length() > 0 {
		rating := strings.TrimSpace(ratingEl.Text())
		// 清理评分文本，只保留主要信息
		rating = strings.ReplaceAll(rating, "\n", " ")
		rating = javdbRe1.ReplaceAllString(rating, " ")
		return rating
	}
	return ""
}

// extractReleaseDate 提取发布日期
func (p *JavdbPlugin) extractReleaseDate(s *goquery.Selection) string {
	metaEl := s.Find(".meta")
	if metaEl.Length() > 0 {
		date := strings.TrimSpace(metaEl.Text())
		return date
	}
	return ""
}

// extractTags 提取标签
func (p *JavdbPlugin) extractTags(s *goquery.Selection) []string {
	var tags []string
	s.Find(".tags .tag").Each(func(i int, tagEl *goquery.Selection) {
		tag := strings.TrimSpace(tagEl.Text())
		if tag != "" {
			tags = append(tags, tag)
		}
	})
	return tags
}

// cleanTitle 清理标题
func (p *JavdbPlugin) cleanTitle(title string) string {
	title = strings.TrimSpace(title)
	// 移除多余的空格
	title = javdbRe1.ReplaceAllString(title, " ")
	return title
}

// parseTime 解析时间字符串
func (p *JavdbPlugin) parseTime(dateStr string) time.Time {
	if dateStr == "" {
		return time.Now()
	}

	// 常见的日期格式
	layouts := []string{
		"2006-01-02",
		"2006/01/02",
		"01-02-2006",
		"01/02/2006",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, dateStr); err == nil {
			return t
		}
	}

	return time.Now()
}

// fetchDetailMagnetLinks 并发获取详情页磁力链接
func (p *JavdbPlugin) fetchDetailMagnetLinks(client *http.Client, searchResults []model.SearchResult, keyword string) []model.SearchResult {
	if len(searchResults) == 0 {
		if p.debugMode {
			log.Printf("[JAVDB] 无搜索结果需要获取详情页")
		}
		return []model.SearchResult{}
	}

	if p.debugMode {
		log.Printf("[JAVDB] 开始获取 %d 个搜索结果的详情页磁力链接", len(searchResults))
	}

	// 使用通道控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	resultsChan := make(chan []model.SearchResult, len(searchResults))

	// 根据客户端超时调整策略
	var finalResults []model.SearchResult
	useTimeout := client.Timeout <= 5*time.Second // 短超时客户端使用超时机制

	for i, result := range searchResults {
		// 检查是否已经被限流，如果是则停止启动新的goroutine
		if atomic.LoadInt32(&p.rateLimited) == 1 {
			if p.debugMode {
				log.Printf("[JAVDB] 检测到限流状态，停止启动新的详情页请求")
			}
			break
		}

		wg.Add(1)
		go func(r model.SearchResult, index int) {
			defer wg.Done()
			semaphore <- struct{}{}        // 获取信号量
			defer func() { <-semaphore }() // 释放信号量

			// 在goroutine内部再次检查限流状态
			if atomic.LoadInt32(&p.rateLimited) == 1 {
				if p.debugMode {
					log.Printf("[JAVDB] goroutine内检测到限流状态，跳过详情页请求: %s", r.Title)
				}
				return
			}

			if p.debugMode {
				log.Printf("[JAVDB] 开始处理第 %d 个搜索结果: %s", index+1, r.Title)
			}

			// 从Content中提取详情页URL
			detailURL := p.extractDetailURLFromContent(r.Content)
			if detailURL == "" {
				if p.debugMode {
					log.Printf("[JAVDB] 跳过无详情页URL的结果: %s", r.Title)
					log.Printf("[JAVDB] Content内容: %s", r.Content)
				}
				return
			}

			if p.debugMode {
				log.Printf("[JAVDB] 第 %d 个结果详情页URL: %s", index+1, detailURL)
			}

			// 获取详情页磁力链接
			magnetLinks := p.fetchDetailPageMagnetLinks(client, detailURL)
			if p.debugMode {
				log.Printf("[JAVDB] 第 %d 个结果获取到 %d 个磁力链接", index+1, len(magnetLinks))
			}

			if len(magnetLinks) > 0 {
				// 为每个磁力链接创建一个SearchResult
				var results []model.SearchResult
				for _, link := range magnetLinks {
					// 复制基础结果
					newResult := r
					// 清理Content中的详情页URL
					newResult.Content = p.cleanContent(r.Content)
					// 设置磁力链接
					newResult.Links = []model.Link{link}
					// 更新唯一ID - 基于磁力链接URL哈希确保一致性
					linkHash := fmt.Sprintf("%x", md5.Sum([]byte(link.URL)))[:8]
					newResult.UniqueID = fmt.Sprintf("%s-magnet-%s", newResult.UniqueID, linkHash)
					newResult.MessageID = newResult.UniqueID
					results = append(results, newResult)
				}
				resultsChan <- results
				if p.debugMode {
					log.Printf("[JAVDB] 第 %d 个结果成功创建 %d 个最终结果", index+1, len(results))
				}
			} else if p.debugMode {
				log.Printf("[JAVDB] 详情页无磁力链接: %s", r.Title)
			}
		}(result, i)
	}

	// 等待所有goroutine完成的信号
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(resultsChan)
		close(done)
	}()

	// 收集结果
	if useTimeout {
		// 短超时客户端：4秒超时机制，快速返回部分结果
		timeout := time.After(4 * time.Second)
	collectLoop:
		for {
			select {
			case results, ok := <-resultsChan:
				if !ok {
					break collectLoop
				}
				finalResults = append(finalResults, results...)
				if p.debugMode {
					log.Printf("[JAVDB] 收集到一批结果，数量: %d，总数: %d", len(results), len(finalResults))
				}
			case <-timeout:
				if p.debugMode {
					log.Printf("[JAVDB] ⏰ 4秒超时，返回已获取的 %d 个结果", len(finalResults))
				}
				break collectLoop
			case <-done:
				if p.debugMode {
					log.Printf("[JAVDB] 所有详情页请求完成")
				}
				break collectLoop
			}
		}
	} else {
		// 长超时客户端：等待所有结果完成
		for {
			select {
			case results, ok := <-resultsChan:
				if !ok {
					goto finished
				}
				finalResults = append(finalResults, results...)
				if p.debugMode {
					log.Printf("[JAVDB] 收集到一批结果，数量: %d，总数: %d", len(results), len(finalResults))
				}
			case <-done:
				if p.debugMode {
					log.Printf("[JAVDB] 所有详情页请求完成")
				}
				goto finished
			}
		}
	finished:
	}

	if p.debugMode {
		log.Printf("[JAVDB] 最终收集到 %d 个结果", len(finalResults))
		// 如果遇到了限流，提示用户
		if atomic.LoadInt32(&p.rateLimited) == 1 {
			log.Printf("[JAVDB] 本次搜索遇到429限流，结果可能不完整")
		}
	}

	return finalResults
}

// extractDetailURLFromContent 从Content中提取详情页URL
func (p *JavdbPlugin) extractDetailURLFromContent(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "详情页URL: ") {
			return strings.TrimPrefix(line, "详情页URL: ")
		}
	}
	return ""
}

// cleanContent 清理Content，移除详情页URL行
func (p *JavdbPlugin) cleanContent(content string) string {
	lines := strings.Split(content, "\n")
	var cleanedLines []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "详情页URL: ") {
			cleanedLines = append(cleanedLines, line)
		}
	}
	return strings.Join(cleanedLines, "\n")
}

// fetchDetailPageMagnetLinks 获取详情页的磁力链接
func (p *JavdbPlugin) fetchDetailPageMagnetLinks(client *http.Client, detailURL string) []model.Link {
	if p.debugMode {
		log.Printf("[JAVDB] 开始获取详情页磁力链接: %s", detailURL)
	}

	// 检查缓存
	if cached, found := p.detailCache.Load(detailURL); found {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[JAVDB] 使用缓存的详情页链接: %s", detailURL)
			}
			return links
		}
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		if p.debugMode {
			log.Printf("[JAVDB] 创建详情页请求失败: %v", err)
		}
		return []model.Link{}
	}

	// 设置请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", BaseURL+"/")

	if p.debugMode {
		log.Printf("[JAVDB] 发送详情页请求...")
	}

	resp, err := p.doRequestWithRateLimitRetry(req, client)
	if err != nil {
		if p.debugMode {
			log.Printf("[JAVDB] 详情页请求失败: %v", err)
		}
		return []model.Link{}
	}
	defer resp.Body.Close()

	if p.debugMode {
		log.Printf("[JAVDB] 详情页请求响应状态: %d", resp.StatusCode)
	}

	if resp.StatusCode != 200 {
		if p.debugMode {
			log.Printf("[JAVDB] 详情页HTTP状态错误: %d", resp.StatusCode)
		}
		return []model.Link{}
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if p.debugMode {
			log.Printf("[JAVDB] 读取详情页响应失败: %v", err)
		}
		return []model.Link{}
	}

	if p.debugMode {
		bodyStr := string(body)
		log.Printf("[JAVDB] 详情页响应体长度: %d", len(bodyStr))
		// 检查页面是否包含磁力链接相关内容
		if strings.Contains(bodyStr, "magnet:") {
			magnetCount := strings.Count(bodyStr, "magnet:")
			log.Printf("[JAVDB] 详情页包含 %d 个magnet字符串", magnetCount)
		} else {
			log.Printf("[JAVDB] 详情页不包含magnet字符串")
		}

		// 检查是否包含预期的磁力链接容器元素
		if strings.Contains(bodyStr, "magnets-content") {
			log.Printf("[JAVDB] 找到magnets-content容器")
		} else {
			log.Printf("[JAVDB] 未找到magnets-content容器")
		}

		if strings.Contains(bodyStr, "magnet-links") {
			log.Printf("[JAVDB] 找到magnet-links容器")
		} else {
			log.Printf("[JAVDB] 未找到magnet-links容器")
		}
	}

	// 解析磁力链接
	links := p.parseMagnetLinks(string(body))

	// 缓存结果
	if len(links) > 0 {
		p.detailCache.Store(detailURL, links)
	}

	if p.debugMode {
		log.Printf("[JAVDB] 从详情页提取到 %d 个磁力链接: %s", len(links), detailURL)
	}

	return links
}

// parseMagnetLinks 解析磁力链接
func (p *JavdbPlugin) parseMagnetLinks(htmlContent string) []model.Link {
	var links []model.Link

	if p.debugMode {
		log.Printf("[JAVDB] 开始解析磁力链接")
	}

	// 使用goquery解析HTML
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		if p.debugMode {
			log.Printf("[JAVDB] 解析详情页HTML失败: %v", err)
		}
		return links
	}

	if p.debugMode {
		// 检查关键容器元素
		magnetsContentEl := doc.Find("#magnets-content")
		log.Printf("[JAVDB] 找到#magnets-content元素数量: %d", magnetsContentEl.Length())

		magnetLinksEl := doc.Find("#magnets-content .magnet-links")
		log.Printf("[JAVDB] 找到#magnets-content .magnet-links元素数量: %d", magnetLinksEl.Length())

		magnetItemsEl := doc.Find("#magnets-content .magnet-links .item")
		log.Printf("[JAVDB] 找到#magnets-content .magnet-links .item元素数量: %d", magnetItemsEl.Length())

		// 如果没有找到预期元素，尝试其他可能的选择器
		if magnetItemsEl.Length() == 0 {
			log.Printf("[JAVDB] 尝试其他可能的磁力链接选择器...")

			altSelectors := []string{
				".magnet-links .item",
				"[href^='magnet:']",
				"a[href*='magnet:']",
				".item [href^='magnet:']",
			}

			for _, selector := range altSelectors {
				altEls := doc.Find(selector)
				if altEls.Length() > 0 {
					log.Printf("[JAVDB] 找到替代选择器 '%s' 的元素数量: %d", selector, altEls.Length())
				}
			}
		}
	}

	// 查找磁力链接区域: .magnet-links .item (因为#magnets-content本身就有magnet-links类)
	doc.Find(".magnet-links .item").Each(func(i int, s *goquery.Selection) {
		if p.debugMode {
			log.Printf("[JAVDB] 开始解析第 %d 个磁力链接项", i+1)
		}

		// 提取磁力链接URL - 从.magnet-name下的a标签获取href
		magnetEl := s.Find(".magnet-name a")
		if p.debugMode {
			log.Printf("[JAVDB] 第 %d 个项找到.magnet-name a元素数量: %d", i+1, magnetEl.Length())
		}

		if magnetEl.Length() == 0 {
			if p.debugMode {
				log.Printf("[JAVDB] 第 %d 个项无.magnet-name a元素，跳过", i+1)
			}
			return
		}

		magnetURL, _ := magnetEl.Attr("href")
		if magnetURL == "" {
			if p.debugMode {
				log.Printf("[JAVDB] 第 %d 个项磁力链接URL为空，跳过", i+1)
			}
			return
		}

		// 验证是否为磁力链接
		if !strings.HasPrefix(magnetURL, "magnet:") {
			if p.debugMode {
				log.Printf("[JAVDB] 第 %d 个项不是磁力链接: %s，跳过", i+1, magnetURL)
			}
			return
		}

		if p.debugMode {
			log.Printf("[JAVDB] 第 %d 个项原始磁力URL: %s", i+1, magnetURL)
		}

		// 解码HTML实体
		magnetURL = strings.ReplaceAll(magnetURL, "&amp;", "&")

		if p.debugMode {
			log.Printf("[JAVDB] 第 %d 个项解码后磁力URL: %s", i+1, magnetURL)
		}

		link := model.Link{
			Type:     "magnet",
			URL:      magnetURL,
			Password: "", // 磁力链接无需密码
		}

		links = append(links, link)

		if p.debugMode {
			// 提取资源名称用于调试日志
			nameEl := s.Find(".magnet-name .name")
			resourceName := strings.TrimSpace(nameEl.Text())
			// 提取文件信息用于调试日志
			metaEl := s.Find(".magnet-name .meta")
			fileInfo := strings.TrimSpace(metaEl.Text())
			log.Printf("[JAVDB] 成功提取第 %d 个磁力链接: %s (%s)", i+1, resourceName, fileInfo)
		}
	})

	if p.debugMode {
		log.Printf("[JAVDB] 磁力链接解析完成，共找到 %d 个链接", len(links))
	}

	return links
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	javdbRe1 = regexp.MustCompile(`\s+`)
)
