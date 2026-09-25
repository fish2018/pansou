package util

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoWithRetrySucceedsFirstTry(t *testing.T) {
	var calls int32
	err := DoWithRetry(RetryConfig{Attempts: 3, BaseDelay: time.Millisecond}, func(int) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("首次成功不该重试，实际调用 %d 次", calls)
	}
}

func TestDoWithRetryStopsAtAttemptsAndPreservesError(t *testing.T) {
	sentinel := errors.New("上游 500")
	var calls int32
	err := DoWithRetry(RetryConfig{Attempts: 4, BaseDelay: time.Millisecond}, func(int) error {
		atomic.AddInt32(&calls, 1)
		return sentinel
	})
	if err == nil {
		t.Fatal("应当失败")
	}
	if calls != 4 {
		t.Errorf("应尝试 4 次，实际 %d", calls)
	}
	// 原始错误链必须保留，否则上层没法区分"超时"和"500"
	if !errors.Is(err, sentinel) {
		t.Errorf("错误链丢失: %v", err)
	}
	if err.Error() == sentinel.Error() {
		t.Error("应带上尝试次数，便于排查")
	}
}

// 成功即停：中途成功后不该继续重试。
func TestDoWithRetryStopsOnSuccess(t *testing.T) {
	var calls int32
	err := DoWithRetry(RetryConfig{Attempts: 5, BaseDelay: time.Millisecond}, func(attempt int) error {
		atomic.AddInt32(&calls, 1)
		if attempt < 2 {
			return errors.New("暂时失败")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("第 3 次成功即应停止，实际调用 %d 次", calls)
	}
}

// 成功路径不该有额外等待（否则每次正常请求都被拖慢）。
func TestDoWithRetryNoSleepOnSuccess(t *testing.T) {
	start := time.Now()
	err := DoWithRetry(RetryConfig{Attempts: 3, BaseDelay: 2 * time.Second}, func(int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("成功路径不该等待，耗时 %v", elapsed)
	}
}

func TestRetryDelayBackoffAndCap(t *testing.T) {
	cfg := RetryConfig{BaseDelay: 100 * time.Millisecond, Multiplier: 2, MaxDelay: 250 * time.Millisecond}
	if got := retryDelay(cfg, 0); got != 100*time.Millisecond {
		t.Errorf("首次等待应为 BaseDelay，实际 %v", got)
	}
	if got := retryDelay(cfg, 1); got != 200*time.Millisecond {
		t.Errorf("第二次应为 200ms，实际 %v", got)
	}
	if got := retryDelay(cfg, 2); got != 250*time.Millisecond {
		t.Errorf("第三次应被封顶到 250ms，实际 %v", got)
	}
}

// 固定间隔（BaseDelay == MaxDelay）是既有 30 多处复制的形态，行为必须一致。
func TestRetryDelayFixedKeepsLegacyBehaviour(t *testing.T) {
	cfg := RetryConfig{BaseDelay: 500 * time.Millisecond, MaxDelay: 500 * time.Millisecond}
	for attempt := 0; attempt < 4; attempt++ {
		if got := retryDelay(cfg, attempt); got != 500*time.Millisecond {
			t.Errorf("第 %d 次等待应为固定 500ms，实际 %v", attempt, got)
		}
	}
}

// 抖动必须真的把等待铺开，且不越界。
func TestRetryDelayJitterSpreadsWithinBounds(t *testing.T) {
	cfg := RetryConfig{BaseDelay: 100 * time.Millisecond, MaxDelay: 100 * time.Millisecond, Jitter: true}
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := retryDelay(cfg, 0)
		if got < 75*time.Millisecond || got > 125*time.Millisecond {
			t.Fatalf("抖动越界: %v", got)
		}
		seen[got] = true
	}
	if len(seen) < 5 {
		t.Errorf("抖动几乎不起作用，只出现 %d 种取值", len(seen))
	}
}

func TestDoWithRetryOnRetryObservability(t *testing.T) {
	var seen []time.Duration
	_ = DoWithRetry(RetryConfig{
		Attempts:  3,
		BaseDelay: time.Millisecond,
		OnRetry: func(_ int, _ error, wait time.Duration) {
			seen = append(seen, wait)
		},
	}, func(int) error { return errors.New("x") })

	if len(seen) != 2 {
		t.Errorf("3 次尝试应有 2 次重试回调（最后一次失败不等），实际 %d", len(seen))
	}
}
