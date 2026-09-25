package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"pansou/model"
)

// httpStatusError 携带状态码的请求失败。
// 用类型而不是错误字符串传递状态码，是为了按原因归类失败——
// "被限流"和"频道不存在"需要完全不同的处置。
type httpStatusError struct {
	channel string
	code    int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("频道 %s 返回状态码 %d", e.channel, e.code)
}

// failureClass 把错误归成可直接统计的类别。
func failureClass(err error) string {
	if err == nil {
		return ""
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		switch {
		case se.code == http.StatusTooManyRequests:
			return "限流429"
		case se.code == http.StatusForbidden:
			return "禁止403"
		case se.code == http.StatusNotFound:
			return "不存在404"
		case se.code >= 500:
			return "服务端5xx"
		default:
			return fmt.Sprintf("状态码%d", se.code)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "超时"
	}
	if errors.Is(err, context.Canceled) {
		return "已取消"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "网络错误"
	}
	return "其它错误"
}

// taskTiming 记录单个任务的耗时与结果，用于定位慢项。
type taskTiming struct {
	id       string
	duration time.Duration
	err      error
}

// batchSearchOutcome 记录一次批量搜索（TG 频道或插件）的完整度。
//
// "任务失败"与"任务超时未返回"必须分开统计：前者是上游的常态（频道不存在、
// 站点改版、单站限流），后者说明批截止过紧或上游整体变慢。两者的处置不同——
// 只有超时缺失才值得后台补齐，也只有它需要压低缓存有效期。
//
// 本类型不依赖全局配置，TTL 与补齐开关都由调用方传入，便于单测覆盖各分支。
type batchSearchOutcome struct {
	total       int
	succeeded   int
	failed      int
	returned    map[string]bool
	missing     []string
	timings     []taskTiming
	failClasses map[string]int
	// errSamples 保留每类失败的示例错误文本：归类（如"其它错误"）不足以定位问题，
	// 需要一条原始报错才能知道到底是超时、解析失败还是上游拒绝。
	errSamples map[string]string
	// yielded 记录本轮真正贡献了可用结果的任务数；empty 记录"成功但零产出"的任务。
	// 二者用来区分"确实没有匹配内容"与"响应超时只拿到部分空结果"。
	yielded int
	empty   []string
	// requireYield 表示"整批零产出"应视为不完整。
	// 插件路径需要置位：那里 4 秒窗口内返回空、结果靠后台补齐是常态。
	// 频道路径不置位——频道确实可能没有匹配内容，那种空结果应当正常缓存。
	requireYield bool
}

func newBatchSearchOutcome(total int) *batchSearchOutcome {
	return &batchSearchOutcome{
		total:       total,
		returned:    make(map[string]bool, total),
		failClasses: make(map[string]int),
		errSamples:  make(map[string]string),
	}
}

// observe 记录一个已返回任务的结果与耗时，err 非 nil 表示该任务失败。
func (o *batchSearchOutcome) observe(id string, err error, duration time.Duration) {
	o.returned[id] = true
	o.timings = append(o.timings, taskTiming{id: id, duration: duration, err: err})
	if err != nil {
		o.failed++
		class := failureClass(err)
		o.failClasses[class]++
		if _, seen := o.errSamples[class]; !seen {
			o.errSamples[class] = trimErrText(err.Error())
		}
		return
	}
	o.succeeded++
}

// slowestTasks 返回耗时最长的前 n 个任务，用于找出拖慢整批的少数项。
func (o *batchSearchOutcome) slowestTasks(n int) []taskTiming {
	sorted := make([]taskTiming, len(o.timings))
	copy(sorted, o.timings)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].duration > sorted[j].duration
	})
	if len(sorted) > n {
		return sorted[:n]
	}
	return sorted
}

// failureSummary 把失败原因按出现次数降序拼成一行，便于直接读日志判断症结。
func (o *batchSearchOutcome) failureSummary(limit int) string {
	if len(o.failClasses) == 0 {
		return ""
	}
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(o.failClasses))
	for k, v := range o.failClasses {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	if len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s×%d", it.k, it.v))
	}
	summary := strings.Join(parts, " ")
	// 附上最高频失败的一条原始报错，避免只看到"其它错误×N"却无从下手
	if len(items) > 0 {
		if sample := o.errSamples[items[0].k]; sample != "" {
			summary += fmt.Sprintf("（示例: %s）", sample)
		}
	}
	return summary
}

