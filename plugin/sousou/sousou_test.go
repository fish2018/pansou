package sousou

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestCleanPansoTitleRemovesSEOSuffix(t *testing.T) {
	got := cleanPansoTitle("逆时之证（34集） - 网盘搜索-找网盘资源就上盘搜VIP")
	if got != "逆时之证（34集）" {
		t.Fatalf("title = %q", got)
	}
}

func TestParsePansoPasswordFromMarkup(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div>百度网盘 提取码：a1b2</div>`))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsePansoPassword(doc.Selection); got != "a1b2" {
		t.Fatalf("password = %q, want a1b2", got)
	}
}

func TestParsePansoDatetime(t *testing.T) {
	got := parsePansoDatetime("2025-10-02 14:44:38")
	if got.IsZero() || got.Year() != 2025 || got.Month() != 10 || got.Day() != 2 {
		t.Fatalf("parsed time = %v", got)
	}
}
