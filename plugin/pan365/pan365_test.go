package pan365

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapturedSearchResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := r.URL.Query().Get("disk_type")
		body, err := os.ReadFile(filepath.Join("testdata", "search-"+provider+".json"))
		if err != nil {
			t.Error(err)
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer server.Close()
	p := NewPan365Plugin()
	p.baseURL = server.URL
	for _, provider := range []string{"quark", "baidu"} {
		entries, err := p.searchProvider(context.Background(), server.Client(), "流浪地球", provider)
		if err != nil || len(entries) != 2 {
			t.Fatalf("%s: entries=%v, error=%v", provider, entries, err)
		}
		for _, entry := range entries {
			if entry.DiskType != provider || !strings.Contains(entry.Title, "流浪地球") {
				t.Fatalf("incorrect entry: %+v", entry)
			}
		}
	}
}

func TestSearchProvidersLimitsCacheAndRefresh(t *testing.T) {
	const keyword = "流浪地球 & 特别版"
	var mu sync.Mutex
	counts := make(map[string]int)
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("Referer") == "" {
			t.Error("missing browser headers")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/interface/search":
			query := r.URL.Query()
			provider := query.Get("disk_type")
			if query.Get("keyword") != keyword || query.Get("page") != "1" || query.Get("pageSize") != "20" || (provider != "quark" && provider != "baidu") {
				t.Errorf("incorrect search query: %v", query)
			}
			mu.Lock()
			counts[provider]++
			mu.Unlock()
			items := []searchItem{{Title: "无关资源", URL: "irrelevant", DiskType: provider}}
			for i := 0; i < maxCandidatesPerType+3; i++ {
				item := searchItem{Title: fmt.Sprintf("%s %d", keyword, i), URL: fmt.Sprintf("%s+/%d==", provider, i), DiskType: provider, Source: provider}
				items = append(items, item, item)
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "data": map[string]interface{}{"list": items}})
		case "/api/transfer-share/transfer-share":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("transfer must be a JSON POST")
			}
			var request struct {
				URL string `json:"encrypted_url"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			mu.Lock()
			counts[request.URL]++
			attempt := counts[request.URL]
			mu.Unlock()
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old; old = peak.Load() {
				if peak.CompareAndSwap(old, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			provider, suffix, ok := strings.Cut(request.URL, "+/")
			if !ok {
				t.Errorf("encrypted token was changed: %q", request.URL)
			}
			id := strings.TrimSuffix(suffix, "==")
			if request.URL == "quark+/1==" {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			host := "pan.quark.cn"
			if provider == "baidu" {
				host = "pan.baidu.com"
			}
			shareURL := fmt.Sprintf("https://%s/s/share%s_%d?pwd=a1b2&sort=name#list", host, id, attempt)
			if request.URL == "baidu+/2==" {
				shareURL = "https://pan.baidu.com.evil.example/s/advert"
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"code": 200, "data": map[string]interface{}{
				"share_url": shareURL, "original_url": fmt.Sprintf("https://%s/s/original%s?pwd=old1", host, id), "passcode": nil,
			}})
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := NewPan365Plugin()
	p.baseURL = server.URL
	var firstIDs, firstURLs []string
	for run := 0; run < 3; run++ {
		results, err := p.searchImpl(server.Client(), keyword, map[string]interface{}{"refresh": run == 2})
		if err != nil || len(results) != 2*maxCandidatesPerType-2 {
			t.Fatalf("run %d: got %d results, error %v", run, len(results), err)
		}
		for i, result := range results {
			if result.Channel != "" || len(result.Links) != 1 || result.Links[0].WorkTitle != result.Title || result.Links[0].Password != "a1b2" || !strings.HasPrefix(result.UniqueID, "pan365-") {
				t.Errorf("incorrect result: %+v", result)
			}
			if run == 0 {
				firstIDs = append(firstIDs, result.UniqueID)
				firstURLs = append(firstURLs, result.Links[0].URL)
			} else if result.UniqueID != firstIDs[i] {
				t.Errorf("identity changed when generated share changed: %q != %q", result.UniqueID, firstIDs[i])
			}
			if run == 1 && result.Links[0].URL != firstURLs[i] {
				t.Error("cached share was not reused")
			}
			if run == 2 && result.Links[0].URL == firstURLs[i] {
				t.Error("refresh did not resolve a new share")
			}
		}
		results[0].Links[0].URL = "caller mutation"
		mu.Lock()
		for _, provider := range []string{"quark", "baidu"} {
			if counts[provider] != run+1 {
				t.Errorf("provider %s was not searched", provider)
			}
			for i := 0; i < maxCandidatesPerType+3; i++ {
				key := fmt.Sprintf("%s+/%d==", provider, i)
				want := 1
				if run == 2 {
					want = 2
				}
				if key == "quark+/1==" || key == "baidu+/2==" {
					want = run + 1 // 失败不缓存。
				}
				if i >= maxCandidatesPerType {
					want = 0
				}
				if counts[key] != want {
					t.Errorf("run %d: %s resolved %d times, want %d", run, key, counts[key], want)
				}
			}
		}
		mu.Unlock()
	}
	if peak.Load() > resolveConcurrency || peak.Load() < 2 {
		t.Errorf("unexpected resolver concurrency: %d", peak.Load())
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSearchFailuresAreNotEmptySuccesses(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"empty", `{"code":200,"data":{"list":[],"search_stats":[{"success":true}]}}`, "", 200},
		{"all routes fail", `{"code":200,"data":{"list":[],"search_stats":[{"success":false}]}}`, "线路全部失败", 200},
		{"missing schema", `{"code":200,"data":{}}`, "缺少资源列表", 200},
		{"verification HTML", `<html>verify</html>`, "JSON", 200},
		{"API error", `{"code":500}`, "code=500", 200},
		{"HTTP error", `unavailable`, "code=503", 503},
		{"response limit", strings.Repeat("x", maxResponseBytes+1), "大小限制", 200},
		{"challenge", `{"code":42901}`, "CHALLENGE_REQUIRED", 200},
		{"rate limit", `limited`, "HTTP 429", 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("request has no deadline")
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			p := NewPan365Plugin()
			results, err := p.searchImpl(client, "流浪地球", nil)
			if len(results) != 0 {
				t.Fatal("unexpected results")
			}
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "[pan365]") {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if tc.name == "challenge" || tc.name == "rate limit" {
				before := calls.Load()
				if _, err := p.searchImpl(client, "第二次搜索", nil); err == nil || calls.Load() != before {
					t.Error("cooldown did not stop additional requests")
				}
			}
		})
	}
}

func TestNormalizeShareLinks(t *testing.T) {
	cases := []struct {
		raw, code, wantURL, wantPassword, wantType string
	}{
		{"https://pan.quark.cn/s/example?", "", "https://pan.quark.cn/s/example", "", "quark"},
		{"https://pan.baidu.com/s/1_ab-CD?pwd=a1b2&amp;sort=name#list", "c3d4", "https://pan.baidu.com/s/1_ab-CD?pwd=a1b2&sort=name#list", "a1b2", "baidu"},
		{"https://pan.baidu.com/s/example", "A1b2", "https://pan.baidu.com/s/example", "A1b2", "baidu"},
		{"https://pan.quark.cn/s/example 提取码：d4e5", "", "https://pan.quark.cn/s/example", "d4e5", "quark"},
		{"https://pan.baidu.com.evil.example/s/example", "", "", "", ""},
		{"https://pan.quark.cn@evil.example/s/example", "", "", "", ""},
		{"https://evil.example/?url=https://pan.quark.cn/s/example", "", "", "", ""},
		{"https://pan.quark.cn:443/s/example", "", "", "", ""},
		{"https://pan.quark.cn/s/", "", "", "", ""},
		{"https://pan.quark.cn/s/example?pwd=%xx", "", "", "", ""},
		{"javascript:alert(1)", "", "", "", ""},
	}
	for _, tc := range cases {
		link, ok := normalizeShareLink(tc.raw, tc.code)
		if ok != (tc.wantType != "") || link.URL != tc.wantURL || link.Password != tc.wantPassword || link.Type != tc.wantType {
			t.Errorf("normalize(%q)=(%+v, %v), want %s, %s, %s", tc.raw, link, ok, tc.wantURL, tc.wantPassword, tc.wantType)
		}
	}
}

func TestResolverHasOwnDeadline(t *testing.T) {
	called := false
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		called = true
		deadline, ok := r.Context().Deadline()
		if remaining := time.Until(deadline); !ok || remaining > requestTimeout || remaining < requestTimeout-time.Second {
			t.Errorf("resolver request lacks its own timeout: %v", remaining)
		}
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: r}, nil
	})}
	_, err := NewPan365Plugin().resolveEntry(context.Background(), client, searchItem{URL: "encrypted", DiskType: "quark"}, false)
	if !called || err == nil {
		t.Fatal("expected one failed request")
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("PAN365_LIVE_TEST") != "1" {
		t.Skip("set PAN365_LIVE_TEST=1 to query the live site")
	}
	p := NewPan365Plugin()
	client := &http.Client{Timeout: 12 * time.Second}
	for _, provider := range []string{"quark", "baidu"} {
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		entries, err := p.searchProvider(ctx, client, "流浪地球", provider)
		if err != nil || len(entries) == 0 {
			cancel()
			t.Fatalf("%s: entries=%d error=%v", provider, len(entries), err)
		}
		found := false
		for _, entry := range entries[:min(3, len(entries))] {
			share, resolveErr := p.resolveEntry(ctx, client, entry, false)
			if resolveErr != nil {
				t.Logf("%s: skipped unavailable resource: %v", provider, resolveErr)
				continue
			}
			if share.link.Type == provider && share.link.URL != "" {
				found = true
				t.Logf("%s: %s", provider, entry.Title)
				break
			}
		}
		cancel()
		if !found {
			t.Fatalf("%s: none of the first three candidates resolved", provider)
		}
	}
}
