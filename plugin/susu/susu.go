package susu

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 缓存相关变量
var (
	// 帖子ID缓存
	postIDCache = sync.Map{}

	// 按钮列表缓存
	buttonListCache = sync.Map{}

	// 按钮详情缓存
	buttonDetailCache = sync.Map{}

	// JWT解析结果缓存
	jwtDecodeCache = sync.Map{}

	// 链接类型判断缓存
	linkTypeCache = sync.Map{}
)

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) Gecko/20100101 Firefox/120.0",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
}

// 初始化随机数种子
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewSusuAsyncPlugin())

	// 启动缓存清理
	go startCacheCleaner()

	// 初始化随机数种子
	rand.Seed(time.Now().UnixNano())
}

// startCacheCleaner 定期清理缓存
func startCacheCleaner() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		// 清空所有缓存
		postIDCache = sync.Map{}
		buttonListCache = sync.Map{}
		buttonDetailCache = sync.Map{}
		jwtDecodeCache = sync.Map{}
		linkTypeCache = sync.Map{}
	}
}

// getRandomUA 获取随机UA
func getRandomUA() string {
	return userAgents[rand.Intn(len(userAgents))]
}

const (
	BaseURL = "https://susuifa.com"
	// 搜索API
	SearchURL = BaseURL + "/?type=post&s=%s"
	// 获取网盘按钮列表API
	ButtonListURL = BaseURL + "/?rest_route=/b2/v1/getDownloadData"
	// 获取网盘详情API
	ButtonDetailURL = BaseURL + "/?rest_route=/b2/v1/getDownloadPageData"
	// 最大重试次数
	MaxRetries = 1
	// 最大并发数
	MaxConcurrency = 4
)

type downloadButton struct {
	Name string `json:"name"`
	Link string `json:"link"`
	URL  string `json:"url"`
	Attr struct {
		TQ string `json:"tq"`
		JY string `json:"jy"`
	} `json:"attr"`
}

type downloadGroup struct {
	Button []downloadButton `json:"button"`
}

type buttonDetailResponse struct {
	Button downloadButton `json:"button"`
}

// SusuAsyncPlugin SuSu网站搜索异步插件
type SusuAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewSusuAsyncPlugin 创建新的SuSu搜索异步插件
func NewSusuAsyncPlugin() *SusuAsyncPlugin {
	return &SusuAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("susu", 1), // 高优先级
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *SusuAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *SusuAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *SusuAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf(SearchURL, url.QueryEscape(keyword))

	// 发送请求
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", getRandomUA())
	setBrowserHeaders(req, BaseURL+"/")

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[susu] 搜索请求返回状态码: %d", resp.StatusCode)
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	var wg sync.WaitGroup
	resultChan := make(chan model.SearchResult, 20)

	// 创建信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrency)

	// 预先收集所有需要处理的项
	var items []*goquery.Selection

	// 将关键词转为小写，用于不区分大小写的比较
	lowerKeyword := strings.ToLower(keyword)

	// 将关键词按空格分割，用于支持多关键词搜索
	keywords := strings.Fields(lowerKeyword)

	// 预先过滤不包含关键词的帖子
	doc.Find(".post-list-item").Each(func(i int, s *goquery.Selection) {
		// 提取标题
		title := s.Find(".post-info h2 a").Text()
		title = strings.TrimSpace(title)
		lowerTitle := strings.ToLower(title)

		// 提取内容描述
		content := s.Find(".post-excerpt").Text()
		content = strings.TrimSpace(content)
		// lowerContent := strings.ToLower(content)

		// 检查每个关键词是否在标题或内容中
		matched := true
		for _, kw := range keywords {
			// 对于所有关键词，检查是否在标题或内容中
			if !strings.Contains(lowerTitle, kw) {
				matched = false
				break
			}
		}

		// 只添加匹配的帖子
		if matched {
			items = append(items, s)
		}
	})

	// 并发处理每个搜索结果项
	for _, s := range items {
		wg.Add(1)

		go func(s *goquery.Selection) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 提取帖子ID
			postID := p.extractPostID(s)
			if postID == "" {
				return
			}

			// 提取标题
			title := s.Find(".post-info h2 a").Text()
			title = strings.TrimSpace(title)

			// 提取内容描述
			content := s.Find(".post-excerpt").Text()
			content = strings.TrimSpace(content)

			// 提取日期时间
			datetimeStr := s.Find(".list-footer time.b2timeago").AttrOr("datetime", "")
			var datetime time.Time
			if datetimeStr != "" {
				parsedTime, err := time.Parse("2006-01-02 15:04:05", datetimeStr)
				if err == nil {
					datetime = parsedTime
				}
			}

			// 提取分类标签
			var tags []string
			s.Find(".post-list-cat-item").Each(func(i int, t *goquery.Selection) {
				tag := strings.TrimSpace(t.Text())
				if tag != "" {
					tags = append(tags, tag)
				}
			})

			// 获取网盘链接
			links, err := p.getLinks(client, postID)
			if err != nil || len(links) == 0 {
				return
			}

			// 创建搜索结果
			result := model.SearchResult{
				UniqueID: fmt.Sprintf("susu-%s", postID),
				Channel:  "",
				Title:    title,
				Content:  content,
				Datetime: datetime,
				Links:    links,
				Tags:     tags,
			}

			resultChan <- result
		}(s)
	}

	// 等待所有goroutine完成
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// 收集结果
	var results []model.SearchResult
	for result := range resultChan {
		results = append(results, result)
	}

	// 由于我们已经在前面过滤了不匹配的帖子，这里不需要再次过滤
	return results, nil
}

