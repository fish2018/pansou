package service

import (
	"fmt"
	"net/url"
	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"sort"
	"strings"
)

// ============================================================
// 搜索结果合并逻辑
// ============================================================

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

// mergeSearchResults 智能合并搜索结果，去重并保留最完整的信息
func mergeSearchResults(existing []model.SearchResult, newResults []model.SearchResult) []model.SearchResult {
	// 使用map进行去重和合并，以UniqueID作为唯一标识
	resultMap := make(map[string]model.SearchResult)
	
	// 先添加现有结果
	for _, result := range existing {
		key := generateResultKey(result)
		resultMap[key] = result
	}
	
	// 合并新结果，如果UniqueID相同则选择信息更完整的
	for _, newResult := range newResults {
		key := generateResultKey(newResult)
		if existingResult, exists := resultMap[key]; exists {
			// 选择信息更完整的结果
			resultMap[key] = selectBetterResult(existingResult, newResult)
		} else {
			// 新结果，直接添加
			resultMap[key] = newResult
		}
	}
	
	// 转换回切片
	merged := make([]model.SearchResult, 0, len(resultMap))
	for _, result := range resultMap {
		merged = append(merged, result)
	}
	
	// 按时间排序（最新的在前）
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Datetime.After(merged[j].Datetime)
	})
	
	return merged
}

// generateResultKey 生成结果的唯一标识键
func generateResultKey(result model.SearchResult) string {
	// 使用UniqueID作为主要标识，如果没有则使用MessageID，最后使用标题
	if result.UniqueID != "" {
		return result.UniqueID
	}
	if result.MessageID != "" {
		return result.MessageID
	}
	return fmt.Sprintf("title_%s_%s", result.Title, result.Channel)
}

// selectBetterResult 选择信息更完整的结果
func selectBetterResult(existing, new model.SearchResult) model.SearchResult {
	// 计算信息完整度得分
	existingScore := calculateCompletenessScore(existing)
	newScore := calculateCompletenessScore(new)
	
	if newScore > existingScore {
		return new
	}
	return existing
}

// calculateCompletenessScore 计算结果信息的完整度得分
func calculateCompletenessScore(result model.SearchResult) int {
	score := 0
	
	// 有UniqueID加分
	if result.UniqueID != "" {
		score += 10
	}
	
	// 有链接信息加分
	if len(result.Links) > 0 {
		score += 5
		// 每个链接额外加分
		score += len(result.Links)
	}
	
	// 有内容加分
	if result.Content != "" {
		score += 3
	}
	
	// 标题长度加分（更详细的标题）
	score += len(result.Title) / 10
	
	// 有频道信息加分
	if result.Channel != "" {
		score += 2
	}
	
	// 有标签加分
	score += len(result.Tags)
	
	return score
}

