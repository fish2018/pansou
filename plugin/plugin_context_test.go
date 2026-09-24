package plugin

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pansou/model"
)

// ext 中的上下文被取消后，AsyncSearch 应立刻返回，而不是等满响应超时。
func TestAsyncSearchHonoursExtContext(t *testing.T) {
	p := NewBaseAsyncPlugin("ctxprobe-async", 1)

	slowSearch := func(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
		time.Sleep(3 * time.Second)
		return []model.SearchResult{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	results, err := p.AsyncSearch("ctxprobe-keyword-a", slowSearch, "", map[string]interface{}{
		ExtContextKey: ctx,
		"refresh":     true,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("上下文取消后仍等待了 %v", elapsed)
	}
	if len(results) != 0 {
		t.Fatalf("取消时应返回空结果，得到 %d 条", len(results))
	}
}

// 同样的取消语义要作用于 AsyncSearchWithResult。
func TestAsyncSearchWithResultHonoursExtContext(t *testing.T) {
	p := NewBaseAsyncPlugin("ctxprobe-result", 1)

	slowSearch := func(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
		time.Sleep(3 * time.Second)
		return []model.SearchResult{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result, err := p.AsyncSearchWithResult("ctxprobe-keyword-b", slowSearch, "", map[string]interface{}{
		ExtContextKey: ctx,
		"refresh":     true,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("上下文取消后仍等待了 %v", elapsed)
	}
	if result.IsFinal {
		t.Fatal("取消时不应标记为最终结果")
	}
}

// 未提供上下文时行为不变：正常完成即返回结果。
func TestAsyncSearchWithoutContextStillWorks(t *testing.T) {
	p := NewBaseAsyncPlugin("ctxprobe-plain", 1)

	fastSearch := func(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
		return []model.SearchResult{{UniqueID: "ctxprobe:1", Title: keyword}}, nil
	}

	results, err := p.AsyncSearch("ctxprobe-keyword-c", fastSearch, "", map[string]interface{}{"refresh": true})

	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，得到 %d", len(results))
	}
}

// ContextFromExt 的边界行为。
func TestContextFromExt(t *testing.T) {
	if ContextFromExt(nil) == nil {
		t.Fatal("空 ext 应返回 background 上下文")
	}
	if ctx := ContextFromExt(map[string]interface{}{ExtContextKey: "not-a-context"}); ctx == nil {
		t.Fatal("类型不符时应返回 background 上下文")
	}

	type ctxKey struct{}
	want := context.WithValue(context.Background(), ctxKey{}, "v")
	if got := ContextFromExt(map[string]interface{}{ExtContextKey: want}); got != want {
		t.Fatal("应返回 ext 中传入的上下文")
	}
}
