package sdso

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
)

const (
	// 调试日志开关
	DebugLog = false
	// 默认每种网盘类型获取页数
	DefaultPagesPerType = 2
	// 最大允许每种网盘类型页数（防止过度请求）
	MaxAllowedPagesPerType = 5
	// AES解密配置
	AESKey = "4OToScUFOaeVTrHE"
	AESIV  = "9CLGao1vHKqm17Oz"
)

// 支持的网盘类型列表
var SupportedCloudTypes = []string{"baidu", "quark", "xunlei", "ali"}

// SDSOPlugin SDSO搜索插件
type SDSOPlugin struct {
	*plugin.BaseAsyncPlugin
}

// APIResponse SDSO API响应结构
type APIResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Total int        `json:"total"`
		List  []DataItem `json:"list"`
	} `json:"data"`
}

// DataItem 搜索结果项
type DataItem struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url"` // 加密的网盘链接
	Type        string     `json:"type"`
	From        string     `json:"from"` // 网盘类型: quark/xunlei/aliyun/baidu
	Content     *string    `json:"content"`
	GmtCreate   string     `json:"gmtCreate"`
	GmtShare    string     `json:"gmtShare"`
	FileCount   int        `json:"fileCount"`
	CreatorID   *string    `json:"creatorId"`
	CreatorName string     `json:"creatorName"`
	FileInfos   []FileInfo `json:"fileInfos"`
}

// FileInfo 文件信息
type FileInfo struct {
	Category      *string `json:"category"`
	FileExtension *string `json:"fileExtension"`
	FileID        string  `json:"fileId"`
	FileName      string  `json:"fileName"`
	Type          *string `json:"type"`
}

// PageResult 页面搜索结果
type PageResult struct {
	pageNo   int
	fromType string
	results  []model.SearchResult
	err      error
}

func init() {
	p := &SDSOPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("sdso", 3), // 优先级3 = 普通质量数据源
	}
	plugin.RegisterGlobalPlugin(p)
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *SDSOPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果（推荐方法）
func (p *SDSOPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *SDSOPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[%s] 开始搜索，关键词: %s\n", p.Name(), keyword)
	}

	// 1. 从扩展参数中获取每种网盘类型的页数配置
	pagesPerType := DefaultPagesPerType
	if ext != nil {
		if pages, ok := ext["pages_per_type"].(int); ok && pages > 0 {
			pagesPerType = pages
			if pagesPerType > MaxAllowedPagesPerType {
				pagesPerType = MaxAllowedPagesPerType
				if DebugLog {
					fmt.Printf("[%s] 每种网盘类型页数限制在最大值: %d\n", p.Name(), MaxAllowedPagesPerType)
				}
			}
		} else if pagesFloat, ok := ext["pages_per_type"].(float64); ok && pagesFloat > 0 {
			pagesPerType = int(pagesFloat)
			if pagesPerType > MaxAllowedPagesPerType {
				pagesPerType = MaxAllowedPagesPerType
			}
		}
		// 保持向后兼容：如果设置了 pages 参数，则平均分配给各网盘类型
		if pages, ok := ext["pages"].(int); ok && pages > 0 {
			pagesPerType = pages / len(SupportedCloudTypes)
			if pagesPerType == 0 {
				pagesPerType = 1
			}
			if pagesPerType > MaxAllowedPagesPerType {
				pagesPerType = MaxAllowedPagesPerType
			}
		}
	}

	totalTasks := len(SupportedCloudTypes) * pagesPerType
	if DebugLog {
		fmt.Printf("[%s] 将分别搜索 %d 种网盘类型，每种 %d 页，总计 %d 个并发任务\n",
			p.Name(), len(SupportedCloudTypes), pagesPerType, totalTasks)
	}

	// 2. 并发请求多个网盘类型的多页数据
	var wg sync.WaitGroup
	resultsChan := make(chan PageResult, totalTasks)

	// 启动并发任务：遍历网盘类型和页数
	for _, cloudType := range SupportedCloudTypes {
		for pageNo := 1; pageNo <= pagesPerType; pageNo++ {
			wg.Add(1)
			go func(fromType string, page int) {
				defer wg.Done()

				results, err := p.fetchSinglePageWithType(client, keyword, page, fromType)
				resultsChan <- PageResult{
					pageNo:   page,
					fromType: fromType,
					results:  results,
					err:      err,
				}
			}(cloudType, pageNo)
		}
	}

	// 等待所有任务完成
	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// 3. 收集所有页面结果
	var allResults []model.SearchResult
	successTasks := 0
	errorTasks := 0
	resultsByType := make(map[string]int) // 统计各网盘类型的结果数

	for pageResult := range resultsChan {
		if pageResult.err != nil {
			errorTasks++
			if DebugLog {
				fmt.Printf("[%s] %s网盘第%d页请求失败: %v\n", p.Name(), pageResult.fromType, pageResult.pageNo, pageResult.err)
			}
			continue
		}

		successTasks++
		allResults = append(allResults, pageResult.results...)
		resultsByType[pageResult.fromType] += len(pageResult.results)
		if DebugLog {
			fmt.Printf("[%s] %s网盘第%d页成功获取 %d 个结果\n", p.Name(), pageResult.fromType, pageResult.pageNo, len(pageResult.results))
		}
	}

	if DebugLog {
		fmt.Printf("[%s] 分类搜索完成: 成功%d任务, 失败%d任务, 总结果%d个\n",
			p.Name(), successTasks, errorTasks, len(allResults))
		for cloudType, count := range resultsByType {
			fmt.Printf("[%s]   - %s网盘: %d个结果\n", p.Name(), cloudType, count)
		}
	}

	// 4. 如果所有任务都失败，返回错误
	if successTasks == 0 {
		return nil, fmt.Errorf("[%s] 所有搜索任务都失败", p.Name())
	}

	// 5. 关键词过滤
	beforeFilterCount := len(allResults)
	filteredResults := plugin.FilterResultsByKeyword(allResults, keyword)

	if DebugLog {
		fmt.Printf("[%s] 关键词过滤: 过滤前%d项 -> 过滤后%d项\n",
			p.Name(), beforeFilterCount, len(filteredResults))
	}

	return filteredResults, nil
}

