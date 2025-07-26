package service

import (
	"context"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
	"time"

	"pansou/config"
	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/cache"
	"pansou/util/pool"
	"sync"
	"regexp"
)

// 优先关键词列表
var priorityKeywords = []string{"合集", "系列", "全", "完", "最新", "附"}

// 全局缓存实例和缓存是否初始化标志
var (
	twoLevelCache    *cache.TwoLevelCache
	enhancedTwoLevelCache *cache.EnhancedTwoLevelCache
	cacheInitialized bool
)

// 初始化缓存
func init() {
	if config.AppConfig != nil && config.AppConfig.CacheEnabled {
		var err error
		// 优先使用增强版缓存
		enhancedTwoLevelCache, err = cache.NewEnhancedTwoLevelCache()
		if err == nil {
			cacheInitialized = true
			return
		}
		
		// 如果增强版缓存初始化失败，回退到原始缓存
		twoLevelCache, err = cache.NewTwoLevelCache()
		if err == nil {
			cacheInitialized = true
		}
	}
}

// SearchService 搜索服务
type SearchService struct {
	pluginManager *plugin.PluginManager
}

// NewSearchService 创建搜索服务实例并确保缓存可用
func NewSearchService(pluginManager *plugin.PluginManager) *SearchService {
	// 检查缓存是否已初始化，如果未初始化则尝试重新初始化
	if !cacheInitialized && config.AppConfig != nil && config.AppConfig.CacheEnabled {
		var err error
		// 优先使用增强版缓存
		enhancedTwoLevelCache, err = cache.NewEnhancedTwoLevelCache()
		if err == nil {
			cacheInitialized = true
		} else {
			// 如果增强版缓存初始化失败，回退到原始缓存
			twoLevelCache, err = cache.NewTwoLevelCache()
			if err == nil {
				cacheInitialized = true
			}
		}
	}
	
	// 将主缓存注入到异步插件中
	injectMainCacheToAsyncPlugins(pluginManager, enhancedTwoLevelCache)

	return &SearchService{
		pluginManager: pluginManager,
	}
}

// injectMainCacheToAsyncPlugins 将主缓存系统注入到异步插件中
func injectMainCacheToAsyncPlugins(pluginManager *plugin.PluginManager, mainCache *cache.EnhancedTwoLevelCache) {
	// 如果缓存或插件管理器不可用，直接返回
	if mainCache == nil || pluginManager == nil {
		return
	}
	
	// 创建缓存更新函数
	cacheUpdater := func(key string, data []byte, ttl time.Duration) error {
		return mainCache.Set(key, data, ttl)
	}
	
	// 获取所有插件
	plugins := pluginManager.GetPlugins()
	
	// 遍历所有插件，找出异步插件
	for _, p := range plugins {
		// 检查插件是否实现了SetMainCacheUpdater方法
		if asyncPlugin, ok := p.(interface{ SetMainCacheUpdater(func(string, []byte, time.Duration) error) }); ok {
			// 注入缓存更新函数
			asyncPlugin.SetMainCacheUpdater(cacheUpdater)
		}
	}
}

