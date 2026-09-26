package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"pansou/config"
	"pansou/util"
)

// TG 阶段的可达性门。
//
// 背景（实测）：t.me 在被墙的网络里不是"连接被拒绝"，而是"连接被静默丢包"，于是 111 个频道请求
// 全部挂满超时——日志是 `[searchTG] 成功 0/111，失败 0，超时未完成 111`，整阶段稳定 4.00 秒，
// 而这 4 秒换来的结果恒为 0 条。前端第一次 `src=tg` 调用（设计意图是"返回快"）因此退化成最慢的
// 一条路径，用户看到的就是空等 4 秒。
//
// 三条硬约束，缺一不可：
//
//  1. **探测只在后台跑，搜索路径只读状态**。如果把探测塞进请求里，等于拿"1.5 秒探测"换
//     "4 秒空等"，收益被砍掉大半，还让每次搜索都多一次出网。结论过期时也只读旧结论（fail-open），
//     绝不阻塞。
//  2. **门开时早返回，且不写缓存**。网络不通导致的空结果若写进 60 分钟 TTL 的缓存，
//     会在网络恢复后继续骗人。
//  3. **拿不准一律按"可达"处理**。门只用来省掉必然失败的请求；任何不确定都不该让 TG 真的少跑一轮。
//
// 判定依据是"能否拿到 HTTP 响应"而不是状态码：404 同样证明网络通，只有传输层失败才算不可达。
const (
	// 单次探测的总预算。取值仍明显小于 TGChannelRequestTimeout（默认 4 秒）才有意义：
	// 实测经本地 socks5 代理访问 t.me 的握手+响应头偶尔会超过 1.5 秒，预算太紧会把
	// "慢但可用"误判成"不可达"，所以放到 3 秒。探测在后台跑，预算不影响请求时延。
	tgProbeBudget = 3 * time.Second
	// 可达结论的保鲜期：可达时没必要频繁探测。
	tgReachableTTL = 5 * time.Minute
	// 不可达结论的保鲜期：故意取短，t.me 恢复后一分钟内自动回到正常路径。
	tgUnreachableTTL = 60 * time.Second
	// 后台重探的检查间隔：只在结论过期或正在确认失败时才真正发出探测请求。
	tgProbeCheckInterval = 3 * time.Second
	// 探测用 HEAD 而不是 GET：只取响应头，不下载正文。
	// 实测经代理访问 t.me 频道页：HEAD 1.17~1.69s / 0 字节，GET 1.58~2.16s / 133,273 字节。
	// 判定语义不受影响——被墙时 HEAD 同样挂到超时（实测 5s 打满、下载 0 字节）。
	//
	// 连续失败多少次才敢关门。
	//
	// 实测教训：单次探测超时（经代理访问 t.me 时真实发生过）若直接判不可达，会把可用的 TG
	// 静默跳过——这是这个机制最危险的失效模式。因此必须连续失败到该次数才翻转，
	// 期间保持上一次结论（fail-open），并在 3 秒内尽快复检，确认过程通常几秒内结束。
	tgProbeRequiredFailures = 2
)

var (
	// fail-open：默认可达，未探测过或结论过期都按可达处理。
	tgReachable   atomic.Bool
	tgCheckedAt   atomic.Int64 // UnixNano
	tgProbeReason atomic.Value // string
	tgProbeMu     sync.Mutex
	tgProbeOnce   sync.Once
	// 日志只在"首次结论"与"结论翻转"时各打一次，避免周期性刷屏。
	tgLoggedOnce   atomic.Bool
	tgLastLoggedOK atomic.Bool
	// 连续探测失败次数：达到 tgProbeRequiredFailures 才敢把门关上。
	tgProbeFailStreak atomic.Int32
)

func init() {
	tgReachable.Store(true)
	tgProbeReason.Store("尚未探测（按可达处理）")
}

// StartTGReachabilityProbe 启动后台探测循环。重复调用无副作用。
func StartTGReachabilityProbe() {
	tgProbeOnce.Do(func() {
		log.Printf("[TG可达性] 后台探测已启动：可达结论保鲜 %s、不可达结论保鲜 %s；搜索路径只读结论不阻塞",
			tgReachableTTL, tgUnreachableTTL)
		go tgProbeLoop()
	})
}

func tgProbeLoop() {
	probeTGReachability(context.Background(), nil)

	ticker := time.NewTicker(tgProbeCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		// 正在确认失败（连击未达阈值）时尽快复检，让确认过程几秒内结束；
		// 否则按结论保鲜期决定是否重探。
		if streak := tgProbeFailStreak.Load(); streak > 0 && streak < tgProbeRequiredFailures {
			probeTGReachability(context.Background(), nil)
			continue
		}
		if tgReachabilityFresh() {
			continue
		}
		probeTGReachability(context.Background(), nil)
	}
}

// TGReachable 报告 TG 阶段当前是否值得执行。只读内存，无 IO，可在请求路径上直接调用。
func TGReachable() bool {
	return tgReachable.Load()
}

