package feikuai

import "testing"

func TestParseFeikuaiWebTimeISOWithoutTimezone(t *testing.T) {
	got := parseFeikuaiWebTime("上传时间 2026-08-16T13:11:58")
	if got.IsZero() {
		t.Fatal("parseFeikuaiWebTime returned zero time")
	}
	if got.Year() != 2026 || got.Month() != 8 || got.Day() != 16 || got.Hour() != 13 || got.Minute() != 11 || got.Second() != 58 {
		t.Fatalf("parsed time = %v", got)
	}
}

func TestExtractWebPassword(t *testing.T) {
	if got := extractWebPassword("https://pan.baidu.com/s/example?pwd=abcd", ""); got != "abcd" {
		t.Fatalf("password = %q, want abcd", got)
	}
	if got := extractWebPassword("https://pan.baidu.com/s/example", "提取码：efgh"); got != "efgh" {
		t.Fatalf("password = %q, want efgh", got)
	}
}
