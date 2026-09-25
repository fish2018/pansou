package util

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"pansou/model"
)

// normalizeUrl 标准化URL，将URL编码的中文部分解码为中文，用于去重
func normalizeUrl(rawUrl string) string {
	// 解码URL中的编码字符
	decoded, err := url.QueryUnescape(rawUrl)
	if err != nil {
		// 如果解码失败，返回原始URL
		return rawUrl
	}
	return decoded
}

// isSupportedLink 检查链接是否为支持的网盘链接
func isSupportedLink(url string) bool {
	lowerURL := strings.ToLower(url)

	// 检查是否为百度网盘链接
	if BaiduPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为天翼云盘链接
	if TianyiPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为UC网盘链接
	if UCPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为123网盘链接
	if Pan123Pattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为光鸭云盘链接
	if GuangyaPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为夸克网盘链接
	if QuarkPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为迅雷网盘链接
	if XunleiPanPattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为115网盘链接
	if Pan115Pattern.MatchString(lowerURL) {
		return true
	}

	// 检查是否为移动云盘链接
	if MobilePanPattern.MatchString(lowerURL) {
		return true
	}

	// 使用通用模式检查其他网盘链接
	return AllPanLinksPattern.MatchString(lowerURL)
}

// normalizeBaiduPanURL 标准化百度网盘URL，确保链接格式正确并且包含密码参数
func normalizeBaiduPanURL(url string, password string) string {
	// 清理URL，确保获取正确的链接部分
	url = CleanBaiduPanURL(url)

	// 如果URL已经包含pwd参数，不需要再添加
	if strings.Contains(url, "?pwd=") {
		return url
	}

	// 如果有提取到密码，且URL不包含pwd参数，则添加
	if password != "" {
		// 确保密码是4位
		if len(password) > 4 {
			password = password[:4]
		}
		return url + "?pwd=" + password
	}

	return url
}

// normalizeTianyiPanURL 标准化天翼云盘URL，确保链接格式正确
func normalizeTianyiPanURL(url string, password string) string {
	// 清理URL，确保获取正确的链接部分
	url = CleanTianyiPanURL(url)

	// 天翼云盘链接通常不在URL中包含密码参数，所以这里不做处理
	// 但是我们确保返回的是干净的链接
	return url
}

// normalizeUCPanURL 标准化UC网盘URL，确保链接格式正确
func normalizeUCPanURL(url string, password string) string {
	// 清理URL，确保获取正确的链接部分
	url = CleanUCPanURL(url)

	// UC网盘链接通常使用?public=1参数表示公开分享
	// 确保链接格式正确，但不添加密码参数
	return url
}

// normalize123PanURL 标准化123网盘URL，确保链接格式正确
func normalize123PanURL(url string, password string) string {
	// 清理URL，确保获取正确的链接部分
	url = Clean123PanURL(url)

	// 123网盘链接通常不在URL中包含密码参数
	// 但是我们确保返回的是干净的链接
	return url
}

// normalize115PanURL 标准化115网盘URL，确保链接格式正确
func normalize115PanURL(url string, password string) string {
	// 清理URL，确保获取正确的链接部分，只保留到password=后面4位密码
	url = Clean115PanURL(url)

	// 115网盘链接已经在Clean115PanURL中处理了密码部分
	// 这里不需要额外添加密码参数
	return url
}

