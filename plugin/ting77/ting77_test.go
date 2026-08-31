package ting77

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSearchImplResolvesTokenRedirects(t *testing.T) {
	var tokenRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			fmt.Fprint(w, `<a class="resource-row" href="/resource/resource-1">
				<div class="row-body"><h2 class="row-title">仙逆年番 4K</h2><p class="row-desc">资源简介</p>
				<div class="row-tags"><span class="row-tag">仙逆</span><span class="row-tag">动画</span></div><span class="row-size">10GB</span></div>
				<div class="row-meta-col"><div class="row-clouds"><span class="cloud-badge quark">夸克</span><span class="cloud-badge baidu">百度</span></div><span class="row-date">2026-08-31</span></div>
			</a>`)
		case "/api/link/token":
			tokenRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"code":0,"data":{"token":"token-%s","ts":"123"}}`, r.URL.Query().Get("type"))
		case "/go":
			if r.URL.Query().Get("type") == "baidu" {
				http.Redirect(w, r, "https://pan.baidu.com/s/abc?pwd=a1b2", http.StatusFound)
				return
			}
			http.Redirect(w, r, "https://pan.quark.cn/s/xyz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := NewTing77Plugin()
	p.baseURL = server.URL
	results, err := p.searchImpl(server.Client(), "仙逆", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || len(results[0].Links) != 2 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].UniqueID != "ting77-resource-1" || !strings.Contains(results[0].Content, "大小: 10GB") {
		t.Fatalf("result = %+v", results[0])
	}
	passwords := map[string]string{}
	for _, link := range results[0].Links {
		passwords[link.Type] = link.Password
	}
	if passwords["baidu"] != "a1b2" || passwords["quark"] != "" {
		t.Fatalf("passwords = %+v", passwords)
	}
	results, err = p.searchImpl(server.Client(), "仙逆", nil)
	if err != nil || len(results) != 1 {
		t.Fatalf("cached search: results=%d err=%v", len(results), err)
	}
	if got := tokenRequests.Load(); got != 2 {
		t.Fatalf("token requests = %d, want 2", got)
	}
}

func TestDetectLinkType(t *testing.T) {
	tests := map[string]string{
		"https://pan.quark.cn/s/abc":           "quark",
		"https://pan.baidu.com/s/abc":          "baidu",
		"https://www.alipan.com/s/abc":         "aliyun",
		"https://www.aliyundrive.com/s/abc":    "aliyun",
		"https://example.com/resource/unknown": "others",
	}
	for rawURL, want := range tests {
		if got := detectLinkType(rawURL); got != want {
			t.Errorf("detectLinkType(%q) = %q, want %q", rawURL, got, want)
		}
	}
}
