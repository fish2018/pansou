package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"pansou/config"
	pansoujson "pansou/util/json"
)

// adaptiveConcurrency 按持续累积的观测逐步调整出口并发上限。
//
// 为什么必须是"持续调整"而不是"前期测出一个固定值"：出口容量取决于部署方的网络、
// 代理质量、各站点对同一 IP 的限流策略——每个人的设备与线路都不同，任何一次性测出的
// 数值都只对那台机器成立。控制器因此只依赖**相对信号**，天然跨环境可用：
//
//	排队信号 = 本轮每任务 p90 / 历史最好 p90（"地板值"，近似无排队时的延迟）
//	丢弃信号 = 因出口闸取不到槽位而未执行的任务数
//
// 未观察到排队与丢弃 → 加性增，继续探测容量；观察到 → 乘性减，退回安全水位。
// 地板值随观测缓慢上抬，避免线路整体变慢后被永久误判为"一直排队"。
//
// 参考 Netflix/concurrency-limits 的延迟型（Vegas/Gradient）与 AIMD 思路，
// 以及 failsafe-go 自适应限流器的 min/initial/max 三段结构与"无需失败即可发现过载"。
type adaptiveConcurrency struct {
	mu       sync.Mutex
	limit    float64
	minLimit float64
	maxLimit float64
	// floor 是观测到的最好 p90，近似"没有排队时的延迟"。
	floor time.Duration
	// win 是当前窗口内已完成任务的耗时样本。
	win []time.Duration
	// dropped 是窗口内因出口闸未取到槽位而未执行的任务数。
	dropped int
	// adjustCount 记录已调整次数，用于日志与"渐进"节流。
	adjustCount int
	persistPath string
	loaded      bool
}

const (
	// adaptiveQueueFactor 超过地板值多少倍就判定为排队。1.6 倍留出网络抖动余量：
	// 实测同插件相邻两轮 p90 的波动远小于此，而真正的排队（并发过高）会成倍拉长。
	adaptiveQueueFactor = 1.6
	// adaptiveIncreaseRatio 加性增比例：每次调整最多增加当前值的 1/8。
	adaptiveIncreaseRatio = 0.125
	// adaptiveDecreaseRatio 乘性减比例：观察到排队或丢弃时降到 85%。
	adaptiveDecreaseRatio = 0.85
	// adaptiveFloorRelax 地板值上抬速度：与当前观测值的差按 1/8 收敛。
	// 线路整体变慢时，地板必须先抬起来，否则会一直判"排队"并把并发压到底。
	adaptiveFloorRelax = 0.125
	// adaptiveMinLimit 并发下限：低于此值单轮波动会直接放大成"少拿到结果"。
	adaptiveMinLimit = 8
)

func newAdaptiveConcurrency(taskCount, ceiling int) *adaptiveConcurrency {
	if ceiling <= 0 {
		ceiling = defaultOutboundMaxConcurrency
	}
	maxLimit := float64(ceiling)
	// 初始值取任务数的四分之一：对未知部署先保守起步，再由观测逐步爬升。
	// 这也正是"逐步调整"的体现——不预设答案，只给一个安全的起点。
	initial := float64(taskCount) / 4
	if initial < adaptiveMinLimit {
		initial = adaptiveMinLimit
	}
	if initial > maxLimit {
		initial = maxLimit
	}
	if maxLimit < adaptiveMinLimit {
		maxLimit = adaptiveMinLimit
	}
	ac := &adaptiveConcurrency{
		limit:    initial,
		minLimit: adaptiveMinLimit,
		maxLimit: maxLimit,
	}
	ac.persistPath = adaptivePersistPath()
	ac.load()
	return ac
}

// adaptivePersistPath 返回落盘路径。没有缓存目录时返回空串，表示只用内存态。
func adaptivePersistPath() string {
	base := ""
	if config.AppConfig != nil {
		base = config.AppConfig.CachePath
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "adaptive_concurrency.json")
}

type adaptiveState struct {
	Limit float64 `json:"limit"`
	Floor int64   `json:"floor_ns"`
}

