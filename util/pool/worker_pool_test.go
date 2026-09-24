package pool

import (
	"testing"
	"time"
)

// 超时后应立即返回已完成的结果，不能等仍在运行的慢任务结束
func TestExecuteBatchWithTimeoutDoesNotWaitForSlowTasks(t *testing.T) {
	tasks := []Task{
		func() interface{} { return "fast" },
		func() interface{} { time.Sleep(2 * time.Second); return "slow" },
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

// 所有任务按时完成时应返回全部结果
func TestExecuteBatchWithTimeoutAllComplete(t *testing.T) {
	tasks := make([]Task, 10)
	for i := range tasks {
		i := i
		tasks[i] = func() interface{} { return i }
	}
	results := ExecuteBatchWithTimeout(tasks, 3, time.Second)
	if len(results) != len(tasks) {
		t.Fatalf("期望 %d 个结果，得到 %d", len(tasks), len(results))
	}
}
