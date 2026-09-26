package service

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// liveness 存活观测：累积"每轮每个插件/频道到底有没有产出、有没有报错"，
// 供 /api/health 一眼看出哪些是失效的。
//
// 为什么只做观测而不是做淘汰：本轮实测显示，窗口内零产出的插件（lou1/duanjuw/zhizhen 等）
// 其请求仍然会产出内容并进入缓存，拿掉它们是丢数据而不是省开销；而每轮都报错的插件
// （erxiao/qupanshe 一类）总共只占约 1% 的出口请求，省下来不值一个会静默丢数据的机制。
// 所以这里只把事实摆出来：谁在报错、谁从未产出、谁只是偶尔失败——由部署方决定怎么处置。
const (
	livenessYield uint8 = iota
	livenessZero
	livenessFail
)

type livenessEntry struct {
	// ring 是最近的逐轮结果，容量 livenessWindow。
	// 用滑动窗口而不是累计计数：否则一个插件早期连续失败过，恢复后也会被永久标成坏，
	// 名单会越积越长、最后没人看——这与"能一眼看出当前谁失效"的目标相反。
	ring      []uint8
	LastYield time.Time
	LastErr   string
}

func (e *livenessEntry) push(code uint8) {
	if len(e.ring) >= livenessWindow {
		copy(e.ring, e.ring[1:])
		e.ring = e.ring[:len(e.ring)-1]
	}
	e.ring = append(e.ring, code)
}

// counts 返回窗口内的轮数、有产出轮、失败轮、零产出轮。
func (e *livenessEntry) counts() (rounds, yielded, failed, zero int) {
	for _, c := range e.ring {
		switch c {
		case livenessYield:
			yielded++
		case livenessFail:
			failed++
		default:
			zero++
		}
	}
	return len(e.ring), yielded, failed, zero
}

// livenessRegistry 进程级累积。必须是进程级：单次请求看不到"这个插件连续多少轮没产出"。
type livenessRegistry struct {
	mu       sync.Mutex
	plugins  map[string]*livenessEntry
	channels map[string]*livenessEntry
}

const (
	// livenessMinRounds 判定"失效"所需的最小样本轮数。太少会被首轮冷启动误判：
	// 第一轮很多插件确实什么都还没抓到。
	livenessMinRounds = 5
	// livenessWindow 滑动窗口轮数。20 轮足以判定"当前是否失效"，又短到恢复后能被冲刷掉。
	livenessWindow = 20
	// livenessTopN 每类最多返回的条目数，避免 /api/health 体积失控。
	livenessTopN = 40
	// livenessLastErrLen 最近一次错误文本的截断长度。
	livenessLastErrLen = 120
)

var (
	livenessGlobal = &livenessRegistry{
		plugins:  make(map[string]*livenessEntry),
		channels: make(map[string]*livenessEntry),
	}
)

// record 记一轮结果。yielded 为"本轮产出的可用条目数"（插件按链接数、频道按结果数）。
func (r *livenessRegistry) record(m map[string]*livenessEntry, name string, yielded int, err error) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := m[name]
	if !ok {
		e = &livenessEntry{}
		m[name] = e
	}
	switch {
	case err != nil:
		e.push(livenessFail)
		msg := trimErrText(err.Error())
		if len(msg) > livenessLastErrLen {
			msg = msg[:livenessLastErrLen]
		}
		e.LastErr = msg
	case yielded > 0:
		e.push(livenessYield)
		e.LastYield = time.Now()
		e.LastErr = ""
	default:
		e.push(livenessZero)
	}
}

// ObservePlugin 记一轮插件结果。
func ObservePlugin(name string, yielded int, err error) {
	livenessGlobal.record(livenessGlobal.plugins, name, yielded, err)
}

// ObserveChannel 记一轮频道结果。
func ObserveChannel(name string, yielded int, err error) {
	livenessGlobal.record(livenessGlobal.channels, name, yielded, err)
}

