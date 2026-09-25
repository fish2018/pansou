package util

import (
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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

// PageParseStatus 表示一次搜索结果页解析的可信程度。
//
// 引入它的原因：0 条结果既可能是"该频道确实没有匹配内容"，也可能是
// "t.me 改版导致解析失效"。两者原先完全不可区分，站点一改版就会静默归零，
// 只能等用户反馈才发现。
type PageParseStatus int

const (
	// ParseStatusOK 正常解析出了消息。
	ParseStatusOK PageParseStatus = iota
	// ParseStatusNoMessages 页面明确给出无结果标记，0 条是可信结果。
	ParseStatusNoMessages
	// ParseStatusStructureChanged 页面含消息块却一条都没解析出来，
	// 通常意味着页面结构已变，需要告警。
	ParseStatusStructureChanged
)

// ParseSearchResults 解析搜索结果页面，只返回结果与翻页参数。
// 第二个返回值是历史遗留的翻页参数占位：该功能从未实现，调用方也都忽略它，
// 保留仅为兼容既有签名，实际恒为空串。
func ParseSearchResults(html string, channel string) ([]model.SearchResult, string, error) {
	results, _, status, err := ParseSearchResultsWithStatus(html, channel)
	_ = status
	return results, "", err
}

// ParseSearchResultsWithStatus 在结果之外额外返回解析可信度，
// 让调用方能区分"频道没有内容"与"解析失效"。
func ParseSearchResultsWithStatus(html string, channel string) ([]model.SearchResult, string, PageParseStatus, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, "", ParseStatusStructureChanged, err
	}

	var results []model.SearchResult
	// recognized 统计"能被识别为一条消息"的块数（通过 data-post 与时间校验）。
	recognized := 0

	doc.Find(".tgme_widget_message_wrap").Each(func(i int, s *goquery.Selection) {
		result, ok, hasResult := parseTgMessage(s, channel)
		if !ok {
			return
		}
		recognized++
		if hasResult {
			results = append(results, result)
		}
	})

	return results, "", judgeParseStatus(doc, recognized, len(results)), nil
}

// judgeParseStatus 判定本次解析的可信度，供调用方区分"频道没有内容"与"解析失效"。
//
// 注意不能用 len(results)==0 作为失效依据：消息识别成功但整条不含受支持的网盘链接时同样得到
// 0 条结果，那是正常页面。只有"页面里明明有消息块，却一个都识别不出来"才说明结构变了。
func judgeParseStatus(doc *goquery.Document, recognized, resultCount int) PageParseStatus {
	if resultCount != 0 {
		return ParseStatusOK
	}
	switch {
	case doc.Find(".tme_no_messages_found").Length() > 0:
		return ParseStatusNoMessages
	case recognized == 0 && doc.Find(".tgme_widget_message_wrap").Length() > 0:
		return ParseStatusStructureChanged
	}
	return ParseStatusOK
}

// parseTgMessage 解析一条消息块。
//
// ok 表示"能识别为一条消息"（口径与原实现里 recognized 的自增条件一致：有 data-post、能拆出
// 消息 ID、有可解析的时间）；hasResult 表示这条消息含有受支持的网盘链接。两者是两件事：
// 识别成功但不含链接的消息会被正常丢弃，不能据此判断页面结构变了。
func parseTgMessage(s *goquery.Selection, channel string) (model.SearchResult, bool, bool) {
	messageDiv := s.Find(".tgme_widget_message")

	// 提取消息ID
	dataPost, exists := messageDiv.Attr("data-post")
	if !exists {
		return model.SearchResult{}, false, false
	}

	parts := strings.Split(dataPost, "/")
	if len(parts) != 2 {
		return model.SearchResult{}, false, false
	}

	messageID := parts[1]

	// 生成全局唯一ID
	uniqueID := channel + "_" + messageID

	// 提取时间
	timeStr, exists := messageDiv.Find(".tgme_widget_message_date time").Attr("datetime")
	if !exists {
		return model.SearchResult{}, false, false
	}

	datetime, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		return model.SearchResult{}, false, false
	}

	// 获取消息文本元素
	messageTextElem := messageDiv.Find(".tgme_widget_message_text")

	// 直接从DOM拼出带换行的正文（<br> 记为换行），不再序列化HTML
	messageTextWithBreaks := messageTextWithBreaks(messageTextElem)

	// 获取消息的纯文本内容
	messageText := messageTextElem.Text()

	// 提取标题
	title := extractTitle(messageTextWithBreaks, messageText)

	links := collectMessageLinks(s, messageText, messageTextWithBreaks)
	tags := extractMessageTags(messageTextElem)
	images := extractMessageImages(messageDiv)

	// 只有包含链接的消息才添加到结果中
	if len(links) == 0 {
		return model.SearchResult{}, true, false
	}

	// 为每个链接提取作品标题
	links = extractWorkTitlesForLinks(links, messageText, title)

	return model.SearchResult{
		MessageID: messageID,
		UniqueID:  uniqueID,
		Channel:   channel,
		Datetime:  datetime,
		Title:     title,
		Content:   messageText,
		Links:     links,
		Tags:      tags,
		Images:    images,
	}, true, true
}

