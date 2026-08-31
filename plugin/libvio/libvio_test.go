package libvio

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestExtractDirectPanLinks(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<div class="playlist-panel netdisk-panel">
			<a class="netdisk-item" href="https://pan.quark.cn/s/quark123"><span class="netdisk-name">夸克</span></a>
			<a class="netdisk-item" href="https://drive.uc.cn/s/uc123?public=1"><span class="netdisk-name">UC</span></a>
			<a class="netdisk-item" href="https://pan.baidu.com/s/baidu123?pwd=a1b2"><span class="netdisk-name">百度</span></a>
			<a class="netdisk-item" href="https://example.com/ignored">忽略</a>
		</div>`))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	p := NewLibvioPlugin()
	links := p.extractDirectPanLinks(doc, "仙逆")
	if len(links) != 3 {
		t.Fatalf("links = %+v", links)
	}
	types := map[string]string{}
	for _, link := range links {
		if link.WorkTitle != "仙逆" {
			t.Fatalf("work title = %q", link.WorkTitle)
		}
		types[link.Type] = link.Password
	}
	if types["quark"] != "" || types["uc"] != "" || types["baidu"] != "a1b2" {
		t.Fatalf("types = %+v", types)
	}
}
