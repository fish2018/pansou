package libvio

import (
	"compress/gzip"
	"context"
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
	BaseURL        = "https://libvio.host"
	SearchPath     = "/search/-------------.html"
	UserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	MaxConcurrency = 6 // 详情页最大并发数
	MaxPages       = 1 // 最大搜索页数（暂时只搜索第一页）
)

// LibvioPlugin LIBVIO插件
type LibvioPlugin struct {
	*plugin.BaseAsyncPlugin
	debugMode   bool
	detailCache sync.Map // 缓存详情页结果
	playCache   sync.Map // 缓存播放页结果
	cacheTTL    time.Duration
}

// NewLibvioPlugin 创建新的LIBVIO插件实例
func NewLibvioPlugin() *LibvioPlugin {
	// 检查调试模式
	debugMode := false // 开启调试模式

	p := &LibvioPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("libvio", 1, true),
		debugMode:       debugMode,
		cacheTTL:        30 * time.Minute,
	}

	return p
}

// Name 返回插件名称
func (p *LibvioPlugin) Name() string {
	return "libvio"
}

// DisplayName 返回插件显示名称
func (p *LibvioPlugin) DisplayName() string {
	return "LIBVIO"
}

// Description 返回插件描述
func (p *LibvioPlugin) Description() string {
	return "LIBVIO - 影视资源网盘下载"
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *LibvioPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *LibvioPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// setRequestHeaders 设置请求头
func (p *LibvioPlugin) setRequestHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

// doRequest 发送HTTP请求
func (p *LibvioPlugin) doRequest(client *http.Client, url string, referer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	p.setRequestHeaders(req, referer)

	if p.debugMode {
		log.Printf("[Libvio] 发送请求: %s", url)
	}

	resp, err := client.Do(req)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 请求失败: %v", err)
		}
		return nil, err
	}

	if p.debugMode {
		log.Printf("[Libvio] 响应状态: %d", resp.StatusCode)
	}

	return resp, nil
}

// searchImpl 实际的搜索实现
func (p *LibvioPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	searchURL := fmt.Sprintf("%s%s?wd=%s&submit=", BaseURL, SearchPath, url.QueryEscape(keyword))

	if p.debugMode {
		log.Printf("[Libvio] 开始搜索: %s", keyword)
		log.Printf("[Libvio] 搜索URL: %s", searchURL)
	}

	// 发送搜索请求
	resp, err := p.doRequest(client, searchURL, BaseURL)
	if err != nil {
		return nil, fmt.Errorf("发送搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索响应状态码异常: %d", resp.StatusCode)
	}

	// 处理响应体（可能是gzip压缩的）
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil, err
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("解析HTML失败: %w", err)
	}

	// 提取搜索结果
	results := p.extractSearchResults(doc, keyword)

	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个搜索结果", len(results))
	}

	// 并发获取详情页的下载链接
	results = p.enrichWithDetailLinks(client, results, keyword)

	if p.debugMode {
		// 统计链接数量
		totalLinks := 0
		for i, r := range results {
			log.Printf("[Libvio] 结果 %d: %s, 链接数: %d", i+1, r.Title, len(r.Links))
			totalLinks += len(r.Links)
		}
		log.Printf("[Libvio] 总计: %d 个结果，%d 个链接", len(results), totalLinks)
	}

	// 过滤结果
	filteredResults := plugin.FilterResultsByKeyword(results, keyword)

	if p.debugMode {
		log.Printf("[Libvio] 过滤后剩余 %d 个结果", len(filteredResults))
	}

	return filteredResults, nil
}

// getResponseReader 获取响应读取器（处理gzip压缩）
func (p *LibvioPlugin) getResponseReader(resp *http.Response) (io.Reader, error) {
	var reader io.Reader = resp.Body

	// 检查Content-Encoding
	contentEncoding := resp.Header.Get("Content-Encoding")
	if p.debugMode {
		log.Printf("[Libvio] Content-Encoding: %s", contentEncoding)
	}

	// 如果是gzip压缩，手动解压
	if contentEncoding == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("创建gzip reader失败: %w", err)
		}
		// 注意：不要在这里关闭gzReader，它需要在外部使用
		reader = gzReader
	}

	return reader, nil
}