// Search 执行搜索
func (s *SearchService) Search(keyword string, channels []string, concurrency int, forceRefresh bool, resultType string, sourceType string, plugins []string, ext map[string]interface{}) (model.SearchResponse, error) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}
	
	// 参数预处理
	// 源类型标准化
	if sourceType == "" {
		sourceType = "all"
	}

	// 插件参数规范化处理
	if sourceType == "tg" {
		// 对于只搜索Telegram的请求，忽略插件参数
		plugins = nil
	} else if sourceType == "all" || sourceType == "plugin" {
		// 检查是否为空列表或只包含空字符串
		if plugins == nil || len(plugins) == 0 {
			plugins = nil
		} else {
			// 检查是否有非空元素
			hasNonEmpty := false
			for _, p := range plugins {
				if p != "" {
					hasNonEmpty = true
					break
				}
			}

			// 如果全是空字符串，视为未指定
			if !hasNonEmpty {
				plugins = nil
			} else {
				// 检查是否包含所有插件
				allPlugins := s.pluginManager.GetPlugins()
				allPluginNames := make([]string, 0, len(allPlugins))
				for _, p := range allPlugins {
					allPluginNames = append(allPluginNames, strings.ToLower(p.Name()))
				}

				// 创建请求的插件名称集合（忽略空字符串）
				requestedPlugins := make([]string, 0, len(plugins))
				for _, p := range plugins {
					if p != "" {
						requestedPlugins = append(requestedPlugins, strings.ToLower(p))
					}
				}

				// 如果请求的插件数量与所有插件数量相同，检查是否包含所有插件
				if len(requestedPlugins) == len(allPluginNames) {
					// 创建映射以便快速查找
					pluginMap := make(map[string]bool)
					for _, p := range requestedPlugins {
						pluginMap[p] = true
					}

					// 检查是否包含所有插件
					allIncluded := true
					for _, name := range allPluginNames {
						if !pluginMap[name] {
							allIncluded = false
							break
						}
					}

					// 如果包含所有插件，统一设为nil
					if allIncluded {
						plugins = nil
					}
				}
			}
		}
	}
	
	// 如果未指定并发数，使用配置中的默认值
	if concurrency <= 0 {
		concurrency = config.AppConfig.DefaultConcurrency
	}

	// 并行获取TG搜索和插件搜索结果
	var tgResults []model.SearchResult
	var pluginResults []model.SearchResult
	
	var wg sync.WaitGroup
	var tgErr, pluginErr error
	
	// 如果需要搜索TG
	if sourceType == "all" || sourceType == "tg" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tgResults, tgErr = s.searchTG(keyword, channels, forceRefresh)
		}()
	}
	// 如果需要搜索插件
	if sourceType == "all" || sourceType == "plugin" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 对于插件搜索，我们总是希望获取最新的缓存数据
			// 因此，即使forceRefresh=false，我们也需要确保获取到最新的缓存
			pluginResults, pluginErr = s.searchPlugins(keyword, plugins, forceRefresh, concurrency, ext)
		}()
	}
	
	// 等待所有搜索完成
	wg.Wait()
	
	// 检查错误
	if tgErr != nil {
		return model.SearchResponse{}, tgErr
	}
	if pluginErr != nil {
		return model.SearchResponse{}, pluginErr
	}
	
	// 合并结果
	allResults := mergeSearchResults(tgResults, pluginResults)

	// 按照优化后的规则排序结果
	sortResultsByTimeAndKeywords(allResults)

	// 过滤结果，只保留有时间的结果或包含优先关键词的结果到Results中
	filteredForResults := make([]model.SearchResult, 0, len(allResults))
	for _, result := range allResults {
		// 有时间的结果或包含优先关键词的结果保留在Results中
		if !result.Datetime.IsZero() || getKeywordPriority(result.Title) > 0 {
			filteredForResults = append(filteredForResults, result)
		}
	}

	// 合并链接按网盘类型分组（使用所有过滤后的结果）
	mergedLinks := mergeResultsByType(allResults)

	// 构建响应
	var total int
	if resultType == "merged_by_type" {
		// 计算所有类型链接的总数
		total = 0
		for _, links := range mergedLinks {
			total += len(links)
		}
	} else {
		// 只计算filteredForResults的数量
		total = len(filteredForResults)
	}

	response := model.SearchResponse{
		Total:        total,
		Results:      filteredForResults, // 使用进一步过滤的结果
		MergedByType: mergedLinks,
	}

	// 根据resultType过滤返回结果
	return filterResponseByType(response, resultType), nil
}

// filterResponseByType 根据结果类型过滤响应
func filterResponseByType(response model.SearchResponse, resultType string) model.SearchResponse {
	switch resultType {
	case "merged_by_type":
		// 只返回MergedByType，Results设为nil，结合omitempty标签，JSON序列化时会忽略此字段
		return model.SearchResponse{
			Total:        response.Total,
			MergedByType: response.MergedByType,
			Results:      nil,
		}
	case "all":
		return response
	case "results":
		// 只返回Results
		return model.SearchResponse{
			Total:   response.Total,
			Results: response.Results,
		}
	default:
		// // 默认返回全部
		// return response
		return model.SearchResponse{
			Total:        response.Total,
			MergedByType: response.MergedByType,
			Results:      nil,
		}
	}
}