// passwordFor 返回某条链接对应的提取码。
//
// 先用"这条链接自己附近"的短窗口取值（primaryContext），取不到才退回原来的整条扫描逻辑
// （fallbackContext）——**保证不退化**：宁可回到旧行为，也不能因为附近没找到就返回空。
func passwordFor(linkURL, primaryContext, fallbackContext string) string {
	if pw := extractCodeNear(linkURL, primaryContext); pw != "" {
		return pw
	}
	return ExtractPassword(fallbackContext, linkURL)
}

// nearbyPasswordWindow 是"链接附近"的取值窗口长度。
//
// 太短会漏掉写在下一行的提取码，太长会把别的链接的码吃进来。60 足以跨一到两行，
// 配合"遇到下一个链接就截断"的规则，多链接消息里不会互相串码。
const nearbyPasswordWindow = 60

// anchorCandidates 给出一条链接在正文里可能的写法，按"越精确越先试"的顺序。
func anchorCandidates(linkURL string) []string {
	// 正文里写的链接常常不带查询参数，而 href 可能带（如 ?pwd=xxxx），先去查询串
	trimmed := linkURL
	if i := strings.IndexAny(trimmed, "?#"); i > 0 {
		trimmed = trimmed[:i]
	}
	trimmed = strings.TrimRight(trimmed, "/")

	candidates := []string{trimmed}

	// 去掉 scheme：正文里也可能写成不带协议的形式
	noScheme := trimmed
	if i := strings.Index(noScheme, "//"); i >= 0 {
		noScheme = noScheme[i+2:]
	}
	noWWW := strings.TrimPrefix(noScheme, "www.")

	if noWWW != noScheme {
		candidates = append(candidates, noWWW)
	}
	if noScheme != trimmed {
		candidates = append(candidates, noScheme)
	}

	// 最后退到路径末段（如 /s/pan123 里的 pan123）：锚到"这个链接的标识"，不依赖主机写法。
	// 太短的不试——4 个字符以内极容易在正文里撞上别的东西。
	if i := strings.LastIndex(noWWW, "/"); i >= 0 && len(noWWW)-i-1 > 5 {
		candidates = append(candidates, noWWW[i:])
	}

	return candidates
}

// extractCodeNear 在 context 里以 linkURL 为锚，只在它后面一小段找提取码。
//
// 为什么要按锚定位：原来的 ExtractPassword 拿到的是**整条消息**的正文，它按"提取码"切分后
// 返回第一个合法码，与链接本身没有关联。于是一条消息里列了多个网盘链接、各带各的提取码时，
// 所有链接都会拿到第一个码——用错码解不开盘，而且是静默的。
//
// context 里找不到锚点时有两种情况：
//   - context 很短（按钮标签），说明整段都是这条链接的上下文，直接在里面找；
//   - context 很长（整条正文却找不到这个链接），无法判断位置，返回空让调用方回退旧逻辑。
func extractCodeNear(linkURL, context string) string {
	if context == "" {
		return ""
	}

	// 锚点要试多个形态：调用方拿到的 URL 可能已经被规范化过（去 scheme、去 www.、去尾斜杠），
	// 与正文里写的形态不一定一致。实测 123 网盘就是这样——正文写 www.123pan.com，
	// 传进来的是 123pan.com，按原样一个都找不到。
	window := ""
	for _, anchor := range anchorCandidates(linkURL) {
		pos := strings.Index(context, anchor)
		if pos < 0 {
			continue
		}
		rest := context[pos+len(anchor):]
		// 遇到下一个链接就截断：多链接消息里这一段属于当前链接，不能越过下一条
		if next := strings.Index(rest, "http"); next >= 0 {
			rest = rest[:next]
		}
		window = rest
		break
	}

	if window == "" {
		if len(context) <= 2*nearbyPasswordWindow {
			// 短上下文（典型是按钮标签）：它本身就是这条链接的上下文
			window = context
		} else {
			// 长上下文里找不到锚点，无法判断位置，返回空让调用方回退旧逻辑
			return ""
		}
	}

	if len(window) > nearbyPasswordWindow {
		window = window[:nearbyPasswordWindow]
	}

	// 窗口末尾可能正好切断一个多字节字符，按 rune 边界回退，避免拿到半个字
	for len(window) > 0 && !utf8.ValidString(window) {
		window = window[:len(window)-1]
	}

	matches := NearbyPasswordPattern.FindStringSubmatch(window)
	if len(matches) > 1 && isValidPassword(matches[1]) {
		return matches[1]
	}
	return ""
}

