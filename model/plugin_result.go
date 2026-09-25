package model

import (
	"time"
)

// PluginSearchResult 插件搜索结果
type PluginSearchResult struct {
	Results   []SearchResult `json:"results"`   // 搜索结果
	IsFinal   bool           `json:"is_final"`  // 是否为最终完整结果
	Timestamp time.Time      `json:"timestamp"` // 结果时间戳
	Source    string         `json:"source"`    // 插件来源
	Message   string         `json:"message"`   // 状态描述（可选）
}

// IsEmpty 检查结果是否为空
func (p *PluginSearchResult) IsEmpty() bool {
	return len(p.Results) == 0
}

// Count 返回结果数量
func (p *PluginSearchResult) Count() int {
	return len(p.Results)
}

// GetResults 获取搜索结果列表
func (p *PluginSearchResult) GetResults() []SearchResult {
	if p.Results == nil {
		return []SearchResult{}
	}
	return p.Results
}