// extractSearchResults 从HTML中提取搜索结果
func (p *LibvioPlugin) extractSearchResults(doc *goquery.Document, keyword string) []model.SearchResult {
	var results []model.SearchResult

	// 选择所有搜索结果项
	doc.Find("ul.stui-vodlist li").Each(func(i int, s *goquery.Selection) {
		// 提取标题和详情页链接
		titleElem := s.Find(".stui-vodlist__detail h4 a")
		title := strings.TrimSpace(titleElem.Text())
		if title == "" {
			title, _ = titleElem.Attr("title")
		}

		detailPath, _ := titleElem.Attr("href")
		if detailPath == "" {
			// 尝试从缩略图链接获取
			thumbLink := s.Find("a.stui-vodlist__thumb")
			detailPath, _ = thumbLink.Attr("href")
		}

		if title == "" || detailPath == "" {
			return
		}

		// 构建完整的详情页URL
		detailURL := BaseURL + detailPath

		// 提取其他信息
		episodeInfo := strings.TrimSpace(s.Find(".pic-text").Text())
		rating := strings.TrimSpace(s.Find(".pic-tag").Text())

		// 从详情页路径提取ID（如：/detail/4095.html -> 4095）
		idMatch := regexp.MustCompile(`/detail/(\d+)\.html`).FindStringSubmatch(detailPath)
		resourceID := ""
		if len(idMatch) > 1 {
			resourceID = idMatch[1]
		} else {
			resourceID = fmt.Sprintf("%d", time.Now().UnixNano())
		}

		if p.debugMode {
			log.Printf("[Libvio] 提取结果 %d: %s, URL: %s", i+1, title, detailURL)
		}

		// 构建内容描述
		content := ""
		if episodeInfo != "" {
			content = episodeInfo
		}
		if rating != "" {
			if content != "" {
				content += " | "
			}
			content += "评分: " + rating
		}

		result := model.SearchResult{
			Title:     title,
			Content:   content,
			Channel:   "",
			MessageID: fmt.Sprintf("%s-%s", p.Name(), resourceID),
			UniqueID:  fmt.Sprintf("%s-%s", p.Name(), resourceID),
			Datetime:  time.Now(),
			Links:     []model.Link{}, // 稍后填充
		}

		// 将详情页URL存储在Tags中供后续使用
		result.Tags = []string{detailURL}

		results = append(results, result)
	})

	return results
}

// enrichWithDetailLinks 并发获取详情页的下载链接
func (p *LibvioPlugin) enrichWithDetailLinks(client *http.Client, results []model.SearchResult, keyword string) []model.SearchResult {
	if len(results) == 0 {
		return results
	}

	if p.debugMode {
		log.Printf("[Libvio] 开始获取 %d 个详情页的下载链接", len(results))
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	semaphore := make(chan struct{}, MaxConcurrency)

	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 添加小延迟避免请求过快
			time.Sleep(time.Duration(idx*50) * time.Millisecond)

			// 从Tags中获取详情页URL
			if len(results[idx].Tags) > 0 {
				detailURL := results[idx].Tags[0]
				links := p.fetchDetailPageLinks(client, detailURL, results[idx].Title)

				mu.Lock()
				results[idx].Links = links
				// 清空Tags，避免返回给用户
				results[idx].Tags = nil
				mu.Unlock()

				if p.debugMode {
					log.Printf("[Libvio] 详情页 %d/%d 获取到 %d 个链接", idx+1, len(results), len(links))
				}
			}
		}(i)
	}

	wg.Wait()

	validResults := results[:0]
	for _, result := range results {
		if len(result.Links) > 0 {
			validResults = append(validResults, result)
		}
	}
	return validResults
}

