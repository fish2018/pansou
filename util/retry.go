package util

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// AbortError 包住一个"重试没有意义"的错误，让 DoWithRetry 立即停止而不是把它重试掉。
//
// 为什么需要它：仓里的重试并不都是"任何失败都值得再来一次"。例如 4xx（目标明确拒绝）、
// 登录失效（cookie 过期）、响应格式不对（再试还会不对）——这些继续重试只是白等。
// 没有这个信号时，这些站点无法收敛到组件，只能各自保留一份循环。
//
// 用法：return util.Abort(fmt.Errorf("..."))；判定用 errors.Is(err, util.ErrAborted)
// 或直接看 DoWithRetry 的返回值——它会原样透出被包住的错误。
type abortError struct{ err error }

func (e *abortError) Error() string { return e.err.Error() }
func (e *abortError) Unwrap() error { return e.err }

// ErrAborted 是 AbortError 的哨兵，便于上层用 errors.Is 判定"这是中止而非重试耗尽"。
var ErrAborted = errors.New("重试被中止")

// Is 让 errors.Is(err, ErrAborted) 对任意 AbortError 成立。
func (e *abortError) Is(target error) bool { return target == ErrAborted }

// Abort 把 err 标记为"不再重试"。err 为 nil 时返回 nil。
func Abort(err error) error {
	if err == nil {
		return nil
	}
	return &abortError{err: err}
}

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
	// DelayFunc 直接决定第 attempt 次失败后的等待时长（attempt 从 0 起），优先级最高。
	//
	// 存在的原因是"退避曲线"并非只有倍率一种：仓里有线性退避（attempt×200ms）、
	// 区间随机延迟（限流场景）、以及完全不退避的实现。要把它们也收敛进来，
	// 就不能只提供 BaseDelay×Multiplier 这一种表达。返回 <=0 表示该次不等待。
	DelayFunc func(attempt int) time.Duration
	// OnRetry 在每次失败后、等待前调用（可为 nil），便于观测。
	OnRetry func(attempt int, err error, wait time.Duration)

	// WaitFunc 自定义"如何等待"，替代默认的 time.Sleep；返回非 nil 表示**立即停止重试**
	// 并把该错误作为最终结果返回（不做"重试 N 次后仍失败"的包装——它不是重试耗尽，
	// 而是被主动中断）。
	//
	// 存在的原因是原实现里有两处等的是**可被请求上下文取消**的等待（NewTimer + select
	// ctx.Done），不是 time.Sleep。没有这个口子时，那两个站点只能把等待留在自己的闭包内、
	// 组件侧 DelayFunc 返回 0——功能正确但形态特殊，也无法让取消语义在全仓统一。
	//
	//	WaitFunc: func(w time.Duration) error {
	//		timer := time.NewTimer(w)
	//		defer timer.Stop()
	//		select {
	//		case <-timer.C:
	//			return nil
	//		case <-ctx.Done():
	//			return ctx.Err()
	//		}
	//	}
	WaitFunc func(wait time.Duration) error
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
			// 被标记为"重试没有意义"的错误立即停止，不等待、不再试
			if errors.Is(err, ErrAborted) {
				break
			}
			if attempt == attempts-1 {
				break // 最后一次失败不再等待
			}
			wait := retryDelay(cfg, attempt)
			if cfg.OnRetry != nil {
				cfg.OnRetry(attempt, err, wait)
			}
			if cfg.WaitFunc != nil {
				// 等待被中断（典型是上下文取消）：立即停止，不是重试耗尽
				if waitErr := cfg.WaitFunc(wait); waitErr != nil {
					return waitErr
				}
			} else if wait > 0 {
				time.Sleep(wait)
			}
			continue
		}
		return nil
	}

	if lastErr == nil {
		// attempts>=1 时循环必然至少跑一次，走到这里说明 fn 是 nil——当作配置错误报出来，
		// 而不是静默地"成功"。
		return fmt.Errorf("重试未执行：fn 为空")
	}
	if errors.Is(lastErr, ErrAborted) {
		// 主动中止：不要包装成"重试 N 次后仍失败"，那是误导——它第一次就决定不重试了
		return lastErr
	}
	if attempts > 1 {
		return fmt.Errorf("重试 %d 次后仍失败: %w", attempts, lastErr)
	}
	return lastErr
}

// retryDelay 计算第 attempt 次失败后的等待时长（attempt 从 0 起）。
func retryDelay(cfg RetryConfig, attempt int) time.Duration {
	if cfg.DelayFunc != nil {
		return cfg.DelayFunc(attempt)
	}
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
