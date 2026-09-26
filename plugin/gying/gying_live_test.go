package gying

import (
	"os"
	"testing"

	"pansou/plugin"
)

// 站点在 2026-09 加了 browser_pow 计算验证，当时一度被判定为已关闭。
// 实际站点仍在运行（登录 + 搜索均已实测通过），所以这里改回按环境变量开关，
// 不再无条件跳过——无条件跳过会让人误以为站点不可用，从而放过真实回归。
func TestGyingLiveLoginAndSearch(t *testing.T) {
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("set GYING_TEST_USERNAME and GYING_TEST_PASSWORD to run the live test")
	}

	p := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}
	scraper, cookie, err := p.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin() error = %v", err)
	}
	if scraper == nil || cookie == "" {
		t.Fatalf("doLogin() returned incomplete session: scraper=%v cookieLen=%d", scraper != nil, len(cookie))
	}

	results, err := p.searchWithScraper("一人之下", scraper)
	if err != nil {
		t.Fatalf("searchWithScraper() error = %v", err)
	}
	t.Logf("received %d results", len(results))
	for _, result := range results {
		if len(result.Links) == 0 {
			t.Errorf("result %q has no links", result.Title)
		}
	}
}
