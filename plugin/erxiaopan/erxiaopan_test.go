package erxiaopan

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func fixtureDocument(t *testing.T, name string) *goquery.Document {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func fixtureText(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestParseCapturedPages(t *testing.T) {
	p := NewErxiaopanPlugin()
	entries := p.parseSearchEntries(fixtureDocument(t, "search.html"))
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	movie := entries[0]
	if movie.result.UniqueID != "erxiaopan-13756" || movie.result.Title != "GT赛车：极速狂飙 Gran Turismo (2023)" {
		t.Errorf("unexpected entry: %+v", movie)
	}
	if movie.url != defaultBaseURL+"/index.php/vod/detail/id/13756.html" {
		t.Errorf("unexpected detail url: %s", movie.url)
	}
	if movie.result.Channel != "" {
		t.Errorf("plugin results must keep an empty channel: %+v", movie.result)
	}
	for _, tag := range []string{"花卷电影", "2023", "美国", "日本", "花卷原盘"} {
		if !contains(movie.result.Tags, tag) {
			t.Errorf("missing tag %q in %v", tag, movie.result.Tags)
		}
	}
	for _, want := range []string{"分类：花卷电影", "年份：2023", "地区：美国 / 日本", "导演：尼尔·布洛姆坎普", "主演：大卫·哈伯 / 奥兰多·布鲁姆 / 阿奇·马德基", "剧情："} {
		if !strings.Contains(movie.result.Content, want) {
			t.Errorf("content missing %q: %s", want, movie.result.Content)
		}
	}
	if len(movie.result.Images) != 1 || !strings.HasPrefix(movie.result.Images[0], "https://img.alicdn.com/") {
		t.Errorf("unexpected poster: %v", movie.result.Images)
	}

	series := entries[1]
	if series.result.UniqueID != "erxiaopan-254" || !contains(series.result.Tags, "全39集") {
		t.Errorf("unexpected series entry: %+v", series)
	}

	result := parseDetail(fixtureDocument(t, "detail.html"), series)
	if result.Title != "狂飙" || result.Channel != "" || len(result.Links) != 3 {
		t.Fatalf("unexpected detail result: %+v", result)
	}
	wantTypes := []string{"quark", "baidu", "tianyi"}
	wantPasswords := []string{"", "tqwi", "9rnf"}
	for i, link := range result.Links {
		if link.Type != wantTypes[i] || link.Password != wantPasswords[i] {
			t.Errorf("unexpected link %d: %+v", i, link)
		}
		if link.WorkTitle != "狂飙" {
			t.Errorf("single-episode share should not carry an episode suffix: %+v", link)
		}
	}
	if result.Links[2].URL != "https://cloud.189.cn/t/vQfIfujmeuye" {
		t.Errorf("tianyi url kept inline text: %+v", result.Links[2])
	}
	if result.Links[1].URL != "https://pan.baidu.com/s/1GrudU8TA6YuU9skrzSqQ8A?pwd=tqwi" {
		t.Errorf("baidu url lost its password parameter: %+v", result.Links[1])
	}
	if !strings.Contains(result.Content, "来源："+series.url) || !strings.Contains(result.Content, "又名：kuangbiao") {
		t.Errorf("missing detail metadata: %s", result.Content)
	}
	for _, tag := range []string{"花卷剧集", "2023", "大陆", "犯罪", "剧情", "国产"} {
		if !contains(result.Tags, tag) {
			t.Errorf("missing tag %q in %v", tag, result.Tags)
		}
	}
	if len(result.Images) != 1 || !strings.HasPrefix(result.Images[0], "https://pic3.yzzyimg.online/") {
		t.Errorf("unexpected poster: %v", result.Images)
	}
}

const detailPageTemplate = `<div class="box view-heading"><div class="video-info"><div class="video-info-header"><h1 class="page-title">狂飙 %[1]s</h1></div></div></div>
<div class="module" id="download-list"><div class="module-row-one"><div class="module-row-info">
<a class="module-row-text copy" href="javascript:;" data-clipboard-text="https://pan.quark.cn/s/abc%[1]s"></a>
<div class="module-row-title"><h4>狂飙 %[1]s - 第1集</h4><p>https://pan.quark.cn/s/abc%[1]s</p></div></div></div></div>`

const secondPageHTML = `<html><body><div class="module-items"><div class="module-search-item">
<div class="module-item-pic"><h3><a href="/index.php/vod/detail/id/999.html">狂飙 番外篇</a></h3></div>
<div class="video-info-header"><h3><a href="/index.php/vod/detail/id/999.html">狂飙 番外篇</a></h3></div>
</div></div></body></html>`

func TestSearchPaginatesAndCachesDetails(t *testing.T) {
	var searchHits, detailHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/vod/search/page/2/"):
			atomic.AddInt32(&searchHits, 1)
			fmt.Fprint(w, secondPageHTML)
		case strings.HasPrefix(r.URL.Path, "/index.php/vod/search/"):
			atomic.AddInt32(&searchHits, 1)
			fmt.Fprint(w, fixtureText(t, "search.html"))
		case detailPathPattern.MatchString(r.URL.Path):
			atomic.AddInt32(&detailHits, 1)
			id := detailPathPattern.FindStringSubmatch(r.URL.Path)[1]
			fmt.Fprintf(w, detailPageTemplate, id)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	p := NewErxiaopanPlugin()
	p.baseURL = server.URL

	results, err := p.searchImpl(server.Client(), " 狂飙 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(results), results)
	}
	if atomic.LoadInt32(&searchHits) != 2 || atomic.LoadInt32(&detailHits) != 3 {
		t.Fatalf("unexpected request counts: search=%d detail=%d", searchHits, detailHits)
	}
	for _, result := range results {
		if len(result.Links) == 0 || result.Links[0].Type != "quark" {
			t.Errorf("unexpected result: %+v", result)
		}
	}

	if _, err := p.searchImpl(server.Client(), "狂飙", nil); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&detailHits) != 3 {
		t.Errorf("detail cache was not reused: %d requests", detailHits)
	}

	if _, err := p.searchImpl(server.Client(), "狂飙", map[string]interface{}{"refresh": true}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&detailHits) != 6 {
		t.Errorf("refresh did not bypass the detail cache: %d requests", detailHits)
	}
}

func TestSearchReportsUnrecognizedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "<html><body>训练中</body></html>")
	}))
	defer server.Close()

	p := NewErxiaopanPlugin()
	p.baseURL = server.URL
	if _, err := p.searchImpl(server.Client(), "狂飙", nil); err == nil {
		t.Fatal("expected an error for an unrecognized page")
	}
}

