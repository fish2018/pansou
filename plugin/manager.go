package plugin

import (
	"fmt"
	"strings"
)

// ============================================================
// 插件管理器
// ============================================================

// PluginManager 异步插件管理器
type PluginManager struct {
	plugins []AsyncSearchPlugin
}

// NewPluginManager 创建新的异步插件管理器
func NewPluginManager() *PluginManager {
	return &PluginManager{
		plugins: make([]AsyncSearchPlugin, 0),
	}
}

// RegisterPlugin 注册异步插件
func (pm *PluginManager) RegisterPlugin(plugin AsyncSearchPlugin) {
	// 如果插件支持延迟初始化，先执行初始化
	if initPlugin, ok := plugin.(InitializablePlugin); ok {
		if err := initPlugin.Initialize(); err != nil {
			fmt.Printf("[PluginManager] 插件 %s 初始化失败: %v，跳过注册\n", plugin.Name(), err)
			return
		}
	}

	pm.plugins = append(pm.plugins, plugin)
}

// RegisterAllGlobalPlugins 注册所有全局异步插件
func (pm *PluginManager) RegisterAllGlobalPlugins() {
	allPlugins := GetRegisteredPlugins()
	for _, plugin := range allPlugins {
		pm.RegisterPlugin(plugin)
	}
}

// RegisterGlobalPluginsWithFilter 根据过滤器注册全局异步插件
// enabledPlugins: nil表示未设置（不启用任何插件），空切片表示设置为空（不启用任何插件），具体列表表示启用指定插件
func (pm *PluginManager) RegisterGlobalPluginsWithFilter(enabledPlugins []string) {
	allPlugins := GetRegisteredPlugins()

	// nil 表示未设置环境变量，不启用任何插件
	if enabledPlugins == nil {
		return
	}

	// 空切片表示设置为空字符串，也不启用任何插件
	if len(enabledPlugins) == 0 {
		return
	}

	// 创建启用插件名称的映射表，用于快速查找
	enabledMap := make(map[string]bool)
	for _, name := range enabledPlugins {
		enabledMap[strings.ToLower(name)] = true
	}

	// 只注册在启用列表中的插件
	for _, plugin := range allPlugins {
		if enabledMap[strings.ToLower(plugin.Name())] {
			pm.RegisterPlugin(plugin)
		}
	}
}

// GetPlugins 获取所有注册的异步插件
func (pm *PluginManager) GetPlugins() []AsyncSearchPlugin {
	return pm.plugins
}