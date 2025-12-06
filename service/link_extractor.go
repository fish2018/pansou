package service

import (
	"regexp"
	"strings"

	"pansou/util"
)

// ============================================================
// 链接提取和标题配对逻辑
// ============================================================

// 用于从消息内容中提取链接-标题对应关系的函数
func extractLinkTitlePairs(content string) map[string]string {
	// 首先尝试使用换行符分割的方法
	if strings.Contains(content, "\n") {
		return extractLinkTitlePairsWithNewlines(content)
	}
	
	// 如果没有换行符，使用正则表达式直接提取
	return extractLinkTitlePairsWithoutNewlines(content)
}

// 处理有换行符的情况
func extractLinkTitlePairsWithNewlines(content string) map[string]string {
	// 结果映射：链接URL -> 对应标题
	linkTitleMap := make(map[string]string)
	
	// 按行分割内容
	lines := strings.Split(content, "\n")
	
	// 链接正则表达式
	linkRegex := regexp.MustCompile(`https?://[^\s"']+`)
	
	// 第一遍扫描：识别标题-链接对
	var lastTitle string
	var lastTitleIndex int
	
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		
		// 检查当前行是否包含链接
		links := linkRegex.FindAllString(line, -1)
		
		if len(links) > 0 {
			// 当前行包含链接
			
			// 检查是否是标准链接行（以"链接："、"地址："等开头）
			isStandardLinkLine := isLinkLine(line)
			
			if isStandardLinkLine && lastTitle != "" {
				// 标准链接行，使用上一个标题
				for _, link := range links {
					linkTitleMap[link] = lastTitle
				}
			} else if !isStandardLinkLine {
				// 非标准链接行，可能是"标题：链接"格式
				titleFromLine := extractTitleFromLinkLine(line)
				if titleFromLine != "" {
					// 是"标题：链接"格式
					for _, link := range links {
						linkTitleMap[link] = titleFromLine
					}
				} else if lastTitle != "" {
					// 其他情况，使用上一个标题
					for _, link := range links {
						linkTitleMap[link] = lastTitle
					}
				}
			}
		} else {
			// 当前行不包含链接，可能是标题行
			// 检查下一行是否为链接行
			if i+1 < len(lines) {
				nextLine := strings.TrimSpace(lines[i+1])
				if isLinkLine(nextLine) || linkRegex.MatchString(nextLine) {
					// 下一行是链接行或包含链接，当前行很可能是标题
					lastTitle = cleanTitle(line)
					lastTitleIndex = i
				}
			} else {
				// 最后一行，也可能是标题
				lastTitle = cleanTitle(line)
				lastTitleIndex = i
			}
		}
	}
	
	// 第二遍扫描：处理没有匹配到标题的链接
	// 为每个链接找到最近的上文标题
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		
		links := linkRegex.FindAllString(line, -1)
		if len(links) == 0 {
			continue
		}
		
		for _, link := range links {
			if _, exists := linkTitleMap[link]; !exists {
				// 链接没有匹配到标题，尝试找最近的上文标题
				nearestTitle := ""
				
				// 向上查找最近的标题行
				for j := i - 1; j >= 0; j-- {
					if j == lastTitleIndex || (j+1 < len(lines) && 
						linkRegex.MatchString(lines[j+1]) && 
						!linkRegex.MatchString(lines[j])) {
						candidateTitle := cleanTitle(lines[j])
						if candidateTitle != "" {
							nearestTitle = candidateTitle
							break
						}
					}
				}
				
				if nearestTitle != "" {
					linkTitleMap[link] = nearestTitle
				}
			}
		}
	}
	
	return linkTitleMap
}

// 处理没有换行符的情况
func extractLinkTitlePairsWithoutNewlines(content string) map[string]string {
	// 结果映射：链接URL -> 对应标题
	linkTitleMap := make(map[string]string)
	
	// 使用精确的网盘链接正则表达式集合，避免贪婪匹配
	linkPatterns := []*regexp.Regexp{
		util.TianyiPanPattern,  // 天翼云盘
		util.BaiduPanPattern,   // 百度网盘
		util.QuarkPanPattern,   // 夸克网盘
		util.AliyunPanPattern,  // 阿里云盘
		util.UCPanPattern,      // UC网盘
		util.Pan123Pattern,     // 123网盘
		util.Pan115Pattern,     // 115网盘
		util.XunleiPanPattern,  // 迅雷网盘
	}
	
	// 收集所有链接及其位置
	type linkInfo struct {
		url string
		pos int
	}
	var allLinks []linkInfo
	
	// 使用各个精确正则表达式查找链接
	for _, pattern := range linkPatterns {
		matches := pattern.FindAllString(content, -1)
		for _, match := range matches {
			pos := strings.Index(content, match)
			if pos >= 0 {
				allLinks = append(allLinks, linkInfo{url: match, pos: pos})
			}
		}
	}
	
	// 按位置排序
	for i := 0; i < len(allLinks)-1; i++ {
		for j := i + 1; j < len(allLinks); j++ {
			if allLinks[i].pos > allLinks[j].pos {
				allLinks[i], allLinks[j] = allLinks[j], allLinks[i]
			}
		}
	}
	
	// URL标准化和去重
	uniqueLinks := make(map[string]string) // 标准化URL -> 原始URL
	var links []string
	
	for _, linkInfo := range allLinks {
		// 标准化URL（将URL编码转换为中文）
		normalized := normalizeUrl(linkInfo.url)
		
		// 如果这个标准化URL还没有见过，则保留
		if _, exists := uniqueLinks[normalized]; !exists {
			uniqueLinks[normalized] = linkInfo.url
			links = append(links, linkInfo.url)
		}
	}
	
	if len(links) == 0 {
		return linkTitleMap
	}
	
	// 使用链接位置分割内容
	segments := make([]string, len(links)+1)
	lastPos := 0
	
	// 查找每个链接的位置，并提取链接前的文本作为段落
	for i, link := range links {
		idx := strings.Index(content[lastPos:], link)
		if idx == -1 {
			// 链接在content中不存在，跳过
			continue
		}
		pos := idx + lastPos
		if pos > lastPos {
			segments[i] = content[lastPos:pos]
		}
		lastPos = pos + len(link)
	}
	
	// 最后一段
	if lastPos < len(content) {
		segments[len(links)] = content[lastPos:]
	}
	
	// 从每个段落中提取标题
	for i, link := range links {
		// 当前链接的标题应该在当前段落的末尾
		var title string
		
		// 如果是第一个链接
		if i == 0 {
			// 提取第一个段落作为标题
			title = extractTitleBeforeLink(segments[i])
		} else {
			// 从上一个链接后的文本中提取标题
			title = extractTitleBeforeLink(segments[i])
		}
		
		// 如果提取到了标题，保存链接-标题对应关系
		if title != "" {
			linkTitleMap[link] = title
		}
	}
	
	return linkTitleMap
}