// load 读取上次收敛到的值，让"持续积累"跨重启保留。失败一律降级为不加载。
func (a *adaptiveConcurrency) load() {
	if a.persistPath == "" {
		return
	}
	data, err := os.ReadFile(a.persistPath)
	if err != nil {
		return
	}
	var st adaptiveState
	if err := pansoujson.Unmarshal(data, &st); err != nil {
		// 文件损坏不该影响搜索，只记一行。
		fmt.Printf("[自适应并发] 状态文件解析失败，按初始值开始: %v\n", err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st.Limit >= a.minLimit && st.Limit <= a.maxLimit {
		a.limit = st.Limit
	}
	if st.Floor > 0 {
		a.floor = time.Duration(st.Floor)
	}
	a.loaded = true
	fmt.Printf("[自适应并发] 已载入上次收敛值：并发 %.0f，地板 %v\n", a.limit, a.floor)
}

// save 落盘当前值，best-effort：失败只记一行，不影响搜索。
func (a *adaptiveConcurrency) save() {
	if a.persistPath == "" {
		return
	}
	a.mu.Lock()
	st := adaptiveState{Limit: a.limit, Floor: int64(a.floor)}
	path := a.persistPath
	a.mu.Unlock()
	data, err := pansoujson.Marshal(st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Printf("[自适应并发] 状态落盘失败: %v\n", err)
	}
}

// limit 返回当前并发上限（取整，至少 1）。
func (a *adaptiveConcurrency) limitValue() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := int(a.limit)
	if v < 1 {
		v = 1
	}
	return v
}

// observeTask 记录一个已完成任务的耗时。
func (a *adaptiveConcurrency) observeTask(d time.Duration) {
	if a == nil || d <= 0 {
		return
	}
	a.mu.Lock()
	a.win = append(a.win, d)
	a.mu.Unlock()
}

// observeDropped 记录一个因出口闸未取到槽位而未执行的任务。
func (a *adaptiveConcurrency) observeDropped() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.dropped++
	a.mu.Unlock()
}

// p90Of 返回样本的 p90（不修改窗口）。
func p90Of(s []time.Duration) time.Duration {
	if len(s) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(s))
	copy(cp, s)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(0.9 * float64(len(cp)-1))
	return cp[idx]
}

// adjust 在一个批任务结束后调用一次：依据本窗口的排队与丢弃信号调整并发上限。
// 返回是否发生了调整、以及调整前后的值，供调用方记录。
func (a *adaptiveConcurrency) adjust() (changed bool, before, after int, reason string) {
	if a == nil {
		return false, 0, 0, ""
	}
	a.mu.Lock()
	win := a.win
	a.win = nil
	dropped := a.dropped
	a.dropped = 0
	limit := a.limit
	before = int(limit)
	floor := a.floor
	a.mu.Unlock()

	if len(win) == 0 {
		return false, int(limit), int(limit), "本窗口无样本"
	}
	p90 := p90Of(win)

	// 地板值：向下立即跟随（更快的最好值就是新地板），向上缓慢收敛。
	if floor == 0 || p90 < floor {
		floor = p90
	} else if p90 > floor {
		floor = floor + time.Duration(float64(p90-floor)*adaptiveFloorRelax)
	}

	queued := floor > 0 && float64(p90) > float64(floor)*adaptiveQueueFactor
	switch {
	case dropped > 0 || queued:
		limit *= adaptiveDecreaseRatio
		reason = fmt.Sprintf("观察到排队或丢弃（本窗口 p90 %v、地板 %v、未取到槽位 %d 个）",
			p90.Round(time.Millisecond), floor.Round(time.Millisecond), dropped)
	default:
		limit += limit * adaptiveIncreaseRatio
		reason = fmt.Sprintf("无排队无丢弃（本窗口 p90 %v、地板 %v），继续探测容量",
			p90.Round(time.Millisecond), floor.Round(time.Millisecond))
	}
	if limit > a.maxLimit {
		limit = a.maxLimit
	}
	if limit < a.minLimit {
		limit = a.minLimit
	}

	a.mu.Lock()
	changed = int(limit) != int(a.limit)
	a.limit = limit
	a.floor = floor
	a.adjustCount++
	a.mu.Unlock()

	if changed {
		a.save()
	}
	return changed, before, int(limit), reason
}

// adaptiveConcurrencyFor 为本轮任务取得控制器（按出口上限构造，任务数只影响初值）。
var (
	adaptiveGlobal *adaptiveConcurrency
	adaptiveMu     sync.Mutex
	adaptiveCeil   int
)

// sharedAdaptiveConcurrency 返回进程级控制器。
//
// 必须是进程级而不是每请求一个：出口是全局共享资源，"同一个出口被多个并发请求一起压满"
// 才是真实场景；按请求独立统计会让每个请求都以为容量充足。
// 任务数变化时通过 ensureCeiling 抬高天花板而不是重建，保留已积累的收敛值。
func sharedAdaptiveConcurrency(taskCount, ceiling int) *adaptiveConcurrency {
	adaptiveMu.Lock()
	defer adaptiveMu.Unlock()
	if adaptiveGlobal == nil {
		adaptiveGlobal = newAdaptiveConcurrency(taskCount, ceiling)
		adaptiveCeil = ceiling
	} else if ceiling > adaptiveCeil {
		adaptiveGlobal.mu.Lock()
		adaptiveGlobal.maxLimit = float64(ceiling)
		adaptiveGlobal.mu.Unlock()
		adaptiveCeil = ceiling
	}
	return adaptiveGlobal
}
