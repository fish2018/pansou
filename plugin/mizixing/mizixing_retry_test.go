package mizixing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功即返回响应，
// 且失败时把**真实状态码**带出来。
func TestDoRequestWithRetrySucceedsAfterFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	p := NewMizixingPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.doRequestWithRetry(req, srv.Client(), 3)
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
}

// 一直失败时错误必须带上真实状态码。
//
// 这里锁住一个修过的回归：原先Do成功但状态码非 200 时只执行 lastErr = err，
// err 为 nil 会把失败原因清空，最终只报出 "%!w(<nil>)"，状态码丢失、无法定位。
func TestDoRequestWithRetryReportsStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	p := NewMizixingPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.doRequestWithRetry(req, srv.Client(), 2)
	if err == nil {
		t.Fatal("上游一直 403 时必须报错")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误里必须带真实状态码，实际: %v", err)
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Errorf("失败原因被清空（修过的回归）: %v", err)
	}
}
