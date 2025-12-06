package service

import (
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
)

// ============================================================
// 搜索结果排序和评分逻辑
// ============================================================

// 优先关键词列表
var priorityKeywords = []string{"合集", "系列", "全", "完", "最新", "附", "complete"}

// ResultScore 搜索结果评分结构
type ResultScore struct {
	Result       model.SearchResult
	TimeScore    float64 // 时间得分
	KeywordScore int     // 关键词得分
	PluginScore  int     // 插件等级得分
	TotalScore   float64 // 综合得分
}

// 插件等级缓存
var (
	pluginLevelCache = sync.Map{} // 插件等级缓存
)

// sortResultsByTimeAndKeywords 根据时间和关键词排序结果
func sortResultsByTimeAndKeywords(results []model.SearchResult) {
	// 1. 计算每个结果的综合得分
	scores := make([]ResultScore, len(results))

	for i, result := range results {
		source := getResultSource(result)

		scores[i] = ResultScore{
			Result:       result,
			TimeScore:    calculateTimeScore(result.Datetime),
			KeywordScore: getKeywordPriority(result.Title),
			PluginScore:  getPluginLevelScore(source),
			TotalScore:   0, // 稍后计算
		}

		// 计算综合得分
		scores[i].TotalScore = scores[i].TimeScore +
			float64(scores[i].KeywordScore) +
			float64(scores[i].PluginScore)
	}

	// 2. 按综合得分排序
	// sort.Slice(scores, func(i, j int) bool {
	// 	return scores[i].TotalScore > scores[j].TotalScore
	// })

	// 使用更高效的排序(避免import sort)
	for i := 0; i < len(scores)-1; i++ {
		for j := i + 1; j < len(scores); j++ {
			if scores[i].TotalScore < scores[j].TotalScore {
				scores[i], scores[j] = scores[j], scores[i]
			}
		}
	}

	// 3. 更新原数组
	for i, score := range scores {
		results[i] = score.Result
	}
}

// getKeywordPriority 获取标题中包含优先关键词的优先级
func getKeywordPriority(title string) int {
	title = strings.ToLower(title)
	for i, keyword := range priorityKeywords {
		if strings.Contains(title, keyword) {
			// 返回优先级得分（数组索引越小，优先级越高，最高400分）
			return (len(priorityKeywords) - i) * 70
		}
	}
	return 0
}

// getResultSource 从SearchResult推断数据来源
func getResultSource(result model.SearchResult) string {
	if result.Channel != "" {
		// 来自TG频道
		return "tg:" + result.Channel
	} else if result.UniqueID != "" && strings.Contains(result.UniqueID, "-") {
		// 来自插件：UniqueID格式通常为 "插件名-ID"
		parts := strings.SplitN(result.UniqueID, "-", 2)
		if len(parts) >= 1 {
			return "plugin:" + parts[0]
		}
	}
	return "unknown"
}

// getPluginLevelBySource 根据来源获取插件等级
func getPluginLevelBySource(source string) int {
	// 尝试从缓存获取
	if level, ok := pluginLevelCache.Load(source); ok {
		return level.(int)
	}

	parts := strings.Split(source, ":")
	if len(parts) != 2 {
		pluginLevelCache.Store(source, 3)
		return 3 // 默认等级
	}

	if parts[0] == "tg" {
		pluginLevelCache.Store(source, 3)
		return 3 // TG搜索等同于等级3
	}

	if parts[0] == "plugin" {
		level := getPluginPriorityByName(parts[1])
		pluginLevelCache.Store(source, level)
		return level
	}

	pluginLevelCache.Store(source, 3)
	return 3
}

// getPluginPriorityByName 根据插件名获取优先级
func getPluginPriorityByName(pluginName string) int {
	// 从插件管理器动态获取真实的优先级 (O(1)哈希查找)
	if pluginInstance, exists := plugin.GetPluginByName(pluginName); exists {
		return pluginInstance.Priority()
	}
	return 3 // 默认等级
}

// getPluginLevelScore 获取插件等级得分
func getPluginLevelScore(source string) int {
	level := getPluginLevelBySource(source)

	switch level {
	case 1:
		return 1000 // 等级1插件：1000分
	case 2:
		return 500 // 等级2插件：500分
	case 3:
		return 0 // 等级3插件：0分
	case 4:
		return -200 // 等级4插件：-200分
	default:
		return 0 // 默认使用等级3得分
	}
}

// calculateTimeScore 计算时间得分
func calculateTimeScore(datetime time.Time) float64 {
	if datetime.IsZero() {
		return 0 // 无时间信息得0分
	}

	now := time.Now()
	daysDiff := now.Sub(datetime).Hours() / 24

	// 时间得分：越新得分越高，最大500分（增加时间权重）
	switch {
	case daysDiff <= 1:
		return 500 // 1天内
	case daysDiff <= 3:
		return 400 // 3天内
	case daysDiff <= 7:
		return 300 // 1周内
	case daysDiff <= 30:
		return 200 // 1月内
	case daysDiff <= 90:
		return 100 // 3月内
	case daysDiff <= 365:
		return 50 // 1年内
	default:
		return 20 // 1年以上
	}
}
