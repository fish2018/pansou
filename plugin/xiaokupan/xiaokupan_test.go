package xiaokupan

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const testSearchResponse = `{"t":10,"i":0,"p":{"k":["result"],"v":[{"t":10,"i":1,"p":{"k":["searchResults"],"v":[{"t":10,"i":2,"p":{"k":["total","merged_by_type"],"v":[{"t":0,"s":2},{"t":10,"i":3,"p":{"k":["quark","baidu"],"v":[{"t":9,"i":4,"a":[{"t":10,"i":5,"p":{"k":["url","password","note","datetime","source","images"],"v":[{"t":1,"s":"https://pan.quark.cn/s/abc"},{"t":1,"s":""},{"t":1,"s":"仙逆 4K 链接:"},{"t":1,"s":"2026-09-01T01:02:03Z"},{"t":1,"s":"tg:test"},{"t":9,"i":6,"a":[{"t":1,"s":"https://example.com/poster.jpg"}]}]}}]},{"t":9,"i":7,"a":[{"t":10,"i":8,"p":{"k":["url","password","note","datetime","source","images"],"v":[{"t":1,"s":"https://pan.baidu.com/s/xyz?pwd=6666"},{"t":1,"s":"6666"},{"t":1,"s":"仙逆 1080P"},{"t":1,"s":"2026-08-31T01:02:03Z"},{"t":1,"s":"web:test"},{"t":9,"i":9,"a":[]}]}}]}]}}]}}]}}]}}`

func TestParseSearchResponse(t *testing.T) {
	results, err := parseSearchResponse([]byte(testSearchResponse))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %#v", results)
	}
	if results[0].Channel != "" || results[0].Title != "仙逆 4K" || results[0].Links[0].Type != "quark" {
		t.Fatalf("unexpected first result: %#v", results[0])
	}
	if len(results[0].Images) != 1 || results[0].Links[0].WorkTitle != "仙逆 4K" {
		t.Fatalf("unexpected first result metadata: %#v", results[0])
	}
	if results[1].Links[0].Password != "6666" || !strings.HasPrefix(results[1].UniqueID, "xiaokupan-") {
		t.Fatalf("unexpected second result: %#v", results[1])
	}
}

func TestSearchImpl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_serverFn/test-function" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-tsr-serverFn") != "true" {
			t.Errorf("missing server function header")
		}
		if !strings.Contains(r.URL.Query().Get("payload"), "仙逆") {
			t.Errorf("payload does not contain keyword: %s", r.URL.Query().Get("payload"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, testSearchResponse)
	}))
	defer server.Close()

	p := NewXiaokupanPlugin()
	p.baseURL = server.URL
	p.serverFunctionID = "test-function"
	results, err := p.searchImpl(server.Client(), " 仙逆 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 filtered results, got %#v", results)
	}
}

func TestDiscoverServerFunctionID(t *testing.T) {
	wantID := strings.Repeat("a", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<script type="module" src="/assets/index-test.js"></script>`)
		case "/assets/index-test.js":
			fmt.Fprintf(w, "var search=as(`%s`),route=jc(`/s/$query`);", wantID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := NewXiaokupanPlugin()
	p.baseURL = server.URL
	gotID, err := p.discoverServerFunctionID(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if gotID != wantID {
		t.Fatalf("expected %s, got %s", wantID, gotID)
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("XIAOKUPAN_LIVE_TEST") != "1" {
		t.Skip("set XIAOKUPAN_LIVE_TEST=1 to query xiaokupan.com")
	}
	keyword := os.Getenv("XIAOKUPAN_LIVE_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}

	p := NewXiaokupanPlugin()
	results, err := p.searchImpl(&http.Client{Timeout: 35 * time.Second}, keyword, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	if results[0].Channel != "" || len(results[0].Links) == 0 || results[0].Links[0].URL == "" {
		t.Fatalf("live search returned invalid result: %#v", results[0])
	}
	t.Logf("live search for %q returned %d results; first: %q (%s)", keyword, len(results), results[0].Title, results[0].Links[0].URL)
}