// extractPostID 从搜索结果项中提取帖子ID
func (p *SusuAsyncPlugin) extractPostID(s *goquery.Selection) string {
	// 生成缓存键
	html, _ := s.Html()
	cacheKey := fmt.Sprintf("postid:%x", md5sum(html))

	// 检查缓存
	if cachedID, ok := postIDCache.Load(cacheKey); ok {
		return cachedID.(string)
	}

	// 方法1：从列表项ID属性提取
	itemID, exists := s.Attr("id")
	if exists && strings.HasPrefix(itemID, "item-") {
		postID := strings.TrimPrefix(itemID, "item-")
		postIDCache.Store(cacheKey, postID)
		return postID
	}

	// 方法2：从详情页链接提取
	href, exists := s.Find(".post-info h2 a").Attr("href")
	if exists {
		re := regexp.MustCompile(`/(\d+)\.html`)
		matches := re.FindStringSubmatch(href)
		if len(matches) > 1 {
			postID := matches[1]
			postIDCache.Store(cacheKey, postID)
			return postID
		}
	}

	return ""
}

// getLinks 获取网盘链接
func (p *SusuAsyncPlugin) getLinks(client *http.Client, postID string) ([]model.Link, error) {
	// 检查缓存
	if cachedLinks, ok := buttonListCache.Load(postID); ok {
		return cachedLinks.([]model.Link), nil
	}

	form := url.Values{
		"post_id": {postID},
		"guest":   {""},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ButtonListURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("[susu] 创建按钮列表请求失败: %w", err)
	}
	setAPIHeaders(req, fmt.Sprintf("%s/%s.html", BaseURL, postID))

	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return nil, fmt.Errorf("[susu] 获取按钮列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[susu] 按钮列表请求返回状态码: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("[susu] 读取按钮列表失败: %w", err)
	}

	var groups []downloadGroup
	if err := json.Unmarshal(respBody, &groups); err != nil {
		return nil, fmt.Errorf("[susu] 解析按钮列表失败: %w", err)
	}

	totalButtons := 0
	for _, group := range groups {
		totalButtons += len(group.Button)
	}
	if totalButtons == 0 {
		return nil, fmt.Errorf("[susu] 帖子 %s 没有可用下载按钮", postID)
	}

	linkChan := make(chan model.Link, totalButtons)
	var wgLinks sync.WaitGroup
	semaphore := make(chan struct{}, MaxConcurrency)

	for groupIndex, group := range groups {
		for buttonIndex := range group.Button {
			wgLinks.Add(1)
			go func(index, i int) {
				defer wgLinks.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()

				link, err := p.getButtonDetail(client, postID, index, i)
				if err == nil && link.URL != "" {
					linkChan <- link
				}
			}(groupIndex, buttonIndex)
		}
	}

	// 等待所有goroutine完成
	go func() {
		wgLinks.Wait()
		close(linkChan)
	}()

	// 收集结果
	links := make([]model.Link, 0, totalButtons)
	seen := make(map[string]struct{}, totalButtons)
	for link := range linkChan {
		key := link.Type + "\x00" + link.URL + "\x00" + link.Password
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		links = append(links, link)
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("[susu] 帖子 %s 的下载按钮均未返回有效链接", postID)
	}

	// 缓存结果
	buttonListCache.Store(postID, links)

	return links, nil
}

