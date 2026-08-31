package xiaoyu

import (
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestParsePage(t *testing.T) {
	html := `
		<div class="count">找到 <strong>21</strong> 条，当前第 <strong>1/3</strong> 页</div>
		<div class="search-list">
			<div class="item">
				<div class="name"><a class="open" data-url="cGFuLmJhaWR1LmNvbS9zLzFtNy1XVWI0MFk1YUNuVjBOc1c5emdRP3B3ZD1zYXJp" data-code="sari" data-id="65860">仙逆</a></div>
				<div class="atips"><a>2026-08-17 02:19:50</a></div>
				<div class="atips"><a>【内容】：标题：仙逆，提取码：sari</a></div>
				<a class="copy" data-code="仙逆链接: https://pan.baidu.com/s/1m7-WUb40Y5aCnV0NsW9zgQ?pwd=sari 提取码: sari"></a>
			</div>
			<div class="item">
				<div class="name"><a class="open" data-url="" data-code=" " data-id="142883">仙逆短剧</a></div>
				<div class="atips"><a>2026-05-26 01:26:21</a></div>
				<div class="atips"><a>【内容】：标题：仙逆短剧</a></div>
				<a class="copy" data-code="仙逆短剧链接：https://pan.quark.cn/s/2fbe6c1bd46b"></a>
			</div>
		</div>`
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}

	page := parsePage(doc)
	if page.TotalPages != 3 {
		t.Fatalf("expected 3 pages, got %d", page.TotalPages)
	}
	if len(page.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(page.Results))
	}

	first := page.Results[0]
	if first.UniqueID != "xiaoyu-65860" || first.Title != "仙逆" {
		t.Fatalf("unexpected first result: %#v", first)
	}
	if len(first.Links) != 1 || first.Links[0].Type != "baidu" || first.Links[0].Password != "sari" {
		t.Fatalf("unexpected baidu link: %#v", first.Links)
	}
	if first.Links[0].WorkTitle != "仙逆" {
		t.Fatalf("unexpected work title: %#v", first.Links[0])
	}

	second := page.Results[1]
	if second.Links[0].Type != "quark" || second.Links[0].URL != "https://pan.quark.cn/s/2fbe6c1bd46b" {
		t.Fatalf("unexpected fallback link: %#v", second.Links[0])
	}
}

func TestDecodeDataURL(t *testing.T) {
	got := decodeDataURL("cGFuLnh1bmxlaS5jb20vcy9hYmM/cHdkPTEyMzQj")
	if got != "https://pan.xunlei.com/s/abc?pwd=1234#" {
		t.Fatalf("unexpected decoded URL: %q", got)
	}
}

func TestNormalizeLinkRejectsUnknownHost(t *testing.T) {
	linkType, linkURL := normalizeLink("https://example.com/not-a-pan-link")
	if linkType != "" || linkURL != "" {
		t.Fatalf("unexpected accepted link: %s %s", linkType, linkURL)
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("XIAOYU_LIVE_TEST") != "1" {
		t.Skip("set XIAOYU_LIVE_TEST=1 to query xykmovie.com")
	}

	keyword := os.Getenv("XIAOYU_LIVE_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}
	p := NewXiaoyuPlugin()
	results, err := p.searchImpl(p.client, keyword, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	t.Logf("live search for %q returned %d results; first result: %q (%s)", keyword, len(results), results[0].Title, results[0].Links[0].Type)
}
