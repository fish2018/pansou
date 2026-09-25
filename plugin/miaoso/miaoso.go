package miaoso

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"
)

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewMiaosouPlugin())
}

const (
	// API基础URL
	BaseURL = "https://miaosou.fun/api/secendsearch"

	// 默认参数
	MaxRetries     = 3
	TimeoutSeconds = 30

	// AES解密参数
	AESKey = "4OToScUFOaeVTrHE"
	AESIV  = "9CLGao1vHKqm17Oz"
)

// 预编译的正则表达式
var (
	// HTML标签清理正则
	htmlTagRegex = regexp.MustCompile(`<[^>]*>`)

	// 时间格式解析
	timeLayout = "2006-01-02 15:04:05"
)

// 常用UA列表
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.107 Safari/537.36",
}

// MiaosouPlugin miaoso网盘搜索插件
type MiaosouPlugin struct {
	*plugin.BaseAsyncPlugin
}

// NewMiaosouPlugin 创建新的miaosou插件
func NewMiaosouPlugin() *MiaosouPlugin {
	return &MiaosouPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("miaoso", 3), // 优先级3，标准质量数据源
	}
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *MiaosouPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *MiaosouPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	return p.AsyncSearchWithResult(keyword, p.searchImpl, p.MainCacheKey, ext)
}

// searchImpl 实际的搜索实现
func (p *MiaosouPlugin) searchImpl(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 处理扩展参数
	searchKeyword := keyword
	if ext != nil {
		if titleEn, exists := ext["title_en"]; exists {
			if titleEnStr, ok := titleEn.(string); ok && titleEnStr != "" {
				searchKeyword = titleEnStr
			}
		}
	}

	// 构建请求URL
	searchURL := fmt.Sprintf("%s?name=%s&pageNo=1", BaseURL, url.QueryEscape(searchKeyword))

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), TimeoutSeconds*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建请求失败: %w", p.Name(), err)
	}

	// 设置请求头
	p.setRequestHeaders(req, searchKeyword)

	// 发送HTTP请求（带重试机制）
	resp, err := p.doRequestWithRetry(req, client)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", p.Name(), err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("[%s] 请求返回状态码: %d", p.Name(), resp.StatusCode)
	}

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[%s] 读取响应失败: %w", p.Name(), err)
	}

	// 解析响应
	var apiResp MiaosouResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("[%s] JSON解析失败: %w", p.Name(), err)
	}

	// 检查API响应状态
	if apiResp.Code != 200 {
		return nil, fmt.Errorf("[%s] API错误: %s", p.Name(), apiResp.Msg)
	}

	// 转换为标准格式
	results := make([]model.SearchResult, 0, len(apiResp.Data.List))
	for _, item := range apiResp.Data.List {
		result, err := p.convertToSearchResult(item)
		if err != nil {
			continue // 跳过转换失败的项目
		}

		// 只添加有链接的结果
		if len(result.Links) > 0 {
			results = append(results, result)
		}
	}

	// 关键词过滤
	return results, nil
}

// setRequestHeaders 设置请求头
func (p *MiaosouPlugin) setRequestHeaders(req *http.Request, keyword string) {
	req.Header.Set("User-Agent", userAgents[0])
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("satoken", "503eb9c9-a07f-485c-a659-6c99facbb67f")
	req.Header.Set("Referer", fmt.Sprintf("https://miaosou.fun/info?searchKey=%s", url.QueryEscape(keyword)))
}

// doRequestWithRetry 带重试机制的HTTP请求
func (p *MiaosouPlugin) doRequestWithRetry(req *http.Request, client *http.Client) (*http.Response, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
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

	return nil, fmt.Errorf("重试 %d 次后仍然失败: %w", MaxRetries, lastErr)
}

