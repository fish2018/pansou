package plugin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"pansou/model"
)

// ============================================================
// 插件接口定义
// ============================================================

// AsyncSearchPlugin 异步搜索插件接口
type AsyncSearchPlugin interface {
	// Name 返回插件名称
	Name() string

	// Priority 返回插件优先级
	Priority() int

	// AsyncSearch 异步搜索方法
	AsyncSearch(keyword string, searchFunc func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error), mainCacheKey string, ext map[string]interface{}) ([]model.SearchResult, error)

	// SetMainCacheKey 设置主缓存键
	SetMainCacheKey(key string)

	// SetCurrentKeyword 设置当前搜索关键词（用于日志显示）
	SetCurrentKeyword(keyword string)

	// Search 兼容性方法（内部调用AsyncSearch）
	Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error)

	// SkipServiceFilter 返回是否跳过Service层的关键词过滤
	// 对于磁力搜索等需要宽泛结果的插件，应返回true
	SkipServiceFilter() bool
}

// PluginWithWebHandler 支持Web路由的插件接口
// 插件可以选择实现此接口来注册自定义的HTTP路由
type PluginWithWebHandler interface {
	AsyncSearchPlugin // 继承搜索插件接口

	// RegisterWebRoutes 注册Web路由
	// router: gin的路由组，插件可以在此注册自己的路由
	RegisterWebRoutes(router *gin.RouterGroup)
}

// InitializablePlugin 支持延迟初始化的插件接口
// 插件可以实现此接口，将初始化逻辑延迟到真正被使用时执行
type InitializablePlugin interface {
	AsyncSearchPlugin // 继承搜索插件接口

	// Initialize 执行插件初始化（创建目录、加载数据等）
	// 只会被调用一次，应该是幂等的
	Initialize() error
}