// mergeResultsByType 将搜索结果按网盘类型分组
func mergeResultsByType(results []model.SearchResult, keyword string, cloudTypes []string) model.MergedLinks {
	// 创建合并结果的映射
	mergedLinks := make(model.MergedLinks, 12) // 预分配容量，假设有12种不同的网盘类型
	
	// 用于去重的映射，键为URL
	uniqueLinks := make(map[string]model.MergedLink)
	
	// 将关键词转为小写，用于不区分大小写的匹配
	lowerKeyword := strings.ToLower(keyword)
	
	// 遍历所有搜索结果
	for _, result := range results {
		// 提取消息中的链接-标题对应关系
		linkTitleMap := extractLinkTitlePairs(result.Content)
		
		// 如果没有从内容中提取到标题，尝试直接从内容中匹配
		if len(linkTitleMap) == 0 && len(result.Links) > 0 && !strings.Contains(result.Content, "\n") {
			// 这是没有换行符的情况，尝试直接匹配
			linkTitleMap = matchLinksWithoutNewlines(result.Content, result.Links)
		}
		
		for _, link := range result.Links {
			// 优先使用链接的WorkTitle字段，如果为空则回退到传统方式
			title := result.Title // 默认使用消息标题
			
			if link.WorkTitle != "" {
				// 如果链接有WorkTitle字段，优先使用
				title = link.WorkTitle
			} else {
				// 如果没有WorkTitle，使用传统方式从映射中获取该链接对应的标题
				if specificTitle, found := linkTitleMap[link.URL]; found && specificTitle != "" {
					title = specificTitle
				} else {
					// 尝试前缀匹配
					for mappedLink, mappedTitle := range linkTitleMap {
						if strings.HasPrefix(mappedLink, link.URL) {
							title = mappedTitle
							break
						}
					}
				}
			}
			
			// 检查插件是否需要跳过Service层过滤
			skipKeywordFilter := shouldSkipFilterForResult(result)
			
			// 关键词过滤
			if !skipKeywordFilter && keyword != "" {
				if !strings.Contains(strings.ToLower(title), lowerKeyword) {
					continue
				}
			}
			
			// 确定数据来源
			source := determineResultSource(result)
			
			// 裁剪标题
			title = util.CutTitleByKeywords(title, []string{"简介", "描述"})
			
			// 优先使用链接自己的时间
			linkDatetime := result.Datetime
			if !link.Datetime.IsZero() {
				linkDatetime = link.Datetime
			}
			
			mergedLink := model.MergedLink{
				URL:      link.URL,
				Password: link.Password,
				Note:     title,
				Datetime: linkDatetime,
				Source:   source,
				Images:   result.Images,
			}
			
			// 去重逻辑
			if existingLink, exists := uniqueLinks[link.URL]; exists {
				if mergedLink.Datetime.After(existingLink.Datetime) {
					uniqueLinks[link.URL] = mergedLink
				}
			} else {
				uniqueLinks[link.URL] = mergedLink
			}
		}
	}
	
	// 按原始顺序收集唯一链接
	orderedLinks := collectOrderedLinks(results, uniqueLinks)
	
	// 按类型分组
	for _, mergedLink := range orderedLinks {
		linkType := determineLinkType(mergedLink.URL)
		mergedLinks[linkType] = append(mergedLinks[linkType], mergedLink)
	}
	
	// 过滤cloudTypes
	if len(cloudTypes) > 0 {
		return filterLinksByCloudTypes(mergedLinks, cloudTypes)
	}
	
	return mergedLinks
}

// shouldSkipFilterForResult 判断结果是否应跳过关键词过滤
func shouldSkipFilterForResult(result model.SearchResult) bool {
	if result.UniqueID != "" && strings.Contains(result.UniqueID, "-") {
		parts := strings.SplitN(result.UniqueID, "-", 2)
		if len(parts) >= 1 {
			pluginName := parts[0]
			if pluginInstance, exists := plugin.GetPluginByName(pluginName); exists {
				return pluginInstance.SkipServiceFilter()
			}
		}
	}
	return false
}

// determineResultSource 确定结果来源
func determineResultSource(result model.SearchResult) string {
	if result.Channel != "" {
		return "tg:" + result.Channel
	} else if result.UniqueID != "" && strings.Contains(result.UniqueID, "-") {
		parts := strings.SplitN(result.UniqueID, "-", 2)
		if len(parts) >= 1 {
			return "plugin:" + parts[0]
		}
	}
	return "unknown"
}

// matchLinksWithoutNewlines 处理没有换行符的链接匹配(辅助函数)
func matchLinksWithoutNewlines(content string, links []model.Link) map[string]string {
	linkTitleMap := make(map[string]string)
	
	// 支持多种网盘链接前缀
	linkPrefixes := []string{"天翼链接：", "百度链接：", "夸克链接：", "阿里链接：", "UC链接：", "115链接：", "迅雷链接：", "123链接：", "链接："}
	
	var parts []string
	
	// 尝试找到匹配的前缀
	for _, prefix := range linkPrefixes {
		if strings.Contains(content, prefix) {
			parts = strings.Split(content, prefix)
			break
		}
	}
	
	// 如果找到了匹配的前缀并且分割成功
	if len(parts) > 1 && len(links) <= len(parts)-1 {
		titles := make([]string, 0, len(parts))
		titles = append(titles, cleanTitle(parts[0]))
		
		// 处理每个包含链接的部分
		for i := 1; i < len(parts)-1; i++ {
			part := parts[i]
			linkEnd := findLinkEnd(part)
			
			if linkEnd > 0 {
				title := cleanTitle(part[linkEnd:])
				titles = append(titles, title)
			}
		}
		
		// 将标题与链接关联
		for i, link := range links {
			if i < len(titles) {
				linkTitleMap[link.URL] = titles[i]
			}
		}
	}
	
	return linkTitleMap
}

