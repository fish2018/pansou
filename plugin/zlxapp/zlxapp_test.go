package zlxapp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestParseListItemsAndConvert(t *testing.T) {
	html := `<!doctype html><script>
const listItemsData = [{"id":26226,"title":"仙逆（更至155）","description":"","is_type":2,"code":"6666","url":"https:\/\/pan.baidu.com\/s\/fallback?pwd=6666","times":"2026-09-01","category":{"name":"全网搜"},"tag_names":["动漫"],"links":[{"id":26306,"url":"https:\/\/pan.baidu.com\/s\/1abc?pwd=6666","code":"6666","is_type":2,"target_name":"仙逆 百度","source_title":"仙逆（更至155）","status":1,"is_delete":0},{"id":26307,"url":"https:\/\/pan.quark.cn\/s\/quark123","code":"","is_type":0,"target_name":"","source_title":"仙逆 夸克","status":1,"is_delete":0}]}];
const otherData = [{"nested":[1,2,3]}];
</script>`

	items, err := parseListItems([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	result, ok := convertItem(items[0])
	if !ok {
		t.Fatal("expected item to convert")
	}
	if result.UniqueID != "zlxapp-26226" || result.Channel != "" || result.Title != "仙逆（更至155）" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.Links) != 2 {
		t.Fatalf("expected 2 links, got %#v", result.Links)
	}
	if result.Links[0].Type != "baidu" || result.Links[0].Password != "6666" || result.Links[0].WorkTitle != "仙逆 百度" {
		t.Fatalf("unexpected baidu link: %#v", result.Links[0])
	}
	if result.Links[1].Type != "quark" || result.Links[1].WorkTitle != "仙逆 夸克" {
		t.Fatalf("unexpected quark link: %#v", result.Links[1])
	}
	if len(result.Tags) != 2 || result.Tags[0] != "动漫" || result.Tags[1] != "全网搜" {
		t.Fatalf("unexpected tags: %#v", result.Tags)
	}
}

func TestSearchImpl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/%E4%BB%99%E9%80%86.html" && r.URL.EscapedPath() != "/s/%E4%BB%99%E9%80%86.html" {
			t.Errorf("unexpected path: %s (%s)", r.URL.Path, r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<script>const listItemsData = [
{"id":1,"title":"仙逆 4K","description":"","is_type":0,"code":"","url":"https:\/\/pan.quark.cn\/s\/abc","times":"2026-09-01","category":{"name":"动漫"},"links":[]},
{"id":2,"title":"无关内容","description":"","is_type":0,"code":"","url":"https:\/\/pan.quark.cn\/s\/other","times":"","category":{"name":""},"links":[]},
{"id":3,"title":"仙逆 未知网盘","description":"","is_type":9,"code":"","url":"https:\/\/example.com\/invalid","times":"","category":{"name":""},"links":[]}
];</script>`)
	}))
	defer server.Close()

	p := NewZlxappPlugin()
	p.baseURL = server.URL
	results, err := p.searchImpl(server.Client(), " 仙逆 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 filtered result, got %#v", results)
	}
	if results[0].Links[0].URL != "https://pan.quark.cn/s/abc" || results[0].Links[0].WorkTitle != "仙逆 4K" {
		t.Fatalf("unexpected link: %#v", results[0].Links[0])
	}
}

func TestFindJSONArrayEndHandlesBracketsInStrings(t *testing.T) {
	data := []byte(`[{"title":"测试 ] [ 资源","nested":[1,2]}];`)
	end, err := findJSONArrayEnd(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data[:end+1]); got != `[{"title":"测试 ] [ 资源","nested":[1,2]}]` {
		t.Fatalf("unexpected array: %s", got)
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("ZLXAPP_LIVE_TEST") != "1" {
		t.Skip("set ZLXAPP_LIVE_TEST=1 to query zlxapp.top")
	}
	keyword := os.Getenv("ZLXAPP_LIVE_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}

	p := NewZlxappPlugin()
	results, err := p.searchImpl(&http.Client{Timeout: 20 * time.Second}, keyword, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	if len(results[0].Links) == 0 || results[0].Links[0].URL == "" {
		t.Fatalf("live search returned invalid result: %#v", results[0])
	}
	t.Logf("live search for %q returned %d results; first: %q (%s)", keyword, len(results), results[0].Title, results[0].Links[0].URL)
}
