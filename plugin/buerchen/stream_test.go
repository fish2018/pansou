package buerchen

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCapturedSearchAndDailyResponses(t *testing.T) {
	body, err := os.ReadFile("testdata/search.sse")
	if err != nil {
		t.Fatal(err)
	}
	p := NewBuerchenPlugin()
	entries, err := p.readSearchStream(strings.NewReader(string(body)), "流浪地球", 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v, error=%v", entries, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("keyword") != "流浪地球" {
			t.Error("daily search downloaded an unfiltered catalogue")
		}
		body, err := os.ReadFile("testdata/daily.json")
		if err != nil {
			t.Error(err)
		}
		w.Write(body)
	}))
	defer server.Close()
	p.baseURL = server.URL
	groups, err := p.searchDaily(context.Background(), server.Client(), "流浪地球")
	if err != nil || len(groups[0]) != 2 || len(groups[1]) != 2 || len(groups[2]) != 0 {
		t.Fatalf("daily groups=%v, error=%v", groups, err)
	}
}

func TestSearchStreamFramingAndFailures(t *testing.T) {
	const event = `{"title":"流浪地球","url":"token+/==","is_type":0,"is_invalid":0}`
	var many strings.Builder
	for i := 0; i < maxCandidatesPerType+5; i++ {
		fmt.Fprintf(&many, "data: {\"title\":\"流浪地球 %d\",\"url\":\"token%d\",\"is_type\":0}\n\n", i, i)
	}
	cases := []struct {
		name, body, wantError string
		wantCount             int
	}{
		{"empty", "data: [DONE]\n\n", "", 0},
		{"CRLF and metadata", ": heartbeat\r\nevent: message\r\nid: 1\r\ndata: " + event + "\r\n\r\ndata: [DONE]\r\n\r\n", "", 1},
		{"multiline JSON", "data: {\"title\":\"流浪地球\",\n" + "data: \"url\":\"token\",\"is_type\":0}\n\ndata: [DONE]", "", 1},
		{"duplicates", "data: " + event + "\n\ndata: " + event + "\n\ndata: [DONE]\n\n", "", 1},
		{"invalid resource", "data: " + strings.Replace(event, `"is_invalid":0`, `"is_invalid":1`, 1) + "\n\ndata: [DONE]\n\n", "", 0},
		{"wrong provider", "data: " + strings.Replace(event, `"is_type":0`, `"is_type":2`, 1) + "\n\ndata: [DONE]\n\n", "", 0},
		{"candidate limit", many.String(), "", maxCandidatesPerType},
		{"challenge SSE", "data: {\"code\":42901,\"message\":\"CHALLENGE_REQUIRED\"}\n\n", "CHALLENGE_REQUIRED", 0},
		{"challenge JSON", `{"code":42901}`, "CHALLENGE_REQUIRED", 0},
		{"HTML", "<html>verify</html>", "未识别到 SSE", 0},
		{"malformed event", "data: {oops}\n\n", "格式错误", 0},
		{"no routes", "data: [DONE] 暂无可用线路\n\n", "暂无可用搜索线路", 0},
		{"truncated stream", "data: " + event + "\n\n", "未正常结束", 1},
		{"oversized stream", strings.Repeat(": heartbeat\n", maxResponseBytes/12+2), "大小限制", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := NewBuerchenPlugin().readSearchStream(strings.NewReader(tc.body), "流浪地球", 0)
			if len(entries) != tc.wantCount {
				t.Errorf("got %d entries, want %d", len(entries), tc.wantCount)
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "[buerchen]") {
				t.Errorf("error=%v, want %q", err, tc.wantError)
			}
		})
	}
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChallengeCooldownAndEmptySearch(t *testing.T) {
	for _, code := range []int{42901, 429, 0} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("request has no deadline")
				}
				status, body, contentType := 200, fmt.Sprintf(`{"code":%d}`, code), "application/json"
				if code == 429 {
					status = 429
				} else if code == 0 {
					body = `{"success":true,"data":{"data":[]}}`
					if r.URL.Path == "/api/other/web_search" {
						body, contentType = "data: [DONE]\n\n", "text/event-stream"
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			p := NewBuerchenPlugin()
			results, err := p.searchImpl(client, "流浪地球", nil)
			if len(results) != 0 {
				t.Fatal("unexpected results")
			}
			if code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "[buerchen]") {
				t.Fatalf("challenge was silently treated as empty search: %v", err)
			}
			before := calls.Load()
			if _, err := p.searchImpl(client, "第二次搜索", nil); err == nil || calls.Load() != before {
				t.Error("cooldown did not stop additional requests")
			}
		})
	}
}

func TestEncodeURIComponent(t *testing.T) {
	for raw, want := range map[string]string{
		"a+/==":                               "a%2B%2F%3D%3D",
		"流浪地球 & !'()*~":                       "%E6%B5%81%E6%B5%AA%E5%9C%B0%E7%90%83%20%26%20!'()*~",
		"https://pan.baidu.com/s/1x?pwd=ab12": "https%3A%2F%2Fpan.baidu.com%2Fs%2F1x%3Fpwd%3Dab12",
	} {
		if got := encodeURIComponent(raw); got != want {
			t.Errorf("encodeURIComponent(%q)=%q, want %q", raw, got, want)
		}
	}
}
