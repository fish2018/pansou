package quarksoo

import "testing"

func TestParseSearchResultsHandlesNestedTableCells(t *testing.T) {
	html := `<table><thead><tr><th>剧名</th><th>网盘</th></tr></thead><tbody>
<tr><td><div class="drama-cell"><span><mark>仙逆</mark></span></div></td><td><a href="https://pan.qoark.cn/s/xn">夸克网盘</a></td></tr>
</tbody></table>`

	results := NewQuarksooAsyncPlugin().parseSearchResults(html, "仙逆")
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Title != "仙逆" {
		t.Fatalf("title = %q, want 仙逆", results[0].Title)
	}
	if len(results[0].Links) != 1 || results[0].Links[0].Type != "quark" {
		t.Fatalf("unexpected links: %#v", results[0].Links)
	}
}