// findLinkEnd 查找链接结束位置
func findLinkEnd(text string) int {
	for i, c := range text {
		if c == ' ' || c == '窃' || c == '东' || c == '迎' || c == '千' || c == '我' || c == '恋' || c == '将' || c == '野' ||
			c == '合' || c == '集' || c == '天' || c == '翼' || c == '网' || c == '盘' || c == '(' || c == '（' {
			return i
		}
	}
	return -1
}

// determineLinkType 确定链接类型(简化版)
func determineLinkType(url string) string {
	url = strings.ToLower(url)
	if strings.Contains(url, "quark") {
		return "quark"
	} else if strings.Contains(url, "baidu") {
		return "baidu"
	} else if strings.Contains(url, "xunlei") {
		return "xunlei"
	} else if strings.Contains(url, "aliyundrive") || strings.Contains(url, "alipan") {
		return "aliyun"
	} else if strings.Contains(url, "uc.cn") {
		return "uc"
	} else if strings.Contains(url, "123pan") || strings.Contains(url, "123684") {
		return "123"
	} else if strings.Contains(url, "115") {
		return "115"
	} else if strings.Contains(url, "tianyi") {
		return "tianyi"
	} else if strings.Contains(url, "caiyun.139") {
		return "mobile"
	} else if strings.Contains(url, "pikpak") {
		return "pikpak"
	} else if strings.HasPrefix(url, "magnet:") {
		return "magnet"
	} else if strings.HasPrefix(url, "ed2k:") {
		return "ed2k"
	}
	return "unknown"
}

// collectOrderedLinks 按原始顺序收集唯一链接
func collectOrderedLinks(results []model.SearchResult, uniqueLinks map[string]model.MergedLink) []model.MergedLink {
	orderedLinks := make([]model.MergedLink, 0, len(uniqueLinks))
	
	for _, result := range results {
		for _, link := range result.Links {
			if mergedLink, exists := uniqueLinks[link.URL]; exists {
				// 检查是否已添加
				found := false
				for _, existing := range orderedLinks {
					if existing.URL == link.URL {
						found = true
						break
					}
				}
				if !found {
					orderedLinks = append(orderedLinks, mergedLink)
				}
			}
		}
	}
	
	return orderedLinks
}

// filterLinksByCloudTypes 按云盘类型过滤链接
func filterLinksByCloudTypes(mergedLinks model.MergedLinks, cloudTypes []string) model.MergedLinks {
	filteredLinks := make(model.MergedLinks)
	
	// 创建允许类型映射
	allowedTypes := make(map[string]bool)
	for _, cloudType := range cloudTypes {
		allowedTypes[strings.ToLower(strings.TrimSpace(cloudType))] = true
	}
	
	// 只保留指定类型的链接
	for linkType, links := range mergedLinks {
		if allowedTypes[strings.ToLower(linkType)] {
			filteredLinks[linkType] = links
		}
	}
	
	return filteredLinks
}

// filterResponseByType 根据结果类型过滤响应
func filterResponseByType(response model.SearchResponse, resultType string) model.SearchResponse {
	switch resultType {
	case "merged_by_type":
		return model.SearchResponse{
			Total:        response.Total,
			MergedByType: response.MergedByType,
			Results:      nil,
		}
	case "all":
		return response
	case "results":
		return model.SearchResponse{
			Total:   response.Total,
			Results: response.Results,
		}
	default:
		return model.SearchResponse{
			Total:        response.Total,
			MergedByType: response.MergedByType,
			Results:      nil,
		}
	}
}
