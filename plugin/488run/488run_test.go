package run488

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchAndPollAccumulation(t *testing.T) {
	var pollCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{
				Name:  "yd_frontend",
				Value: "test-session-token-123",
				Path:  "/",
			})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html></html>"))

		case "/api/frontend/search":
			if !strings.Contains(r.Header.Get("Cookie"), "yd_frontend=test-session-token-123") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":403,"message":"forbidden"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"code": 0,
				"data": {
					"complete": false,
					"search_id": "sid-abc",
					"total": 1,
					"items": [
						{
							"id": "tg-1001",
							"title": "后宫甄嬛传 (2011) 1080P 中粤双语",
							"desc": "甄嬛传 4K收藏版",
							"disk_type": "quark",
							"link": "https://pan.quark.cn/s/809f9f470a5c",
							"share_link": "https://pan.quark.cn/s/809f9f470a5c",
							"poster": "/api/frontend/tg/image-proxy?url=https%3A%2F%2Fexample.com%2Fa.jpg",
							"source": "tg",
							"source_label": "TG频道",
							"share_time": "2026-03-26 17:45:02"
						}
					]
				}
			}`))

		case "/api/frontend/search/poll":
			if r.URL.Query().Get("id") != "sid-abc" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := pollCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				_, _ = w.Write([]byte(`{
					"code": 0,
					"data": {
						"complete": false,
						"total": 2,
						"items": [
							{
								"id": "web-2002",
								"title": "甄嬛传 全76集 百度网盘 提取码： 26天前",
								"desc": "提取码: 623f",
								"disk_type": "baidu",
								"link": "https://pan.baidu.com/s/18T1CvYMISp2VVpeVnIArow?pwd=623f提取码:623f",
								"source": "web",
								"source_label": "全网检索",
								"share_time": "2026-03-26 18:10:00"
							}
						]
					}
				}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"code": 0,
				"data": {
					"complete": true,
					"total": 3,
					"items": [
						{
							"id": "web-3003",
							"title": "甄嬛传 蓝光原盘 迅雷云盘",
							"desc": "全集打包",
							"disk_type": "xunlei",
							"link": "https://pan.xunlei.com/s/VP-xfFjEh77MwQlGDSk21n6cA1?pwd=wh2n",
							"source": "web",
							"source_label": "全网检索",
							"share_time": "2026-03-26 18:20:00"
						},
						{
							"id": "web-invalid",
							"title": "无关内容测试",
							"disk_type": "quark",
							"link": "https://pan.quark.cn/s/invalid111",
							"share_time": "2026-03-26 18:20:00"
						}
					]
				}
			}`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := NewRun488Plugin()
	p.baseURL = srv.URL
	p.pollDelay = 20 * time.Millisecond
	p.maxPolls = 4

	results, err := p.searchImpl(srv.Client(), "甄嬛传", nil)
	if err != nil {
		t.Fatalf("searchImpl failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 filtered results, got %d", len(results))
	}

	// 按时间降序排列，最新一条应为 xunlei
	if results[0].Links[0].Type != "xunlei" || results[0].Links[0].Password != "wh2n" {
		t.Fatalf("unexpected first result link: %+v", results[0].Links[0])
	}
	if results[1].Links[0].Type != "baidu" || results[1].Links[0].Password != "623f" ||
		results[1].Links[0].URL != "https://pan.baidu.com/s/18T1CvYMISp2VVpeVnIArow?pwd=623f" ||
		results[1].Title != "甄嬛传 全76集 百度网盘" {
		t.Fatalf("unexpected second result: %+v", results[1])
	}
	if results[2].Links[0].Type != "quark" {
		t.Fatalf("unexpected third result link: %+v", results[2].Links[0])
	}
	if len(results[2].Images) != 1 || !strings.HasPrefix(results[2].Images[0], srv.URL+"/api/frontend/tg/image-proxy") {
		t.Fatalf("unexpected image url: %+v", results[2].Images)
	}
	for _, r := range results {
		if r.Channel != "" {
			t.Fatalf("expected empty Channel for plugin result, got %q", r.Channel)
		}
		if !strings.HasPrefix(r.UniqueID, "488run-") {
			t.Fatalf("expected UniqueID prefix 488run-, got %q", r.UniqueID)
		}
		for _, l := range r.Links {
			if l.WorkTitle == "" {
				t.Fatalf("expected non-empty WorkTitle on link %+v", l)
			}
		}
	}
}

func TestCookieRefreshOnUnauthorized(t *testing.T) {
	var cookieGen atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			n := cookieGen.Add(1)
			http.SetCookie(w, &http.Cookie{
				Name:  "yd_frontend",
				Value: fmt.Sprintf("token-%d", n),
				Path:  "/",
			})
			w.WriteHeader(http.StatusOK)
		case "/api/frontend/search":
			if !strings.Contains(r.Header.Get("Cookie"), "yd_frontend=token-1") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":403,"message":"invalid session"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"code": 0,
				"data": {
					"complete": true,
					"items": [
						{
							"id": "tg-1",
							"title": "凡人修仙传 4K",
							"disk_type": "uc",
							"link": "https://drive.uc.cn/s/e0f6c7f150bb4?public=1"
						}
					]
				}
			}`))
		}
	}))
	defer srv.Close()

	p := NewRun488Plugin()
	p.baseURL = srv.URL
	p.cookie = "yd_frontend=stale-token"
	p.cookieAt = time.Now()

	results, err := p.searchImpl(srv.Client(), "凡人修仙传", nil)
	if err != nil {
		t.Fatalf("expected automatic cookie refresh to succeed, got error: %v", err)
	}
	if len(results) != 1 || results[0].Links[0].Type != "uc" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestLive488RunSearch(t *testing.T) {
	if os.Getenv("RUN_LIVE_488RUN_TEST") != "1" {
		t.Skip("skipping live 488.run test; set RUN_LIVE_488RUN_TEST=1 to run")
	}

	p := NewRun488Plugin()
	client := &http.Client{Timeout: 15 * time.Second}
	results, err := p.searchImpl(client, "甄嬛传", nil)
	if err != nil {
		t.Fatalf("live searchImpl failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected non-empty live search results for 甄嬛传")
	}
	t.Logf("live search returned %d results; first: %s (%s)", len(results), results[0].Title, results[0].Links[0].URL)
}
