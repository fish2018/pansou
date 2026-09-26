package service

import (
	"sort"
	"sync"
	"time"
)

// pluginTimingTracker 记录每个插件近期的任务耗时，以及"连续被批截止放弃"的轮次。
//
// 它服务于两件事：
//  1. 批截止不再拍一个固定秒数，而是按"波次 × 每任务 p90"推导（见 deriveBatchDeadline）；
//  2. 任务提交顺序按历史 p50 升序（短作业优先），并对连续被放弃的插件做老化提升，
//     避免短作业优先的经典缺陷——长作业饥饿。
//
// 样本只保留有界窗口，长期运行时不会无界增长。所有方法可并发调用。
type pluginTimingTracker struct {
	mu      sync.Mutex
	samples map[string][]time.Duration
	starved map[string]int
}

const (
	// timingWindowSize 每个插件保留的样本数。20 个样本足以让 p90 有实际意义，
	// 又不至于让很久以前的旧网络状况长期影响排序。
	timingWindowSize = 20
	// agingRounds 连续被批截止放弃达到该轮次后，该插件下一轮优先提交。
	// 取 2 而非 1：单次被放弃常是瞬时抖动，连续两次才说明它确实排在队尾。
	agingRounds = 2
)

func newPluginTimingTracker() *pluginTimingTracker {
	return &pluginTimingTracker{
		samples: make(map[string][]time.Duration),
		starved: make(map[string]int),
	}
}

// observe 记录一次任务耗时（仅统计正常返回的任务）。
func (t *pluginTimingTracker) observe(name string, d time.Duration) {
	if t == nil || name == "" || d <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := append(t.samples[name], d)
	if len(s) > timingWindowSize {
		s = s[len(s)-timingWindowSize:]
	}
	t.samples[name] = s
}

// markTimedOut 记一次"本轮被批截止放弃"，连续轮次用于老化。
func (t *pluginTimingTracker) markTimedOut(name string) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	t.starved[name]++
	t.mu.Unlock()
}

// markReturned 记一次"本轮正常返回"，连续被放弃的计数清零。
func (t *pluginTimingTracker) markReturned(name string) {
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	delete(t.starved, name)
	t.mu.Unlock()
}

// percentile 返回该插件历史耗时的分位数（q 取 0~1），无样本时返回 false。
func (t *pluginTimingTracker) percentile(name string, q float64) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	src := t.samples[name]
	s := make([]time.Duration, len(src))
	copy(s, src)
	t.mu.Unlock()
	if len(s) == 0 {
		return 0, false
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	idx := int(q * float64(len(s)-1))
	return s[idx], true
}

// priorityFor 给出排序键：越小的越先提交。
// 连续被放弃的插件给 -1（排到最前），其余用历史 p50；无样本的排在中位。
func (t *pluginTimingTracker) priorityFor(name string) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	starved := t.starved[name]
	t.mu.Unlock()
	if starved >= agingRounds {
		return -1
	}
	if p50, ok := t.percentile(name, 0.5); ok {
		return p50
	}
	// 无样本：排在"已知是快的"之后、"已知是慢的"之前。
	return 2 * time.Second
}

// sortedByShortestFirst 按短作业优先重排插件，带老化提升。
//
// 稳定性很重要：优先级相同时保持原有注册顺序，避免同一批插件在多次请求间来回抖动。
func (t *pluginTimingTracker) sortedByShortestFirst(names []string) []string {
	if len(names) == 0 {
		return names
	}
	type item struct {
		name  string
		key   time.Duration
		order int
	}
	items := make([]item, len(names))
	for i, n := range names {
		items[i] = item{name: n, key: t.priorityFor(n), order: i}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].key != items[j].key {
			return items[i].key < items[j].key
		}
		return items[i].order < items[j].order
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.name
	}
	return out
}

// aggregateP90 返回所有插件样本合并后的 p90 与样本总数，供批截止推导使用。
// 合并样本而不是取"各插件 p90 的最大值"：后者会被单个偶发抖动的插件顶高，
// 而批截止关心的是"一整批任务里典型的最慢情况"。
func (t *pluginTimingTracker) aggregateP90() (time.Duration, int) {
	if t == nil {
		return 0, 0
	}
	t.mu.Lock()
	total := 0
	for _, s := range t.samples {
		total += len(s)
	}
	merged := make([]time.Duration, 0, total)
	for _, s := range t.samples {
		merged = append(merged, s...)
	}
	t.mu.Unlock()
	if len(merged) == 0 {
		return 0, 0
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i] < merged[j] })
	idx := int(0.9 * float64(len(merged)-1))
	return merged[idx], len(merged)
}

// statsLine 输出一行紧凑的分布摘要，取样本最多的前 limit 个插件。
func (t *pluginTimingTracker) statsLine(limit int) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(t.samples))
	for name, s := range t.samples {
		rows = append(rows, row{name: name, n: len(s)})
	}
	t.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := ""
	for _, r := range rows {
		p50, _ := t.percentile(r.name, 0.5)
		p90, _ := t.percentile(r.name, 0.9)
		if out != "" {
			out += " "
		}
		out += r.name + ":" + p50.Round(time.Millisecond).String() + "/" + p90.Round(time.Millisecond).String()
	}
	return out
}
