package buerchen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSearchDailyAndStreamResolution(t *testing.T) {
	const keyword = "流浪地球 & 特别版"
	var mu sync.Mutex
	counts := make(map[string]int)
	var active, peak atomic.Int32
	track := func() func() {
		current := active.Add(1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		return func() { active.Add(-1) }
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Referer") == "" {
			t.Error("missing browser headers")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/other/web_search":
			query := r.URL.Query()
			provider, _ := strconv.Atoi(query.Get("is_type"))
			if query.Get("title") != keyword || (query.Get("is_type") != "0" && provider != 2 && provider != 4) || r.Header.Get("Accept") != "text/event-stream" {
				t.Errorf("incorrect SSE search: %v", query)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": heartbeat\n\n")
			for i := 0; i < maxCandidatesPerType+3; i++ {
				token := fmt.Sprintf("%d+/%d==", provider, i)
				if provider == 4 && i == 0 {
					token = "https://pan.xunlei.com/s/direct?pwd=d4e5"
				}
				event := map[string]interface{}{"title": fmt.Sprintf("%s %d", keyword, i), "url": token, "is_type": provider}
				body, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", body, body)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case "/api2/content":
			if r.URL.Query().Get("keyword") != keyword {
				t.Error("daily search must filter on the server")
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]interface{}{"data": []interface{}{
				map[string]string{"title": keyword + " 合集", "quarkLink": "dailyQ:0123", "baiduLink": "dailyB:4567", "xunleiLink": ""},
				map[string]string{"title": "无关每日更新", "quarkLink": "irrelevant"},
			}}})
		case "/api2/decrypt":
			defer track()()
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			token := payload["encryptUrl"]
			mu.Lock()
			counts["decrypt:"+token]++
			mu.Unlock()
			host := "pan.quark.cn"
			if token == "dailyB:4567" {
				host = "pan.baidu.com"
			} else if token != "dailyQ:0123" {
				t.Errorf("daily token was changed: %q", token)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": map[string]string{"url": "https://" + host + "/s/original?pwd=old1"}})
		case "/api/other/save_url":
			defer track()()
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("save_url must be a JSON POST")
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			original, err := url.QueryUnescape(payload["url"])
			if err != nil || payload["url"] != url.QueryEscape(original) || !strings.Contains(payload["title"], keyword) {
				t.Errorf("incorrect percent-encoded JSON payload: %v", payload)
			}
			mu.Lock()
			counts["save:"+original]++
			attempt := counts["save:"+original]
			mu.Unlock()
			var provider int
			id := "daily"
			if strings.HasPrefix(original, "https://") {
				if strings.Contains(original, "pan.baidu.com") {
					provider = 2
				}
			} else {
				parts := strings.Split(original, "+/")
				if len(parts) != 2 {
					t.Errorf("token changed: %q", original)
					http.Error(w, "bad token", 400)
					return
				}
				provider, _ = strconv.Atoi(parts[0])
				id = strings.TrimSuffix(parts[1], "==")
			}
			if original == "0+/1==" {
				http.Error(w, "unavailable", 503)
				return
			}
			host := map[int]string{0: "pan.quark.cn", 2: "pan.baidu.com", 4: "pan.xunlei.com"}[provider]
			shareURL := fmt.Sprintf("https://%s/s/share%s_%d?pwd=6666", host, id, attempt)
			if original == "4+/2==" {
				shareURL = "https://buerchen.top/s/advert.html"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "data": map[string]string{"url": shareURL}})
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := NewBuerchenPlugin()
	p.baseURL = server.URL
	var firstIDs, firstURLs []string
	for run := 0; run < 3; run++ {
		results, err := p.searchImpl(server.Client(), keyword, map[string]interface{}{"refresh": run == 2})
		if err != nil || len(results) != 3*maxCandidatesPerType-2 {
			t.Fatalf("run %d: results=%d, error=%v", run, len(results), err)
		}
		dailyCount := 0
		for i, result := range results {
			if result.Channel != "" || len(result.Links) != 1 || result.Links[0].WorkTitle != result.Title || !strings.HasPrefix(result.UniqueID, "buerchen-") {
				t.Errorf("incorrect result: %+v", result)
			}
			link := result.Links[0]
			if strings.Contains(result.Content, "每日更新") {
				dailyCount++
			}
			direct := strings.Contains(link.URL, "/s/direct?")
			if (direct && link.Password != "d4e5") || (!direct && link.Password != "6666") {
				t.Errorf("incorrect password: %+v", link)
			}
			if run == 0 {
				firstIDs = append(firstIDs, result.UniqueID)
				firstURLs = append(firstURLs, link.URL)
			} else if result.UniqueID != firstIDs[i] {
				t.Errorf("generated shares changed resource identity: %s != %s", result.UniqueID, firstIDs[i])
			}
			if run == 1 && link.URL != firstURLs[i] {
				t.Error("cached URL was not reused")
			}
			if run == 2 && !direct && link.URL == firstURLs[i] {
				t.Error("refresh did not resolve the link again")
			}
		}
		if dailyCount != 2 {
			t.Errorf("lost daily resources: got %d", dailyCount)
		}
		results[0].Links[0].URL = "caller mutation"
		mu.Lock()
		want := 1
		if run == 2 {
			want = 2
		}
		for _, key := range []string{"decrypt:dailyQ:0123", "decrypt:dailyB:4567", "save:0+/0==", "save:https://pan.baidu.com/s/original?pwd=old1"} {
			if counts[key] != want {
				t.Errorf("run %d: %s requests=%d, want %d", run, key, counts[key], want)
			}
		}
		for _, key := range []string{"save:0+/1==", "save:4+/2=="} {
			if counts[key] != run+1 {
				t.Errorf("failed links must not be cached: %s count=%d", key, counts[key])
			}
		}
		for _, key := range []string{"save:0+/5==", "save:2+/5==", "save:4+/6==", "decrypt:irrelevant", "save:https://pan.xunlei.com/s/direct?pwd=d4e5"} {
			if counts[key] != 0 {
				t.Errorf("unnecessary resolution: %s", key)
			}
		}
		mu.Unlock()
	}
	// 两个同时进行的搜索仍共享解析并发上限。
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.searchImpl(server.Client(), keyword, map[string]interface{}{"refresh": true}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak.Load() > resolveConcurrency || peak.Load() < 2 {
		t.Errorf("unexpected resolver concurrency: %d", peak.Load())
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("BUERCHEN_LIVE_TEST") != "1" {
		t.Skip("set BUERCHEN_LIVE_TEST=1 to query the live site")
	}
	p := NewBuerchenPlugin()
	client := &http.Client{Timeout: 12 * time.Second}
	// 每条线路最多尝试三项，找到一条可用分享后停止。
	for _, provider := range []int{0, 2, 4} {
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		entries, err := p.searchProvider(ctx, client, "流浪地球", provider)
		if err != nil || len(entries) == 0 {
			cancel()
			t.Fatalf("%s: entries=%d, error=%v", providerType(provider), len(entries), err)
		}
		found := false
		for _, entry := range entries[:min(3, len(entries))] {
			share, resolveErr := p.resolveEntry(ctx, client, entry, false)
			if resolveErr != nil {
				t.Logf("%s: skipped unavailable resource: %v", providerType(provider), resolveErr)
				continue
			}
			if share.link.Type == providerType(provider) {
				found = true
				t.Logf("SSE %s: %s", providerType(provider), entry.title)
				break
			}
		}
		cancel()
		if !found {
			t.Fatalf("%s: none of the first three candidates resolved", providerType(provider))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	daily, err := p.searchDaily(ctx, client, "流浪地球")
	if err != nil || len(daily[1]) == 0 {
		t.Fatalf("daily Baidu results=%d, error=%v", len(daily[1]), err)
	}
	share, err := p.resolveEntry(ctx, client, daily[1][0], false)
	if err != nil || share.link.Type != "baidu" || share.link.Password == "" {
		t.Fatalf("daily Baidu resolution: %v", err)
	}
	t.Logf("daily Baidu: %s", daily[1][0].title)
}