func TestNormalizeShareLink(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		nearby   string
		wantType string
		wantURL  string
		wantPwd  string
		wantOK   bool
	}{
		{name: "quark plain", raw: "https://pan.quark.cn/s/27d16e07ad4e", wantType: "quark", wantURL: "https://pan.quark.cn/s/27d16e07ad4e", wantOK: true},
		{name: "baidu pwd param", raw: "https://pan.baidu.com/s/1GrudU8TA6YuU9skrzSqQ8A?pwd=tqwi", wantType: "baidu", wantURL: "https://pan.baidu.com/s/1GrudU8TA6YuU9skrzSqQ8A?pwd=tqwi", wantPwd: "tqwi", wantOK: true},
		{name: "tianyi inline code", raw: "https://cloud.189.cn/t/vQfIfujmeuye（访问码：9rnf）", wantType: "tianyi", wantURL: "https://cloud.189.cn/t/vQfIfujmeuye", wantPwd: "9rnf", wantOK: true},
		{name: "tianyi web share", raw: "https://cloud.189.cn/web/share?code=A3yMBvrMRJVr", wantType: "tianyi", wantURL: "https://cloud.189.cn/web/share?code=A3yMBvrMRJVr", wantOK: true},
		{name: "password in text", raw: "https://pan.quark.cn/s/93c19cadd477", nearby: "夸克网盘 提取码：a1b2", wantType: "quark", wantURL: "https://pan.quark.cn/s/93c19cadd477", wantPwd: "a1b2", wantOK: true},
		{name: "123pan", raw: "https://www.123pan.com/s/abcd-efgh", wantType: "123", wantURL: "https://www.123pan.com/s/abcd-efgh", wantOK: true},
		{name: "mobile cloud", raw: "https://caiyun.feixin.10086.cn/p/abcdef", wantType: "mobile", wantURL: "https://caiyun.feixin.10086.cn/p/abcdef", wantOK: true},
		{name: "magnet", raw: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x", wantType: "magnet", wantURL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x", wantOK: true},
		{name: "relative list", raw: "/list/share", wantOK: false},
		{name: "internal detail", raw: defaultBaseURL + "/index.php/vod/detail/id/254.html", wantOK: false},
		{name: "unrelated host", raw: "https://example.com/s/abc", wantOK: false},
		{name: "share index without id", raw: "https://pan.quark.cn/s/", wantOK: false},
		{name: "credentials in url", raw: "http://user:pass@pan.quark.cn/s/abc", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link, ok := normalizeShareLink(tc.raw, tc.nearby)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (link %+v)", ok, tc.wantOK, link)
			}
			if !ok {
				return
			}
			if link.Type != tc.wantType || link.URL != tc.wantURL || link.Password != tc.wantPwd {
				t.Fatalf("got %+v, want type=%s url=%s password=%s", link, tc.wantType, tc.wantURL, tc.wantPwd)
			}
		})
	}
}

