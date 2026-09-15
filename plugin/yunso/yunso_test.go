package yunso

import (
	"encoding/base64"
	"testing"
)

func TestDecryptYunsoURLAcceptsPlainURL(t *testing.T) {
	const shareURL = "https://pan.quark.cn/s/example"
	got, err := decryptYunsoURL(shareURL)
	if err != nil {
		t.Fatalf("decryptYunsoURL returned error: %v", err)
	}
	if got != shareURL {
		t.Fatalf("decryptYunsoURL = %q, want %q", got, shareURL)
	}
}

func TestDecryptYunsoURLNormalizesEmptyQuery(t *testing.T) {
	const shareURL = "https://pan.quark.cn/s/785c610a9b0c"
	legacy := []byte(shareURL + "?")
	for i := range legacy {
		legacy[i] ^= yunsoDecryptBytes[i%len(yunsoDecryptBytes)]
	}
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", shareURL + "?", shareURL},
		{"whitespace", " \n" + shareURL + "?\t", shareURL},
		{"fragment", shareURL + "?#files", shareURL + "#files"},
		{"real query", shareURL + "?pwd=abcd&sort=name", shareURL + "?pwd=abcd&sort=name"},
		{"question in query value", shareURL + "?note=what?", shareURL + "?note=what?"},
		{"question in fragment", shareURL + "#files?", shareURL + "#files?"},
		{"encoded question", shareURL + "%3F", shareURL + "%3F"},
		{"base64", base64.StdEncoding.EncodeToString([]byte(shareURL + "?")), shareURL},
		{"unpadded base64", base64.RawStdEncoding.EncodeToString([]byte(shareURL + "?")), shareURL},
		{"legacy XOR", base64.StdEncoding.EncodeToString(legacy), shareURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decryptYunsoURL(tc.input)
			if err != nil {
				t.Fatalf("decryptYunsoURL returned error: %v", err)
			}
			if got != tc.want {
				t.Errorf("URL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseItemsDeduplicatesEmptyQueryAndPreservesPasswords(t *testing.T) {
	const fragment = `
	<div class="layui-card" data-qid="1">
	  <a onclick="open_sid(this)" id="quark-new" url="https://pan.quark.cn/s/785c610a9b0c?" pa="abcd">电影合集</a>
	</div>
	<div class="layui-card" data-qid="2">
	  <a onclick="open_sid(this)" id="quark-old" url="https://pan.quark.cn/s/785c610a9b0c">电影合集</a>
	</div>
	<div class="layui-card" data-qid="3">
	  <a onclick="open_sid(this)" id="baidu" url="https://pan.baidu.com/s/example?pwd=efgh&amp;sort=name">电影备用</a>
	</div>`
	p := NewYunsoAsyncPlugin()
	items, err := p.parseItems(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("parsed %d items, want 3", len(items))
	}
	results := p.convertResults(p.deduplicateItems(items))
	if len(results) != 2 {
		t.Fatalf("got %d results after deduplication, want 2", len(results))
	}
	for _, result := range results {
		if len(result.Links) != 1 {
			t.Fatalf("unexpected links: %+v", result.Links)
		}
		link := result.Links[0]
		switch link.Type {
		case "quark":
			if link.URL != "https://pan.quark.cn/s/785c610a9b0c" || link.Password != "abcd" {
				t.Errorf("unexpected quark link: %+v", link)
			}
		case "baidu":
			if link.URL != "https://pan.baidu.com/s/example?pwd=efgh&sort=name" || link.Password != "efgh" {
				t.Errorf("unexpected baidu link: %+v", link)
			}
		default:
			t.Errorf("unexpected link type: %+v", link)
		}
	}
}