// 根据时间和关键词排序结果
func sortResultsByTimeAndKeywords(results []model.SearchResult) {
	sort.Slice(results, func(i, j int) bool {
		// 检查是否有零值时间
		iZeroTime := results[i].Datetime.IsZero()
		jZeroTime := results[j].Datetime.IsZero()

		// 如果两者都是零值时间，按关键词优先级排序
		if iZeroTime && jZeroTime {
			iPriority := getKeywordPriority(results[i].Title)
			jPriority := getKeywordPriority(results[j].Title)
			if iPriority != jPriority {
				return iPriority > jPriority
			}
			// 如果优先级也相同，按标题字母顺序排序
			return results[i].Title < results[j].Title
		}

		// 如果只有一个是零值时间，将其排在后面
		if iZeroTime {
			return false // i排在后面
		}
		if jZeroTime {
			return true // j排在后面，i排在前面
		}

		// 两者都有正常时间，使用原有逻辑
		// 计算两个结果的时间差（以天为单位）
		timeDiff := daysBetween(results[i].Datetime, results[j].Datetime)

		// 如果时间差超过30天，按时间排序（新的在前面）
		if abs(timeDiff) > 30 {
			return results[i].Datetime.After(results[j].Datetime)
		}

		// 如果时间差在30天内，先检查时间差是否超过1天
		if abs(timeDiff) > 1 {
			return results[i].Datetime.After(results[j].Datetime)
		}

		// 如果时间差在1天内，检查关键词优先级
		iPriority := getKeywordPriority(results[i].Title)
		jPriority := getKeywordPriority(results[j].Title)

		// 如果优先级不同，优先级高的排在前面
		if iPriority != jPriority {
			return iPriority > jPriority
		}

		// 如果优先级相同且时间差在1天内，仍然按时间排序（新的在前面）
		return results[i].Datetime.After(results[j].Datetime)
	})
}

// 计算两个时间之间的天数差
func daysBetween(t1, t2 time.Time) float64 {
	duration := t1.Sub(t2)
	return duration.Hours() / 24
}

// 绝对值
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// 获取标题中包含优先关键词的优先级
func getKeywordPriority(title string) int {
	title = strings.ToLower(title)
	for i, keyword := range priorityKeywords {
		if strings.Contains(title, keyword) {
			// 返回优先级（数组索引越小，优先级越高）
			return len(priorityKeywords) - i
		}
	}
	return 0
}

