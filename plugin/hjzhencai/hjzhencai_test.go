package hjzhencai

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestParseCapturedPages(t *testing.T) {
	p := NewHJZhencaiPlugin()
	entries := p.parseSearchEntries(fixtureDocument(t, "search.html"))
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	wantTitles := []string{"流浪地球2", "流浪地球", "流浪地球2：再次冒险"}
	for i, entry := range entries {
		if entry.result.Title != wantTitles[i] || entry.result.UniqueID != fmt.Sprintf("hjzhencai-%d", 6522+i) {
			t.Errorf("unexpected entry: %+v", entry)
		}
	}
	result := parseDetail(fixtureDocument(t, "detail.html"), entries[0])
	if result.Title != "流浪地球2" || result.Channel != "" || len(result.Links) != 3 {
		t.Fatalf("unexpected detail: %+v", result)
	}
	wantTypes := []string{"quark", "baidu", "tianyi"}
	wantPasswords := []string{"", "4mm3", ""}
	for i, link := range result.Links {
		if link.Type != wantTypes[i] || link.Password != wantPasswords[i] || link.WorkTitle != "流浪地球2" {
			t.Errorf("unexpected link %d: %+v", i, link)
		}
	}
	if result.Links[2].URL != "https://cloud.189.cn/web/share?code=A3yMBvrMRJVr" {
		t.Errorf("lost Tianyi share code: %+v", result.Links[2])
	}
	if result.Datetime.Format(time.RFC3339) != "2026-02-05T00:00:00+08:00" {
		t.Errorf("unexpected update date: %s", result.Datetime)
	}
	if len(result.Images) != 1 || strings.Contains(result.Images[0], "/load.gif") {
		t.Errorf("unexpected poster: %v", result.Images)
	}
	if !strings.Contains(result.Content, entries[0].url) || len(result.Tags) == 0 {
		t.Errorf("missing source metadata: %+v", result)
	}
}

