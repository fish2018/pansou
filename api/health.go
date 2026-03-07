package api

import (
	"pansou/config"
)

type HealthResponse struct {
	Status         string   `json:"status" jsonschema:"状态，如果是ok则表示服务正常"`
	AuthEnabled    bool     `json:"auth_enabled" jsonschema:"是否启用认证，如果启用则需要先通过登录获取token"`
	PluginsEnabled bool     `json:"plugins_enabled" jsonschema:"是否启用异步插件"`
	ChannelsCount  int      `json:"channels_count" jsonschema:"是否启用异步插件"`
	Channels       []string `json:"channels" jsonschema:"异步插件列表"`
	PluginCount    int      `json:"plugin_count" jsonschema:"插件数量"`
	Plugins        []string `json:"plugins" jsonschema:"插件列表"`
}

func Health() HealthResponse {
	// 根据配置决定是否返回插件信息
	pluginCount := 0
	pluginNames := []string{}
	pluginsEnabled := config.AppConfig.AsyncPluginEnabled

	if pluginsEnabled && searchService != nil && searchService.GetPluginManager() != nil {
		plugins := searchService.GetPluginManager().GetPlugins()
		pluginCount = len(plugins)
		for _, p := range plugins {
			pluginNames = append(pluginNames, p.Name())
		}
	}
	// 获取频道信息
	channels := config.AppConfig.DefaultChannels
	channelsCount := len(channels)

	response := HealthResponse{
		Status:         "ok",
		AuthEnabled:    config.AppConfig.AuthEnabled, // 添加认证状态
		PluginsEnabled: pluginsEnabled,
		Channels:       channels,
		ChannelsCount:  channelsCount,
	}

	// 只有当插件启用时才返回插件相关信息
	if pluginsEnabled {
		response.PluginCount = pluginCount
		response.Plugins = pluginNames
	}

	return response
}
