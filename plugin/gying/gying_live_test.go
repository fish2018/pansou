package gying

import (
	"os"
	"testing"

	"pansou/plugin"
)

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
