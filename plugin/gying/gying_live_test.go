package gying

import (
	"os"
	"testing"

	"pansou/plugin"
)

// gying 站点已关闭（2026-09 确认），此实测用例无法再通过。
// 保留主体代码用于站点恢复后复验，但先无条件跳过，避免误以为可以跑通。
func TestGyingLiveLoginAndSearch(t *testing.T) {
	t.Skip("gying 站点已关闭，实测用例暂停；站点恢复后移除本行即可复验")

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