// getButtonDetail 获取按钮详情
func (p *SusuAsyncPlugin) getButtonDetail(client *http.Client, postID string, index, i int) (model.Link, error) {
	// 生成缓存键
	cacheKey := fmt.Sprintf("%s:%d:%d", postID, index, i)

	// 检查缓存
	if cachedLink, ok := buttonDetailCache.Load(cacheKey); ok {
		return cachedLink.(model.Link), nil
	}

	form := url.Values{
		"post_id": {postID},
		"index":   {fmt.Sprintf("%d", index)},
		"i":       {fmt.Sprintf("%d", i)},
		"guest":   {""},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ButtonDetailURL, strings.NewReader(form.Encode()))
	if err != nil {
		return model.Link{}, fmt.Errorf("[susu] 创建按钮详情请求失败: %w", err)
	}
	setAPIHeaders(req, fmt.Sprintf("%s/download?post_id=%s&index=%d&i=%d", BaseURL, postID, index, i))

	// 发送请求（带重试）
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return model.Link{}, fmt.Errorf("[susu] 获取按钮详情失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return model.Link{}, fmt.Errorf("[susu] 按钮详情请求返回状态码: %d", resp.StatusCode)
	}

	// 读取响应体
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return model.Link{}, fmt.Errorf("[susu] 读取按钮详情失败: %w", err)
	}

	// 解析响应
	var buttonDetail buttonDetailResponse
	if err := json.Unmarshal(respBody, &buttonDetail); err != nil {
		return model.Link{}, fmt.Errorf("[susu] 解析按钮详情失败: %w", err)
	}

	// 如果URL为空
	if buttonDetail.Button.URL == "" {
		return model.Link{}, fmt.Errorf("[susu] 按钮URL为空")
	}

	// 解析JWT token获取真实链接
	realURL, err := p.decodeJWTURL(buttonDetail.Button.URL)
	if err != nil {
		return model.Link{}, fmt.Errorf("[susu] 解析JWT失败: %w", err)
	}
	realURL = strings.TrimSpace(realURL)
	parsedURL, err := url.Parse(realURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return model.Link{}, fmt.Errorf("[susu] 按钮返回无效链接")
	}

	// 创建链接
	link := model.Link{
		URL:      realURL,
		Type:     p.determineLinkType(realURL, buttonDetail.Button.Name),
		Password: extractPassword(realURL, buttonDetail.Button.Attr.TQ),
	}

	// 缓存结果
	buttonDetailCache.Store(cacheKey, link)

	return link, nil
}

// decodeJWTURL 解析JWT token获取真实链接
func (p *SusuAsyncPlugin) decodeJWTURL(jwtToken string) (string, error) {
	if strings.HasPrefix(jwtToken, "http://") || strings.HasPrefix(jwtToken, "https://") {
		return jwtToken, nil
	}

	// 检查缓存
	if cachedURL, ok := jwtDecodeCache.Load(jwtToken); ok {
		return cachedURL.(string), nil
	}

	// 分割JWT
	parts := strings.Split(jwtToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("无效的JWT格式")
	}

	// 解码Payload
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("解码Payload失败: %w", err)
	}

	// 解析JSON
	var payloadData struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &payloadData); err != nil {
		return "", fmt.Errorf("解析Payload JSON失败: %w", err)
	}
	if strings.TrimSpace(payloadData.Data.URL) == "" {
		return "", fmt.Errorf("JWT Payload中没有链接")
	}

	// 缓存结果
	jwtDecodeCache.Store(jwtToken, payloadData.Data.URL)

	return payloadData.Data.URL, nil
}

func setBrowserHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", getRandomUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", referer)
}

func setAPIHeaders(req *http.Request, referer string) {
	setBrowserHeaders(req, referer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	req.Header.Set("Origin", BaseURL)
}

func extractPassword(rawURL, fallback string) string {
	parsedURL, err := url.Parse(rawURL)
	if err == nil {
		for _, key := range []string{"pwd", "password", "passcode", "code"} {
			if password := strings.TrimSpace(parsedURL.Query().Get(key)); password != "" {
				return password
			}
		}
	}
	return strings.TrimSpace(fallback)
}

// determineLinkType 根据URL和名称确定链接类型
func (p *SusuAsyncPlugin) determineLinkType(url, name string) string {
	// 生成缓存键
	cacheKey := fmt.Sprintf("%s:%s", url, name)

	// 检查缓存
	if cachedType, ok := linkTypeCache.Load(cacheKey); ok {
		return cachedType.(string)
	}

	lowerURL := strings.ToLower(url)
	lowerName := strings.ToLower(name)

	var linkType string

	// 根据URL判断
	switch {
	case strings.Contains(lowerURL, "pan.baidu.com"):
		linkType = "baidu"
	case strings.Contains(lowerURL, "alipan.com") || strings.Contains(lowerURL, "aliyundrive.com"):
		linkType = "aliyun"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		linkType = "xunlei"
	case strings.Contains(lowerURL, "pan.quark.cn"):
		linkType = "quark"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		linkType = "tianyi"
	case strings.Contains(lowerURL, "115.com"):
		linkType = "115"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		linkType = "uc"
	case strings.Contains(lowerURL, "caiyun.139.com"):
		linkType = "mobile"
	case strings.Contains(lowerURL, "123pan.com"):
		linkType = "123"
	case strings.Contains(lowerURL, "mypikpak.com"):
		linkType = "pikpak"
	default:
		// 根据名称判断
		switch {
		case strings.Contains(lowerName, "百度"):
			linkType = "baidu"
		case strings.Contains(lowerName, "阿里"):
			linkType = "aliyun"
		case strings.Contains(lowerName, "迅雷"):
			linkType = "xunlei"
		case strings.Contains(lowerName, "夸克"):
			linkType = "quark"
		case strings.Contains(lowerName, "天翼"):
			linkType = "tianyi"
		case strings.Contains(lowerName, "115"):
			linkType = "115"
		case strings.Contains(lowerName, "uc"):
			linkType = "uc"
		case strings.Contains(lowerName, "移动") || strings.Contains(lowerName, "彩云"):
			linkType = "mobile"
		case strings.Contains(lowerName, "123"):
			linkType = "123"
		case strings.Contains(lowerName, "pikpak"):
			linkType = "pikpak"
		default:
			linkType = "others"
		}
	}

	// 缓存结果
	linkTypeCache.Store(cacheKey, linkType)

	return linkType
}

// doRequestWithRetry 发送HTTP请求并支持重试
func (p *SusuAsyncPlugin) doRequestWithRetry(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i <= maxRetries; i++ {
		// 如果不是第一次尝试，等待一段时间
		if i > 0 {
			// 指数退避算法
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			time.Sleep(backoff)
		}

		// 克隆请求，避免重用同一个请求对象
		reqClone := req.Clone(req.Context())

		// 发送请求
		resp, err = client.Do(reqClone)

		// 如果请求成功或者是不可重试的错误，则退出循环
		if err == nil || !isRetriableError(err) {
			break
		}
	}

	return resp, err
}

// isRetriableError 判断错误是否可以重试
func isRetriableError(err error) bool {
	if err == nil {
		return false
	}

	// 判断是否是网络错误或超时错误
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout() || netErr.Temporary()
	}

	// 其他可能需要重试的错误类型
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "EOF")
}

// md5sum 计算字符串的MD5值的简化版本
func md5sum(s string) uint32 {
	h := uint32(0)
	for i := 0; i < len(s); i++ {
		h = h*31 + uint32(s[i])
	}
	return h
}
