package util

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
	"pansou/config"
)

// 全局HTTP客户端
var httpClient *http.Client

// InitHTTPClient 初始化HTTP客户端
func InitHTTPClient() {
	proxyURL := ""
	if config.AppConfig != nil {
		proxyURL = config.AppConfig.ProxyURL
	}

	client, err := NewHTTPClient(proxyURL)
	if err != nil {
		client, _ = NewHTTPClient("")
	}
	httpClient = client

	// 插件走的是 http.DefaultTransport，也要一并调优，否则插件路径仍然
	// 每主机只保留 2 条空闲连接。
	TuneDefaultTransport()
}

// upstreamPoolSettings 返回上游连接池设置，供共享客户端与 DefaultTransport 共用。
func upstreamPoolSettings() (int, time.Duration) {
	maxIdleConnsPerHost := 20
	idleConnTimeout := 90 * time.Second
	if config.AppConfig != nil {
		if config.AppConfig.UpstreamMaxIdleConnsPerHost > 0 {
			maxIdleConnsPerHost = config.AppConfig.UpstreamMaxIdleConnsPerHost
		}
		if config.AppConfig.UpstreamIdleConnTimeout > 0 {
			idleConnTimeout = config.AppConfig.UpstreamIdleConnTimeout
		}
	}
	return maxIdleConnsPerHost, idleConnTimeout
}

// TuneDefaultTransport 调优 http.DefaultTransport 的连接池。
//
// plugin/ 下 77 个文件、104 处自建 &http.Client{}，没有任何插件使用共享客户端；
// 而插件框架自己给的客户端也是 &http.Client{Timeout: ...}（Transport 为 nil），
// 它们全都落在 http.DefaultTransport 上。DefaultTransport 默认每主机只保留
// 2 条空闲连接：并发突发结束后多余连接立刻被关闭，下一轮搜索必须重新做
// TCP+TLS 握手。实测（5 轮 × 8 并发，同一主机）新建连接 32 条降至 8 条。
//
// 除了连接池，这里还要统一代理解析：DefaultTransport 自带的
// ProxyFromEnvironment 只识别 HTTP_PROXY/HTTPS_PROXY/NO_PROXY，不认识
// 本项目文档中的 PROXY，也不认识 ALL_PROXY。插件路径恰恰全走
// DefaultTransport，所以只设置 PROXY 或 ALL_PROXY 时插件会整体退回直连
// （TG 频道路径走共享客户端，不受影响）。详见 BuildProxyFunc。
func TuneDefaultTransport() {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	maxIdleConnsPerHost, idleConnTimeout := upstreamPoolSettings()
	transport.MaxIdleConnsPerHost = maxIdleConnsPerHost
	transport.MaxIdleConns = max(100, maxIdleConnsPerHost)
	transport.IdleConnTimeout = idleConnTimeout

	rawProxyURL := ""
	if config.AppConfig != nil {
		rawProxyURL = config.AppConfig.ProxyURL
	}
	if err := applyProxyFunc(transport, rawProxyURL); err != nil {
		fmt.Printf("[HTTP] 代理解析失败，插件路径退回标准环境变量代理: %v\n", err)
	}

	fmt.Printf("[HTTP] 已调优 DefaultTransport：每主机空闲连接 %d，保活 %v，代理=%s（插件均使用该连接池）\n",
		maxIdleConnsPerHost, idleConnTimeout, describeProxy(rawProxyURL))
}

// describeProxy 用于启动日志，避免把含账号密码的代理地址整条打出来。
func describeProxy(rawProxyURL string) string {
	if strings.TrimSpace(rawProxyURL) == "" {
		return "标准环境变量(HTTP_PROXY/HTTPS_PROXY/NO_PROXY)"
	}
	return "已配置代理"
}

