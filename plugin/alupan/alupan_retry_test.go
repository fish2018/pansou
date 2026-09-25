package alupan

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功返回响应、失败带真实状态码。
func TestDoRequestWithRetrySucceedsAfterFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewAlupanPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := p.doRequestWithRetry(req, srv.Client(), 3)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
	// 200ms + 400ms：测量目标代码本身耗时，退避仍生效
	if elapsed < 600*time.Millisecond {
		t.Errorf("退避未生效，耗时仅 %v", elapsed)
	}
}

// 一直失败必须报错并带状态码，且不得出现被清空的 "%!w(<nil>)"。
func TestDoRequestWithRetryReportsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := NewAlupanPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.doRequestWithRetry(req, srv.Client(), 2)
	if err == nil {
		t.Fatal("上游一直 502 时必须报错")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("错误里必须带真实状态码，实际: %v", err)
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Errorf("失败原因被清空: %v", err)
	}
}
