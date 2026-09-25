package kkmao

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功即返回响应、
// 失败时把真实状态码带出来。
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

	p := NewKkMaoPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := p.doRequestWithRetry(req, srv.Client(), 3, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
	// 退避曲线 100ms + 200ms：耗时本身就是"指数退避仍生效"的证据
	if elapsed < 300*time.Millisecond {
		t.Errorf("退避未生效，耗时仅 %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("退避异常偏大: %v", elapsed)
	}
}

// 一直失败必须报错，且错误里带真实状态码而不是被清空的 "%!w(<nil>)"。
func TestDoRequestWithRetryReportsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := NewKkMaoPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.doRequestWithRetry(req, srv.Client(), 2, time.Millisecond)
	if err == nil {
		t.Fatal("上游一直 403 时必须报错")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误里必须带真实状态码，实际: %v", err)
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Errorf("失败原因被清空: %v", err)
	}
}
