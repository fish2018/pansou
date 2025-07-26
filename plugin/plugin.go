package plugin

import (
	"strings"
	"sync"

	"pansou/model"
)

// 全局插件注册表
var (
	globalRegistry     = make(map[string]SearchPlugin)
	globalRegistryLock sync.RWMutex
)

// SearchPlugin 搜索插件接口
type SearchPlugin interface {
	// Name 返回插件名称
	Name() string
	
	// Search 执行搜索并返回结果
	// ext参数用于传递额外的搜索参数，插件可以根据需要使用或忽略
	Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error)
	
	// Priority 返回插件优先级（可选，用于控制结果排序）
	Priority() int
}

// RegisterGlobalPlugin 注册插件到全局注册表
// 这个函数应该在每个插件的init函数中被调用
func RegisterGlobalPlugin(plugin SearchPlugin) {
	if plugin == nil {
		return
	}
	
	globalRegistryLock.Lock()
	defer globalRegistryLock.Unlock()
	
	name := plugin.Name()
	if name == "" {
		return
	}
	
	globalRegistry[name] = plugin
}

// GetRegisteredPlugins 获取所有已注册的插件
func GetRegisteredPlugins() []SearchPlugin {
	globalRegistryLock.RLock()
	defer globalRegistryLock.RUnlock()
	
	plugins := make([]SearchPlugin, 0, len(globalRegistry))
	for _, plugin := range globalRegistry {
		plugins = append(plugins, plugin)
	}
	
	return plugins
}

// PluginManager 插件管理器
type PluginManager struct {
	plugins []SearchPlugin
}

// NewPluginManager 创建新的插件管理器
func NewPluginManager() *PluginManager {
	return &PluginManager{
		plugins: make([]SearchPlugin, 0),
	}
}

// RegisterPlugin 注册插件
func (pm *PluginManager) RegisterPlugin(plugin SearchPlugin) {
	pm.plugins = append(pm.plugins, plugin)
}

// RegisterAllGlobalPlugins 注册所有全局插件
func (pm *PluginManager) RegisterAllGlobalPlugins() {
	for _, plugin := range GetRegisteredPlugins() {
		pm.RegisterPlugin(plugin)
	}
}

// GetPlugins 获取所有注册的插件
func (pm *PluginManager) GetPlugins() []SearchPlugin {
	return pm.plugins
} 

// FilterResultsByKeyword 根据关键词过滤搜索结果的全局辅助函数
// 供非BaseAsyncPlugin类型的插件使用
func FilterResultsByKeyword(results []model.SearchResult, keyword string) []model.SearchResult {
	if keyword == "" {
		return results
	}
	
	// 预估过滤后会保留80%的结果
	filteredResults := make([]model.SearchResult, 0, len(results)*8/10)

	// 将关键词转为小写，用于不区分大小写的比较
	lowerKeyword := strings.ToLower(keyword)

	// 将关键词按空格分割，用于支持多关键词搜索
	keywords := strings.Fields(lowerKeyword)

	for _, result := range results {
		// 将标题和内容转为小写
		lowerTitle := strings.ToLower(result.Title)
		lowerContent := strings.ToLower(result.Content)

		// 检查每个关键词是否在标题或内容中
		matched := true
		for _, kw := range keywords {
			// 对于所有关键词，检查是否在标题或内容中
			if !strings.Contains(lowerTitle, kw) && !strings.Contains(lowerContent, kw) {
				matched = false
				break
			}
		}

		if matched {
			filteredResults = append(filteredResults, result)
		}
	}

	return filteredResults
} 