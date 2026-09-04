package u3c3

import (
	"os"
	"strings"
	"testing"

	"pansou/plugin"
)

func TestParseSearchResultsHasLinks(t *testing.T) {
	p := &U3c3Plugin{BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("u3c3", 5, true), activeURL: legacyBaseURL}
	fixture := `<table><tbody><tr class="default"><td><a title="Anime">Anime</a></td><td><a href="/torrent/1">One Piece 001</a></td><td><a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567">magnet</a></td><td>1 GB</td><td>2026-09-04</td></tr></tbody></table>`
	results, err := p.parseSearchResults(fixture)
	if err != nil {
		t.Fatalf("parseSearchResults() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("parseSearchResults() returned no results")
	}
	for _, result := range results {
		if len(result.Links) == 0 || !strings.HasPrefix(result.Links[0].URL, "magnet:") {
			t.Fatalf("result %q has invalid links: %#v", result.Title, result.Links)
		}
	}
}

func TestU3c3LiveSearch(t *testing.T) {
	if os.Getenv("U3C3_LIVE_TEST") == "" {
		t.Skip("set U3C3_LIVE_TEST=1 to run the live source test")
	}

	p := &U3c3Plugin{BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("u3c3", 5, true), activeURL: BaseURL}
	search2, err := p.getSearch2Parameter()
	if err != nil {
		t.Fatalf("getSearch2Parameter() error = %v", err)
	}
	t.Logf("active URL=%s search2=%s", p.getActiveURL(), search2)
	results, err := p.doSearch("One Piece", search2)
	if err != nil {
		t.Fatalf("doSearch() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("doSearch() returned no results")
	}
	for _, result := range results {
		if len(result.Links) == 0 {
			t.Fatalf("result %q has no links", result.Title)
		}
	}
}
