package quarksoo

import (
	"crypto/md5"
	"fmt"
	"html"
	"math/rand"
	"net/http"
	"net/url"
	"pansou/util"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"pansou/model"
	"pansou/plugin"
)

// 在init函数中注册插件
func init() {
	// 注册插件
	plugin.RegisterGlobalPlugin(NewQuarksooAsyncPlugin())
}

// BaseURL 是 API 基础地址。声明为变量而非常量是为了让测试能指向本地假服务器，
// 从而验证重试次数与最终结果，而不是只能靠肉眼看代码。
var BaseURL = "https://quarksoo.cc/search.php"

const (
	// 默认参数
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

// QuarksooAsyncPlugin quarksoo网盘搜索异步插件
type QuarksooAsyncPlugin struct {
	*plugin.BaseAsyncPlugin
	retries int
}

// NewQuarksooAsyncPlugin 创建新的quarksoo异步插件
func NewQuarksooAsyncPlugin() *QuarksooAsyncPlugin {
	return &QuarksooAsyncPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("quarksoo", 3), // 启用Service层过滤
		retries:         MaxRetries,
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *QuarksooAsyncPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *QuarksooAsyncPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.doSearch, p.MainCacheKey, ext)
}

// doSearch 实际的搜索实现
func (p *QuarksooAsyncPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 初始化随机数种子
	rand.Seed(time.Now().UnixNano())

	// 构建搜索URL
	searchURL := fmt.Sprintf("%s?q=%s", BaseURL, url.QueryEscape(keyword))

	// 创建请求
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("User-Agent", getRandomUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://quarksoo.cc/")

	var responseBody []byte

	// 重试逻辑收敛到 util.DoWithRetry：这段循环原本全仓复制了 30 多份，每份都要自己
	// 处理"最后一次不再等待""错误怎么包装""响应体在循环里怎么关"。
	// 这里的参数保持既有行为不变（固定 500ms、共 p.retries+1 次尝试）。
	err = util.DoWithRetry(util.RetryConfig{
		Attempts:  p.retries + 1,
		BaseDelay: 500 * time.Millisecond,
		MaxDelay:  500 * time.Millisecond,
	}, func(_ int) error {
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("请求失败: %w", err)
		}
		// 读完立即关闭：由组件保证每轮独立，不会像 defer 那样压到函数返回
		body, readErr := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("读取响应失败: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("API返回非200状态码: %d", resp.StatusCode)
		}
		responseBody = body
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 解析HTML内容
	htmlContent := string(responseBody)
	results := p.parseSearchResults(htmlContent, keyword)

	// 去重
	uniqueResults := p.deduplicateResults(results)

	// 使用过滤功能过滤结果（二次过滤）
	filteredResults := plugin.FilterResultsByKeyword(uniqueResults, keyword)

	return filteredResults, nil
}

// parseSearchResults 从HTML中解析搜索结果
func (p *QuarksooAsyncPlugin) parseSearchResults(htmlContent string, keyword string) []model.SearchResult {
	var results []model.SearchResult

	// 提前过滤：检查标题是否包含关键词
	lowerKeyword := strings.ToLower(keyword)
	keywords := strings.Fields(lowerKeyword)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return results
	}
	doc.Find("tr").Each(func(_ int, row *goquery.Selection) {
		cells := row.Find("td")
		if cells.Length() < 2 {
			return
		}

		title := cleanQuarksooText(cells.Eq(0).Text())
		if title == "" || strings.Contains(title, "剧名") || strings.Contains(title, "网盘链接") {
			return
		}
		anchor := cells.Eq(1).Find("a[href]").First()
		if anchor.Length() == 0 {
			anchor = row.Find("a[href]").First()
		}
		linkURL := strings.TrimSpace(anchor.AttrOr("href", ""))
		lowerLink := strings.ToLower(linkURL)
		if !strings.Contains(lowerLink, "pan.qoark.cn") && !strings.Contains(lowerLink, "pan.quark.cn") {
			return
		}

		lowerTitle := strings.ToLower(title)
		for _, kw := range keywords {
			if !strings.Contains(lowerTitle, kw) {
				return
			}
		}

		uniqueIDKey := fmt.Sprintf("%s|%s", title, linkURL)
		hash := md5.Sum([]byte(uniqueIDKey))
		results = append(results, model.SearchResult{
			UniqueID: fmt.Sprintf("quarksoo-%x", hash[:8]),
			Title:    title,
			Links: []model.Link{{
				Type:     "quark",
				URL:      linkURL,
				Password: "",
			}},
			Channel:  "",
			Datetime: time.Now(),
		})
	})

	return results
}

func cleanQuarksooText(value string) string {
	value = html.UnescapeString(value)
	return strings.TrimSpace(strings.Join(strings.Fields(value), " "))
}

// deduplicateResults 去除重复结果
func (p *QuarksooAsyncPlugin) deduplicateResults(results []model.SearchResult) []model.SearchResult {
	seen := make(map[string]bool)
	unique := make([]model.SearchResult, 0, len(results))

	for _, result := range results {
		// 使用UniqueID进行去重
		if !seen[result.UniqueID] {
			seen[result.UniqueID] = true
			unique = append(unique, result)
		}
	}

	// 按标题排序（保持一致性）
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Title < unique[j].Title
	})

	return unique
}

// 生成随机IP
func generateRandomIP() string {
	return fmt.Sprintf("%d.%d.%d.%d",
		rand.Intn(223)+1, // 避免0和255
		rand.Intn(255),
		rand.Intn(255),
		rand.Intn(254)+1) // 避免0
}

// 获取随机UA
func getRandomUA() string {
	return userAgents[rand.Intn(len(userAgents))]
}
