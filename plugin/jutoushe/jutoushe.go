package jutoushe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
)

type JutoushePlugin struct {
	*plugin.BaseAsyncPlugin
}

func init() {
	p := &JutoushePlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("jutoushe", 1),
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *JutoushePlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果（推荐方法）
func (p *JutoushePlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实现搜索逻辑
func (p *JutoushePlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 1. 构建搜索URL
	baseURL := "https://1.star2.cn"
	searchURL := fmt.Sprintf("%s/search/?keyword=%s", baseURL, url.QueryEscape(keyword))

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 3. 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 4. 设置请求头，避免反爬虫检测
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", baseURL+"/")

	// 5. 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 6. 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 7. 解析搜索结果页面
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] HTML解析失败: %w", p.Name(), err)
	}

	// 8. 提取搜索结果
	var results []model.SearchResult
	doc.Find("ul.erx-list li.item").Each(func(i int, s *goquery.Selection) {
		// 提取标题和链接
		linkElem := s.Find(".a a.main")
		title := strings.TrimSpace(linkElem.Text())
		detailPath, exists := linkElem.Attr("href")

		if !exists || title == "" {
			return // 跳过无效项
		}

		// 构建完整的详情页URL
		detailURL := baseURL + detailPath

		// 提取发布时间
		timeStr := strings.TrimSpace(s.Find(".i span.time").Text())
		publishTime := p.parseDate(timeStr)

		// 构建唯一ID
		uniqueID := fmt.Sprintf("%s-%s", p.Name(), p.extractIDFromURL(detailPath))

		// 创建搜索结果（先不获取下载链接）
		result := model.SearchResult{
			UniqueID: uniqueID,
			Title:    title,
			Content:  fmt.Sprintf("剧透社影视资源：%s", title),
			Datetime: publishTime,
			Tags:     p.extractTags(title),
			Links:    []model.Link{}, // 稍后从详情页获取
			Channel:  "",             // 插件搜索结果必须为空字符串
		}

		// 异步获取详情页的下载链接
		if links := p.getDetailLinks(client, detailURL); len(links) > 0 {
			result.Links = links
			results = append(results, result)
		}
	})

	// 9. 关键词过滤
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	return filteredResults, nil
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *JutoushePlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", maxRetries, lastErr)
}

// getDetailLinks 获取详情页的下载链接
func (p *JutoushePlugin) getDetailLinks(client *http.Client, detailURL string) []model.Link {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", detailURL, nil)
	if err != nil {
		return nil
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://1.star2.cn/")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil
	}

	var links []model.Link

	// 提取下载链接
	doc.Find(".dlipp-cont-bd a.dlipp-dl-btn").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}

		// 过滤掉无效链接
		if !p.isValidNetworkDriveURL(href) {
			return
		}

		// 确定网盘类型和提取提取码
		cloudType := p.determineCloudType(href)
		password := p.extractPassword(href)

		link := model.Link{
			Type:     cloudType,
			URL:      href,
			Password: password,
		}

		links = append(links, link)
	})

	return links
}

// determineCloudType 根据URL确定网盘类型
func (p *JutoushePlugin) determineCloudType(url string) string {
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
	case strings.Contains(url, "115.com"):
		return "115"
	case strings.Contains(url, "123pan.com"):
		return "123"
	case strings.Contains(url, "caiyun.139.com"):
		return "mobile"
	case strings.Contains(url, "mypikpak.com"):
		return "pikpak"
	default:
		return "others"
	}
}

// extractPassword 从URL中提取提取码
func (p *JutoushePlugin) extractPassword(url string) string {
	// 处理百度网盘的pwd参数
	if strings.Contains(url, "pan.baidu.com") && strings.Contains(url, "pwd=") {
		re := jutousheRe1
		matches := re.FindStringSubmatch(url)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	// 其他网盘暂不处理提取码
	return ""
}

// isValidNetworkDriveURL 验证是否为有效的网盘链接
func (p *JutoushePlugin) isValidNetworkDriveURL(url string) bool {
	if url == "" {
		return false
	}

	// 检查是否为HTTP/HTTPS链接
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return false
	}

	// 检查是否包含已知网盘域名
	knownDomains := []string{
		"pan.quark.cn", "drive.uc.cn", "pan.baidu.com",
		"aliyundrive.com", "alipan.com", "pan.xunlei.com",
		"cloud.189.cn", "115.com", "123pan.com",
		"caiyun.139.com", "mypikpak.com",
	}

	for _, domain := range knownDomains {
		if strings.Contains(url, domain) {
			return true
		}
	}

	return false
}

// extractIDFromURL 从URL路径中提取ID
func (p *JutoushePlugin) extractIDFromURL(urlPath string) string {
	// 从 /dm/8100.html 提取 8100
	re := jutousheRe2
	matches := re.FindStringSubmatch(urlPath)
	if len(matches) > 2 {
		return matches[2]
	}

	// 如果无法提取，使用完整路径作为ID
	return strings.ReplaceAll(urlPath, "/", "_")
}

// extractTags 从标题中提取标签
func (p *JutoushePlugin) extractTags(title string) []string {
	var tags []string

	// 提取分类标签
	categoryPattern := jutousheRe3
	matches := categoryPattern.FindAllStringSubmatch(title, -1)
	for _, match := range matches {
		if len(match) > 1 {
			tags = append(tags, match[1])
		}
	}

	// 如果没有提取到分类，添加默认标签
	if len(tags) == 0 {
		tags = append(tags, "影视资源")
	}

	return tags
}

// parseDate 解析日期字符串
func (p *JutoushePlugin) parseDate(dateStr string) time.Time {
	if dateStr == "" {
		return time.Now()
	}

	// 尝试解析 YYYY-MM-DD 格式
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		return t
	}

	// 尝试解析 YYYY年MM月DD日 格式
	re := jutousheRe4
	matches := re.FindStringSubmatch(dateStr)
	if len(matches) == 4 {
		year, _ := strconv.Atoi(matches[1])
		month, _ := strconv.Atoi(matches[2])
		day, _ := strconv.Atoi(matches[3])
		return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.Local)
	}

	// 解析失败，返回当前时间
	return time.Now()
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	jutousheRe1 = regexp.MustCompile(`pwd=([^&]+)`)
	jutousheRe2 = regexp.MustCompile(`/([^/]+)/(\d+)\.html`)
	jutousheRe3 = regexp.MustCompile(`【([^】]+)】`)
	jutousheRe4 = regexp.MustCompile(`(\d{4})年(\d{1,2})月(\d{1,2})日`)
)