// ParseSearchResults 解析搜索结果页面
func ParseSearchResults(html string, channel string) ([]model.SearchResult, string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, "", err
	}

	var results []model.SearchResult
	var nextPageParam string

	// 查找消息块
	doc.Find(".tgme_widget_message_wrap").Each(func(i int, s *goquery.Selection) {
		messageDiv := s.Find(".tgme_widget_message")

		// 提取消息ID
		dataPost, exists := messageDiv.Attr("data-post")
		if !exists {
			return
		}

		parts := strings.Split(dataPost, "/")
		if len(parts) != 2 {
			return
		}

		messageID := parts[1]

		// 生成全局唯一ID
		uniqueID := channel + "_" + messageID

		// 提取时间
		timeStr, exists := messageDiv.Find(".tgme_widget_message_date time").Attr("datetime")
		if !exists {
			return
		}

		datetime, err := time.Parse(time.RFC3339, timeStr)
		if err != nil {
			return
		}

		// 获取消息文本元素
		messageTextElem := messageDiv.Find(".tgme_widget_message_text")

		// 获取消息文本的HTML内容
		messageHTML, _ := messageTextElem.Html()

		// 获取消息的纯文本内容
		messageText := messageTextElem.Text()

		// 提取标题
		title := extractTitle(messageHTML, messageText)

		// 提取网盘链接 - 使用更精确的方法
		var links []model.Link
		var foundLinks = make(map[string]bool)            // 用于去重
		var baiduLinkPasswords = make(map[string]string)  // 存储百度链接和对应的密码
		var tianyiLinkPasswords = make(map[string]string) // 存储天翼链接和对应的密码
		var ucLinkPasswords = make(map[string]string)     // 存储UC链接和对应的密码
		var pan123LinkPasswords = make(map[string]string) // 存储123网盘链接和对应的密码
		var pan115LinkPasswords = make(map[string]string) // 存储115网盘链接和对应的密码
		var aliyunLinkPasswords = make(map[string]string) // 存储阿里云盘链接和对应的密码

		// 1. 从文本内容中提取所有网盘链接和密码
		extractedLinks := ExtractNetDiskLinks(messageText)

		// 2. 从消息正文和行内键盘按钮中提取链接
		//
		// Telegram 网页版会把 inline keyboard 渲染在
		// .tgme_widget_message_inline_keyboard 中，它与
		// .tgme_widget_message_text 是同级节点。因此不能只遍历正文中的
		// <a>，否则按钮里的网盘链接会被完全忽略。
		s.Find(".tgme_widget_message_text a, .tgme_widget_message_inline_keyboard a[href]").Each(func(i int, a *goquery.Selection) {
			href, exists := a.Attr("href")
			if !exists {
				return
			}

			// 使用更精确的方式匹配网盘链接
			if isSupportedLink(href) {
				linkType := GetLinkType(href)
				// 某些频道会把提取码写在按钮文字中，因此同时使用正文和
				// 按钮标签作为密码提取上下文。
				passwordContext := messageText
				if buttonText := strings.TrimSpace(a.Text()); buttonText != "" {
					passwordContext += "\n" + buttonText
				}
				password := ExtractPassword(passwordContext, href)

				// 如果是百度网盘链接，记录链接和密码的对应关系
				if linkType == "baidu" {
					// 提取链接的基本部分（不含密码参数）
					baseURL := href
					if strings.Contains(href, "?pwd=") {
						baseURL = href[:strings.Index(href, "?pwd=")]
					}

					// 记录密码
					if password != "" {
						baiduLinkPasswords[baseURL] = password
					} else if _, exists := baiduLinkPasswords[baseURL]; !exists {
						// 即使没有密码，也保留无密码的百度分享链接。
						baiduLinkPasswords[baseURL] = ""
					}
				} else if linkType == "tianyi" {
					// 如果是天翼云盘链接，记录链接和密码的对应关系
					baseURL := CleanTianyiPanURL(href)

					// 记录密码
					if password != "" {
						tianyiLinkPasswords[baseURL] = password
					} else {
						// 即使没有密码，也添加到映射中，以便后续处理
						if _, exists := tianyiLinkPasswords[baseURL]; !exists {
							tianyiLinkPasswords[baseURL] = ""
						}
					}
				} else if linkType == "uc" {
					// 如果是UC网盘链接，记录链接和密码的对应关系
					baseURL := CleanUCPanURL(href)

					// 记录密码
					if password != "" {
						ucLinkPasswords[baseURL] = password
					} else {
						// 即使没有密码，也添加到映射中，以便后续处理
						if _, exists := ucLinkPasswords[baseURL]; !exists {
							ucLinkPasswords[baseURL] = ""
						}
					}
				} else if linkType == "123" {
					// 如果是123网盘链接，记录链接和密码的对应关系
					baseURL := Clean123PanURL(href)

					// 记录密码
					if password != "" {
						pan123LinkPasswords[baseURL] = password
					} else {
						// 即使没有密码，也添加到映射中，以便后续处理
						if _, exists := pan123LinkPasswords[baseURL]; !exists {
							pan123LinkPasswords[baseURL] = ""
						}
					}
				} else if linkType == "115" {
					// 如果是115网盘链接，记录链接和密码的对应关系
					baseURL := Clean115PanURL(href)

					// 记录密码
					if password != "" {
						pan115LinkPasswords[baseURL] = password
					} else {
						// 即使没有密码，也添加到映射中，以便后续处理
						if _, exists := pan115LinkPasswords[baseURL]; !exists {
							pan115LinkPasswords[baseURL] = ""
						}
					}
				} else if linkType == "aliyun" {
					// 如果是阿里云盘链接，记录链接和密码的对应关系
					baseURL := CleanAliyunPanURL(href)

					// 记录密码
					if password != "" {
						aliyunLinkPasswords[baseURL] = password
					} else {
						// 即使没有密码，也添加到映射中，以便后续处理
						if _, exists := aliyunLinkPasswords[baseURL]; !exists {
							aliyunLinkPasswords[baseURL] = ""
						}
					}
				} else {
					// 非特殊处理的网盘链接直接添加
					// 使用标准化的URL进行去重
					normalizedHref := normalizeUrl(href)
					if !foundLinks[normalizedHref] {
						foundLinks[normalizedHref] = true
						links = append(links, model.Link{
							Type:     linkType,
							URL:      normalizedHref, // 使用标准化的URL
							Password: password,
						})
					}
				}
			}
		})

		// 3. 处理从文本中提取的链接
		for _, linkURL := range extractedLinks {
			linkType := GetLinkType(linkURL)
			password := ExtractPassword(messageText, linkURL)

			// 如果是百度网盘链接，记录链接和密码的对应关系
			if linkType == "baidu" {
				// 提取链接的基本部分（不含密码参数）
				baseURL := linkURL
				if strings.Contains(linkURL, "?pwd=") {
					baseURL = linkURL[:strings.Index(linkURL, "?pwd=")]
				}

				// 记录密码
				if password != "" {
					baiduLinkPasswords[baseURL] = password
				} else if _, exists := baiduLinkPasswords[baseURL]; !exists {
					// 即使没有密码，也保留无密码的百度分享链接。
					baiduLinkPasswords[baseURL] = ""
				}
			} else if linkType == "tianyi" {
				// 如果是天翼云盘链接，记录链接和密码的对应关系
				baseURL := CleanTianyiPanURL(linkURL)

				// 记录密码
				if password != "" {
					tianyiLinkPasswords[baseURL] = password
				} else {
					// 即使没有密码，也添加到映射中，以便后续处理
					if _, exists := tianyiLinkPasswords[baseURL]; !exists {
						tianyiLinkPasswords[baseURL] = ""
					}
				}
			} else if linkType == "uc" {
				// 如果是UC网盘链接，记录链接和密码的对应关系
				baseURL := CleanUCPanURL(linkURL)

				// 记录密码
				if password != "" {
					ucLinkPasswords[baseURL] = password
				} else {
					// 即使没有密码，也添加到映射中，以便后续处理
					if _, exists := ucLinkPasswords[baseURL]; !exists {
						ucLinkPasswords[baseURL] = ""
					}
				}
			} else if linkType == "123" {
				// 如果是123网盘链接，记录链接和密码的对应关系
				baseURL := Clean123PanURL(linkURL)

				// 记录密码
				if password != "" {
					pan123LinkPasswords[baseURL] = password
				} else {
					// 即使没有密码，也添加到映射中，以便后续处理
					if _, exists := pan123LinkPasswords[baseURL]; !exists {
						pan123LinkPasswords[baseURL] = ""
					}
				}
			} else if linkType == "115" {
				// 如果是115网盘链接，记录链接和密码的对应关系
				baseURL := Clean115PanURL(linkURL)

				// 记录密码
				if password != "" {
					pan115LinkPasswords[baseURL] = password
				} else {
					// 即使没有密码，也添加到映射中，以便后续处理
					if _, exists := pan115LinkPasswords[baseURL]; !exists {
						pan115LinkPasswords[baseURL] = ""
					}
				}
			} else if linkType == "aliyun" {
				// 如果是阿里云盘链接，记录链接和密码的对应关系
				baseURL := CleanAliyunPanURL(linkURL)

				// 记录密码
				if password != "" {
					aliyunLinkPasswords[baseURL] = password
				} else {
					// 即使没有密码，也添加到映射中，以便后续处理
					if _, exists := aliyunLinkPasswords[baseURL]; !exists {
						aliyunLinkPasswords[baseURL] = ""
					}
				}
			} else {
				// 非特殊处理的网盘链接直接添加
				// 使用标准化的URL进行去重
				normalizedLinkURL := normalizeUrl(linkURL)
				if !foundLinks[normalizedLinkURL] {
					foundLinks[normalizedLinkURL] = true
					links = append(links, model.Link{
						Type:     linkType,
						URL:      normalizedLinkURL, // 使用标准化的URL
						Password: password,
					})
				}
			}
		}

		// 4. 处理百度网盘链接，确保每个链接只有一个版本（带密码的完整版本）
		for baseURL, password := range baiduLinkPasswords {
			normalizedURL := normalizeBaiduPanURL(baseURL, password)

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "baidu",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 5. 处理天翼云盘链接，确保每个链接只有一个版本
		for baseURL, password := range tianyiLinkPasswords {
			normalizedURL := normalizeTianyiPanURL(baseURL, password)

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "tianyi",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 6. 处理UC网盘链接，确保每个链接只有一个版本
		for baseURL, password := range ucLinkPasswords {
			normalizedURL := normalizeUCPanURL(baseURL, password)

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "uc",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 7. 处理123网盘链接，确保每个链接只有一个版本
		for baseURL, password := range pan123LinkPasswords {
			normalizedURL := normalize123PanURL(baseURL, password)

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "123",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 8. 处理115网盘链接，确保每个链接只有一个版本
		for baseURL, password := range pan115LinkPasswords {
			normalizedURL := normalize115PanURL(baseURL, password)

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "115",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 9. 处理阿里云盘链接，确保每个链接只有一个版本
		for baseURL, password := range aliyunLinkPasswords {
			normalizedURL := CleanAliyunPanURL(baseURL) // 阿里云盘URL通常不包含密码参数

			// 确保链接不重复
			if !foundLinks[normalizedURL] {
				foundLinks[normalizedURL] = true
				links = append(links, model.Link{
					Type:     "aliyun",
					URL:      normalizedURL,
					Password: password,
				})
			}
		}

		// 提取标签
		var tags []string
		messageTextElem.Find("a[href^='?q=%23']").Each(func(i int, a *goquery.Selection) {
			tag := a.Text()
			if strings.HasPrefix(tag, "#") {
				tags = append(tags, tag[1:])
			}
		})

		// 提取图片链接（只从消息内容区域提取，排除用户头像）
		var images []string
		var foundImages = make(map[string]bool) // 用于去重

		// 获取消息气泡区域，排除用户头像区域
		messageBubble := messageDiv.Find(".tgme_widget_message_bubble")

		// 1. 从消息内容中的图片包装元素提取图片
		messageBubble.Find(".tgme_widget_message_photo_wrap").Each(func(i int, photoWrap *goquery.Selection) {
			// 检查style属性中的background-image
			style, exists := photoWrap.Attr("style")
			if exists {
				imageURL := extractImageURLFromStyle(style)
				if imageURL != "" && !foundImages[imageURL] {
					foundImages[imageURL] = true
					images = append(images, imageURL)
				}
			}
		})

		// 2. 从消息内容中的其他可能包含图片的元素提取（排除用户头像）
		messageBubble.Find("img").Each(func(i int, img *goquery.Selection) {
			src, exists := img.Attr("src")
			if exists && src != "" && !foundImages[src] {
				foundImages[src] = true
				images = append(images, src)
			}
		})

		// 只有包含链接的消息才添加到结果中
		if len(links) > 0 {
			// 为每个链接提取作品标题
			links = extractWorkTitlesForLinks(links, messageText, title)

			results = append(results, model.SearchResult{
				MessageID: messageID,
				UniqueID:  uniqueID,
				Channel:   channel,
				Datetime:  datetime,
				Title:     title,
				Content:   messageText,
				Links:     links,
				Tags:      tags,
				Images:    images,
			})
		}
	})

	return results, nextPageParam, nil
}

// CutTitleByKeywords 根据关键词进行裁剪，保留最前关键词前的部分
func CutTitleByKeywords(title string, keywords []string) string {
	minIdx := -1
	for _, kw := range keywords {
		if idx := strings.Index(title, kw); idx >= 0 && (minIdx == -1 || idx < minIdx) {
			minIdx = idx
		}
	}
	if minIdx > 0 {
		return strings.TrimSpace(title[:minIdx])
	}
	return strings.TrimSpace(title)
}

// extractImageURLFromStyle 从CSS样式字符串中提取background-image的URL
func extractImageURLFromStyle(style string) string {
	// 查找background-image:url('...') 或 background-image:url("...")
	startPattern := "background-image:url('"
	endPattern := "')"

	startIndex := strings.Index(style, startPattern)
	if startIndex != -1 {
		startIndex += len(startPattern)
		endIndex := strings.Index(style[startIndex:], endPattern)
		if endIndex != -1 {
			return style[startIndex : startIndex+endIndex]
		}
	}

	// 尝试双引号格式
	startPattern = `background-image:url("`
	endPattern = `")`

	startIndex = strings.Index(style, startPattern)
	if startIndex != -1 {
		startIndex += len(startPattern)
		endIndex := strings.Index(style[startIndex:], endPattern)
		if endIndex != -1 {
			return style[startIndex : startIndex+endIndex]
		}
	}

	// 尝试无引号格式
	startPattern = "background-image:url("
	endPattern = ")"

	startIndex = strings.Index(style, startPattern)
	if startIndex != -1 {
		startIndex += len(startPattern)
		endIndex := strings.Index(style[startIndex:], endPattern)
		if endIndex != -1 {
			url := style[startIndex : startIndex+endIndex]
			// 移除可能的引号
			url = strings.Trim(url, "'\"")
			return url
		}
	}

	return ""
}

// extractTitle 从消息HTML和文本内容中提取标题
// htmlToPlainText 把一段 HTML 片段转成纯文本：剥离标签、丢弃注释、解码实体。
// 语义等价于"为这段 HTML 建一个 goquery 文档再取 Text()"，但不需要建 DOM——
// extractTitle 原先对每条消息都做一次这种建树，是解析路径里最浪费的一步。
func htmlToPlainText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '<' {
			// 注释整体丢弃，与解析器的行为一致
			if strings.HasPrefix(s[i:], "<!--") {
				end := strings.Index(s[i:], "-->")
				if end < 0 {
					break
				}
				i += end + 3
				continue
			}
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return html.UnescapeString(b.String())
}

func extractTitle(htmlContent string, textContent string) string {
	// 按 <br> 分行解析 HTML。部分频道第一行是“📅 9月9日”之类的
	// 日期头，真正的作品名在下一行；如果把日期当标题，服务层的
	// 关键词过滤会把已经提取到的按钮链接全部过滤掉。
	if htmlContent != "" {
		htmlWithNewlines := brTagPattern.ReplaceAllString(htmlContent, "\n")
		for _, line := range strings.Split(htmlToPlainText(htmlWithNewlines), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || isTelegramDateHeader(line) || isTitleMetadataLine(line) {
				continue
			}
			if strings.HasPrefix(line, "名称：") {
				return strings.TrimSpace(line[len("名称："):])
			}
			if strings.HasPrefix(line, "#") && !strings.Contains(line, "名称") {
				continue
			}
			return CutTitleByKeywords(line, []string{"简介", "描述"})
		}
	}

	// 如果HTML解析失败，则使用纯文本内容
	lines := strings.Split(textContent, "\n")
	if len(lines) == 0 {
		return ""
	}

	// 第一行通常是标题
	firstLine := strings.TrimSpace(lines[0])

	// 如果第一行只是标签(以#开头且不包含实际内容)，尝试从第二行或"名称："字段提取
	if strings.HasPrefix(firstLine, "#") {
		// 检查是否有"名称："字段
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "名称：") {
				return strings.TrimSpace(line[len("名称："):])
			}
		}

		// 如果没有"名称："字段，尝试使用第二行
		if len(lines) > 1 {
			secondLine := strings.TrimSpace(lines[1])
			if strings.HasPrefix(secondLine, "名称：") {
				return strings.TrimSpace(secondLine[len("名称："):])
			}
			// 如果第二行不是空的且不是标签，使用第二行
			if secondLine != "" && !strings.HasPrefix(secondLine, "#") {
				result := secondLine
				result = CutTitleByKeywords(result, []string{"简介", "描述"})
				return result
			}
		}
	}

	// 如果第一行以"名称："开头，则提取冒号后面的内容作为标题
	if strings.HasPrefix(firstLine, "名称：") {
		return strings.TrimSpace(firstLine[len("名称："):])
	}

	// 否则直接使用第一行作为标题
	result := firstLine
	// 统一裁剪：遇到简介/描述等关键字时，只保留前半部分
	result = CutTitleByKeywords(result, []string{"简介", "描述"})
	return result
}

var brTagPattern = regexp.MustCompile(`(?i)<br\s*/?>`)
var telegramDateHeaderPattern = regexp.MustCompile(`^📅?\s*\d{1,4}(?:年\d{1,2}月\d{1,2}日|[-/.]\d{1,2}[-/.]\d{1,2})$|^📅?\s*\d{1,2}月\d{1,2}日$`)

func isTelegramDateHeader(line string) bool {
	return telegramDateHeaderPattern.MatchString(strings.TrimSpace(line))
}

func isTitleMetadataLine(line string) bool {
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"类型：", "分享：", "网盘：", "简介：", "描述：", "更多资源："} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// extractWorkTitlesForLinks 为每个链接提取作品标题
func extractWorkTitlesForLinks(links []model.Link, messageText string, defaultTitle string) []model.Link {
	if len(links) == 0 {
		return links
	}

	// 如果链接数量 <= 4，认为是同一个作品的不同网盘链接
	if len(links) <= 4 {
		for i := range links {
			links[i].WorkTitle = defaultTitle
		}
		return links
	}

	// 如果链接数量 > 4，尝试为每个链接匹配具体的作品标题
	lines := strings.Split(messageText, "\n")

	// 检测是否是单行格式："作品名丨网盘：链接" 或 "作品名 网盘：链接"
	if isSingleLineFormat(lines) {
		return extractWorkTitlesFromSingleLineFormat(links, lines, defaultTitle)
	}

	// 其他格式：尝试通过上下文匹配
	return extractWorkTitlesFromContext(links, messageText, defaultTitle)
}

// isSingleLineFormat 检测是否是单行格式
func isSingleLineFormat(lines []string) bool {
	singleLineCount := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 检测是否包含："作品名丨网盘：链接" 或类似格式
		if strings.Contains(line, "丨") && strings.Contains(line, "：") && (strings.Contains(line, "http://") || strings.Contains(line, "https://")) {
			singleLineCount++
		}
	}

	// 如果超过一半的行都符合单行格式，则认为是单行格式
	return singleLineCount > len(lines)/3
}

// extractWorkTitlesFromSingleLineFormat 从单行格式中提取作品标题
func extractWorkTitlesFromSingleLineFormat(links []model.Link, lines []string, defaultTitle string) []model.Link {
	// 为每个链接构建URL到作品标题的映射
	urlToWorkTitle := make(map[string]string)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 匹配格式: "作品名丨网盘名：链接" 或 "作品名 网盘名：链接"
		// 提取作品名和链接
		var workTitle string
		var linkURL string

		// 优先匹配 "作品名丨网盘：链接" 格式
		if strings.Contains(line, "丨") {
			parts := strings.Split(line, "丨")
			if len(parts) >= 2 {
				workTitle = strings.TrimSpace(parts[0])
				// 从第二部分提取链接
				restPart := parts[1]
				if idx := strings.Index(restPart, "http"); idx >= 0 {
					linkURL = extractFirstURL(restPart[idx:])
				}
			}
		} else if strings.Contains(line, "：") {
			// 匹配 "作品名 网盘：链接" 格式
			colonIdx := strings.Index(line, "：")
			if colonIdx > 0 {
				beforeColon := line[:colonIdx]
				afterColon := line[colonIdx+len("："):]

				// 尝试从冒号前提取作品名（去除网盘名）
				workTitle = extractWorkTitleBeforeColon(beforeColon)

				// 从冒号后提取链接
				if idx := strings.Index(afterColon, "http"); idx >= 0 {
					linkURL = extractFirstURL(afterColon[idx:])
				}
			}
		}

		// 如果成功提取了作品名和链接，添加到映射
		if workTitle != "" && linkURL != "" {
			// 标准化URL用于匹配
			normalizedURL := normalizeUrl(linkURL)
			urlToWorkTitle[normalizedURL] = workTitle
		}
	}

	// 为每个链接设置作品标题
	for i := range links {
		normalizedURL := normalizeUrl(links[i].URL)
		if workTitle, found := urlToWorkTitle[normalizedURL]; found {
			links[i].WorkTitle = workTitle
		} else {
			links[i].WorkTitle = defaultTitle
		}
	}

	return links
}

