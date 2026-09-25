package util

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
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
}

// NewHTTPClient 创建HTTP客户端，可按需指定本客户端使用的代理。
func NewHTTPClient(proxyURL string) (*http.Client, error) {
	// 连接池配置：空闲连接保活时间直接影响"快速兜底"这类密集访问路径的
	// 冷启动成本——连接一旦被回收，下次搜索要多付一次 TCP+TLS 握手。
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

	if err := applyProxy(transport, proxyURL); err != nil {
		return nil, err
	}

	// 创建客户端
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(60) * time.Second,
	}

	return client, nil
}

func applyProxy(transport *http.Transport, rawProxyURL string) error {
	rawProxyURL = strings.TrimSpace(rawProxyURL)
	if rawProxyURL == "" {
		return nil
	}

	proxyURL, err := url.Parse(rawProxyURL)
	if err != nil {
		return fmt.Errorf("代理地址解析失败: %w", err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return fmt.Errorf("代理地址必须包含协议和主机")
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "socks5", "socks5h":
		if proxyURL.Scheme == "socks5h" {
			clone := *proxyURL
			clone.Scheme = "socks5"
			proxyURL = &clone
		}

		// 创建SOCKS5代理拨号器
		dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return fmt.Errorf("SOCKS5代理初始化失败: %w", err)
		}

		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	case "http", "https":
		// HTTP/HTTPS代理
		transport.Proxy = http.ProxyURL(proxyURL)
	default:
		return fmt.Errorf("不支持的代理协议: %s", proxyURL.Scheme)
	}

	return nil
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