// collectMessageLinks 汇总一条消息里的网盘链接。两个来源：正文与行内键盘按钮里的 <a>，
// 以及正文文本中直接写出的裸链接。两者过同一套"按网盘类型归集 + 去重 + 补全密码"的处理。
//
// Telegram 网页版会把 inline keyboard 渲染在 .tgme_widget_message_inline_keyboard 中，
// 它与 .tgme_widget_message_text 是同级节点。因此不能只遍历正文中的 <a>，
// 否则按钮里的网盘链接会被完全忽略。
func collectMessageLinks(scope *goquery.Selection, messageText, textWithBreaks string) []model.Link {
	cand := newLinkCandidates()

	scope.Find(".tgme_widget_message_text a, .tgme_widget_message_inline_keyboard a[href]").Each(func(i int, a *goquery.Selection) {
		href, exists := a.Attr("href")
		if !exists {
			return
		}
		if !isSupportedLink(href) {
			return
		}

		// 某些频道会把提取码写在按钮文字中，因此同时使用正文和按钮标签作为密码提取上下文。
		buttonText := strings.TrimSpace(a.Text())
		passwordContext := messageText
		if buttonText != "" {
			passwordContext += "\n" + buttonText
		}

		// 按钮链接的最近上下文是按钮标签本身；正文里若也写了这个链接，则以正文为准
		cand.add(GetLinkType(href), href, passwordFor(href, buttonText+"\n"+textWithBreaks, passwordContext))
	})

	// 处理从文本中提取的链接。主上下文用**带换行的**正文：ExtractPassword 拿到的是 .Text()，
	// 它会把 <br> 折叠成一行，按锚定位就无从谈起。
	for _, linkURL := range ExtractNetDiskLinks(messageText) {
		cand.add(GetLinkType(linkURL), linkURL, passwordFor(linkURL, textWithBreaks, messageText))
	}

	return cand.finalize()
}

// linkCandidates 收集一条消息里的候选链接。
//
// direct 保存"不需要按密码归并"的链接（保持出现顺序）；byType 按网盘类型存 baseURL 到密码的
// 对应关系——这些类型的链接要等整条消息扫完才能确定该用哪个密码，因此最后统一补全。
type linkCandidates struct {
	direct []model.Link
	found  map[string]bool
	byType map[string]map[string]string
}

func newLinkCandidates() *linkCandidates {
	return &linkCandidates{
		found:  make(map[string]bool),
		byType: make(map[string]map[string]string),
	}
}

// add 把一条候选链接归位。
//
// 原实现在"按钮链接"和"正文裸链接"两处各写了一遍同样的六路分支，两处必须保持一致；
// 合并成一处，避免改一边漏一边。两处语义本来就相同，只有原始 URL 的来源和密码上下文不同。
func (c *linkCandidates) add(linkType, rawURL, password string) {
	byType := c.byType[linkType]
	if byType == nil {
		byType = make(map[string]string)
		c.byType[linkType] = byType
	}

	switch linkType {
	case "baidu":
		// 百度链接要剥掉 ?pwd= 参数：密码单独存，最后再拼回完整形态
		baseURL := rawURL
		if strings.Contains(rawURL, "?pwd=") {
			baseURL = rawURL[:strings.Index(rawURL, "?pwd=")]
		}
		c.setPassword(byType, baseURL, password)
	case "tianyi":
		c.setPassword(byType, CleanTianyiPanURL(rawURL), password)
	case "uc":
		c.setPassword(byType, CleanUCPanURL(rawURL), password)
	case "123":
		c.setPassword(byType, Clean123PanURL(rawURL), password)
	case "115":
		c.setPassword(byType, Clean115PanURL(rawURL), password)
	case "aliyun":
		c.setPassword(byType, CleanAliyunPanURL(rawURL), password)
	default:
		// 非特殊处理的网盘链接直接添加，使用标准化的URL进行去重
		normalizedHref := normalizeUrl(rawURL)
		if !c.found[normalizedHref] {
			c.found[normalizedHref] = true
			c.direct = append(c.direct, model.Link{
				Type:     linkType,
				URL:      normalizedHref,
				Password: password,
			})
		}
	}
}

