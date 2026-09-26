package plugin

import (
	"time"

	"pansou/config"
)

// 异步插件的结果发布窗口。
//
// 框架只等 AsyncResponseTimeout（默认 4 秒）就把已经发布的结果返回给调用方，
// 到点还没返回的插件会被**整体丢弃**——不是少几条，而是一条都没有。实测 qqpd
// 5.27 秒、weibo 8.54 秒、gying 3.1~4.6 秒，都贴着或越过这条线，于是"测试搜索
// 有数据、正式搜索什么都没有"。
//
// 所以插件要自己卡一个发布截止：到点先把已经拿到的结果交出去，剩下的请求在后台
// 自行结束。这样部署方把窗口调大时，预算跟着放大，能真正换来更完整的结果，
// 而不是白等一批会被丢掉的数据。
const (
	// defaultAsyncResponseWindow 与 plugin 包 defaultAsyncResponseTimeout 的默认值一致。
	defaultAsyncResponseWindow = 4 * time.Second
	// publishSafetyMargin 留给"结果回传 + 框架收尾"的余量。
	publishSafetyMargin = 800 * time.Millisecond
	// minPublishBudget 下限：窗口被配得过小时也要留出最低抓取时间。
	minPublishBudget = 1800 * time.Millisecond
	// maxPublishBudget 上限：避免部署方把窗口配得极大时插件长时间占着出口。
	maxPublishBudget = 8 * time.Second
)

// PublishBudget 返回插件从**搜索开始**算起、必须交出结果的时间预算。
//
// 调用方应当在开始搜索时算一次 deadline，之后所有"还要不要发新请求"的判断都
// 以它为界；到点仍未完成的部分不要阻塞返回。
func PublishBudget() time.Duration {
	window := defaultAsyncResponseWindow
	if config.AppConfig != nil && config.AppConfig.AsyncResponseTimeoutDur > 0 {
		window = config.AppConfig.AsyncResponseTimeoutDur
	}

	budget := window - publishSafetyMargin
	if budget < minPublishBudget {
		budget = minPublishBudget
	}
	if budget > maxPublishBudget {
		budget = maxPublishBudget
	}
	return budget
}
