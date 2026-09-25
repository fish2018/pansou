package util

import (
	"strings"
	"testing"
)

// 代理地址常写成 http://user:pass@host:port。main.go 启动时原先直接打印原文，
// 等于把凭据写进日志与容器日志采集链路。
func TestMaskProxyURLHidesCredentials(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		mustNot  []string
		mustHave []string
	}{
		{
			name:     "带用户名密码的 HTTP 代理",
			in:       "http://alice:s3cr3t@proxy.example.com:8080",
			mustNot:  []string{"alice", "s3cr3t"},
			mustHave: []string{"proxy.example.com:8080"},
		},
		{
			name:     "socks5 带凭据",
			in:       "socks5://bob:hunter2@127.0.0.1:1080",
			mustNot:  []string{"bob", "hunter2"},
			mustHave: []string{"127.0.0.1:1080"},
		},
		{
			name:     "无凭据的代理地址保持可读",
			in:       "http://127.0.0.1:7897",
			mustNot:  []string{},
			mustHave: []string{"127.0.0.1:7897"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MaskProxyURL(c.in)
			for _, bad := range c.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("输出泄漏了 %q: %s", bad, got)
				}
			}
			for _, want := range c.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("输出应保留 %q: %s", want, got)
				}
			}
		})
	}
}

// 解析不出来时不能回退成打印原文——那等于脱敏失效。
func TestMaskProxyURLNeverFallsBackToRaw(t *testing.T) {
	for _, in := range []string{"不是地址", "://", "user:pass@", "  "} {
		got := MaskProxyURL(in)
		if strings.Contains(got, "pass") || strings.Contains(got, "不是地址") {
			t.Errorf("输入 %q 的输出泄漏了原文: %s", in, got)
		}
	}
	if MaskProxyURL("") != "未配置" {
		t.Errorf("空值应给出明确文案: %s", MaskProxyURL(""))
	}
}

// channel 直接来自请求参数，未转义时可注入 ? / # 污染路径与查询串。
func TestBuildSearchURLEscapesChannel(t *testing.T) {
	got := BuildSearchURL("evil?x=1#frag", "仙逆", "")
	if strings.Contains(got, "evil?x=1") {
		t.Errorf("channel 未转义，查询串被污染: %s", got)
	}
	if !strings.Contains(got, "t.me/s/") {
		t.Errorf("host 应仍是 t.me: %s", got)
	}
	// 查询参数必须仍然是关键词自己的
	if !strings.Contains(got, "?q=") {
		t.Errorf("关键词参数丢失: %s", got)
	}
}

// 合法频道名不受转义影响（否则会把正常搜索带坏）。
func TestBuildSearchURLKeepsValidChannelReadable(t *testing.T) {
	got := BuildSearchURL("tgsearchers6", "仙逆", "")
	if !strings.Contains(got, "t.me/s/tgsearchers6") {
		t.Errorf("合法频道名被改动了: %s", got)
	}
}