// fetchDetailPageLinks 获取详情页的下载链接
func (p *LibvioPlugin) fetchDetailPageLinks(client *http.Client, detailURL string, workTitle string) []model.Link {
	if p.debugMode {
		log.Printf("[Libvio] 开始获取详情页: %s", detailURL)
	}

	// 检查缓存
	if cached, ok := p.detailCache.Load(detailURL); ok {
		if links, ok := cached.([]model.Link); ok {
			if p.debugMode {
				log.Printf("[Libvio] 使用缓存的详情页结果: %s, 链接数: %d", detailURL, len(links))
			}
			return links
		}
	}

	// 访问详情页
	resp, err := p.doRequest(client, detailURL, BaseURL)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 获取详情页失败: %s, 错误: %v", detailURL, err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Libvio] 详情页响应状态码异常: %s, 状态码: %d", detailURL, resp.StatusCode)
		}
		return nil
	}

	// 处理响应体
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	// 解析HTML
	doc, err := goquery.NewDocumentFromReader(reader)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 解析详情页HTML失败: %v", err)
		}
		return nil
	}

	// 新版详情页直接暴露网盘链接，无需再访问播放页。
	links := p.extractDirectPanLinks(doc, workTitle)
	if len(links) > 0 {
		p.cacheDetailLinks(detailURL, links)
		return links
	}

	// 兼容旧版详情页：从下载播放页的 player_aaaa 中提取链接。
	playLinks := p.extractDownloadPlayLinks(doc)

	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个下载播放页链接", len(playLinks))
	}

	if len(playLinks) == 0 {
		if p.debugMode {
			log.Printf("[Libvio] 未找到下载链接")
		}
		return nil
	}

	// 获取网盘链接
	links = make([]model.Link, 0, len(playLinks))
	for _, playLink := range playLinks {
		if p.debugMode {
			log.Printf("[Libvio] 获取网盘链接: %s", playLink.URL)
		}
		panLink := p.fetchPanLink(client, playLink.URL, detailURL)
		if panLink != nil {
			panLink.WorkTitle = workTitle
			if panLink.Password == "" {
				panLink.Password = extractPassword(panLink.URL)
			}
			links = append(links, *panLink)
		} else if p.debugMode {
			log.Printf("[Libvio] 未能获取网盘链接: %s", playLink.URL)
		}
	}

	if p.debugMode {
		log.Printf("[Libvio] 详情页 %s 最终获取到 %d 个网盘链接", detailURL, len(links))
	}

	p.cacheDetailLinks(detailURL, links)
	return links
}

func (p *LibvioPlugin) extractDirectPanLinks(doc *goquery.Document, workTitle string) []model.Link {
	links := make([]model.Link, 0, 4)
	seen := make(map[string]struct{})
	doc.Find(".netdisk-panel a.netdisk-item[href]").Each(func(_ int, selection *goquery.Selection) {
		rawURL, _ := selection.Attr("href")
		rawURL = strings.TrimSpace(rawURL)
		parsed, err := url.Parse(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return
		}
		linkType := p.mapPanType("", rawURL)
		if linkType == "others" {
			return
		}
		password := extractPassword(rawURL)
		key := rawURL + "\x00" + password
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		links = append(links, model.Link{
			URL:       rawURL,
			Type:      linkType,
			Password:  password,
			WorkTitle: workTitle,
		})
	})
	return links
}

func (p *LibvioPlugin) cacheDetailLinks(detailURL string, links []model.Link) {
	p.detailCache.Store(detailURL, links)
	go func() {
		time.Sleep(p.cacheTTL)
		p.detailCache.Delete(detailURL)
	}()
}

