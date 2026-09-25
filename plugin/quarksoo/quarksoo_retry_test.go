package quarksoo

import (
	"net/http"
	"net/http/httptest"
	"pansou/config"
	"sync/atomic"
	"testing"
	"time"
)

// 重试逻辑收敛到 util.DoWithRetry 后，行为必须与原先复制粘贴的循环一致：
// 固定 500ms 间隔、共 p.retries+1 次尝试、最后一次失败不再等待、失败时返回错误。
func TestSearchRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前两次返回 500，第三次成功
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><body><table><tr><td>nothing</td></tr></table></body></html>`))
	}))
	defer srv.Close()

	saved := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = saved }()

	useTempCacheDir(t)
	p := NewQuarksooAsyncPlugin()
	// 缩短等待，避免用例真的等满 500ms×2
	start := time.Now()
	// 两个用例共用同一关键词，靠 useTempCacheDir 的独立缓存目录隔离：
	// 磁盘缓存会跨运行留存，不清的话第二次运行会直接吃到上次的结果。
	_, err := p.Search("同一关键词", nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("第三次应当成功，实际报错: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
	if elapsed > 3*time.Second {
		t.Errorf("重试耗时异常: %v", elapsed)
	}
}

// 一直失败时必须返回错误（不能被当成"空结果"），且尝试次数正确。
func TestSearchRetriesExhaustedReturnsError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	saved := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = saved }()

	useTempCacheDir(t)
	p := NewQuarksooAsyncPlugin()
	// 同关键词，隔离见 useTempCacheDir
	res, err := p.Search("同一关键词", nil)
	got := atomic.LoadInt32(&hits)
	t.Logf("请求次数=%d retries=%d 返回条数=%d err=%v", got, p.retries, len(res), err)
	if got != int32(p.retries)+1 {
		t.Errorf("尝试次数应为 retries+1=%d，实际 %d", p.retries+1, got)
	}
	if err == nil {
		t.Error("上游一直 500 时必须报错，而不是返回空结果")
	}
}

// useTempCacheDir 把缓存目录指向本用例专属的临时目录。
//
// 磁盘缓存会跨运行留存：不清的话，同一关键词在第二次运行时会直接命中上次的结果，
// 测的就不是重试而是缓存了。用例里两个测试共用同一关键词，靠这个目录隔离即可，
// 无需靠换关键词或加 refresh。
func useTempCacheDir(t *testing.T) {
	t.Helper()
	saved := config.AppConfig
	if saved == nil {
		saved = &config.Config{}
	}
	cfg := *saved
	cfg.CachePath = t.TempDir()
	cfg.CacheEnabled = true
	// 关键：异步响应超时必须显式给足。Dur 为 0 时框架会立刻返回（空结果 + nil 错误），
	// 搜索转到后台继续跑——看起来就像"重试没发生"，实测过，这个签名极易被误判成缓存命中。
	cfg.AsyncResponseTimeout = 30
	cfg.AsyncResponseTimeoutDur = 30 * time.Second
	config.AppConfig = &cfg
	t.Cleanup(func() { config.AppConfig = saved })
}