func TestDownloadLinksAreScopedAndPreserveCodes(t *testing.T) {
	const body = `
	<a href="https://pan.quark.cn/s/advert">广告</a>
	<div class="down-wrap">
	  <div class="down-src">KUAKE<small>2集</small></div>
	  <div class="down-card"><span class="down-card-name">第1集</span>
	    <a href="https://pan.quark.cn/s/first?">地址</a>
	    <a href="https://pan.quark.cn/s/first">下载</a><span>提取码：a1b2</span>
	  </div>
	  <div class="down-card"><span class="down-card-name">第2集</span>
	    <a href="https://pan.quark.cn/s/second?pwd=c3d4">下载</a><span>提取码：wrong</span>
	  </div>
	  <div class="down-src">BAIDU<small>1集</small></div>
	  <div class="down-card"><span class="down-card-name">第1集</span>
	    <a href="https://pan.baidu.com/s/example?pwd=E5f6&amp;sort=name">下载</a>
	  </div>
	  <div class="down-src">TIANYI<small>1集</small></div>
	  <div class="down-card"><span class="down-card-name">第1集</span>
	    https://cloud.189.cn/web/share?code=shareID123
	  </div>
	  <div class="down-src">UNKNOWN<small>1集</small></div>
	  <div class="down-card"><a href="https://pan.quark.cn.evil.example/s/ad">伪网盘域名</a></div>
	</div>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	links := extractLinks(doc, "测试剧")
	if len(links) != 4 {
		t.Fatalf("got %d links, want 4: %+v", len(links), links)
	}
	wantPasswords := []string{"a1b2", "c3d4", "E5f6", ""}
	wantTitles := []string{"测试剧 第1集", "测试剧 第2集", "测试剧", "测试剧"}
	for i, link := range links {
		if link.Password != wantPasswords[i] || link.WorkTitle != wantTitles[i] {
			t.Errorf("unexpected link %d: %+v", i, link)
		}
	}
	if links[0].URL != "https://pan.quark.cn/s/first" || links[3].Type != "tianyi" {
		t.Errorf("unexpected URLs: %+v", links)
	}
}

func TestNormalizeShareLinkRejectsNonShares(t *testing.T) {
	cases := []struct {
		url      string
		wantType string
	}{
		{"https://pan.quark.cn/s/share123", "quark"},
		{"https://pan.baidu.com/s/1_ab-CD?pwd=1234", "baidu"},
		{"https://cloud.189.cn/t/share123", "tianyi"},
		{"https://cloud.189.cn/web/share?code=share123", "tianyi"},
		{"https://www.alipan.com/s/share123", "aliyun"},
		{"https://drive.uc.cn/s/share123", "uc"},
		{"https://pan.xunlei.com/s/share123", "xunlei"},
		{"https://115.com/s/share123?password=abcd", "115"},
		{"https://www.123pan.com/s/share_123", "123"},
		{"https://www.hjzhencai.top/index.php/vod/play/id/1/sid/1/nid/1.html", ""},
		{"https://pan.quark.cn/", ""},
		{"https://pan.quark.cn/s/", ""},
		{"https://pan.quark.cn.evil.example/s/share123", ""},
		{"https://evil.example/redirect?url=https://pan.quark.cn/s/share123", ""},
		{"https://pan.quark.cn@evil.example/s/share123", ""},
		{"https://cloud.189.cn/web/share", ""},
		{"https://cloud.189.cn/web/share?code=", ""},
		{"javascript:alert(1)", ""},
		{"https://pan.quark.cn/s/share123?pwd=%xx", ""},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			link, ok := normalizeShareLink(tc.url, "")
			if ok != (tc.wantType != "") || link.Type != tc.wantType {
				t.Errorf("normalizeShareLink = (%+v, %v), want type %q", link, ok, tc.wantType)
			}
		})
	}
}

func testSearchCard(id string) string {
	return fmt.Sprintf(`<div class="module-card-item"><div class="module-card-item-title"><a href="/index.php/vod/detail/id/%s.html">流浪地球 &amp; 特别版</a></div></div>`, id)
}

func TestSearchPaginationCacheAndPartialFailure(t *testing.T) {
	const keyword = "流浪地球 & 特别版"
	const secondPage = "/index.php/vod/search/page/2/wd/movie.html"
	var mu sync.Mutex
	counts := make(map[string]int)
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Referer") == "" {
			t.Error("missing request headers")
		}
		switch r.URL.Path {
		case "/index.php/vod/search.html":
			if r.URL.Query().Get("wd") != keyword {
				t.Errorf("keyword was not encoded correctly: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, testSearchCard("1")+testSearchCard("2"))
			fmt.Fprintf(w, `<div id="page"><a class="page-next" title="尾页" href="/index.php/vod/search/page/99.html">尾页</a><a class="page-next" title="下一页" href="%s">下一页</a></div>`, secondPage)
		case secondPage:
			fmt.Fprint(w, testSearchCard("1")+testSearchCard("3")+testSearchCard("4"))
			fmt.Fprintf(w, `<div id="page"><a class="page-next" title="下一页" href="%s">下一页</a></div>`, secondPage)
		default:
			match := detailPathPattern.FindStringSubmatch(r.URL.Path)
			if len(match) == 0 {
				t.Errorf("unexpected request: %s", r.URL)
				http.NotFound(w, r)
				return
			}
			current := active.Add(1)
			defer active.Add(-1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			if match[1] == "2" {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `<div class="module-info-main"><div class="module-info-heading"><h1>流浪地球 &amp; 特别版</h1></div></div>`)
			if match[1] != "4" {
				fmt.Fprintf(w, `<div class="down-wrap"><div class="down-card"><a href="https://pan.quark.cn/s/share%s">下载</a></div></div>`, match[1])
			}
		}
	}))
	defer server.Close()
	p := NewHJZhencaiPlugin()
	p.baseURL = server.URL
	for run := 0; run < 3; run++ {
		ext := map[string]interface{}{"refresh": run == 2}
		results, err := p.searchImpl(server.Client(), keyword, ext)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 2 || results[0].UniqueID != "hjzhencai-1" || results[1].UniqueID != "hjzhencai-3" {
			t.Fatalf("unexpected results: %+v", results)
		}
		if results[0].Links[0].URL != "https://pan.quark.cn/s/share1" {
			t.Fatal("caller mutation leaked into detail cache")
		}
		results[0].Links[0].URL = "https://example.com/mutated"
		mu.Lock()
		wantDetailRequests := 1
		if run == 2 {
			wantDetailRequests = 2
		}
		for _, id := range []string{"1", "3"} {
			if got := counts["/index.php/vod/detail/id/"+id+".html"]; got != wantDetailRequests {
				t.Errorf("run %d: detail %s fetched %d times, want %d", run, id, got, wantDetailRequests)
			}
		}
		if counts[secondPage] != run+1 {
			t.Errorf("pagination loop was not stopped: %v", counts)
		}
		mu.Unlock()
	}
	if peak.Load() > int32(detailConcurrency) {
		t.Errorf("too many simultaneous detail requests: %d", peak.Load())
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSearchErrorsAndEmptyResults(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantError  string
		failDetail bool
	}{
		{"HTTP error", http.StatusForbidden, "forbidden", "HTTP 403", false},
		{"verification page", http.StatusOK, "<html><title>Verify</title></html>", "未识别到搜索页面", false},
		{"response limit", http.StatusOK, strings.Repeat("x", maxResponseBytes+1), "大小限制", false},
		{"all details fail", http.StatusOK, testSearchCard("1"), "HTTP 503", true},
		{"empty search", http.StatusOK, `<div class="module-heading-search">没有找到相关影片</div>`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				status, body := tc.status, tc.body
				if tc.failDetail && strings.Contains(r.URL.Path, "/detail/") {
					status, body = http.StatusServiceUnavailable, "unavailable"
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})}
			results, err := NewHJZhencaiPlugin().searchImpl(client, "流浪地球", nil)
			if len(results) != 0 {
				t.Errorf("unexpected results: %+v", results)
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "[hjzhencai]") {
				t.Errorf("error = %v, want plugin-scoped error containing %q", err, tc.wantError)
			}
		})
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("HJZHENCAI_LIVE_TEST") != "1" {
		t.Skip("set HJZHENCAI_LIVE_TEST=1 to query the live site")
	}
	results, err := NewHJZhencaiPlugin().searchImpl(&http.Client{Timeout: 12 * time.Second}, "流浪地球", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no net-disk resources")
	}
	for _, result := range results {
		var types []string
		for _, link := range result.Links {
			types = append(types, link.Type)
		}
		t.Logf("%s: %s", result.Title, strings.Join(types, ", "))
	}
}
