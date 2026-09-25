package gaoqing888

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功返回响应体、
// 每次尝试各自建 context（超时按次计算）。
func TestFetchBodyRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	start := time.Now()
	body, err := fetchBody(srv.Client(), srv.URL, 5*time.Second, "https://example.com")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	if string(body) != "<html>ok</html>" {
		t.Errorf("响应体不对: %q", string(body))
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
	// 200ms + 400ms：耗时即退避生效的证据
	if elapsed < 600*time.Millisecond {
		t.Errorf("退避未生效，耗时仅 %v", elapsed)
	}
}

// 一直失败必须报错并带状态码。
func TestFetchBodyExhaustedReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := fetchBody(srv.Client(), srv.URL, time.Second, "https://example.com")
	if err == nil {
		t.Fatal("上游一直 503 时必须报错")
	}
}

// 单次尝试的超时必须生效：上游挂住时不能被拖到整轮超时。
func TestFetchBodyPerAttemptTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := fetchBody(srv.Client(), srv.URL, 100*time.Millisecond, "https://example.com"); err == nil {
		t.Fatal("超时应报错")
	}
	// 3 次尝试 × 100ms 超时 + 200ms+400ms 退避，远小于上游的 2s
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("超时未按次生效，耗时 %v", elapsed)
	}
}
