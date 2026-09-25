package javdb

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// javdb 的重试语义与其它插件完全不同：**只在 429 时重试**，非 429（含 5xx）直接返回响应。
// 收敛到 util.DoWithRetry 后这条必须保持。
func TestRateLimitRetryReturnsNon429AsIs(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewJavdbPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.doRequestWithRateLimitRetry(req, srv.Client())
	if err != nil {
		t.Fatalf("非 429 应直接返回响应而不是报错: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("应原样返回 500，实际 %d", resp.StatusCode)
	}
	// 非 429 不重试
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("非 429 不该重试，实际请求 %d 次", got)
	}
	// 也不该被标记为限流
	if atomic.LoadInt32(&p.rateLimited) != 0 {
		t.Error("非 429 不该置 rateLimited 标志")
	}
}

// MaxRetryOnRateLimit 默认为 0（不重试）：遇到 429 时第 1 次即放弃、置 rateLimited、
// 并返回带原因的专用错误。这条最容易在迁移时写错成"仍重试 MaxRetryOnRateLimit 次"。
func TestRateLimitGivesUpImmediatelyWhenRetryDisabled(t *testing.T) {
	if MaxRetryOnRateLimit != 0 {
		t.Skipf("本用例假设默认不重试，当前 MaxRetryOnRateLimit=%d", MaxRetryOnRateLimit)
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := NewJavdbPlugin()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.doRequestWithRateLimitRetry(req, srv.Client())
	if err == nil {
		t.Fatal("429 且不重试时必须报错")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("不重试时应只请求 1 次，实际 %d 次", got)
	}
	if atomic.LoadInt32(&p.rateLimited) != 1 {
		t.Error("放弃重试时必须置 rateLimited 标志")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("错误应说明是 429 限流，实际: %v", err)
	}
	if atomic.LoadInt32(&p.rateLimitCount) != 1 {
		t.Errorf("429 计数应为 1，实际 %d", atomic.LoadInt32(&p.rateLimitCount))
	}
}
