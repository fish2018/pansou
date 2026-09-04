package susu

import (
	"encoding/base64"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestDecodeJWTURL(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"data":{"url":"https://pan.quark.cn/s/example?pwd=abcd"}}`))
	token := "header." + payload + ".signature"

	plugin := NewSusuAsyncPlugin()
	got, err := plugin.decodeJWTURL(token)
	if err != nil {
		t.Fatalf("decodeJWTURL() error = %v", err)
	}
	if want := "https://pan.quark.cn/s/example?pwd=abcd"; got != want {
		t.Fatalf("decodeJWTURL() = %q, want %q", got, want)
	}
}

func TestExtractPassword(t *testing.T) {
	if got := extractPassword("https://pan.example/s/1?pwd=urlpwd", "fallback"); got != "urlpwd" {
		t.Fatalf("extractPassword() = %q, want urlpwd", got)
	}
	if got := extractPassword("https://pan.example/s/1", "fallback"); got != "fallback" {
		t.Fatalf("extractPassword() = %q, want fallback", got)
	}
}

func TestSusuLiveSearch(t *testing.T) {
	if os.Getenv("SUSU_LIVE_TEST") == "" {
		t.Skip("set SUSU_LIVE_TEST=1 to run the live source test")
	}

	p := NewSusuAsyncPlugin()
	results, err := p.doSearch(&http.Client{Timeout: 30 * time.Second}, "瑞克和莫蒂", nil)
	if err != nil {
		t.Fatalf("doSearch() error = %v", err)
	}
	if len(results) == 0 {
		t.Fatal("doSearch() returned no results")
	}
	t.Logf("received %d results", len(results))
	for _, result := range results {
		if len(result.Links) == 0 {
			t.Fatalf("result %q has no links", result.Title)
		}
		for _, link := range result.Links {
			if link.URL == "" {
				t.Fatalf("result %q contains an empty link", result.Title)
			}
			t.Logf("%s: [%s] %s password=%q", result.Title, link.Type, link.URL, link.Password)
		}
	}
}
