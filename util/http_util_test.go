package util

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"pansou/config"
)

// 测试期间把 AppConfig 换成受控值，避免用例之间互相污染。
func withConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	saved := config.AppConfig
	config.AppConfig = cfg
	t.Cleanup(func() { config.AppConfig = saved })
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestBuildProxyFuncUsesConfiguredProxy(t *testing.T) {
	// 关键回归：本项目文档中的 PROXY（以及 ALL_PROXY）不被
	// http.ProxyFromEnvironment 识别，必须由这里显式解析，
	// 否则插件路径会整体退回直连。
	proxyFunc, err := BuildProxyFunc("socks5h://1.1.1.1:7897", "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := proxyFunc(mustParseURL(t, "https://ysapi.yingso.fun/test1"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("配置了代理却解析出直连")
	}
	if got.Scheme != "socks5" || got.Host != "1.1.1.1:7897" {
		t.Fatalf("socks5h 应归一化为 socks5, 实际: %s", got)
	}
}

func TestBuildProxyFuncHonoursNoProxy(t *testing.T) {
	proxyFunc, err := BuildProxyFunc("http://1.1.1.1:7897", "internal.example.com,.corp.local")
	if err != nil {
		t.Fatal(err)
	}

	bypassed, err := proxyFunc(mustParseURL(t, "https://internal.example.com/api"))
	if err != nil {
		t.Fatal(err)
	}
	if bypassed != nil {
		t.Fatalf("NO_PROXY 命中的主机应直连, 实际: %s", bypassed)
	}

	proxied, err := proxyFunc(mustParseURL(t, "https://public.example.com/api"))
	if err != nil {
		t.Fatal(err)
	}
	if proxied == nil || proxied.Host != "1.1.1.1:7897" {
		t.Fatalf("NO_PROXY 未命中的主机应走代理, 实际: %v", proxied)
	}
}

func TestBuildProxyFuncRejectsUnsupportedScheme(t *testing.T) {
	if _, err := BuildProxyFunc("ftp://1.1.1.1:7897", ""); err == nil {
		t.Fatal("不支持的代理协议应报错")
	}
	if _, err := BuildProxyFunc("1.1.1.1:7897", ""); err == nil {
		t.Fatal("缺少协议的地址应报错")
	}
}