// fetchSinglePageWithType 获取指定网盘类型的单页数据
func (p *SDSOPlugin) fetchSinglePageWithType(client *http.Client, keyword string, pageNo int, fromType string) ([]model.SearchResult, error) {
	// 1. 构建搜索URL，添加from参数指定网盘类型
	searchURL := fmt.Sprintf("https://sdso.top/api/sd/search?name=%s&pageNo=%d&from=%s",
		url.QueryEscape(keyword), pageNo, fromType)
	if DebugLog {
		fmt.Printf("[%s] 请求%s网盘第%d页: %s\n", p.Name(), fromType, pageNo, searchURL)
	}

	// 2. 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 3. 创建请求对象
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页创建请求失败: %w", p.Name(), fromType, pageNo, err)
	}

	// 4. 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Referer", "https://sdso.top/")

	// 5. 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页请求失败: %w", p.Name(), fromType, pageNo, err)
	}
	defer resp.Body.Close()

	// 6. 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] %s网盘第%d页返回状态码: %d", p.Name(), fromType, pageNo, resp.StatusCode)
	}

	// 7. 解析响应
	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("[%s] %s网盘第%d页JSON解析失败: %w", p.Name(), fromType, pageNo, err)
	}

	// 8. 检查API响应状态
	if apiResp.Code != 200 {
		return nil, fmt.Errorf("[%s] %s网盘第%d页API错误: %s", p.Name(), fromType, pageNo, apiResp.Msg)
	}

	if DebugLog {
		fmt.Printf("[%s] %s网盘第%d页获取到 %d 个原始结果\n", p.Name(), fromType, pageNo, len(apiResp.Data.List))
	}

	// 9. 转换为标准格式
	results := make([]model.SearchResult, 0, len(apiResp.Data.List))
	processedCount := 0
	skippedCount := 0

	for i, item := range apiResp.Data.List {
		// 解密网盘链接
		decryptedURL, err := DecryptURL(item.URL)
		if err != nil {
			if DebugLog {
				fmt.Printf("[%s] %s网盘第%d页第%d项解密失败: %v\n", p.Name(), fromType, pageNo, i+1, err)
			}
			skippedCount++
			continue
		}

		// 验证是否为有效的网盘链接
		if !isValidPanURL(decryptedURL) {
			if DebugLog {
				fmt.Printf("[%s] %s网盘第%d页第%d项无效链接: %s\n", p.Name(), fromType, pageNo, i+1, decryptedURL)
			}
			skippedCount++
			continue
		}

		// 映射网盘类型
		panType := mapPanType(item.From)
		if panType == "others" {
			skippedCount++
			continue
		}

		// 创建链接对象
		link := model.Link{
			Type:     panType,
			URL:      decryptedURL,
			Password: "", // SDSO返回的链接通常不包含密码
		}

		// 解析时间
		datetime, err := time.Parse("2006-01-02 15:04:05", item.GmtShare)
		if err != nil {
			datetime = time.Now() // 如果解析失败，使用当前时间
		}

		// 清理标题中的HTML标签
		title := cleanHTMLTags(item.Name)

		// 构建搜索结果，UniqueID包含网盘类型和页码避免重复
		result := model.SearchResult{
			UniqueID: fmt.Sprintf("%s-%s-%s-%d", p.Name(), item.ID, fromType, pageNo),
			Title:    title,
			Content:  fmt.Sprintf("分享者: %s | 文件数量: %d | 网盘类型: %s", item.CreatorName, item.FileCount, fromType),
			Links:    []model.Link{link},
			Tags:     []string{item.From, item.Type},
			Channel:  "", // 插件搜索结果必须为空字符串
			Datetime: datetime,
		}

		results = append(results, result)
		processedCount++
	}

	if DebugLog {
		fmt.Printf("[%s] %s网盘第%d页处理完成: 原始%d项 -> 有效%d项 -> 跳过%d项\n",
			p.Name(), fromType, pageNo, len(apiResp.Data.List), processedCount, skippedCount)
	}

	return results, nil
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *SDSOPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
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

