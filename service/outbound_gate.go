package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"pansou/config"
)

// errOutboundGateClosed 表示任务在等出口槽位时 ctx 已结束（通常是批截止到了）。
// 它与"插件自己失败"必须区分：它不代表站点有问题，只代表这一轮没轮到，
// 因此要计入耗时分布的老化计数，而不是计入失败分类。
var errOutboundGateClosed = errors.New("出口并发闸未取到槽位")

// 出口总闸：整个进程共用的一道并发上限，保护"同一个出口"（本地代理/带宽/对方站点限流）。
//
// 为什么需要它：插件的扇出并行度原本完全由调用方的 conc 决定，调用方给 10 就等于把 71 个
// 插件压成 7 波，每波约 4 秒（实测 30.05 秒才返回，且 70/71 个任务在截止前根本没轮到）。
// 解耦之后扇出并行度按插件数给足，但必须有另一道闸防止并发无上限地压向出口——
// 参照 SearXNG 把连接池显式配置化（outgoing.pool_connections / pool_maxsize），
// 以及 Hystrix 按依赖做舱壁隔离的思路。
//
// 注意：容量必须在**调用时**解析，不能放在包 init 里——插件与服务的构造早于 config.Init，
// 那时读到的是 nil 配置，容量会被固定成硬编码默认值且再也补不回来（同样的坑在
// plugin.ensureBackgroundWorkerPool 上踩过一次）。
var (
	outboundGateOnce sync.Once
	outboundGate     chan struct{}
)

const defaultOutboundMaxConcurrency = 128

func ensureOutboundGate() chan struct{} {
	outboundGateOnce.Do(func() {
		limit := defaultOutboundMaxConcurrency
		if config.AppConfig != nil && config.AppConfig.OutboundMaxConcurrency > 0 {
			limit = config.AppConfig.OutboundMaxConcurrency
		}
		outboundGate = make(chan struct{}, limit)
	})
	return outboundGate
}

// acquireOutbound 取一个出口槽位，ctx 结束（含批截止）时放弃等待。
// 返回 false 表示没取到，调用方应立即返回而不是继续发请求。
func acquireOutbound(ctx context.Context) bool {
	// 先查一次 ctx：select 在"槽位可取"与"ctx 已结束"同时为真时是随机选，
	// 已取消的请求仍可能拿到槽位。先判一次让"截止到了就别再占用出口"成为确定行为。
	select {
	case <-ctx.Done():
		return false
	default:
	}
	gate := ensureOutboundGate()
	select {
	case gate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func releaseOutbound() {
	gate := ensureOutboundGate()
	select {
	case <-gate:
	default:
	}
}

// effectiveFanoutConcurrency 计算插件扇出实际使用的并行度。
//
// 规则：取"调用方并行度"与"任务数"的较大者，再用出口上限收口。
// 单纯跟随调用方的 conc 会让插件路径被一个偏小的客户端参数掐死（实测 conc=10 时
// 70/71 个任务等不到结果），而完全无视调用方又会否认它显式表达的资源意图——
// 所以在调用方给得少时补足到任务数，在调用方给得多时以出口上限为准。
func effectiveFanoutConcurrency(requestConc, taskCount, outboundLimit int) int {
	if taskCount <= 0 {
		if requestConc > 0 {
			return requestConc
		}
		return 1
	}
	effective := requestConc
	if effective < taskCount {
		effective = taskCount
	}
	if outboundLimit <= 0 {
		outboundLimit = defaultOutboundMaxConcurrency
	}
	if effective > outboundLimit {
		effective = outboundLimit
	}
	if effective < 1 {
		effective = 1
	}
	return effective
}

// deriveBatchDeadline 由波次推导批任务软截止，取代"拍一个固定秒数"。
//
//	波次 = ceil(任务数 / 有效并发)
//	截止 = 波次 × 每任务 p90 + 余量
//
// 依据：本轮实测每波约 4 秒——conc=10 时 71/10≈7.1 波≈28.4 秒（实测 30.05 秒）；
// conc=71 时 1 波 4.05 秒。因此"任务数与并发度"才是截止的决定因素，
// 而它同时也是自适应并发（阶段 4）要调的同一个量，两者不该互相追尾。
//
// 优先级：显式覆盖（PLUGIN_BATCH_TIMEOUT_SECONDS）> 本公式 > 上限（PLUGIN_TIMEOUT）。
// 显式覆盖必须最高，否则部署方设的值会被公式悄悄吃掉。
func deriveBatchDeadline(taskCount, effectiveConc int, perTaskP90, override, cap time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if taskCount <= 0 {
		taskCount = 1
	}
	if effectiveConc <= 0 {
		effectiveConc = 1
	}
	if perTaskP90 <= 0 {
		// 无历史样本时退回到异步窗口长度：单插件单独跑的最慢值实测 4.04 秒，
		// 正好等于 4 秒窗口，是比"拍 10 秒"更贴近实际的初值。
		perTaskP90 = defaultPerTaskBudget
	}
	waves := (taskCount + effectiveConc - 1) / effectiveConc
	deadline := time.Duration(waves)*perTaskP90 + batchDeadlineMargin
	if cap > 0 && deadline > cap {
		deadline = cap
	}
	if deadline < minBatchDeadline {
		deadline = minBatchDeadline
	}
	return deadline
}

const (
	// batchDeadlineMargin 波次推导之外的余量，覆盖调度与收尾开销。
	// 实测固定开销约 0.2 秒（8.19 对 8.00、10.22 对 10.00、30.20 对 30.00），取 1 秒留足。
	batchDeadlineMargin = 1 * time.Second
	// minBatchDeadline 下限：即便公式算出很小，也要留住 4 秒异步窗口的完整长度。
	minBatchDeadline = 4 * time.Second
	// defaultPerTaskBudget 无历史样本时的每任务耗时假设，取异步窗口长度。
	defaultPerTaskBudget = 4 * time.Second
)
