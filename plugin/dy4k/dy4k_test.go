package dy4k

import (
	"os"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"pansou/model"
)

func TestExtractNewSearchResults(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<div class="space-y-4">
			<div class="flex gap-3">
				<a href="/4K-vodplay/3591-1-1.html"><img src="https://img.example/xian-ni.jpg"><span>更新至第156集</span></a>
				<div class="flex-1">
					<a href="/4K-detail/3591.html">仙逆</a>
					<div class="gap-x-3"><span>8.1</span><span>2023</span><span>大陆</span></div>
					<p>乡村少年踏上修仙之路</p>
				</div>
			</div>
		</div>`))
	if err != nil {
		t.Fatal(err)
	}

	p := NewDy4kPlugin()
	results := make([]model.SearchResult, 0)
	p.extractNewSearchResults(doc, &results)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	result := results[0]
	if result.UniqueID != "dy4k-3591" || result.Title != "仙逆" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.Images) != 1 || result.Images[0] != "https://img.example/xian-ni.jpg" {
		t.Fatalf("unexpected images: %#v", result.Images)
	}
}

func TestExtractDownloadLinks(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<div class="download-item"><a href="https://pan.quark.cn/s/abc123">夸克资源</a></div>
		<div class="download-item"><a href="https://pan.baidu.com/s/abc_123?pwd=9xyz">百度资源</a></div>
		<div class="download-item"><a href="https://drive.uc.cn/s/abcdef1234564?public=1">UC资源</a></div>
		<div class="download-item"><a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567">磁力资源</a></div>`))
	if err != nil {
		t.Fatal(err)
	}

	p := NewDy4kPlugin()
	detail := &detailPageResponse{Title: "仙逆"}
	p.extractDownloadLinks(doc, detail)

	if len(detail.Downloads) != 4 {
		t.Fatalf("expected 4 links, got %d: %#v", len(detail.Downloads), detail.Downloads)
	}
	wantTypes := map[string]bool{"quark": false, "baidu": false, "uc": false, "magnet": false}
	for _, link := range detail.Downloads {
		if _, ok := wantTypes[link.Type]; ok {
			wantTypes[link.Type] = true
		}
		if link.WorkTitle != "仙逆" {
			t.Fatalf("missing work title on link: %#v", link)
		}
		if link.Type == "baidu" && link.Password != "9xyz" {
			t.Fatalf("unexpected baidu password: %#v", link)
		}
	}
	for linkType, found := range wantTypes {
		if !found {
			t.Errorf("missing %s link", linkType)
		}
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("DY4K_LIVE_TEST") != "1" {
		t.Skip("set DY4K_LIVE_TEST=1 to query 4kdy.vip")
	}

	p := NewDy4kPlugin()
	results, err := p.searchImpl(p.optimizedClient, "仙逆", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	if len(results[0].Links) == 0 {
		t.Fatalf("first live result has no links: %#v", results[0])
	}
	t.Logf("live search returned %d results; first result %q has %d links", len(results), results[0].Title, len(results[0].Links))
}
