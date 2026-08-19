package xb6v

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestCurrentSearchEndpoint(t *testing.T) {
	if SearchPath != "/e/search/11index.php" {
		t.Fatalf("unexpected search endpoint: %s", SearchPath)
	}
}

func TestCurrentResultAndMagnetMarkup(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<ul id="post_container">
			<li class="post"><a href="/dianshiju/guoju/29276.html">九门[全集]</a>
				<div class="info"><span class="info_date">2026-08-16</span></div>
			</li>
		</ul>
		<table><tr><td>磁力：<a href="magnet:?xt=urn:btih:abcdef">全集</a></td></tr></table>
	`))
	if err != nil {
		t.Fatal(err)
	}

	p := NewXb6vPlugin()
	pages := p.extractDetailURLs(doc)
	if len(pages) != 1 || !strings.HasSuffix(pages[0].URL, "/dianshiju/guoju/29276.html") {
		t.Fatalf("unexpected detail pages: %+v", pages)
	}
	links, infos := p.extractMagnetLinks(doc, "九门[全集]")
	if len(links) != 1 || links[0].Type != "magnet" || len(infos) != 1 {
		t.Fatalf("unexpected magnet extraction: links=%+v infos=%+v", links, infos)
	}
}
