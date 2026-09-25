package plugin

import (
	"testing"
	"time"

	"pansou/config"
)

func withAppConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	saved := config.AppConfig
	config.AppConfig = cfg
	t.Cleanup(func() { config.AppConfig = saved })
}

// 插件在各自包的 init() 里构造，早于 main 中的 config.Init()，构造那一刻
// config.AppConfig 恒为 nil。此前超时与缓存有效期在构造阶段就被写死，于是
// ASYNC_RESPONSE_TIMEOUT / PLUGIN_TIMEOUT / ASYNC_CACHE_TTL_HOURS 只要不是
// 硬编码默认值（4s/30s/1h）就完全不生效。
// 这个用例复现真实启动顺序：先构造，配置生效后才第一次取用。
func TestPluginClientsResolveConfigAtCallTime(t *testing.T) {
	withAppConfig(t, nil)
	p := NewBaseAsyncPlugin("lazy-timeouts", 1)

	// 配置在构造之后才可用
	withAppConfig(t, &config.Config{
		AsyncResponseTimeoutDur: 20 * time.Second,
		PluginTimeout:           60 * time.Second,
		AsyncCacheTTLHours:      5,
	})

	if got := p.GetClient().Timeout; got != 20*time.Second {
		t.Errorf("短超时客户端 Timeout = %v, 期望 20s（配置未生效）", got)
	}
	if got := p.backgroundHTTPClient().Timeout; got != 60*time.Second {
		t.Errorf("长超时客户端 Timeout = %v, 期望 60s（配置未生效）", got)
	}
	if got := p.getCacheTTL(); got != 5*time.Hour {
		t.Errorf("缓存有效期 = %v, 期望 5h（配置未生效）", got)
	}
}

// 配置缺失时退回硬编码默认值，保持插件可独立构造（例如单元测试里）。
func TestPluginClientsFallBackToDefaultsWithoutConfig(t *testing.T) {
	withAppConfig(t, nil)
	p := NewBaseAsyncPlugin("no-config", 1)

	if got := p.GetClient().Timeout; got != defaultAsyncResponseTimeout {
		t.Errorf("短超时客户端 Timeout = %v, 期望 %v", got, defaultAsyncResponseTimeout)
	}
	if got := p.backgroundHTTPClient().Timeout; got != defaultPluginTimeout {
		t.Errorf("长超时客户端 Timeout = %v, 期望 %v", got, defaultPluginTimeout)
	}
	if got := p.getCacheTTL(); got != defaultCacheTTL {
		t.Errorf("缓存有效期 = %v, 期望 %v", got, defaultCacheTTL)
	}
}

// 客户端按需求创建一次，重复取用必须返回同一实例，避免每次搜索都新建
// http.Client（那会丢掉连接复用）。
func TestPluginClientIsCreatedOnce(t *testing.T) {
	withAppConfig(t, &config.Config{AsyncResponseTimeoutDur: 7 * time.Second})
	p := NewBaseAsyncPlugin("once", 1)

	first := p.GetClient()
	if first != p.GetClient() {
		t.Fatal("重复取用应返回同一实例")
	}
	if got := first.Timeout; got != 7*time.Second {
		t.Errorf("Timeout = %v, 期望 7s", got)
	}
}
