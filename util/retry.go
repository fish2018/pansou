package util

import (
	"fmt"
	"math/rand"
	"time"
)

// RetryConfig 描述一次重试策略。
type RetryConfig struct {
	// Attempts 是总尝试次数（含首次）。<=1 表示不重试。
	Attempts int
	// BaseDelay 是首次重试前的等待时长。
	BaseDelay time.Duration
	// MaxDelay 是等待时长的上限；<=0 表示不封顶。
	// 与 BaseDelay 相等即为固定间隔（全仓原有 30 多处复制的重试都是这个形态）。
	MaxDelay time.Duration
	// Multiplier 是退避倍数；<=1 表示固定间隔。
	Multiplier float64
	// Jitter 为真时对每次等待加入 ±25% 抖动。
	//
	// 抖动不是装饰：全仓的复制粘贴重试统一写死 500ms，上游一旦限流，所有并发插件会在
	// 同一时刻重新打过去，把限流变成雪崩。加抖动把重试时刻铺开。
	Jitter bool
	// OnRetry 在每次失败后、等待前调用（可为 nil），便于观测。
	OnRetry func(attempt int, err error, wait time.Duration)
}

// DoWithRetry 反复执行 fn 直到它返回 nil 错误或尝试次数用尽。
//
// 存在的理由不只是去重：这段循环在全仓被复制了 30 多次，每份都要自己处理
// "最后一次不再等待"、"错误怎么包装"、"响应体在循环里怎么关"（其中 4 份就是在
// 这里把 defer 写进循环体，导致重试 N 次压着 N 个未关闭的响应体）。
//
// fn 收到 0 起始的尝试序号，返回 nil 即成功。失败时返回最后一次的错误（含尝试次数），
// 保留原始错误链以便上层用 errors.Is/As 判断。
func DoWithRetry(cfg RetryConfig, fn func(attempt int) error) error {
	attempts := cfg.Attempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := fn(attempt); err != nil {
			lastErr = err
			if attempt == attempts-1 {
				break // 最后一次失败不再等待
			}
			wait := retryDelay(cfg, attempt)
			if cfg.OnRetry != nil {
				cfg.OnRetry(attempt, err, wait)
			}
			time.Sleep(wait)
			continue
		}
		return nil
	}

	if lastErr == nil {
		// attempts>=1 时循环必然至少跑一次，走到这里说明 fn 是 nil——当作配置错误报出来，
		// 而不是静默地"成功"。
		return fmt.Errorf("重试未执行：fn 为空")
	}
	if attempts > 1 {
		return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
	}
	return lastErr
}

// retryDelay 计算第 attempt 次失败后的等待时长（attempt 从 0 起）。
func retryDelay(cfg RetryConfig, attempt int) time.Duration {
	wait := cfg.BaseDelay
	if wait <= 0 {
		wait = 500 * time.Millisecond // 与全仓既有重试保持一致的默认值
	}
	if cfg.Multiplier > 1 && attempt > 0 {
		for i := 0; i < attempt; i++ {
			wait = time.Duration(float64(wait) * cfg.Multiplier)
			if cfg.MaxDelay > 0 && wait >= cfg.MaxDelay {
				wait = cfg.MaxDelay
				break
			}
		}
	}
	if cfg.MaxDelay > 0 && wait > cfg.MaxDelay {
		wait = cfg.MaxDelay
	}
	if cfg.Jitter {
		// ±25%
		delta := int64(float64(wait) * 0.25)
		if delta > 0 {
			wait += time.Duration(rand.Int63n(2*delta) - delta)
		}
		if wait < 0 {
			wait = 0
		}
	}
	return wait
}