// livenessItem 是返回给调用方的单条观测。
type livenessItem struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Rounds    int    `json:"rounds"`
	Yielded   int    `json:"yielded"`
	Failed    int    `json:"failed"`
	ZeroYield int    `json:"zero_yield"`
	LastYield string `json:"last_yield,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// livenessReport 汇总，供 /api/health 使用。
type livenessReport struct {
	PluginTotal       int            `json:"plugin_total"`
	ChannelTotal      int            `json:"channel_total"`
	PluginStatus      map[string]int `json:"plugin_status"`
	ChannelStatus     map[string]int `json:"channel_status"`
	FailingPlugins    []livenessItem `json:"failing_plugins,omitempty"`
	ZeroYieldPlugins  []livenessItem `json:"zero_yield_plugins,omitempty"`
	DegradedPlugins   []livenessItem `json:"degraded_plugins,omitempty"`
	FailingChannels   []livenessItem `json:"failing_channels,omitempty"`
	ZeroYieldChannels []livenessItem `json:"zero_yield_channels,omitempty"`
	// 各分类截断前的完整数量。列表每类最多 livenessTopN 条，此前只有列表没有总数，
	// 超出部分会被静默隐藏；消费方用 count > len(items) 判断是否被截断。
	FailingPluginCount    int `json:"failing_plugin_count"`
	ZeroYieldPluginCount  int `json:"zero_yield_plugin_count"`
	DegradedPluginCount   int `json:"degraded_plugin_count"`
	FailingChannelCount   int `json:"failing_channel_count"`
	ZeroYieldChannelCount int `json:"zero_yield_channel_count"`
	// 被截断分类的文字说明，一眼看出名单不完整。
	Truncated []string `json:"truncated,omitempty"`
	Note      string   `json:"note"`
}

const livenessNote = "每轮搜索累积，滑动窗口 20 轮：failing=有报错且从未产出；zero_yield=无报错但从未产出（其内容通常经后台补齐进入缓存，不代表无数据，不要据此删除）；" +
	"degraded=有过产出但失败占比过半。窗口内样本不足 5 轮的条目不参与判定；恢复产出后旧失败会被窗口自然冲刷，不会永久留在名单里。"

// classify 依据累积计数给出状态。
func classify(e *livenessEntry) string {
	if e == nil {
		return "insufficient_data"
	}
	rounds, yielded, failed, _ := e.counts()
	if rounds < livenessMinRounds {
		return "insufficient_data"
	}
	if yielded == 0 {
		if failed > 0 {
			return "failing"
		}
		return "zero_yield"
	}
	if failed*2 > rounds {
		return "degraded"
	}
	return "ok"
}

func toItem(name string, e *livenessEntry) livenessItem {
	rounds, yielded, failed, zero := e.counts()
	it := livenessItem{
		Name:      name,
		Status:    classify(e),
		Rounds:    rounds,
		Yielded:   yielded,
		Failed:    failed,
		ZeroYield: zero,
		LastError: e.LastErr,
	}
	if !e.LastYield.IsZero() {
		it.LastYield = e.LastYield.Format("2006-01-02 15:04:05")
	}
	return it
}

// snapshot 生成报告：只列出需要关注的条目，正常的不占体积。
func (r *livenessRegistry) snapshot() livenessReport {
	rep := livenessReport{
		PluginStatus:  make(map[string]int),
		ChannelStatus: make(map[string]int),
		Note:          livenessNote,
	}
	if r == nil {
		return rep
	}
	r.mu.Lock()
	plugins := make(map[string]*livenessEntry, len(r.plugins))
	for k, v := range r.plugins {
		cp := *v
		plugins[k] = &cp
	}
	channels := make(map[string]*livenessEntry, len(r.channels))
	for k, v := range r.channels {
		cp := *v
		channels[k] = &cp
	}
	r.mu.Unlock()

	rep.PluginTotal = len(plugins)
	rep.ChannelTotal = len(channels)

	fill := func(m map[string]*livenessEntry, statusMap map[string]int) (failing, zero, degraded []livenessItem) {
		for name, e := range m {
			st := classify(e)
			statusMap[st]++
			it := toItem(name, e)
			switch st {
			case "failing":
				failing = append(failing, it)
			case "zero_yield":
				zero = append(zero, it)
			case "degraded":
				degraded = append(degraded, it)
			}
		}
		byFailures := func(s []livenessItem) []livenessItem {
			sort.Slice(s, func(i, j int) bool {
				if s[i].Failed != s[j].Failed {
					return s[i].Failed > s[j].Failed
				}
				return s[i].Name < s[j].Name
			})
			if len(s) > livenessTopN {
				return s[:livenessTopN]
			}
			return s
		}
		failing = byFailures(failing)
		zero = byFailures(zero)
		degraded = byFailures(degraded)
		return
	}

	rep.FailingPlugins, rep.ZeroYieldPlugins, rep.DegradedPlugins = fill(plugins, rep.PluginStatus)
	rep.FailingChannels, rep.ZeroYieldChannels, _ = fill(channels, rep.ChannelStatus)

	// 完整数量：状态统计表是未截断的，直接取它，不能用 len(列表)
	rep.FailingPluginCount = rep.PluginStatus["failing"]
	rep.ZeroYieldPluginCount = rep.PluginStatus["zero_yield"]
	rep.DegradedPluginCount = rep.PluginStatus["degraded"]
	rep.FailingChannelCount = rep.ChannelStatus["failing"]
	rep.ZeroYieldChannelCount = rep.ChannelStatus["zero_yield"]

	rep.Truncated = truncationNotes(map[string]int{
		"failing_plugins":     rep.FailingPluginCount - len(rep.FailingPlugins),
		"zero_yield_plugins":  rep.ZeroYieldPluginCount - len(rep.ZeroYieldPlugins),
		"degraded_plugins":    rep.DegradedPluginCount - len(rep.DegradedPlugins),
		"failing_channels":    rep.FailingChannelCount - len(rep.FailingChannels),
		"zero_yield_channels": rep.ZeroYieldChannelCount - len(rep.ZeroYieldChannels),
	})
	return rep
}

// truncationNotes 生成"某分类被截断了几条"的说明，未截断则为空。
func truncationNotes(hidden map[string]int) []string {
	var out []string
	for _, k := range []string{"failing_plugins", "zero_yield_plugins", "degraded_plugins", "failing_channels", "zero_yield_channels"} {
		if hidden[k] > 0 {
			out = append(out, fmt.Sprintf("%s 还有 %d 条未列出（每类最多 %d 条）", k, hidden[k], livenessTopN))
		}
	}
	return out
}

// LivenessSnapshot 供 API 层调用。
func LivenessSnapshot() livenessReport {
	return livenessGlobal.snapshot()
}
