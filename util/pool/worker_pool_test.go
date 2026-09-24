package pool

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// 超时后应立即返回已完成的结果，不再等仍在运行的慢任务。
func TestExecuteBatchWithTimeoutDoesNotWaitForSlowTasks(t *testing.T) {
	tasks := []Task{
		func(ctx context.Context) interface{} { time.Sleep(2 * time.Second); return "slow" },
		func(ctx context.Context) interface{} { return "fast" },
	}

	start := time.Now()
	results := ExecuteBatchWithTimeout(tasks, len(tasks), 200*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("超时 200ms，实际耗时 %v，说明仍在等待慢任务", elapsed)
	}
	if len(results) != 1 || results[0] != "fast" {
		t.Fatalf("应只返回快任务结果，得到 %v", results)
	}
}

// 任务应收到带截止时间的上下文，否则插件无法据此提前返回。
func TestTaskReceivesContextWithDeadline(t *testing.T) {
	var got atomic.Value
	tasks := []Task{
		func(ctx context.Context) interface{} {
			_, ok := ctx.Deadline()
			got.Store(ok)
			return "ok"
		},
	}

	ExecuteBatchWithTimeout(tasks, 1, time.Second)

	if hasDeadline, _ := got.Load().(bool); !hasDeadline {
		t.Fatal("任务未收到带截止时间的上下文")
	}
}

// 全部任务按时完成时应返回全部结果。
func TestExecuteBatchWithTimeoutAllComplete(t *testing.T) {
	tasks := make([]Task, 10)
	for i := range tasks {
		i := i
		tasks[i] = func(ctx context.Context) interface{} { return i }
	}

	results := ExecuteBatchWithTimeout(tasks, 3, time.Second)

	if len(results) != len(tasks) {
		t.Fatalf("期望 %d 个结果，得到 %d", len(tasks), len(results))
	}
}

// 并发小于任务数一半时结果队列会被写满，验证不会永久阻塞。
func TestBatchDoesNotHangWhenResultsFill(t *testing.T) {
	tasks := make([]Task, 6)
	for i := range tasks {
		tasks[i] = func(ctx context.Context) interface{} {
			time.Sleep(400 * time.Millisecond)
			return "r"
		}
	}

	done := make(chan []interface{}, 1)
	go func() { done <- ExecuteBatchWithTimeout(tasks, 2, 200*time.Millisecond) }()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("6 任务 / 并发 2 时未返回：结果队列写满导致阻塞")
	}
}

// 超时后的后台回收不应泄漏 goroutine。
func TestBackgroundReclaimDoesNotLeakGoroutines(t *testing.T) {
	slow := func(ctx context.Context) interface{} {
		time.Sleep(1500 * time.Millisecond)
		return "slow"
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		ExecuteBatchWithTimeout([]Task{slow, slow}, 2, 50*time.Millisecond)
	}

	time.Sleep(3 * time.Second)
	runtime.GC()

	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutine 未回收：before=%d after=%d", before, after)
	}
}

// 任务 panic 应被收敛为该轮无结果，不影响调用方。
func TestTaskPanicIsContained(t *testing.T) {
	tasks := []Task{
		func(ctx context.Context) interface{} { panic("plugin exploded") },
		func(ctx context.Context) interface{} { return "ok" },
	}

	results := ExecuteBatchWithTimeout(tasks, 2, time.Second)

	if len(results) != 2 {
		t.Fatalf("期望 2 个结果位，得到 %d", len(results))
	}
}

// 无超时版本保持原有语义。
func TestExecuteBatchReturnsAllResults(t *testing.T) {
	tasks := make([]Task, 5)
	for i := range tasks {
		i := i
		tasks[i] = func(ctx context.Context) interface{} { return i }
	}

	results := ExecuteBatch(tasks, 2)

	if len(results) != len(tasks) {
		t.Fatalf("期望 %d 个结果，得到 %d", len(tasks), len(results))
	}
}