// 从文本中提取链接前的标题
func extractTitleBeforeLink(text string) string {
	// 移除可能的链接前缀词
	text = strings.TrimSpace(text)
	
	// 查找"链接："前的文本作为标题
	if idx := strings.Index(text, "链接："); idx > 0 {
		return cleanTitle(text[:idx])
	}
	
	// 尝试匹配常见的标题模式
	titlePattern := regexp.MustCompile(`([^链地资网\s]+?(?:\([^)]+\))?(?:\s*\d+K)?(?:\s*臻彩)?(?:\s*MAX)?(?:\s*HDR)?(?:\s*更(?:新)?\d+集))$`)
	matches := titlePattern.FindStringSubmatch(text)
	if len(matches) > 1 {
		return cleanTitle(matches[1])
	}
	
	return cleanTitle(text)
}

// 判断一行是否为链接行（主要包含链接的行）
func isLinkLine(line string) bool {
	lowerLine := strings.ToLower(line)
	return strings.HasPrefix(lowerLine, "链接：") || 
		   strings.HasPrefix(lowerLine, "地址：") ||
		   strings.HasPrefix(lowerLine, "资源地址：") ||
		   strings.HasPrefix(lowerLine, "网盘：") ||
		   strings.HasPrefix(lowerLine, "网盘地址：") ||
		   strings.HasPrefix(lowerLine, "链接:")
}

// 从链接行中提取可能的标题
func extractTitleFromLinkLine(line string) string {
	// 处理"标题：链接"格式
	parts := strings.SplitN(line, "：", 2)
	if len(parts) == 2 && !strings.Contains(parts[0], "http") &&
		!isLinkPrefix(parts[0]) {
		return cleanTitle(parts[0])
	}
	
	// 处理"标题:链接"格式（半角冒号）
	parts = strings.SplitN(line, ":", 2)
	if len(parts) == 2 && !strings.Contains(parts[0], "http") &&
		!isLinkPrefix(parts[0]) {
		return cleanTitle(parts[0])
	}
	
	return ""
}

// 判断是否为链接前缀词（包括网盘名称）
func isLinkPrefix(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	
	// 标准链接前缀词
	if text == "链接" || 
	   text == "地址" || 
	   text == "资源地址" || 
	   text == "网盘" || 
	   text == "网盘地址" {
		return true
	}
	
	// 网盘名称（防止误将网盘名称当作标题）
	cloudDiskNames := []string{
		// 夸克网盘
		"夸克", "夸克网盘", "quark", "夸克云盘",
		
		// 百度网盘
		"百度", "百度网盘", "baidu", "百度云", "bdwp", "bdpan",
		
		// 迅雷网盘
		"迅雷", "迅雷网盘", "xunlei", "迅雷云盘",
		
		// 115网盘
		"115", "115网盘", "115云盘",
		
		// 123网盘
		"123", "123pan", "123网盘", "123云盘",
		
		// 阿里云盘
		"阿里", "阿里云", "阿里云盘", "aliyun", "alipan", "阿里网盘",
		
		// 天翼云盘
		"天翼", "天翼云", "天翼云盘", "tianyi", "天翼网盘",
		
		// UC网盘
		"uc", "uc网盘", "uc云盘",
		
		// 移动云盘
		"移动", "移动云", "移动云盘", "caiyun", "彩云",
		
		// PikPak
		"pikpak", "pikpak网盘",
	}
	
	for _, name := range cloudDiskNames {
		if text == name {
			return true
		}
	}
	
	return false
}

// 清理标题文本
func cleanTitle(title string) string {
	// 移除常见的无关前缀
	title = strings.TrimSpace(title)
	title = strings.TrimPrefix(title, "名称：")
	title = strings.TrimPrefix(title, "标题：")
	title = strings.TrimPrefix(title, "片名：")
	title = strings.TrimPrefix(title, "名称:")
	title = strings.TrimPrefix(title, "标题:")
	title = strings.TrimPrefix(title, "片名:")
	
	// 移除表情符号和特殊字符
	emojiRegex := regexp.MustCompile(`[\p{So}\p{Sk}]`)
	title = emojiRegex.ReplaceAllString(title, "")
	
	return strings.TrimSpace(title)
}

// 判断一行是否为空或只包含空白字符
func isEmpty(line string) bool {
	return strings.TrimSpace(line) == ""
}
