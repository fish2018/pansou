package quarksoo

import (
	"net/http"
	"net/http/httptest"
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

	p := NewQuarksooAsyncPlugin()
	// 缩短等待，避免用例真的等满 500ms×2
	start := time.Now()
	// 必须带 refresh：框架会缓存同一关键词的结果（含空结果），命中缓存就不发请求，
	// 那样测的就不是重试而是缓存了。
	_, err := p.Search("重试后成功", map[string]interface{}{"refresh": true})
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

	p := NewQuarksooAsyncPlugin()
	// 用独立关键词 + refresh 绕开框架缓存：否则会吃到同一进程内前一个用例留下的条目
	// （实测过：复用关键词时这里 0 次请求、err=nil、0 结果）。
	res, err := p.Search("重试耗尽", map[string]interface{}{"refresh": true})
	got := atomic.LoadInt32(&hits)
	t.Logf("请求次数=%d retries=%d 返回条数=%d err=%v", got, p.retries, len(res), err)
	if got != int32(p.retries)+1 {
		t.Errorf("尝试次数应为 retries+1=%d，实际 %d", p.retries+1, got)
	}
	if err == nil {
		t.Error("上游一直 500 时必须报错，而不是返回空结果")
	}
}