// extractFirstURL 从文本中提取第一个URL
func extractFirstURL(text string) string {
	// 提取到空格或换行符为止
	endIdx := len(text)
	if idx := strings.Index(text, " "); idx > 0 && idx < endIdx {
		endIdx = idx
	}
	if idx := strings.Index(text, "\n"); idx > 0 && idx < endIdx {
		endIdx = idx
	}
	if idx := strings.Index(text, "\r"); idx > 0 && idx < endIdx {
		endIdx = idx
	}

	return strings.TrimSpace(text[:endIdx])
}

// extractWorkTitleBeforeColon 从冒号前的文本中提取作品名
func extractWorkTitleBeforeColon(text string) string {
	text = strings.TrimSpace(text)

	// 移除常见的网盘名称
	netdiskNames := []string{
		"夸克网盘", "夸克云盘", "夸克",
		"百度网盘", "百度云盘", "百度云", "百度",
		"迅雷网盘", "迅雷云盘", "迅雷",
		"阿里云盘", "阿里网盘", "阿里云", "阿里",
		"天翼云盘", "天翼网盘", "天翼云", "天翼",
		"UC网盘", "UC云盘", "UC",
		"移动云盘", "移动云", "移动",
		"115网盘", "115云盘", "115",
		"123网盘", "123云盘", "123",
		"PikPak网盘", "PikPak",
		"网盘", "云盘",
	}

	// 从右向左移除网盘名称
	for _, name := range netdiskNames {
		if strings.HasSuffix(text, name) {
			text = strings.TrimSpace(text[:len(text)-len(name)])
			break
		}
	}

	return text
}

// extractWorkTitlesFromContext 通过上下文为链接提取作品标题
func extractWorkTitlesFromContext(links []model.Link, messageText string, defaultTitle string) []model.Link {
	// 简单实现：如果无法精确匹配，则都使用默认标题
	for i := range links {
		links[i].WorkTitle = defaultTitle
	}
	return links
}