// setPassword 记录 baseURL 对应的密码。没有密码时也保留条目——
// 无密码的分享链接同样有效，不能因为没有提取码就把链接丢掉。
func (c *linkCandidates) setPassword(byType map[string]string, baseURL, password string) {
	if password != "" {
		byType[baseURL] = password
		return
	}
	if _, exists := byType[baseURL]; !exists {
		byType[baseURL] = ""
	}
}

// finalize 产出最终链接列表：先直接添加的，再按网盘类型补全密码后的。
//
// 类型顺序与原实现的九段循环一致：百度 → 天翼 → UC → 123 → 115 → 阿里云。
// 同一类型内部多条链接的顺序本来就是 map 随机序（原实现相同），不额外保证。
func (c *linkCandidates) finalize() []model.Link {
	links := c.direct

	specs := []struct {
		typ       string
		normalize func(baseURL, password string) string
	}{
		{"baidu", normalizeBaiduPanURL},
		{"tianyi", normalizeTianyiPanURL},
		{"uc", normalizeUCPanURL},
		{"123", normalize123PanURL},
		{"115", normalize115PanURL},
		// 阿里云盘URL通常不包含密码参数
		{"aliyun", func(baseURL, _ string) string { return CleanAliyunPanURL(baseURL) }},
	}

	for _, spec := range specs {
		for baseURL, password := range c.byType[spec.typ] {
			normalizedURL := spec.normalize(baseURL, password)

			// 确保链接不重复
			if !c.found[normalizedURL] {
				c.found[normalizedURL] = true
				links = append(links, model.Link{
					Type:     spec.typ,
					URL:      normalizedURL,
					Password: password,
				})
			}
		}
	}

	return links
}

// extractMessageTags 提取消息里的标签（形如 #标签 的站内搜索链接）。
func extractMessageTags(messageTextElem *goquery.Selection) []string {
	var tags []string
	messageTextElem.Find("a[href^='?q=%23']").Each(func(i int, a *goquery.Selection) {
		tag := a.Text()
		if strings.HasPrefix(tag, "#") {
			tags = append(tags, tag[1:])
		}
	})
	return tags
}

// extractMessageImages 提取消息内容区的图片，排除用户头像。
func extractMessageImages(messageDiv *goquery.Selection) []string {
	var images []string
	foundImages := make(map[string]bool)

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

	return images
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

// messageTextWithBreaks 从消息正文节点拼出带换行的纯文本：<br> 记为换行，
// 注释丢弃，其它元素取子文本。
//
// 这条路径取代了原先的四步写法——先把正文序列化成 HTML(Html())，
// 再用正则把 <br> 换成换行，再去掉所有标签，最后解一次实体。序列化本身
// 就占解析路径 11.7% 的分配，而结果只是同一棵 DOM 的文本投影。
func messageTextWithBreaks(sel *goquery.Selection) string {
	var b strings.Builder

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				// 解析器已经把字符引用解码进 Data，无需再解实体
				b.WriteString(c.Data)
			case html.ElementNode:
				if strings.EqualFold(c.Data, "br") {
					b.WriteByte('\n')
					continue
				}
				walk(c)
			}
			// 注释、doctype 等节点一律丢弃，与原实现一致
		}
	}

	for _, n := range sel.Nodes {
		walk(n)
	}
	return b.String()
}

// extractTitle 从带换行的正文文本中提取标题。
// textWithBreaks 由 messageTextWithBreaks 从 DOM 直接拼出（<br> 已是换行符），
// textContent 是同一节点的纯文本，仅在正文为空时作为兜底。
func extractTitle(textWithBreaks string, textContent string) string {
	// 按行解析正文。部分频道第一行是“📅 9月9日”之类的日期头，
	// 真正的作品名在下一行；如果把日期当标题，服务层的关键词过滤
	// 会把已经提取到的按钮链接全部过滤掉。
	if textWithBreaks != "" {
		for _, line := range strings.Split(textWithBreaks, "\n") {
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

	// 正文为空时回落到纯文本内容
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