// DecryptURL 解密SDSO网站返回的加密URL
// 输入: Base64编码的密文
// 输出: 解密后的原始网盘链接
func DecryptURL(encryptedURL string) (string, error) {
	if encryptedURL == "" {
		return "", fmt.Errorf("加密URL不能为空")
	}

	// Base64解码
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedURL)
	if err != nil {
		return "", fmt.Errorf("Base64解码失败: %w", err)
	}

	// 检查密文长度
	if len(ciphertext) == 0 {
		return "", fmt.Errorf("密文长度为0")
	}

	// 检查密文长度是否为16的倍数
	if len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("密文长度不是AES块大小的倍数")
	}

	// 创建AES块加密器
	block, err := aes.NewCipher([]byte(AESKey))
	if err != nil {
		return "", fmt.Errorf("创建AES加密器失败: %w", err)
	}

	// 创建CBC模式解密器
	iv := []byte(AESIV)
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("IV长度不正确: 期望%d，实际%d", aes.BlockSize, len(iv))
	}

	mode := cipher.NewCBCDecrypter(block, iv)

	// 解密
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// 去除PKCS7填充
	unpaddedText, err := removePKCS7Padding(plaintext)
	if err != nil {
		return "", fmt.Errorf("去除填充失败: %w", err)
	}

	return string(unpaddedText), nil
}

// removePKCS7Padding 去除PKCS7填充
func removePKCS7Padding(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("数据为空")
	}

	// 获取填充长度
	paddingLen := int(data[len(data)-1])

	// 验证填充长度
	if paddingLen == 0 || paddingLen > len(data) || paddingLen > aes.BlockSize {
		return nil, fmt.Errorf("无效的填充长度: %d", paddingLen)
	}

	// 验证填充字节
	for i := len(data) - paddingLen; i < len(data); i++ {
		if data[i] != byte(paddingLen) {
			return nil, fmt.Errorf("无效的填充字节")
		}
	}

	// 返回去除填充后的数据
	return data[:len(data)-paddingLen], nil
}

// cleanHTMLTags 清理HTML标签
func cleanHTMLTags(text string) string {
	// 移除高亮标签 <span style="color: red;">...</span>
	re := sdsoRe1
	cleaned := re.ReplaceAllString(text, "$1")

	// 移除其他可能的HTML标签
	re2 := sdsoRe2
	cleaned = re2.ReplaceAllString(cleaned, "")

	return strings.TrimSpace(cleaned)
}

// mapPanType 映射网盘类型
func mapPanType(from string) string {
	switch strings.ToLower(from) {
	case "quark":
		return "quark"
	case "xunlei":
		return "xunlei"
	case "aliyun", "ali":
		return "aliyun" // PanSou内部仍使用aliyun标识
	case "baidu":
		return "baidu"
	default:
		return "others"
	}
}

// isValidPanURL 验证是否为有效的网盘链接
func isValidPanURL(url string) bool {
	if url == "" {
		return false
	}

	// 检查是否包含网盘域名特征
	validDomains := []string{
		"pan.quark.cn",
		"pan.xunlei.com",
		"aliyundrive.com",
		"alipan.com",
		"pan.baidu.com",
	}

	urlLower := strings.ToLower(url)
	for _, domain := range validDomains {
		if strings.Contains(urlLower, domain) {
			return true
		}
	}

	return false
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	sdsoRe1 = regexp.MustCompile(`<span[^>]*>(.*?)</span>`)
	sdsoRe2 = regexp.MustCompile(`<[^>]*>`)
)
