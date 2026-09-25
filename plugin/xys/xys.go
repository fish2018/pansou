package xys

import (
	"context"
	"encoding/base64"
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
	"pansou/util/json"
)

const (
	PluginName  = "xys"
	DisplayName = "小云搜索"
	Description = "小云搜索 - 阿里云盘、夸克网盘、百度网盘等多网盘搜索引擎"
	BaseURL     = "https://www.yunso.net"
	TokenPath   = "/index/user/s"
	SearchPath  = "/api/validate/searchX2"
	UserAgent   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxResults  = 50
)

// XysPlugin 小云搜索插件
type XysPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode  bool
	tokenCache sync.Map // 缓存token，避免频繁获取
	cacheTTL   time.Duration
}

// TokenCache token缓存结构
type TokenCache struct {
	Token     string
	Timestamp time.Time
}

// SearchResponse API响应结构
type SearchResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Time string `json:"time"`
	Data string `json:"data"`
}

// init 注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewXysPlugin())
}

// NewXysPlugin 创建新的小云搜索插件实例
func NewXysPlugin() *XysPlugin {
	// 检查调试模式
	debugMode := false // 生产环境关闭调试

	p := &XysPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin(PluginName, 3), // 标准网盘插件，启用Service层过滤
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute, // token缓存30分钟
	}

	return p
}

// Name 插件名称
func (p *XysPlugin) Name() string {
	return PluginName
}

// DisplayName 插件显示名称
func (p *XysPlugin) DisplayName() string {
	return DisplayName
}

// Description 插件描述
func (p *XysPlugin) Description() string {
	return Description
}

// Search 搜索接口
func (p *XysPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	return p.searchImpl(&http.Client{Timeout: 30 * time.Second}, keyword, ext)
}

// searchImpl 搜索实现
func (p *XysPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if p.debugMode {
		log.Printf("[XYS] 开始搜索: %s", keyword)
	}

	// 第一步：获取token
	token, err := p.getToken(client, keyword)
	if err != nil {
		return nil, fmt.Errorf("获取token失败: %w", err)
	}

	if p.debugMode {
		log.Printf("[XYS] 获取到token: %s", token[:10]+"...")
	}

	// 第二步：执行搜索
	results, err := p.executeSearch(client, token, keyword)
	if err != nil {
		return nil, fmt.Errorf("执行搜索失败: %w", err)
	}

	if p.debugMode {
		log.Printf("[XYS] 搜索完成，获取到 %d 个结果", len(results))
	}

	return results, nil
}

// getToken 获取搜索token
func (p *XysPlugin) getToken(client *http.Client, keyword string) (string, error) {
	// 检查缓存
	cacheKey := "token"
	if cached, found := p.tokenCache.Load(cacheKey); found {
		if tokenCache, ok := cached.(TokenCache); ok {
			// 检查是否过期
			if time.Since(tokenCache.Timestamp) < p.cacheTTL {
				if p.debugMode {
					log.Printf("[XYS] 使用缓存的token")
				}
				return tokenCache.Token, nil
			}
		}
	}

	// 构建请求URL
	tokenURL := fmt.Sprintf("%s%s?wd=%s&mode=undefined&stype=undefined",
		BaseURL, TokenPath, url.QueryEscape(keyword))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("[%s] 创建token请求失败: %w", p.Name(), err)
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
		return "", fmt.Errorf("[%s] token请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("[%s] token请求HTTP状态错误: %d", p.Name(), resp.StatusCode)
	}

	// 解析HTML提取token
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("[%s] 解析token页面HTML失败: %w", p.Name(), err)
	}

	// 查找script标签中的DToken定义
	var token string
	doc.Find("script").Each(func(i int, s *goquery.Selection) {
		scriptContent := s.Text()
		if strings.Contains(scriptContent, "DToken") {
			// 使用正则表达式提取token
			re := xysRe1
			matches := re.FindStringSubmatch(scriptContent)
			if len(matches) > 1 {
				token = matches[1]
				if p.debugMode {
					log.Printf("[XYS] 从script中提取到token: %s", token[:10]+"...")
				}
			}
		}
	})

	if token == "" {
		return "", fmt.Errorf("未找到DToken")
	}

	// 缓存token
	p.tokenCache.Store(cacheKey, TokenCache{
		Token:     token,
		Timestamp: time.Now(),
	})

	return token, nil
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *XysPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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
			status := resp.StatusCode
			resp.Body.Close()
			if err == nil {
				// Do 成功但状态码非 200。此前这里只执行 lastErr = err，
				// err 为 nil 时会把 lastErr 清空，三次失败后仅报出
				// "%!w(<nil>)"，真实状态码被丢掉、无法定位失败原因。
				err = fmt.Errorf("HTTP 状态码 %d", status)
			}
		}
		lastErr = err
	}

	return nil, fmt.Errorf("[%s] 重试 %d 次后仍然失败: %w", p.Name(), maxRetries, lastErr)
}