// 未配置任何代理时保持标准语义，即继续识别 HTTP_PROXY/HTTPS_PROXY/NO_PROXY。
func TestBuildProxyFuncFallsBackToEnvironment(t *testing.T) {
	proxyFunc, err := BuildProxyFunc("", "")
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	got, err := proxyFunc(mustParseURL(t, "https://example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Host != "127.0.0.1:7897" {
		t.Fatalf("未配置代理时应走标准环境变量, 实际: %v", got)
	}
}

// 插件框架与 77 个插件文件都自建 &http.Client{Timeout: ...}（Transport 为 nil），
// 因此它们落在 http.DefaultTransport 上。DefaultTransport 默认每主机只保留 2 条
// 空闲连接，并发突发后必须重新握手；这里验证调优确实生效。
func TestTuneDefaultTransportAppliesPoolSettings(t *testing.T) {
	withConfig(t, nil)
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

// 插件路径必须能在只配置 PROXY 的情况下走代理——这正是修复前的缺口。
func TestTuneDefaultTransportWiresConfiguredProxy(t *testing.T) {
	withConfig(t, &config.Config{ProxyURL: "socks5://1.1.1.1:7897"})
	transport := http.DefaultTransport.(*http.Transport)
	t.Cleanup(func() {
		transport.Proxy = http.ProxyFromEnvironment
	})

	TuneDefaultTransport()

	if transport.Proxy == nil {
		t.Fatal("调优后 Proxy 为 nil，插件无法走代理")
	}
	req, err := http.NewRequest(http.MethodGet, "https://ysapi.yingso.fun/test1", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Scheme != "socks5" || got.Host != "1.1.1.1:7897" {
		t.Fatalf("插件路径未使用 PROXY 配置的代理: %v", got)
	}
}

func TestUpstreamPoolSettings(t *testing.T) {
	withConfig(t, nil)

	perHost, idleTimeout := upstreamPoolSettings()
	if perHost != 20 || idleTimeout != 90*time.Second {
		t.Errorf("默认值 = (%d, %v), 期望 (20, 90s)", perHost, idleTimeout)
	}

	withConfig(t, &config.Config{
		UpstreamMaxIdleConnsPerHost: 55,
		UpstreamIdleConnTimeout:     600 * time.Second,
	})
	perHost, idleTimeout = upstreamPoolSettings()
	if perHost != 55 || idleTimeout != 600*time.Second {
		t.Errorf("配置值 = (%d, %v), 期望 (55, 600s)", perHost, idleTimeout)
	}
}

// 只设置 PROXY 时，插件路径（Transport 为 nil 的客户端）必须也能走代理。
// 修复前该路径只认 HTTP_PROXY/HTTPS_PROXY，只配 PROXY 会整体退回直连。
// 用 google.com 作判别：本机直连不通、经代理可通。需要真实网络，默认跳过。
func TestPluginPathProxyWiringLive(t *testing.T) {
	if os.Getenv("PROXY_WIRING_LIVE_TEST") != "1" {
		t.Skip("set PROXY_WIRING_LIVE_TEST=1 to verify proxy wiring against a real proxy")
	}
	proxy := os.Getenv("PROXY")
	if proxy == "" {
		t.Skip("set PROXY to the local proxy address first")
	}

	// 与插件框架一致：Transport 为 nil，落在 http.DefaultTransport
	client := &http.Client{Timeout: 15 * time.Second}
	transport := http.DefaultTransport.(*http.Transport)

	// 对照组：把 Proxy 还原成修复前的状态（只认标准环境变量）。
	// 若此时也能访问成功，说明本机直连可达，用例失去判别力，需要换判别站点。
	savedProxy := transport.Proxy
	transport.Proxy = http.ProxyFromEnvironment
	_, preErr := client.Get("https://www.google.com")
	transport.Proxy = savedProxy
	if preErr == nil {
		t.Skip("本机直连即可访问判别站点，无法证明代理接线，跳过")
	}
	t.Logf("对照组（修复前）确实失败: %v", preErr)

	withConfig(t, &config.Config{ProxyURL: proxy})
	TuneDefaultTransport()

	resp, err := client.Get("https://www.google.com")
	if err != nil {
		t.Fatalf("只配置 PROXY 时插件路径未走代理: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	t.Logf("插件路径经 PROXY(%s) 访问成功: %d", proxy, resp.StatusCode)
}

// 插件手写 &http.Transport{...} 时 Proxy 字段是零值 = 永远直连。这个助手
// 保证它们与 http.DefaultTransport 走同一套代理解析。
func TestProxyFuncForTransportHonoursConfig(t *testing.T) {
	withConfig(t, &config.Config{ProxyURL: "socks5h://1.1.1.1:7897", NoProxy: "内网.example.com"})
	proxyFunc := ProxyFuncForTransport()

	req, err := http.NewRequest(http.MethodGet, "https://ysapi.yingso.fun/test1", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Scheme != "socks5" || got.Host != "1.1.1.1:7897" {
		t.Fatalf("插件手写 Transport 未走配置代理: %v", got)
	}

	bypassed, err := proxyFunc(mustRequest(t, "https://内网.example.com/api"))
	if err != nil {
		t.Fatal(err)
	}
	if bypassed != nil {
		t.Fatalf("NO_PROXY 命中的主机应直连, 实际: %v", bypassed)
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestNormalizeProxyURL(t *testing.T) {
	// 备份并恢复 lookupIPFunc
	savedLookup := lookupIPFunc
	t.Cleanup(func() { lookupIPFunc = savedLookup })

	lookupIPFunc = func(host string) ([]net.IP, error) {
		switch host {
		case "localhost":
			return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, nil
		case "public.example.com":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "intranet.example.com":
			return []net.IP{net.ParseIP("192.168.1.1")}, nil
		case "mixed.example.com":
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.1")}, nil
		case "metadata.internal":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "unknown.domain.invalid":
			return nil, fmt.Errorf("no such host")
		default:
			return net.LookupIP(host)
		}
	}

	tests := []struct {
		name        string
		rawURL      string
		wantURL     string
		expectError bool
	}{
		// 合法外部公开地址
		{
			name:    "public IPv4 HTTP",
			rawURL:  "http://1.1.1.1:8080",
			wantURL: "http://1.1.1.1:8080",
		},
		{
			name:    "public IPv4 HTTPS",
			rawURL:  "https://8.8.8.8:443",
			wantURL: "https://8.8.8.8:443",
		},
		{
			name:    "public IPv4 SOCKS5",
			rawURL:  "socks5://93.184.216.34:1080",
			wantURL: "socks5://93.184.216.34:1080",
		},
		{
			name:    "public IPv4 SOCKS5H normalized to socks5",
			rawURL:  "socks5h://1.0.0.1:1080",
			wantURL: "socks5://1.0.0.1:1080",
		},
		{
			name:    "public domain resolved",
			rawURL:  "http://public.example.com:8080",
			wantURL: "http://public.example.com:8080",
		},
		// 环回地址 (Loopback)
		{
			name:        "reject IPv4 loopback 127.0.0.1",
			rawURL:      "http://127.0.0.1:8080",
			expectError: true,
		},
		{
			name:        "reject IPv4 loopback 127.1.2.3",
			rawURL:      "socks5://127.1.2.3:1080",
			expectError: true,
		},
		{
			name:        "reject IPv6 loopback ::1",
			rawURL:      "http://[::1]:8080",
			expectError: true,
		},
		{
			name:        "reject localhost domain",
			rawURL:      "http://localhost:8080",
			expectError: true,
		},
		// 私有地址 (Private RFC 1918 / RFC 4193)
		{
			name:        "reject 10.0.0.0/8",
			rawURL:      "http://10.0.0.1:8080",
			expectError: true,
		},
		{
			name:        "reject 172.16.0.0/12",
			rawURL:      "http://172.16.0.1:8080",
			expectError: true,
		},
		{
			name:        "reject 192.168.0.0/16",
			rawURL:      "http://192.168.1.1:8080",
			expectError: true,
		},
		{
			name:        "reject IPv6 ULA fc00::/7",
			rawURL:      "http://[fc00::1]:8080",
			expectError: true,
		},
		{
			name:        "reject domain resolving to private IP",
			rawURL:      "http://intranet.example.com:8080",
			expectError: true,
		},
		{
			name:        "reject domain with mixed IPs containing private IP",
			rawURL:      "http://mixed.example.com:8080",
			expectError: true,
		},
		// 链路本地与云元数据地址 (Link-local / 169.254.169.254)
		{
			name:        "reject cloud metadata 169.254.169.254",
			rawURL:      "http://169.254.169.254:80",
			expectError: true,
		},
		{
			name:        "reject link-local 169.254.0.1",
			rawURL:      "http://169.254.0.1:8080",
			expectError: true,
		},
		{
			name:        "reject IPv6 link-local fe80::/10",
			rawURL:      "http://[fe80::1]:8080",
			expectError: true,
		},
		{
			name:        "reject domain resolving to metadata IP",
			rawURL:      "http://metadata.internal:80",
			expectError: true,
		},
		// 未指定地址 (Unspecified)
		{
			name:        "reject IPv4 0.0.0.0",
			rawURL:      "http://0.0.0.0:8080",
			expectError: true,
		},
		{
			name:        "reject IPv6 unspecified ::",
			rawURL:      "http://[::]:8080",
			expectError: true,
		},
		// 广播与组播
		{
			name:        "reject IPv4 multicast",
			rawURL:      "http://224.0.0.1:8080",
			expectError: true,
		},
		{
			name:        "reject IPv4 broadcast",
			rawURL:      "http://255.255.255.255:8080",
			expectError: true,
		},
		// 域名解析失败
		{
			name:        "reject unresolvable domain",
			rawURL:      "http://unknown.domain.invalid:8080",
			expectError: true,
		},
		// 格式与协议校验
		{
			name:        "reject unsupported ftp scheme",
			rawURL:      "ftp://1.1.1.1:21",
			expectError: true,
		},
		{
			name:        "reject missing scheme",
			rawURL:      "1.1.1.1:8080",
			expectError: true,
		},
		{
			name:        "reject empty host",
			rawURL:      "http://:8080",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeProxyURL(tt.rawURL)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error for %q, but got success: %q", tt.rawURL, got)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for %q: %v", tt.rawURL, err)
				}
				if got != tt.wantURL {
					t.Fatalf("normalizeProxyURL(%q) = %q, want %q", tt.rawURL, got, tt.wantURL)
				}
			}
		})
	}
}

func TestValidateProxyHost(t *testing.T) {
	if err := ValidateProxyHost("127.0.0.1"); err == nil {
		t.Fatal("expected error for 127.0.0.1")
	}
	if err := ValidateProxyHost("127.0.0.1:8080"); err == nil {
		t.Fatal("expected error for 127.0.0.1:8080")
	}
	if err := ValidateProxyHost("169.254.169.254"); err == nil {
		t.Fatal("expected error for 169.254.169.254")
	}
	if err := ValidateProxyHost("10.10.10.10:1080"); err == nil {
		t.Fatal("expected error for 10.10.10.10:1080")
	}
	if err := ValidateProxyHost("[::1]:8080"); err == nil {
		t.Fatal("expected error for [::1]:8080")
	}
	if err := ValidateProxyHost("1.1.1.1"); err != nil {
		t.Fatalf("unexpected error for 1.1.1.1: %v", err)
	}
	if err := ValidateProxyHost("1.1.1.1:8080"); err != nil {
		t.Fatalf("unexpected error for 1.1.1.1:8080: %v", err)
	}
	if err := ValidateProxyHost(""); err == nil {
		t.Fatal("expected error for empty host")
	}
}