func extractPassword(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, key := range []string{"pwd", "password", "code"} {
		if value := strings.TrimSpace(parsed.Query().Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// PlayLinkInfo 播放链接信息
type PlayLinkInfo struct {
	URL     string
	PanType string // 网盘类型（从标题提取）
}

// extractDownloadPlayLinks 提取下载播放页链接
func (p *LibvioPlugin) extractDownloadPlayLinks(doc *goquery.Document) []PlayLinkInfo {
	var playLinks []PlayLinkInfo

	// 查找所有播放源
	allHeads := doc.Find(".stui-vodlist__head")
	if p.debugMode {
		log.Printf("[Libvio] 找到 %d 个播放源头部", allHeads.Length())
	}

	allHeads.Each(func(i int, s *goquery.Selection) {
		// 获取标题
		title := strings.TrimSpace(s.Find("h3").Text())

		if p.debugMode {
			log.Printf("[Libvio] 播放源 %d 标题: %s", i+1, title)
		}

		// 只处理包含"下载"的源
		if !strings.Contains(title, "下载") {
			if p.debugMode {
				log.Printf("[Libvio] 跳过非下载源: %s", title)
			}
			return
		}

		// 提取网盘类型
		panType := ""
		if strings.Contains(title, "夸克") || strings.Contains(title, "quark") {
			panType = "quark"
		} else if strings.Contains(title, "UC") || strings.Contains(title, "uc") {
			panType = "uc"
		} else if strings.Contains(title, "百度") || strings.Contains(title, "baidu") {
			panType = "baidu"
		}

		// 提取播放页链接
		playlistLinks := s.Find(".stui-content__playlist li a")
		if p.debugMode {
			log.Printf("[Libvio] 播放列表中有 %d 个链接", playlistLinks.Length())
		}

		// 通常只取第一个链接（合集）
		firstLink := playlistLinks.First()
		if firstLink.Length() > 0 {
			href, exists := firstLink.Attr("href")
			if exists && href != "" {
				// 构建完整URL
				playURL := BaseURL + href

				playLinks = append(playLinks, PlayLinkInfo{
					URL:     playURL,
					PanType: panType,
				})

				if p.debugMode {
					linkText := strings.TrimSpace(firstLink.Text())
					log.Printf("[Libvio] 找到下载链接: %s (%s) [%s]", playURL, panType, linkText)
				}
			}
		}
	})

	return playLinks
}

// fetchPanLink 获取网盘链接
func (p *LibvioPlugin) fetchPanLink(client *http.Client, playURL string, referer string) *model.Link {
	// 检查缓存
	if cached, ok := p.playCache.Load(playURL); ok {
		if link, ok := cached.(*model.Link); ok {
			if p.debugMode {
				log.Printf("[Libvio] 使用缓存的播放页结果: %s", playURL)
			}
			return link
		}
	}

	// 访问播放页
	resp, err := p.doRequest(client, playURL, referer)
	if err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 获取播放页失败: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if p.debugMode {
			log.Printf("[Libvio] 播放页响应状态码异常: %d", resp.StatusCode)
		}
		return nil
	}

	// 处理响应体（可能是gzip压缩的）
	reader, err := p.getResponseReader(resp)
	if err != nil {
		return nil
	}

	// 读取响应体
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil
	}

	// 提取player_aaaa对象
	playerDataRegex := regexp.MustCompile(`var\s+player_aaaa\s*=\s*({[^}]+})`)
	matches := playerDataRegex.FindStringSubmatch(string(body))

	if len(matches) < 2 {
		if p.debugMode {
			log.Printf("[Libvio] 未找到player_aaaa对象")
			// 输出部分body内容用于调试
			bodyStr := string(body)
			if len(bodyStr) > 500 {
				log.Printf("[Libvio] 页面内容前500字符: %s", bodyStr[:500])
			} else {
				log.Printf("[Libvio] 页面内容: %s", bodyStr)
			}
		}
		return nil
	}

	// 解析JSON
	playerJSON := matches[1]
	if p.debugMode {
		log.Printf("[Libvio] 找到player_aaaa: %s", playerJSON)
	}

	// 处理转义字符
	playerJSON = strings.ReplaceAll(playerJSON, `\/`, `/`)

	var playerData map[string]interface{}
	if err := json.Unmarshal([]byte(playerJSON), &playerData); err != nil {
		if p.debugMode {
			log.Printf("[Libvio] 解析player_aaaa失败: %v, JSON: %s", err, playerJSON)
		}
		return nil
	}

	// 提取URL
	panURL, ok := playerData["url"].(string)
	if !ok || panURL == "" {
		if p.debugMode {
			log.Printf("[Libvio] player_aaaa中没有url字段")
		}
		return nil
	}

	// 提取网盘类型
	from, _ := playerData["from"].(string)
	linkType := p.mapPanType(from, panURL)

	link := &model.Link{
		URL:  panURL,
		Type: linkType,
	}

	if p.debugMode {
		log.Printf("[Libvio] 提取到网盘链接: %s (from=%s, type=%s)", panURL, from, linkType)
	}

	// 缓存结果
	p.playCache.Store(playURL, link)

	// 设置缓存过期
	go func() {
		time.Sleep(p.cacheTTL)
		p.playCache.Delete(playURL)
	}()

	return link
}

// mapPanType 映射网盘类型
func (p *LibvioPlugin) mapPanType(from string, url string) string {
	// 首先根据from字段判断
	switch strings.ToLower(from) {
	case "uc":
		return "uc"
	case "quark":
		return "quark"
	case "baidu":
		return "baidu"
	case "aliyun", "alipan":
		return "aliyun"
	case "xunlei", "thunder":
		return "xunlei"
	case "115":
		return "115"
	case "123", "123pan":
		return "123"
	}

	// 如果from字段不明确，根据URL判断
	url = strings.ToLower(url)
	if strings.Contains(url, "drive.uc.cn") {
		return "uc"
	} else if strings.Contains(url, "pan.quark.cn") {
		return "quark"
	} else if strings.Contains(url, "pan.baidu.com") {
		return "baidu"
	} else if strings.Contains(url, "alipan.com") || strings.Contains(url, "aliyundrive.com") {
		return "aliyun"
	} else if strings.Contains(url, "pan.xunlei.com") {
		return "xunlei"
	} else if strings.Contains(url, "115.com") {
		return "115"
	} else if strings.Contains(url, "123pan.com") || strings.Contains(url, "123684.com") {
		return "123"
	} else if strings.Contains(url, "cloud.189.cn") {
		return "tianyi"
	}

	// 默认返回others
	return "others"
}

func init() {
	plugin.RegisterGlobalPlugin(NewLibvioPlugin())
}
