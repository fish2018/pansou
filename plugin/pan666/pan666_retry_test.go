package pan666

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pansou/config"
)

// 重试收敛到 util.DoWithRetry 后行为必须与原先复制粘贴的循环一致：
// 固定 500ms、共 p.retries+1 次尝试、失败返回错误而不是空结果。
func TestSearchRetriesThenSucceeds(t *testing.T) {
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

	useIsolatedCache(t)
	saved := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = saved }()

	p := NewPan666AsyncPlugin()
	if _, err := p.Search("重试用例", nil); err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	// 该插件除搜索本身外还会打别的端点，所以这里只要求"确实重试过"，
	// 不追求精确等于重试次数（精确值由 util/retry_test.go 覆盖）。
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Errorf("应至少请求 3 次（含重试），实际 %d 次", got)
	}
}

// 一直失败必须报错（不能当成空结果），尝试次数为 retries+1。
func TestSearchRetriesExhaustedReturnsError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	useIsolatedCache(t)
	saved := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = saved }()

	p := NewPan666AsyncPlugin()
	_, err := p.Search("重试用例", nil)
	got := atomic.LoadInt32(&hits)
	if got < int32(p.retries)+1 {
		t.Errorf("尝试次数应不少于 retries+1=%d，实际 %d", p.retries+1, got)
	}
	if err == nil {
		t.Error("上游一直 500 时必须报错，而不是返回空结果")
	}
}

// useIsolatedCache 把缓存目录指向用例专属临时目录，并把异步响应超时给足。
//
// 磁盘缓存会跨运行留存，不清的话同一关键词第二次运行会直接吃到上次的结果。
// AsyncResponseTimeoutDur 必须显式给足：为 0 时框架会立刻返回空结果且不报错
// （搜索转到后台），看起来就像"重试没发生"。
func useIsolatedCache(t *testing.T) {
	t.Helper()
	saved := config.AppConfig
	if saved == nil {
		saved = &config.Config{}
	}
	cfg := *saved
	cfg.CachePath = t.TempDir()
	cfg.CacheEnabled = true
	cfg.AsyncResponseTimeout = 30
	cfg.AsyncResponseTimeoutDur = 30 * time.Second
	config.AppConfig = &cfg
	t.Cleanup(func() { config.AppConfig = saved })
}