// trimErrText 压缩错误文本，只保留足够定位问题的一段。
func trimErrText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 100
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
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
	// 插件路径下整批零产出不写缓存：这类空结果多为响应超时下的部分结果，
	// 写进去（哪怕只给短 TTL）也会挡住随后由后台补齐的完整结果。
	//
	// 同时这也是原 `complete()` 的一个盲区：它只看"没有超时也没有失败"，
	// 于是"全部成功但零产出"会被判为完整、按完整 TTL 缓存一整个周期。
	if o.requireYield && o.yielded == 0 {
		return 0, false
	}
	if o.timedOut() > 0 {
		return partialTTL, true
	}
	return fullTTL, true
}

// requireYieldTracking 开启"整批零产出视为不完整"的判定，供插件路径使用。
func (o *batchSearchOutcome) requireYieldTracking() { o.requireYield = true }

// observeYield 记录某任务本轮实际贡献的可用结果条数。
//
// 没有这一步，"成功"会把两类完全不同的情况混在一起：真正有内容，以及
// 响应超时只来得及返回空壳、内容要靠后台补齐。汇总里的"成功 N/M"看着正常，
// 用户这一轮却一条都拿不到。
func (o *batchSearchOutcome) observeYield(id string, contributed int) {
	if contributed > 0 {
		o.yielded++
		return
	}
	o.empty = append(o.empty, id)
}

// shouldBackfill 判断是否值得后台补齐。
// 缺失比例过高（超过三分之一）说明上游整体变慢，此时补齐只会把请求量再放大一倍。
func (o *batchSearchOutcome) shouldBackfill(enabled bool) bool {
	if !enabled || o.succeeded == 0 || o.total == 0 || o.timedOut() == 0 {
		return false
	}
	return o.timedOut()*3 <= o.total
}

// logSummary 输出完整度摘要与慢项明细，便于判断这次是"慢"还是"坏"。
// 只在存在失败或超时未完成时输出，正常批次不产生噪音。
func (o *batchSearchOutcome) logSummary(source, keyword string) {
	if o.timedOut() == 0 && o.failed == 0 && len(o.empty) == 0 {
		return
	}
	fmt.Printf("[%s] %s：成功 %d/%d，失败 %d，超时未完成 %d",
		source, keyword, o.succeeded, o.total, o.failed, o.timedOut())
	if o.requireYield {
		fmt.Printf("，本轮零产出 %d", len(o.empty))
	}
	if summary := o.failureSummary(5); summary != "" {
		fmt.Printf("；失败原因 %s", summary)
	}
	fmt.Println()

	// 零产出明细：这些"成功"的任务这一轮没给出任何可用数据，
	// 不列出来就会被"成功 N/M"掩盖。
	if o.requireYield && len(o.empty) > 0 {
		names := o.empty
		suffix := ""
		if len(names) > 8 {
			suffix = fmt.Sprintf(" 等 %d 个", len(names))
			names = names[:8]
		}
		fmt.Printf("[%s] %s：本轮零产出（内容需靠后台补齐）: %s%s\n",
			source, keyword, strings.Join(names, " "), suffix)
	}

	// 慢项明细：这些是真正决定批截止该定多长的项。
	// 这里只报告不自动剔除——自动剔除会静默丢掉仍在产出结果的频道，
	// 与"兜底数据要完整"的目标冲突，是否裁剪应由部署方按实测决定。
	for _, tm := range o.slowestTasks(5) {
		if tm.duration < 200*time.Millisecond {
			break
		}
		state := "成功"
		if tm.err != nil {
			state = "失败(" + failureClass(tm.err) + ")"
		}
		fmt.Printf("[%s] %s：慢项 %s 耗时 %dms %s\n",
			source, keyword, tm.id, tm.duration.Milliseconds(), state)
	}
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
