package plugin

import (
	"sync"
)

// ============================================================
// 全局插件注册表
// ============================================================

// 全局异步插件注册表
var (
	globalRegistry     = make(map[string]AsyncSearchPlugin)
	globalRegistryLock sync.RWMutex
)

// RegisterGlobalPlugin 注册异步插件到全局注册表
func RegisterGlobalPlugin(plugin AsyncSearchPlugin) {
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

// GetRegisteredPlugins 获取所有已注册的异步插件
func GetRegisteredPlugins() []AsyncSearchPlugin {
	globalRegistryLock.RLock()
	defer globalRegistryLock.RUnlock()

	plugins := make([]AsyncSearchPlugin, 0, len(globalRegistry))
	for _, plugin := range globalRegistry {
		plugins = append(plugins, plugin)
	}

	return plugins
}

// GetPluginByName 根据名称获取已注册的插件
func GetPluginByName(name string) (AsyncSearchPlugin, bool) {
	globalRegistryLock.RLock()
	defer globalRegistryLock.RUnlock()

	plugin, exists := globalRegistry[name]
	return plugin, exists
}