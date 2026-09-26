package plugin

import (
	"testing"
	"time"
)

// 发布预算必须严格小于框架的观察窗口，否则插件交结果的速度永远追不上被丢弃的速度。
func TestPublishBudgetLeavesMarginBelowWindow(t *testing.T) {
	budget := PublishBudget()

	if budget >= defaultAsyncResponseWindow {
		t.Fatalf("发布预算 %v 不小于观察窗口 %v：结果仍会被整体丢弃", budget, defaultAsyncResponseWindow)
	}
	if budget < minPublishBudget || budget > maxPublishBudget {
		t.Fatalf("发布预算 %v 越界，应在 [%v, %v] 内", budget, minPublishBudget, maxPublishBudget)
	}
}

// 窗口被配得过小时预算也不能无限缩水，否则连一次上游请求都发不出去。
func TestPublishBudgetHasFloor(t *testing.T) {
	if minPublishBudget < 500*time.Millisecond {
		t.Fatalf("预算下限 %v 过低，慢插件将没有可用时间", minPublishBudget)
	}
}
