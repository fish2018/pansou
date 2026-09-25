package panlian

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 登录失效属于"重试没有意义"的错误：cookie 过期后再试还是失效。
// 收敛到 util.DoWithRetry 并用 util.Abort 标记后，必须**只请求一次**就返回，
// 且错误仍可被 errors.Is(err, errLoginRequired) 判定。
func TestDoJSONGETAbortsOnLoginRequired(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"msg":"请先登录"}`))
	}))
	defer srv.Close()

	saved := DefaultBaseURL
	DefaultBaseURL = srv.URL
	defer func() { DefaultBaseURL = saved }()

	p := NewPanlianPlugin()
	var out map[string]interface{}

	start := time.Now()
	err := p.doJSONGET(srv.Client(), "", "/all-videos.php", nil, &out)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("登录失效必须报错")
	}
	if !errors.Is(err, errLoginRequired) {
		t.Errorf("应可用 errors.Is 判定登录失效，实际: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("中止后不该重试，实际请求 %d 次", got)
	}
	// 中止不等待：若退避(200/400ms)生效说明中止没起作用
	if elapsed > 150*time.Millisecond {
		t.Errorf("中止应立即返回，实际耗时 %v", elapsed)
	}
}

// 普通失败仍要按次数重试，避免"用 Abort 把重试关掉"这种过度收敛。
func TestDoJSONGETStillRetriesTransientFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	saved := DefaultBaseURL
	DefaultBaseURL = srv.URL
	defer func() { DefaultBaseURL = saved }()

	p := NewPanlianPlugin()
	var out map[string]interface{}
	if err := p.doJSONGET(srv.Client(), "", "/all-videos.php", nil, &out); err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
}
