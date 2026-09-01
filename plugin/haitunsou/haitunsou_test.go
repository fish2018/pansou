package haitunsou

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseEmbeddedListAndConvert(t *testing.T) {
	html := `<script>const list = JSON.parse('[{"id":5581,"title":"资源标题：仙逆\u7279\'别篇资源描述：更新说明\n第二行","is_type":0,"code":null,"url":"https:\/\/pan.quark.cn\/s\/abc","name":"仙逆特别篇","times":"2026-09-01","category":null}]');</script>`
	items, err := parseEmbeddedList([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %#v", items)
	}
	result, ok := convertItem(items[0])
	if !ok {
		t.Fatal("expected item to convert")
	}
	if result.UniqueID != "haitunsou-5581" || result.Channel != "" || result.Title != "仙逆特'别篇" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Links[0].Type != "quark" || result.Links[0].URL != "https://pan.quark.cn/s/abc" {
		t.Fatalf("unexpected link: %#v", result.Links[0])
	}
}

func TestSearchImpl(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/s/%E4%BB%99%E9%80%86.html" {
			t.Errorf("unexpected path: %s", r.URL.EscapedPath())
		}
		fmt.Fprint(w, `<script>const list = JSON.parse('[{"id":1,"title":"资源标题：仙逆 4K资源描述：最新更新","is_type":0,"code":null,"url":"https:\/\/pan.quark.cn\/s\/abc","name":"仙逆 4K","times":"2026-09-01","category":null},{"id":2,"title":"无关内容","is_type":0,"code":null,"url":"https:\/\/pan.quark.cn\/s\/other","name":"无关内容","times":"2026-09-01","category":null}]');</script>`)
	}))
	defer server.Close()

	p := NewHaitunsouPlugin()
	p.baseURL = server.URL
	results, err := p.searchImpl(server.Client(), " 仙逆 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "仙逆 4K" {
		t.Fatalf("unexpected filtered results: %#v", results)
	}
}

func TestDecodeJSSingleQuotedStringEscapes(t *testing.T) {
	decoded, err := decodeJSSingleQuotedString([]byte(`A\x42\u4ed9\uD83D\uDE00\'`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(decoded); got != "AB仙😀'" {
		t.Fatalf("unexpected decoded string: %q", got)
	}
}

func TestNormalizeLinkRejectsMismatchedType(t *testing.T) {
	if _, normalized, _ := normalizeLink("https://pan.baidu.com/s/abc", "", 0); normalized != "" {
		t.Fatalf("expected mismatched link type to be rejected, got %s", normalized)
	}
	linkType, normalized, password := normalizeLink("https://pan.baidu.com/s/abc?pwd=6666", "", 2)
	if linkType != "baidu" || normalized == "" || password != "6666" {
		t.Fatalf("unexpected normalized link: %s %s %s", linkType, normalized, password)
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("HAITUNSOU_LIVE_TEST") != "1" {
		t.Skip("set HAITUNSOU_LIVE_TEST=1 to query haitunsou.com")
	}
	keyword := os.Getenv("HAITUNSOU_LIVE_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}

	p := NewHaitunsouPlugin()
	results, err := p.searchImpl(&http.Client{Timeout: 30 * time.Second}, keyword, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	if results[0].Channel != "" || len(results[0].Links) == 0 || !strings.HasPrefix(results[0].Links[0].URL, "http") {
		t.Fatalf("live search returned invalid result: %#v", results[0])
	}
	t.Logf("live search for %q returned %d results; first: %q (%s)", keyword, len(results), results[0].Title, results[0].Links[0].URL)
}
