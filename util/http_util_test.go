package util

import (
	"net/http"
	"testing"
	"time"

	"pansou/config"
)

// 插件框架与 77 个插件文件都自建 &http.Client{Timeout: ...}（Transport 为 nil），
// 因此它们落在 http.DefaultTransport 上。DefaultTransport 默认每主机只保留 2 条
// 空闲连接，并发突发后必须重新握手；这里验证调优确实生效。
func TestTuneDefaultTransportAppliesPoolSettings(t *testing.T) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport 类型异常: %T", http.DefaultTransport)
	}

	originalPerHost := transport.MaxIdleConnsPerHost
	originalIdleTimeout := transport.IdleConnTimeout
	t.Cleanup(func() {
		transport.MaxIdleConnsPerHost = originalPerHost
		transport.IdleConnTimeout = originalIdleTimeout
	})

	TuneDefaultTransport()

	if transport.MaxIdleConnsPerHost != 20 {
		t.Errorf("MaxIdleConnsPerHost = %d, 期望 20", transport.MaxIdleConnsPerHost)
	}
	if transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConns(%d) 不应小于 MaxIdleConnsPerHost(%d)",
			transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout != 90*time.Second {
		t.Errorf("IdleConnTimeout = %v, 期望 90s", transport.IdleConnTimeout)
	}
}

// 共享客户端只认 config.ProxyURL，不认 HTTP_PROXY 环境变量；而 DefaultTransport
// 的 ProxyFromEnvironment 是插件当前唯一有效的代理来源。调优连接池时一旦顺手改了
// Proxy，插件的代理行为会被直接破坏，所以这个不变量必须锁住。
func TestTuneDefaultTransportKeepsEnvironmentProxy(t *testing.T) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport 类型异常: %T", http.DefaultTransport)
	}

	TuneDefaultTransport()

	if transport.Proxy == nil {
		t.Fatal("调优后 Proxy 为 nil，插件的环境变量代理会失效")
	}

	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("解析代理失败: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:7897" {
		t.Fatalf("HTTPS_PROXY 未被使用: %v", proxyURL)
	}
}

func TestUpstreamPoolSettings(t *testing.T) {
	saved := config.AppConfig
	t.Cleanup(func() { config.AppConfig = saved })

	config.AppConfig = nil
	perHost, idleTimeout := upstreamPoolSettings()
	if perHost != 20 || idleTimeout != 90*time.Second {
		t.Errorf("默认值 = (%d, %v), 期望 (20, 90s)", perHost, idleTimeout)
	}

	config.AppConfig = &config.Config{
		UpstreamMaxIdleConnsPerHost: 55,
		UpstreamIdleConnTimeout:     600 * time.Second,
	}
	perHost, idleTimeout = upstreamPoolSettings()
	if perHost != 55 || idleTimeout != 600*time.Second {
		t.Errorf("配置值 = (%d, %v), 期望 (55, 600s)", perHost, idleTimeout)
	}
}