// convertToSearchResult 将API响应项转换为SearchResult
func (p *MiaosouPlugin) convertToSearchResult(item MiaosouItem) (model.SearchResult, error) {
	// 清理HTML标签
	title := p.cleanHTMLTags(item.Name)
	content := ""
	if item.Content != nil {
		content = *item.Content
	}

	// 解析时间
	datetime, err := time.Parse(timeLayout, item.GmtShare)
	if err != nil {
		datetime = time.Now() // 如果解析失败，使用当前时间
	}

	// 构造链接（这里需要解密URL，目前先保持原样）
	links := []model.Link{}
	if item.URL != "" {
		// 尝试解密URL（这里需要实现具体的解密逻辑）
		decryptedURL := p.decryptURL(item.URL)
		if decryptedURL != "" {
			link := model.Link{
				Type:     p.determineCloudType(item.From),
				URL:      decryptedURL,
				Password: "", // 如果有提取码，需要从其他地方获取
			}
			links = append(links, link)
		}
	}

	// 设置标签
	tags := []string{}
	if item.From != "" {
		tags = append(tags, item.From)
	}
	if item.Type != nil && *item.Type != "" {
		tags = append(tags, *item.Type)
	}

	result := model.SearchResult{
		UniqueID: fmt.Sprintf("%s-%s", p.Name(), item.ID),
		Title:    title,
		Content:  content,
		Datetime: datetime,
		Tags:     tags,
		Links:    links,
		Channel:  "", // 插件搜索结果必须为空字符串
	}

	return result, nil
}

// cleanHTMLTags 清理HTML标签
func (p *MiaosouPlugin) cleanHTMLTags(text string) string {
	// 移除HTML标签
	cleaned := htmlTagRegex.ReplaceAllString(text, "")
	// 清理多余的空格
	cleaned = strings.TrimSpace(cleaned)
	return cleaned
}

// decryptURL 使用AES-CBC解密URL
func (p *MiaosouPlugin) decryptURL(encryptedURL string) string {
	if encryptedURL == "" {
		return ""
	}

	// Base64解码
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedURL)
	if err != nil {
		return ""
	}

	// 准备密钥和IV
	key := []byte(AESKey)
	iv := []byte(AESIV)

	// 创建AES解密器
	block, err := aes.NewCipher(key)
	if err != nil {
		return ""
	}

	// 检查密文长度
	if len(ciphertext) < aes.BlockSize {
		return ""
	}

	// 使用CBC模式解密
	mode := cipher.NewCBCDecrypter(block, iv)

	// 解密
	mode.CryptBlocks(ciphertext, ciphertext)

	// 去除PKCS7填充
	plaintext := p.removePKCS7Padding(ciphertext)
	if plaintext == nil {
		return ""
	}

	return string(plaintext)
}

// removePKCS7Padding 去除PKCS7填充
func (p *MiaosouPlugin) removePKCS7Padding(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}

	// 获取填充长度
	padding := int(data[len(data)-1])

	// 验证填充
	if padding > len(data) || padding > aes.BlockSize {
		return nil
	}

	// 检查填充是否正确
	for i := len(data) - padding; i < len(data); i++ {
		if data[i] != byte(padding) {
			return nil
		}
	}

	// 返回去除填充后的数据
	return data[:len(data)-padding]
}

// determineCloudType 根据平台标识确定网盘类型
func (p *MiaosouPlugin) determineCloudType(from string) string {
	switch strings.ToLower(from) {
	case "quark":
		return "quark"
	case "baidu":
		return "baidu"
	case "uc":
		return "uc"
	case "ali":
		return "aliyun"
	case "xunlei":
		return "xunlei"
	case "tianyi":
		return "tianyi"
	case "115":
		return "115"
	case "123":
		return "123"
	default:
		return "others"
	}
}

// API响应结构定义
type MiaosouResponse struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data MiaosouData `json:"data"`
}

type MiaosouData struct {
	Total int           `json:"total"`
	List  []MiaosouItem `json:"list"`
}

type MiaosouItem struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Type        *string           `json:"type"`
	From        string            `json:"from"`
	Content     *string           `json:"content"`
	GmtCreate   string            `json:"gmtCreate"`
	GmtShare    string            `json:"gmtShare"`
	FileCount   int               `json:"fileCount"`
	CreatorID   *string           `json:"creatorId"`
	CreatorName string            `json:"creatorName"`
	FileInfos   []MiaosouFileInfo `json:"fileInfos"`
}

type MiaosouFileInfo struct {
	Category      *string `json:"category"`
	FileExtension *string `json:"fileExtension"`
	FileID        string  `json:"fileId"`
	FileName      string  `json:"fileName"`
	Type          *string `json:"type"`
}
