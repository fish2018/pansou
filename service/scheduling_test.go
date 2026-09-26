package service

import (
	"context"
	"testing"
	"time"
)

func TestEffectiveFanoutConcurrencyDecouplesFromCaller(t *testing.T) {
	cases := []struct {
		name        string
		requestConc int
		taskCount   int
		limit       int
		want        int
	}{
		{"调用方给得少时补足到任务数", 10, 71, 128, 71},
		{"调用方给得多时以出口上限收口", 200, 71, 128, 128},
		{"两者相同时不变", 71, 71, 128, 71},
		{"上限小于任务数时以上限为准", 10, 200, 64, 64},
		{"无任务时保留调用方值", 10, 0, 128, 10},
		{"上限未配置时按默认上限收口", 500, 71, 0, 128},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveFanoutConcurrency(c.requestConc, c.taskCount, c.limit); got != c.want {
				t.Fatalf("effectiveFanoutConcurrency(%d,%d,%d) = %d, 期望 %d",
					c.requestConc, c.taskCount, c.limit, got, c.want)
			}
		})
	}
}

func TestDeriveBatchDeadlineByWaves(t *testing.T) {
	perTask := 4 * time.Second
	cases := []struct {
		name     string
		tasks    int
		conc     int
		perTask  time.Duration
		override time.Duration
		cap      time.Duration
		want     time.Duration
	}{
		{"一波：71 任务 / 71 并发", 71, 71, perTask, 0, 10 * time.Second, 5 * time.Second},
		{"两波：71 任务 / 36 并发", 71, 36, perTask, 0, 10 * time.Second, 9 * time.Second},
		{"多波被上限截断", 71, 10, perTask, 0, 10 * time.Second, 10 * time.Second},
		{"显式覆盖优先于公式", 71, 71, perTask, 8 * time.Second, 10 * time.Second, 8 * time.Second},
		{"无样本时用默认每任务预算", 71, 71, 0, 0, 10 * time.Second, 5 * time.Second},
		{"公式值过小时抬到下限", 1, 71, 100 * time.Millisecond, 0, 10 * time.Second, 4 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveBatchDeadline(c.tasks, c.conc, c.perTask, c.override, c.cap); got != c.want {
				t.Fatalf("deriveBatchDeadline(%d,%d,%v,%v,%v) = %v, 期望 %v",
					c.tasks, c.conc, c.perTask, c.override, c.cap, got, c.want)
			}
		})
	}
}

func TestTrackerShortestFirstWithAging(t *testing.T) {
	tr := newPluginTimingTracker()
	// fast 快、slow 慢、unknown 无样本
	for i := 0; i < 3; i++ {
		tr.observe("fast", 500*time.Millisecond)
		tr.observe("slow", 9*time.Second)
	}
	got := tr.sortedByShortestFirst([]string{"slow", "unknown", "fast"})
	want := []string{"fast", "unknown", "slow"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("短作业优先排序 = %v, 期望 %v", got, want)
		}
	}

	// slow 连续两轮被放弃后应被老化提升到最前（否则长作业会饥饿）
	tr.markTimedOut("slow")
	tr.markTimedOut("slow")
	got = tr.sortedByShortestFirst([]string{"slow", "unknown", "fast"})
	if got[0] != "slow" {
		t.Fatalf("老化后应把 slow 提到最前, 实际 %v", got)
	}

	// 正常返回一次后老化计数清零，重新按耗时排序
	tr.markReturned("slow")
	got = tr.sortedByShortestFirst([]string{"slow", "unknown", "fast"})
	if got[0] != "fast" {
		t.Fatalf("老化清零后应按耗时排序, 实际 %v", got)
	}
}

func TestTrackerWindowIsBounded(t *testing.T) {
	tr := newPluginTimingTracker()
	for i := 0; i < timingWindowSize*5; i++ {
		tr.observe("p", time.Duration(i+1)*time.Millisecond)
	}
	if n := len(tr.samples["p"]); n != timingWindowSize {
		t.Fatalf("样本窗口应被限制在 %d, 实际 %d", timingWindowSize, n)
	}
	p50, ok := tr.percentile("p", 0.5)
	if !ok || p50 < time.Duration(timingWindowSize*5-timingWindowSize)*time.Millisecond {
		t.Fatalf("窗口应只保留最近的样本, p50=%v", p50)
	}
}

func TestOutboundGateReleasesAndRespectsContext(t *testing.T) {
	gate := ensureOutboundGate()
	if cap(gate) != defaultOutboundMaxConcurrency {
		t.Fatalf("出口闸默认容量 = %d, 期望 %d", cap(gate), defaultOutboundMaxConcurrency)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if !acquireOutbound(ctx) {
		t.Fatal("应能取到槽位")
	}
	releaseOutbound()
	// 取消后不应再阻塞等待
	cancel()
	if acquireOutbound(ctx) {
		releaseOutbound()
		t.Fatal("ctx 已取消时不应取到槽位")
	}
}