func TestWorkTitleDisambiguatesEpisodes(t *testing.T) {
	const body = `<div class="module" id="download-list">
<div class="module-row-one"><div class="module-row-info">
<a class="module-row-text copy" data-clipboard-text="https://pan.quark.cn/s/one"></a>
<div class="module-row-title"><h4>流浪地球2 - 第1集</h4></div></div></div>
<div class="module-row-one"><div class="module-row-info">
<a class="module-row-text copy" data-clipboard-text="https://pan.quark.cn/s/two"></a>
<div class="module-row-title"><h4>流浪地球2 - 第2集</h4></div></div></div>
<div class="module-row-one"><div class="module-row-info">
<a class="module-row-text copy" data-clipboard-text="https://pan.baidu.com/s/three?pwd=abcd"></a>
<div class="module-row-title"><h4>流浪地球2</h4></div></div></div>
</div>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	links := extractLinks(doc, "流浪地球2")
	if len(links) != 3 {
		t.Fatalf("got %d links: %+v", len(links), links)
	}
	wantWorkTitles := []string{"流浪地球2 第1集", "流浪地球2 第2集", "流浪地球2"}
	for i, link := range links {
		if link.WorkTitle != wantWorkTitles[i] {
			t.Errorf("link %d work title = %q, want %q", i, link.WorkTitle, wantWorkTitles[i])
		}
	}
}

func TestSplitMultiSplitsMergedLabels(t *testing.T) {
	cases := map[string][]string{
		"美国 / 日本":           {"美国", "日本"},
		"剧情 / 动作 / 冒险 / 运动": {"剧情", "动作", "冒险", "运动"},
		"中国台湾,法国":           {"中国台湾", "法国"},
		"花卷电影":              {"花卷电影"},
		"":                  nil,
	}
	for input, want := range cases {
		got := splitMulti(input)
		if len(got) != len(want) {
			t.Errorf("splitMulti(%q) = %v, want %v", input, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitMulti(%q)[%d] = %q, want %q", input, i, got[i], want[i])
			}
		}
	}
}

func TestWorkSuffixDropsWorkName(t *testing.T) {
	if got := workSuffix("狂飙 - 第1集", "狂飙"); got != "第1集" {
		t.Errorf("workSuffix = %q, want 第1集", got)
	}
	if got := workSuffix("狂飙", "狂飙"); got != "" {
		t.Errorf("workSuffix with identical label = %q, want empty", got)
	}
	if got := workSuffix("", "狂飙"); got != "" {
		t.Errorf("workSuffix with empty label = %q, want empty", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ERXIAOPAN_LIVE_TEST=1 go test ./plugin/erxiaopan -run TestLiveSearch -count=1 -v
func TestLiveSearch(t *testing.T) {
	if os.Getenv("ERXIAOPAN_LIVE_TEST") != "1" {
		t.Skip("set ERXIAOPAN_LIVE_TEST=1 to hit the live site")
	}
	p := NewErxiaopanPlugin()
	results, err := p.Search("狂飙", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	for _, result := range results {
		if result.Channel != "" || len(result.Links) == 0 {
			t.Errorf("invalid live result: %+v", result)
		}
		for _, link := range result.Links {
			if link.Type == "" || link.Type == "others" {
				t.Errorf("unexpected link type %q in %+v", link.Type, result)
			}
		}
		types := make([]string, 0, len(result.Links))
		for _, link := range result.Links {
			types = append(types, link.Type)
		}
		t.Logf("%s | %s | %v", result.Title, strings.Join(types, ","), result.Tags)
	}
}
