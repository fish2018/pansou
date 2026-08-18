package ikantv

import (
	"strings"
	"testing"
)

func TestConvertResultKeepsOrderAndFields(t *testing.T) {
	items := []apiItem{
		{
			MessageID: "resource-1",
			UniqueID:  "ikantv-resource-1",
			Channel:   "should-be-cleared",
			Datetime:  "2026-08-18T01:26:00+08:00",
			Title:     "藏锋 更至04集 2026-08-18更新 【免会员保存/观看】",
			Content:   "藏锋 更至04集 2026-08-18更新 【免会员保存/观看】",
			Tags:      []string{"最新", "免会员保存"},
			Links: []apiLink{
				{Type: "xunlei", URL: "https://pan.xunlei.com/s/own", WorkTitle: "藏锋 更至04集 2026-08-18更新 【免会员保存/观看】"},
				{Type: "quark", URL: "https://pan.quark.cn/s/own", WorkTitle: "藏锋 更至04集 2026-08-18更新"},
			},
		},
		{
			UniqueID: "ikantv-resource-2",
			Title:    "藏锋 全24集 全 2026-08-18更新",
			Links: []apiLink{
				{Type: "quark", URL: "https://pan.quark.cn/s/full"},
			},
		},
	}

	results := convertResults(items)
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].Title != items[0].Title {
		t.Fatalf("order/title changed: %s", results[0].Title)
	}
	if results[0].Channel != "" {
		t.Fatalf("channel must be empty, got %q", results[0].Channel)
	}
	if results[0].UniqueID != "ikantv-resource-1" {
		t.Fatalf("unique id: %s", results[0].UniqueID)
	}
	if len(results[0].Links) != 2 {
		t.Fatalf("links: %d", len(results[0].Links))
	}
	if !strings.Contains(results[0].Links[0].WorkTitle, "免会员保存") {
		t.Fatalf("xunlei work_title lost badge: %s", results[0].Links[0].WorkTitle)
	}
	if strings.Contains(results[0].Links[1].WorkTitle, "免会员保存") {
		t.Fatalf("quark work_title should not have badge")
	}
}

func TestConvertResultDropsEmptyAndUnknownLinks(t *testing.T) {
	item := apiItem{
		UniqueID: "ikantv-resource-x",
		Title:    "测试",
		Links: []apiLink{
			{Type: "others", URL: "https://example.com/s/x"},
			{Type: "quark", URL: ""},
		},
	}
	if _, ok := convertResult(item); ok {
		t.Fatal("expected drop when no valid links")
	}
}

func TestParseDatetimeRFC3339(t *testing.T) {
	got := parseDatetime("2026-08-18T01:26:00+08:00")
	if got.IsZero() {
		t.Fatal("rfc3339 should parse")
	}
}

func TestConvertResultKeepsVarietyDotTitle(t *testing.T) {
	item := apiItem{
		UniqueID: "ikantv-resource-v",
		Title:    "花开锦绣 更至08.13期 2026-08-18更新",
		Links: []apiLink{
			{Type: "quark", URL: "https://pan.quark.cn/s/var"},
		},
	}
	got, ok := convertResult(item)
	if !ok {
		t.Fatal("expected keep")
	}
	if got.Title != item.Title {
		t.Fatalf("dot in variety title must be kept: %s", got.Title)
	}
}
