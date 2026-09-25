package service

import (
	"fmt"
	"time"

	"pansou/model"
)

// batchSearchOutcome 记录一次批量搜索（TG 频道或插件）的完整度。
//
// "任务失败"与"任务超时未返回"必须分开统计：前者是上游的常态（频道不存在、
// 站点改版、单站限流），后者说明批截止过紧或上游整体变慢。两者的处置不同——
// 只有超时缺失才值得后台补齐，也只有它需要压低缓存有效期。
//
// 本类型不依赖全局配置，TTL 与补齐开关都由调用方传入，便于单测覆盖各分支。
type batchSearchOutcome struct {
	total     int
	succeeded int
	failed    int
	returned  map[string]bool
	missing   []string
}

func newBatchSearchOutcome(total int) *batchSearchOutcome {
	return &batchSearchOutcome{
		total:    total,
		returned: make(map[string]bool, total),
	}
}

// observe 记录一个已返回任务的结果，err 非 nil 表示该任务失败。
func (o *batchSearchOutcome) observe(id string, err error) {
	o.returned[id] = true
	if err != nil {
		o.failed++
		return
	}
	o.succeeded++
}

// finalize 依据提交时的标识列表算出超时未返回的任务。
func (o *batchSearchOutcome) finalize(submitted []string) {
	for _, id := range submitted {
		if !o.returned[id] {
			o.missing = append(o.missing, id)
		}
	}
}

// timedOut 返回超时未完成的任务数。
func (o *batchSearchOutcome) timedOut() int { return len(o.missing) }

// missingIDs 返回超时未完成的任务标识，供后台补齐使用。
func (o *batchSearchOutcome) missingIDs() []string { return o.missing }

// complete 表示这次批量搜索没有超时也没有失败。
func (o *batchSearchOutcome) complete() bool { return len(o.missing) == 0 && o.failed == 0 }

// cacheTTL 按完整度给出缓存有效期，第二个返回值表示是否应该写缓存。
//
// 全部失败时不写：空结果一旦缓存，会在整个 TTL 内被反复命中；
// 有任务超时只写短 TTL：让后续请求能较快重新拿到完整结果，
// 而不是被一次抖动锁住一整个缓存周期。
func (o *batchSearchOutcome) cacheTTL(fullTTL, partialTTL time.Duration) (time.Duration, bool) {
	if o.succeeded == 0 {
		return 0, false
	}
	if o.timedOut() > 0 {
		return partialTTL, true
	}
	return fullTTL, true
}

// shouldBackfill 判断是否值得后台补齐。
// 缺失比例过高（超过三分之一）说明上游整体变慢，此时补齐只会把请求量再放大一倍。
func (o *batchSearchOutcome) shouldBackfill(enabled bool) bool {
	if !enabled || o.succeeded == 0 || o.total == 0 || o.timedOut() == 0 {
		return false
	}
	return o.timedOut()*3 <= o.total
}

// logSummary 输出一行完整度摘要，便于线上判断这次是"慢"还是"坏"。
func (o *batchSearchOutcome) logSummary(source, keyword string) {
	if o.timedOut() == 0 && o.failed == 0 {
		return
	}
	fmt.Printf("[%s] %s：成功 %d/%d，失败 %d，超时未完成 %d\n",
		source, keyword, o.succeeded, o.total, o.failed, o.timedOut())
}

// mergeResults 把多组结果按顺序合并，供后台补齐复用。
func mergeResults(groups [][]model.SearchResult) []model.SearchResult {
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	merged := make([]model.SearchResult, 0, total)
	for _, g := range groups {
		merged = append(merged, g...)
	}
	return merged
}