// 搜索单个频道
func (s *SearchService) searchChannel(keyword string, channel string) ([]model.SearchResult, error) {
	// 构建搜索URL
	url := util.BuildSearchURL(channel, keyword, "")

	// 使用全局HTTP客户端（已配置代理）
	client := util.GetHTTPClient()

	// 创建一个带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 读取响应体
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 解析响应
	results, _, err := util.ParseSearchResults(string(body), channel)
	if err != nil {
		return nil, err
	}

	return results, nil
}

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
	
	// 链接正则表达式 - 精确匹配夸克网盘链接
	linkRegex := regexp.MustCompile(`https?://pan\.quark\.cn/s/[a-zA-Z0-9]+`)
	
	// 提取所有链接
	links := linkRegex.FindAllString(content, -1)
	if len(links) == 0 {
		return linkTitleMap
	}
	
	// 使用链接位置分割内容
	segments := make([]string, len(links)+1)
	lastPos := 0
	
	// 查找每个链接的位置，并提取链接前的文本作为段落
	for i, link := range links {
		pos := strings.Index(content[lastPos:], link) + lastPos
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

// 判断是否为链接前缀词
func isLinkPrefix(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return text == "链接" || 
		   text == "地址" || 
		   text == "资源地址" || 
		   text == "网盘" || 
		   text == "网盘地址"
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

// 将搜索结果按网盘类型分组
func mergeResultsByType(results []model.SearchResult) model.MergedLinks {
	// 创建合并结果的映射
	mergedLinks := make(model.MergedLinks, 10) // 预分配容量，假设有10种不同的网盘类型

	// 用于去重的映射，键为URL
	uniqueLinks := make(map[string]model.MergedLink)

	// 遍历所有搜索结果
	for _, result := range results {
		// 提取消息中的链接-标题对应关系
		linkTitleMap := extractLinkTitlePairs(result.Content)
		
		// 如果没有从内容中提取到标题，尝试直接从内容中匹配
		if len(linkTitleMap) == 0 && len(result.Links) > 0 && !strings.Contains(result.Content, "\n") {
			// 这是没有换行符的情况，尝试直接匹配
			content := result.Content
			
			// 尝试使用"链接："分割内容
			parts := strings.Split(content, "链接：")
			if len(parts) > 1 && len(result.Links) <= len(parts)-1 {
				// 第一部分是第一个标题
				titles := make([]string, 0, len(parts))
				titles = append(titles, cleanTitle(parts[0]))
				
				// 处理每个包含链接的部分，提取标题
				for i := 1; i < len(parts)-1; i++ {
					part := parts[i]
					// 找到链接的结束位置
					linkEnd := -1
					for j, c := range part {
						if c == ' ' || c == '窃' || c == '东' || c == '迎' || c == '千' || c == '我' || c == '恋' || c == '将' || c == '野' {
							linkEnd = j
							break
						}
					}
					
					if linkEnd > 0 {
						// 提取标题
						title := cleanTitle(part[linkEnd:])
						titles = append(titles, title)
					}
				}
				
				// 将标题与链接关联
				for i, link := range result.Links {
					if i < len(titles) {
						linkTitleMap[link.URL] = titles[i]
					}
				}
			}
		}
		
		for _, link := range result.Links {
			// 尝试从映射中获取该链接对应的标题
			title := result.Title // 默认使用消息标题
			
			// 查找完全匹配的链接
			if specificTitle, found := linkTitleMap[link.URL]; found && specificTitle != "" {
				title = specificTitle // 如果找到特定标题，则使用它
			} else {
				// 如果没有找到完全匹配的链接，尝试查找前缀匹配的链接
				for mappedLink, mappedTitle := range linkTitleMap {
					if strings.HasPrefix(mappedLink, link.URL) {
						title = mappedTitle
						break
					}
				}
			}
			
			// 创建合并后的链接
			mergedLink := model.MergedLink{
				URL:      link.URL,
				Password: link.Password,
				Note:     title, // 使用找到的特定标题
				Datetime: result.Datetime,
			}

			// 检查是否已存在相同URL的链接
			if existingLink, exists := uniqueLinks[link.URL]; exists {
				// 如果已存在，只有当当前链接的时间更新时才替换
				if mergedLink.Datetime.After(existingLink.Datetime) {
					uniqueLinks[link.URL] = mergedLink
				}
			} else {
				// 如果不存在，直接添加
				uniqueLinks[link.URL] = mergedLink
			}
		}
	}

	// 将去重后的链接按类型分组
	for url, mergedLink := range uniqueLinks {
		// 获取链接类型
		linkType := ""
		for _, result := range results {
			for _, link := range result.Links {
				if link.URL == url {
					linkType = link.Type
					break
				}
			}
			if linkType != "" {
				break
			}
		}

		// 如果没有找到类型，使用"unknown"
		if linkType == "" {
			linkType = "unknown"
		}

		// 添加到对应类型的列表中
		mergedLinks[linkType] = append(mergedLinks[linkType], mergedLink)
	}

	// 对每种类型的链接按时间排序（新的在前面）
	for linkType, links := range mergedLinks {
		sort.Slice(links, func(i, j int) bool {
			return links[i].Datetime.After(links[j].Datetime)
		})
		mergedLinks[linkType] = links
	}

	return mergedLinks
}

// searchTG 搜索TG频道
func (s *SearchService) searchTG(keyword string, channels []string, forceRefresh bool) ([]model.SearchResult, error) {
	// 生成缓存键
	cacheKey := cache.GenerateTGCacheKey(keyword, channels)
	
	// 如果未启用强制刷新，尝试从缓存获取结果
	if !forceRefresh && cacheInitialized && config.AppConfig.CacheEnabled {
		var data []byte
		var hit bool
		var err error
		
		// 优先使用增强版缓存
		if enhancedTwoLevelCache != nil {
			data, hit, err = enhancedTwoLevelCache.Get(cacheKey)
			
			if err == nil && hit {
				var results []model.SearchResult
				if err := enhancedTwoLevelCache.GetSerializer().Deserialize(data, &results); err == nil {
					// 直接返回缓存数据，不检查新鲜度
					return results, nil
				}
			}
		} else if twoLevelCache != nil {
			data, hit, err = twoLevelCache.Get(cacheKey)
			
			if err == nil && hit {
				var results []model.SearchResult
				if err := cache.DeserializeWithPool(data, &results); err == nil {
					// 直接返回缓存数据，不检查新鲜度
					return results, nil
				}
			}
		}
	}
	
	// 缓存未命中或强制刷新，执行实际搜索
	var results []model.SearchResult
	
	// 使用工作池并行搜索多个频道
	tasks := make([]pool.Task, 0, len(channels))
	
	for _, channel := range channels {
		ch := channel // 创建副本，避免闭包问题
		tasks = append(tasks, func() interface{} {
			results, err := s.searchChannel(keyword, ch)
			if err != nil {
				return nil
			}
			return results
		})
	}
	
	// 执行搜索任务并获取结果
	taskResults := pool.ExecuteBatchWithTimeout(tasks, len(channels), config.AppConfig.PluginTimeout)
	
	// 合并所有频道的结果
	for _, result := range taskResults {
		if result != nil {
			channelResults := result.([]model.SearchResult)
			results = append(results, channelResults...)
		}
	}
	
	// 异步缓存结果
	if cacheInitialized && config.AppConfig.CacheEnabled {
		go func(res []model.SearchResult) {
			ttl := time.Duration(config.AppConfig.CacheTTLMinutes) * time.Minute
			
			// 优先使用增强版缓存
			if enhancedTwoLevelCache != nil {
				data, err := enhancedTwoLevelCache.GetSerializer().Serialize(res)
				if err != nil {
					return
				}
				enhancedTwoLevelCache.Set(cacheKey, data, ttl)
			} else if twoLevelCache != nil {
				data, err := cache.SerializeWithPool(res)
				if err != nil {
					return
				}
				twoLevelCache.Set(cacheKey, data, ttl)
			}
		}(results)
	}
	
	return results, nil
}

// searchPlugins 搜索插件
func (s *SearchService) searchPlugins(keyword string, plugins []string, forceRefresh bool, concurrency int, ext map[string]interface{}) ([]model.SearchResult, error) {
	// 确保ext不为nil
	if ext == nil {
		ext = make(map[string]interface{})
	}
	
	// 生成缓存键
	cacheKey := cache.GeneratePluginCacheKey(keyword, plugins)
	
	// 如果未启用强制刷新，尝试从缓存获取结果
	if !forceRefresh && cacheInitialized && config.AppConfig.CacheEnabled {
		var data []byte
		var hit bool
		var err error
		
		// 优先使用增强版缓存
		if enhancedTwoLevelCache != nil {
			
			// 使用Get方法，它会检查磁盘缓存是否有更新
			// 如果磁盘缓存比内存缓存更新，会自动更新内存缓存并返回最新数据
			data, hit, err = enhancedTwoLevelCache.Get(cacheKey)
			
			if err == nil && hit {
				var results []model.SearchResult
				if err := enhancedTwoLevelCache.GetSerializer().Deserialize(data, &results); err == nil {
					// 返回缓存数据
					return results, nil
				}
			}
		} else if twoLevelCache != nil {
			data, hit, err = twoLevelCache.Get(cacheKey)
			
			if err == nil && hit {
				var results []model.SearchResult
				if err := cache.DeserializeWithPool(data, &results); err == nil {
					// 返回缓存数据
					return results, nil
				}
			}
		}
	}
	
	// 缓存未命中或强制刷新，执行实际搜索
	// 获取所有可用插件
	var availablePlugins []plugin.SearchPlugin
	if s.pluginManager != nil {
		allPlugins := s.pluginManager.GetPlugins()
		
		// 确保plugins不为nil并且有非空元素
		hasPlugins := plugins != nil && len(plugins) > 0
		hasNonEmptyPlugin := false
		
		if hasPlugins {
			for _, p := range plugins {
				if p != "" {
					hasNonEmptyPlugin = true
					break
				}
			}
		}
		
		// 只有当plugins数组包含非空元素时才进行过滤
		if hasPlugins && hasNonEmptyPlugin {
			pluginMap := make(map[string]bool)
			for _, p := range plugins {
				if p != "" { // 忽略空字符串
					pluginMap[strings.ToLower(p)] = true
				}
			}
			
			for _, p := range allPlugins {
				if pluginMap[strings.ToLower(p.Name())] {
					availablePlugins = append(availablePlugins, p)
				}
			}
		} else {
			// 如果plugins为nil、空数组或只包含空字符串，视为未指定，使用所有插件
			availablePlugins = allPlugins
		}
	}
	
	// 控制并发数
	if concurrency <= 0 {
		// 使用配置中的默认值
		concurrency = config.AppConfig.DefaultConcurrency
	}
	
	// 使用工作池执行并行搜索
	tasks := make([]pool.Task, 0, len(availablePlugins))
	for _, p := range availablePlugins {
		plugin := p // 创建副本，避免闭包问题
		tasks = append(tasks, func() interface{} {
			// 检查插件是否为异步插件
			if asyncPlugin, ok := plugin.(interface {
				AsyncSearch(keyword string, searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error), mainCacheKey string, ext map[string]interface{}) ([]model.SearchResult, error)
				SetMainCacheKey(string)
			}); ok {
				// 先设置主缓存键
				asyncPlugin.SetMainCacheKey(cacheKey)
				
				// 是异步插件，调用AsyncSearch方法并传递主缓存键和ext参数
				results, err := asyncPlugin.AsyncSearch(keyword, func(client *http.Client, kw string, extParams map[string]interface{}) ([]model.SearchResult, error) {
					// 这里使用插件的Search方法作为搜索函数，传递ext参数
					return plugin.Search(kw, extParams)
				}, cacheKey, ext)
				
				if err != nil {
					return nil
				}
				return results
			} else {
				// 不是异步插件，直接调用Search方法，传递ext参数
				results, err := plugin.Search(keyword, ext)
				if err != nil {
					return nil
				}
				return results
			}
		})
	}
	
	// 执行搜索任务并获取结果
	results := pool.ExecuteBatchWithTimeout(tasks, concurrency, config.AppConfig.PluginTimeout)
	
	// 合并所有插件的结果
	var allResults []model.SearchResult
	for _, result := range results {
		if result != nil {
			pluginResults := result.([]model.SearchResult)
			allResults = append(allResults, pluginResults...)
		}
	}
	
	// 异步缓存结果
	if cacheInitialized && config.AppConfig.CacheEnabled {
		go func(res []model.SearchResult) {
			ttl := time.Duration(config.AppConfig.CacheTTLMinutes) * time.Minute
			
			// 优先使用增强版缓存
			if enhancedTwoLevelCache != nil {
				data, err := enhancedTwoLevelCache.GetSerializer().Serialize(res)
				if err != nil {
					return
				}
				enhancedTwoLevelCache.Set(cacheKey, data, ttl)
			} else if twoLevelCache != nil {
				data, err := cache.SerializeWithPool(res)
				if err != nil {
					return
				}
				twoLevelCache.Set(cacheKey, data, ttl)
			}
		}(allResults)
	}
	
	return allResults, nil
}

// 合并搜索结果
func mergeSearchResults(tgResults, pluginResults []model.SearchResult) []model.SearchResult {
	// 预估合并后的结果数量
	totalSize := len(tgResults) + len(pluginResults)
	if totalSize == 0 {
		return []model.SearchResult{}
	}
	
	// 创建结果映射，用于去重
	resultMap := make(map[string]model.SearchResult, totalSize)
	
	// 添加TG搜索结果
	for _, result := range tgResults {
		resultMap[result.UniqueID] = result
	}
	
	// 添加或更新插件搜索结果（如果有重复，保留较新的）
	for _, result := range pluginResults {
		if existing, ok := resultMap[result.UniqueID]; ok {
			// 如果已存在，保留较新的
			if result.Datetime.After(existing.Datetime) {
				resultMap[result.UniqueID] = result
			}
		} else {
			resultMap[result.UniqueID] = result
		}
	}
	
	// 转换回切片
	mergedResults := make([]model.SearchResult, 0, len(resultMap))
	for _, result := range resultMap {
		mergedResults = append(mergedResults, result)
	}
	
	return mergedResults
}

// GetPluginManager 获取插件管理器
func (s *SearchService) GetPluginManager() *plugin.PluginManager {
	return s.pluginManager
}
