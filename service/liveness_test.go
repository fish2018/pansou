package service

import (
	"fmt"
	"testing"
)

func newTestLiveness() *livenessRegistry {
	return &livenessRegistry{
		plugins:  make(map[string]*livenessEntry),
		channels: make(map[string]*livenessEntry),
	}
}

func TestLivenessClassify(t *testing.T) {
	r := newTestLiveness()
	p := r.plugins
	// 每轮都报错、从未产出 -> failing
	for i := 0; i < 6; i++ {
		r.record(p, "dead", 0, errStub{})
	}
	// 无报错但从未产出（内容靠后台补齐）-> zero_yield，不能与 failing 混为一谈
	for i := 0; i < 6; i++ {
		r.record(p, "slow", 0, nil)
	}
	// 有产出 -> ok
	for i := 0; i < 6; i++ {
		r.record(p, "good", 3, nil)
	}
	// 有过产出但失败过半 -> degraded
	for i := 0; i < 6; i++ {
		if i%3 == 0 {
			r.record(p, "flaky", 2, nil)
		} else {
			r.record(p, "flaky", 0, errStub{})
		}
	}
	// 样本不足 -> 不判定
	r.record(p, "fresh", 0, errStub{})

	rep := r.snapshot()
	if got := classify(p["dead"]); got != "failing" {
		t.Fatalf("dead = %s", got)
	}
	if got := classify(p["slow"]); got != "zero_yield" {
		t.Fatalf("slow = %s", got)
	}
	if got := classify(p["good"]); got != "ok" {
		t.Fatalf("good = %s", got)
	}
	if got := classify(p["flaky"]); got != "degraded" {
		t.Fatalf("flaky = %s", got)
	}
	if got := classify(p["fresh"]); got != "insufficient_data" {
		t.Fatalf("fresh = %s", got)
	}
	if len(rep.FailingPlugins) != 1 || rep.FailingPlugins[0].Name != "dead" {
		t.Fatalf("failing 列表 = %+v", rep.FailingPlugins)
	}
	if rep.PluginStatus["ok"] != 1 || rep.PluginStatus["degraded"] != 1 {
		t.Fatalf("状态统计 = %+v", rep.PluginStatus)
	}
}

func TestLivenessZeroYieldNotReportedAsFailing(t *testing.T) {
	r := newTestLiveness()
	for i := 0; i < 10; i++ {
		r.record(r.plugins, "slow", 0, nil)
	}
	rep := r.snapshot()
	if len(rep.FailingPlugins) != 0 {
		t.Fatalf("无报错的零产出不该算 failing: %+v", rep.FailingPlugins)
	}
	if len(rep.ZeroYieldPlugins) != 1 {
		t.Fatalf("应出现在 zero_yield 列表: %+v", rep.ZeroYieldPlugins)
	}
	if rep.ZeroYieldPlugins[0].LastError != "" {
		t.Fatalf("零产出项不该带错误文本: %+v", rep.ZeroYieldPlugins[0])
	}
}

func TestLivenessRecoveredPluginLeavesFailingList(t *testing.T) {
	r := newTestLiveness()
	for i := 0; i < 6; i++ {
		r.record(r.plugins, "p", 0, errStub{})
	}
	if len(r.snapshot().FailingPlugins) != 1 {
		t.Fatal("先应被标为 failing")
	}
	r.record(r.plugins, "p", 5, nil)
	rep := r.snapshot()
	if len(rep.FailingPlugins) != 0 {
		t.Fatalf("恢复产出后不该仍在 failing 列表: %+v", rep.FailingPlugins)
	}
	if rep.PluginStatus["degraded"] != 1 {
		t.Fatalf("刚恢复但失败仍占多数，应先是 degraded: %+v", rep.PluginStatus)
	}
	// 持续恢复：旧失败应被 20 轮窗口冲刷掉，而不是永久留在名单里
	for i := 0; i < 15; i++ {
		r.record(r.plugins, "p", 5, nil)
	}
	rep = r.snapshot()
	if rep.PluginStatus["ok"] != 1 {
		t.Fatalf("持续恢复后应转为 ok: %+v（窗口未冲刷旧失败）", rep.PluginStatus)
	}
	if rep.FailingPlugins != nil {
		t.Fatalf("不应再有 failing: %+v", rep.FailingPlugins)
	}
}

func TestLivenessSnapshotsAreIsolated(t *testing.T) {
	r := newTestLiveness()
	for i := 0; i < 6; i++ {
		r.record(r.plugins, "a", 1, nil)
	}
	rep := r.snapshot()
	if len(rep.FailingPlugins) != 0 || rep.PluginTotal != 1 {
		t.Fatalf("报告异常: %+v", rep)
	}
	// 快照后继续累积，不能影响已生成的报告
	for i := 0; i < 6; i++ {
		r.record(r.plugins, "b", 0, errStub{})
	}
	if len(rep.FailingPlugins) != 0 {
		t.Fatalf("已生成的报告被后续写入污染: %+v", rep.FailingPlugins)
	}
	if len(r.snapshot().FailingPlugins) != 1 {
		t.Fatal("新快照应包含新增的 failing")
	}
}

type errStub struct{}

func (errStub) Error() string { return "搜索响应状态码异常: 403" }

func TestLivenessCountsSurviveTopNTruncation(t *testing.T) {
	r := newTestLiveness()
	// 造 50 个全部失败的插件：超过每类 40 条上限
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("dead-%02d", i)
		for k := 0; k < 6; k++ {
			r.record(r.plugins, name, 0, errStub{})
		}
	}
	rep := r.snapshot()
	if len(rep.FailingPlugins) != livenessTopN {
		t.Fatalf("列表应被截断到 %d 条, 实际 %d", livenessTopN, len(rep.FailingPlugins))
	}
	// 关键：总数必须仍是完整的 50，否则前端无从知道名单被截断过
	if rep.FailingPluginCount != 50 {
		t.Fatalf("完整数量应为 50, 实际 %d", rep.FailingPluginCount)
	}
	if rep.PluginStatus["failing"] != 50 {
		t.Fatalf("状态统计应为 50, 实际 %d", rep.PluginStatus["failing"])
	}
	if len(rep.Truncated) == 0 {
		t.Fatal("被截断时必须给出说明，否则超出部分被静默隐藏")
	}
	if rep.FailingPluginCount-len(rep.FailingPlugins) != 10 {
		t.Fatalf("应显示还有 10 条未列出, 实际 %d", rep.FailingPluginCount-len(rep.FailingPlugins))
	}
}

func TestLivenessNoTruncationNoteWhenUnderLimit(t *testing.T) {
	r := newTestLiveness()
	for i := 0; i < 3; i++ {
		r.record(r.plugins, fmt.Sprintf("p-%d", i), 0, errStub{})
	}
	if len(r.snapshot().Truncated) != 0 {
		t.Fatal("未截断时不该出现截断说明")
	}
}