// NewHTTPClient 创建HTTP客户端，可按需指定本客户端使用的代理。
func NewHTTPClient(proxyURL string) (*http.Client, error) {
	// 连接池配置：空闲连接保活时间直接影响"快速兜底"这类密集访问路径的
	// 冷启动成本——连接一旦被回收，下次搜索要多付一次 TCP+TLS 握手。
	maxIdleConnsPerHost, idleConnTimeout := upstreamPoolSettings()
	maxIdleConns := 100
	if maxIdleConns < maxIdleConnsPerHost {
		maxIdleConns = maxIdleConnsPerHost
	}

	// 创建传输配置
	transport := &http.Transport{
		// 启用HTTP/2
		ForceAttemptHTTP2: true,

		// TLS配置
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false, // 生产环境应设为false
		},

		// 连接池优化
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		// TCP连接优化
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
	}

	if err := applyProxyFunc(transport, proxyURL); err != nil {
		return nil, err
	}

	// 创建客户端
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(60) * time.Second,
	}

	return client, nil
}

// BuildProxyFunc 构造统一的代理解析函数。
//
// 代理来源优先级由 config.getProxyURL 决定：PROXY 优先，其次兼容
// HTTPS_PROXY/HTTP_PROXY/ALL_PROXY。这里必须显式构造，不能依赖
// http.ProxyFromEnvironment——它只识别 HTTP_PROXY/HTTPS_PROXY/NO_PROXY，
// 不认识本项目文档中的 PROXY，也不认识 ALL_PROXY。插件路径走的是
// http.DefaultTransport，此前正是因此漏掉了这两种配置：
// 只设置 PROXY 或 ALL_PROXY 时，插件会全部退回直连。
//
// 显式配置代理时按 NO_PROXY 放行直连；未配置任何代理时交回
// http.ProxyFromEnvironment，保持标准语义。
func BuildProxyFunc(rawProxyURL string, noProxy string) (func(*url.URL) (*url.URL, error), error) {
	raw := strings.TrimSpace(rawProxyURL)
	if raw == "" {
		// 保持标准语义，含 ProxyFromEnvironment 对环境变量的单次缓存行为
		return func(u *url.URL) (*url.URL, error) {
			return http.ProxyFromEnvironment(&http.Request{URL: u})
		}, nil
	}

	normalized, err := normalizeProxyURL(raw)
	if err != nil {
		return nil, err
	}

	cfg := &httpproxy.Config{
		HTTPProxy:  normalized,
		HTTPSProxy: normalized,
		NoProxy:    strings.TrimSpace(noProxy),
	}
	return cfg.ProxyFunc(), nil
}

// normalizeProxyURL 校验并归一化代理地址。
// socks5h 归一化为 socks5：Transport 原生支持 socks5，两者都本地解析域名。
func normalizeProxyURL(raw string) (string, error) {
	proxyURL, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("代理地址解析失败: %w", err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return "", fmt.Errorf("代理地址必须包含协议和主机")
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "socks5", "socks5h":
		clone := *proxyURL
		clone.Scheme = "socks5"
		return clone.String(), nil
	case "http", "https":
		return proxyURL.String(), nil
	default:
		return "", fmt.Errorf("不支持的代理协议: %s", proxyURL.Scheme)
	}
}

// applyProxyFunc 把统一解析出的代理解析函数装到 transport 上。
// Transport.Proxy 的签名是 func(*http.Request)，httpproxy 给的是
// func(*url.URL)，这里做一次适配。
func applyProxyFunc(transport *http.Transport, rawProxyURL string) error {
	proxyFunc, err := BuildProxyFunc(rawProxyURL, configuredNoProxy())
	if err != nil {
		return err
	}
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		return proxyFunc(req.URL)
	}
	return nil
}

func configuredNoProxy() string {
	if config.AppConfig != nil {
		return config.AppConfig.NoProxy
	}
	return ""
}

// GetHTTPClient 获取HTTP客户端
func GetHTTPClient() *http.Client {
	if httpClient == nil {
		InitHTTPClient()
	}
	return httpClient
}

// FetchHTML 获取HTML内容
func FetchHTML(targetURL string) (string, error) {
	// 使用优化后的HTTP客户端
	client := GetHTTPClient()

	// 创建请求
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return "", err
	}

	// 设置请求头
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.5")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 读取响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// BuildSearchURL 构建搜索URL
func BuildSearchURL(channel string, keyword string, nextPageParam string) string {
	baseURL := "https://t.me/s/" + channel
	if keyword != "" {
		baseURL += "?q=" + url.QueryEscape(keyword)
		if nextPageParam != "" {
			baseURL += "&" + nextPageParam
		}
	}
	return baseURL
}