// TGReachabilitySnapshot 供 /api/health 暴露，便于一眼看出当前是不是被墙导致的空结果。
func TGReachabilitySnapshot() map[string]interface{} {
	snapshot := map[string]interface{}{
		"reachable":         tgReachable.Load(),
		"reason":            tgReason(),
		"fail_streak":       tgProbeFailStreak.Load(),
		"required_failures": tgProbeRequiredFailures,
	}
	if at := tgCheckedAt.Load(); at > 0 {
		snapshot["checked_seconds_ago"] = int(time.Since(time.Unix(0, at)).Seconds())
	} else {
		snapshot["checked_seconds_ago"] = nil
	}
	return snapshot
}

// ProbeTGReachabilityNow 立即探测一次并返回结论，供测试与需要即时判定的场景使用。
func ProbeTGReachabilityNow(client *http.Client) bool {
	probeTGReachability(context.Background(), client)
	return TGReachable()
}

func tgReason() string {
	reason, _ := tgProbeReason.Load().(string)
	return reason
}

func tgReachabilityFresh() bool {
	at := tgCheckedAt.Load()
	if at == 0 {
		return false
	}
	ttl := tgReachableTTL
	if !tgReachable.Load() || tgProbeFailStreak.Load() > 0 {
		ttl = tgUnreachableTTL
	}
	return time.Since(time.Unix(0, at)) < ttl
}

func probeTGReachability(parent context.Context, client *http.Client) {
	// 单飞：后台循环与手动触发可能同时到达，没必要并发探测同一件事。
	tgProbeMu.Lock()
	defer tgProbeMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, tgProbeBudget)
	defer cancel()

	reachable, reason := probeTGURL(ctx, client, tgProbeURL())
	tgCheckedAt.Store(time.Now().UnixNano())
	tgProbeReason.Store(reason)

	if reachable {
		tgProbeFailStreak.Store(0)
		tgReachable.Store(true)
		logTGProbeResult(true, reason)
		return
	}

	// 失败：先记连击，未达阈值就保持上一次结论（fail-open），不因单次超时关掉 TG。
	streak := tgProbeFailStreak.Add(1)
	if streak < tgProbeRequiredFailures {
		tgProbeReason.Store(fmt.Sprintf("%s（连续失败 %d/%d，暂不改判）", reason, streak, tgProbeRequiredFailures))
		log.Printf("[TG可达性] 探测失败 %d/%d，暂不改判，%s 后复检：%s",
			streak, tgProbeRequiredFailures, tgProbeCheckInterval, reason)
		return
	}
	tgReachable.Store(false)
	confirmed := fmt.Sprintf("连续 %d 次探测失败：%s", streak, reason)
	tgProbeReason.Store(confirmed)
	logTGProbeResult(false, confirmed)
}

// logTGProbeResult 首次结论与翻转各打一次日志；周期性的重复结论不打。
func logTGProbeResult(reachable bool, reason string) {
	if !tgLoggedOnce.Load() {
		tgLoggedOnce.Store(true)
		tgLastLoggedOK.Store(reachable)
		if reachable {
			log.Printf("[TG可达性] 首次探测：可达（%s）", reason)
		} else {
			log.Printf("[TG可达性] 首次探测：不可达，TG 阶段将跳过（不写缓存、不计频道失败）：%s", reason)
		}
		return
	}
	if tgLastLoggedOK.Load() == reachable {
		return
	}
	tgLastLoggedOK.Store(reachable)
	if reachable {
		log.Printf("[TG可达性] 已恢复可达：%s", reason)
		return
	}
	log.Printf("[TG可达性] 转为不可达，TG 阶段将跳过（不写缓存、不计频道失败）：%s", reason)
}

// tgProbeURL 取配置里的第一个频道作为探测目标——用真实频道页而不是站点首页，
// 因为"首页能开但 /s/ 路径被单独阻断"是可能发生的，探测必须覆盖真正要走的路径。
func tgProbeURL() string {
	channel := ""
	if config.AppConfig != nil && len(config.AppConfig.DefaultChannels) > 0 {
		channel = config.AppConfig.DefaultChannels[0]
	}
	if channel == "" {
		return "https://t.me/"
	}
	return fmt.Sprintf("https://t.me/s/%s", channel)
}

// probeTGURL 判断给定 URL 是否可达。返回 (可达, 原因)。
//
// 用 HEAD 而不是 GET：只取响应头，不下载正文。频道页正文有 133KB，探测只需要"能不能拿到响应"
// 这一个比特，没有理由把它拖下来。实测经代理 HEAD 1.17~1.69s（0 字节），GET 1.58~2.16s（133,273 字节）。
//
// 判定标准是"拿到了 HTTP 响应"，而不是状态码：404/403/405 都说明网络通，只有传输层失败
// （DNS、连接、TLS、超时）才算不可达。因此即使某个站点不支持 HEAD 而回 405，结论依然正确。
func probeTGURL(ctx context.Context, client *http.Client, target string) (bool, string) {
	if client == nil {
		client = util.GetHTTPClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return true, fmt.Sprintf("构造探测请求失败，按可达处理: %v", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PanSou-TGProbe)")

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("探测 %s 失败: %v", target, err)
	}
	// HEAD 没有正文，但仍要关闭响应体，否则连接无法复用。
	resp.Body.Close()
	return true, fmt.Sprintf("探测 %s 返回 HTTP %d", target, resp.StatusCode)
}
