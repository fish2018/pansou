package xdpan

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pansou/model"
)

func TestSearchImplUsesMigratedBaiduRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("k") != "仙逆" || r.URL.Query().Get("p") != "baidu" || r.URL.Query().Get("page") != "1" {
				t.Errorf("unexpected search query: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<html><body><van-row><a href="/s/test-id"><van-col><van-card><template><div name="content-title"><span>仙</span><span>逆</span><span> 4K</span></div><div>时间: 2026-09-01 格式: <b>文件夹</b></div></template></van-card></van-col></a></van-row></body></html>`)
		case "/s/test-id":
			fmt.Fprint(w, `<html><body><van-cell title="密码"><b>6666</b></van-cell><script>function onDownload(){window.open('https://pan.baidu.com/s/abc')}</script></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := NewXdpanPlugin()
	p.baseURL = server.URL
	results, err := p.searchImpl(server.Client(), "仙逆", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %#v", results)
	}
	result := results[0]
	if result.Channel != "" || result.Title != "仙逆4K" || len(result.Links) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Links[0].Type != "baidu" || result.Links[0].Password != "6666" || result.Links[0].WorkTitle != "仙逆4K" || !strings.Contains(result.Links[0].URL, "pwd=6666") {
		t.Fatalf("unexpected link: %#v", result.Links[0])
	}
}

func TestFetchSearchResultsReportsCloudflareChallenge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<title>Just a moment...</title>`)
	}))
	defer server.Close()

	p := NewXdpanPlugin()
	p.baseURL = server.URL
	_, err := p.fetchSearchResults(server.Client(), "仙逆")
	if err == nil || !strings.Contains(err.Error(), "Cloudflare") {
		t.Fatalf("expected Cloudflare error, got %v", err)
	}
}

func TestFilterResultsWithLinks(t *testing.T) {
	results := filterResultsWithLinks([]model.SearchResult{
		{Title: "invalid"},
		{Title: "valid", Links: []model.Link{{URL: "https://pan.baidu.com/s/abc"}}},
	})
	if len(results) != 1 || results[0].Title != "valid" {
		t.Fatalf("unexpected filtered results: %#v", results)
	}
}
