package aikanzy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功返回响应。
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

	p := NewAikanzyAsyncPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := p.doRequestWithRetry(req, srv.Client())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
	// backoffBase=200ms，退避 200+400：测量目标代码本身耗时
	if elapsed < 600*time.Millisecond {
		t.Errorf("退避未生效，耗时仅 %v", elapsed)
	}
}

// 锁住修掉的诊断缺陷：非 200 时必须报出真实状态码。
// 原先只保存 client.Do 的 err，状态码非 200 时 err 为 nil，重试耗尽后报出
// "重试 N 次后仍然失败: %!w(<nil>)"，真实状态码丢失、完全无法定位。
func TestDoRequestWithRetryReportsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewAikanzyAsyncPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.doRequestWithRetry(req, srv.Client())
	if err == nil {
		t.Fatal("上游一直 404 时必须报错")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误里必须带真实状态码，实际: %v", err)
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Errorf("状态码丢失（本轮修掉的缺陷回归）: %v", err)
	}
}