// executeSearch 执行搜索请求
func (p *XysPlugin) executeSearch(client *http.Client, token, keyword string) ([]model.SearchResult, error) {
	// 构建搜索URL
	searchURL := fmt.Sprintf("%s%s?DToken2=%s&requestID=undefined&mode=90002&stype=undefined&scope_content=0&wd=%s&uk=&page=1&limit=20&screen_filetype=",
		BaseURL, SearchPath, token, url.QueryEscape(keyword))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", p.Name(), err)
	}

	// 设置完整的请求头
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 搜索请求HTTP状态错误: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应体
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应体失败: %w", p.Name(), err)
	}

	// 解析JSON响应
	var searchResp SearchResponse
	if err := json.Unmarshal(respBody, &searchResp); err != nil {
		return nil, fmt.Errorf("[%s] JSON解析失败: %w", p.Name(), err)
	}

	if searchResp.Code != 0 {
		return nil, fmt.Errorf("[%s] 搜索API返回错误: %s", p.Name(), searchResp.Msg)
	}

	if p.debugMode {
		log.Printf("[XYS] 搜索API响应成功，data长度: %d", len(searchResp.Data))
	}

	// 解析HTML内容
	return p.parseSearchResults(searchResp.Data, keyword)
}

// parseSearchResults 解析搜索结果HTML
func (p *XysPlugin) parseSearchResults(htmlData, keyword string) ([]model.SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlData))
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析搜索结果HTML失败: %w", p.Name(), err)
	}

	var results []model.SearchResult

	// 查找搜索结果项
	doc.Find(".layui-card[data-qid]").Each(func(i int, s *goquery.Selection) {
		if len(results) >= MaxResults {
			return
		}

		result := p.parseResultItem(s, i+1)
		if result != nil {
			results = append(results, *result)
		}
	})

	if p.debugMode {
		log.Printf("[XYS] 解析到 %d 个原始结果", len(results))
	}

	// 关键词过滤（标准网盘插件需要过滤）
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	if p.debugMode {
		log.Printf("[XYS] 关键词过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// parseResultItem 解析单个搜索结果项
func (p *XysPlugin) parseResultItem(s *goquery.Selection, index int) *model.SearchResult {
	// 提取QID
	qid, _ := s.Attr("data-qid")
	if qid == "" {
		return nil
	}

	// 提取标题和链接
	linkEl := s.Find(`a[onclick="open_sid(this)"]`)
	if linkEl.Length() == 0 {
		return nil
	}

	// 提取标题
	title := p.cleanTitle(linkEl.Text())
	if title == "" {
		return nil
	}

	// 提取链接URL
	href, _ := linkEl.Attr("href")
	if href == "" {
		// 尝试从url属性解码
		urlAttr, _ := linkEl.Attr("url")
		if urlAttr != "" {
			if decoded, err := base64.StdEncoding.DecodeString(urlAttr); err == nil {
				href = string(decoded)
			}
		}
	}

	if href == "" {
		if p.debugMode {
			log.Printf("[XYS] 跳过无链接的结果: %s", title)
		}
		return nil
	}

	// 提取密码
	password, _ := linkEl.Attr("pa")

	// 提取时间
	timeStr := strings.TrimSpace(s.Find(".layui-icon-time").Parent().Text())
	publishTime := p.parseTime(timeStr)

	// 提取网盘类型
	platform := p.extractPlatform(s, href)

	// 构建链接对象
	link := model.Link{
		Type:     platform,
		URL:      href,
		Password: password,
	}

	// 构建结果对象
	result := model.SearchResult{
		Title:     title,
		Content:   fmt.Sprintf("来源：%s", platform),
		Channel:   "", // 插件搜索结果必须为空字符串（按开发指南要求）
		MessageID: fmt.Sprintf("%s-%s-%d", p.Name(), qid, index),
		UniqueID:  fmt.Sprintf("%s-%s-%d", p.Name(), qid, index),
		Datetime:  publishTime,
		Links:     []model.Link{link},
		Tags:      []string{platform},
	}

	if p.debugMode {
		log.Printf("[XYS] 解析结果: %s (%s)", title, platform)
	}

	return &result
}

// cleanTitle 清理标题
func (p *XysPlugin) cleanTitle(title string) string {
	if title == "" {
		return ""
	}

	// 移除HTML标签
	re := xysRe2
	cleaned := re.ReplaceAllString(title, "")

	// 移除@符号
	cleaned = strings.ReplaceAll(cleaned, "@", "")

	// 清理多余的空格
	cleaned = strings.TrimSpace(cleaned)
	re = xysRe3
	cleaned = re.ReplaceAllString(cleaned, " ")

	return cleaned
}

// parseTime 解析时间字符串
func (p *XysPlugin) parseTime(timeStr string) time.Time {
	// 清理时间字符串，移除图标等
	timeStr = strings.TrimSpace(timeStr)

	// 查找时间格式 YYYY-MM-DD HH:MM:SS
	re := xysRe4
	matches := re.FindStringSubmatch(timeStr)

	if len(matches) > 1 {
		if t, err := time.Parse("2006-01-02 15:04:05", matches[1]); err == nil {
			return t
		}
	}

	// 如果解析失败，返回当前时间
	return time.Now()
}

// extractPlatform 提取网盘平台类型（按开发指南标准实现）
func (p *XysPlugin) extractPlatform(s *goquery.Selection, href string) string {
	return determineCloudType(href)
}

// determineCloudType 根据URL自动识别网盘类型（按开发指南完整列表）
func determineCloudType(url string) string {
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
	case strings.Contains(url, "magnet:"):
		return "magnet"
	case strings.Contains(url, "ed2k://"):
		return "ed2k"
	default:
		return "others"
	}
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	xysRe1 = regexp.MustCompile(`const\s+DToken\s*=\s*"([^"]+)"`)
	xysRe2 = regexp.MustCompile(`<[^>]*>`)
	xysRe3 = regexp.MustCompile(`\s+`)
	xysRe4 = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}:\d{2})`)
)
