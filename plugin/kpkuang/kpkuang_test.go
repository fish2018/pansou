package kpkuang

import (
	"encoding/base64"
	utiljson "pansou/util/json"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestParseBetaResponse(t *testing.T) {
	payload, err := utiljson.Marshal([]map[string]interface{}{{
		"id": "625161",
		"data": map[string]interface{}{
			"vod_name": "杀手", "vod_year": "2023", "vod_area": "法国/美国",
			"vod_pic": "upload/vod/poster.jpg", "vod_actor": "演员",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("cb({\"code\":1,\"js\":\"" + base64.StdEncoding.EncodeToString(payload) + "\"})")
	items, err := parseBetaResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].id != "625161" || items[0].detailURL != primaryHost+"/voddetail/625161/" {
		t.Fatalf("unexpected beta item: %+v", items)
	}
	if items[0].image != primaryHost+"/upload/vod/poster.jpg" || !strings.Contains(items[0].content, "主演：演员") {
		t.Fatalf("unexpected beta metadata: %+v", items[0])
	}
}

func TestDecryptMyString(t *testing.T) {
	ali := "@@@encrypted@@@ydCHw0QMzBwGEoUMAtLFxMQHREcKgoMDxNLAygcSlZlGxUGERE=MwYsF"
	if got := decryptMyString(ali); got != "https://www.aliyundrive.com/s/DVbnz1xeYiJ" {
		t.Fatalf("aliyun decrypt: %q", got)
	}
	baidu := "@@@encrypted@@@GIXEhNNJAxpHxY8LgoiOiNOAkwxABAWPCAyA25EFlYyBwZcEB0GFBBxHQQTXUpOLBsRDTc=TQEHB"
	if got := decryptMyString(baidu); got != "https://pan.baidu.com/s/1wWR_sc_C9m7FHGbqEst6xA?pwd=vrn4" {
		t.Fatalf("baidu decrypt: %q", got)
	}
}

func TestExtractDetailLinks(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<table>
	<tr><td id="td-pan-0"><span>阿里云盘</span><a data-pan-url="@@@encrypted@@@ydCHw0QMzBwGEoUMAtLFxMQHREcKgoMDxNLAygcSlZlGxUGERE=MwYsF"></a></td></tr>
	<tr><td id="td-magnet-1"><a>磁力</a><div data-clipboard-text="@@@encrypted@@@ydCHw0QMzBwGEoUMAtLFxMQHREcKgoMDxNLAygcSlZlGxUGERE=MwYsF"></div></td></tr>
	</table>`))
	if err != nil {
		t.Fatal(err)
	}
	links := extractDetailLinks(doc)
	if len(links) != 1 || links[0].Type != "aliyun" {
		t.Fatalf("unexpected links: %+v", links)
	}
}

func TestParseDetail(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<a id="cover_showbox" data-original="/poster.jpg"></a>
	<h1 class="uk-card-title"><a>测试片</a> <span>(2024)</span></h1>
	<div class="vodbox"><ul><li><span>导演：</span>张三</li><li><span>地区：</span>中国</li><li><span>年份：</span>2024</li><li><span>简介：</span>简介内容</li></ul></div>`))
	if err != nil {
		t.Fatal(err)
	}
	got := parseDetail(doc, "https://www.kpkuang.fun/voddetail/1/", searchItem{})
	if got.title != "测试片" || got.image != "https://www.kpkuang.fun/poster.jpg" || !strings.Contains(got.content, "导演：张三") {
		t.Fatalf("unexpected detail: %+v", got)
	}
}

func TestParseNormalSearch(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<ul class="fed-list-info"><li><a class="fed-list-pics" href="/voddetail/123/" title="测试"><img data-original="/x.jpg"></a></li></ul>`))
	if err != nil {
		t.Fatal(err)
	}
	items := parseNormalSearch(doc, "https://www.kpkuang.fun")
	if len(items) != 1 || items[0].id != "123" || items[0].image != "https://www.kpkuang.fun/x.jpg" {
		t.Fatalf("unexpected items: %+v", items)
	}
}
