package api

// 请求侧的资源上限。
//
// 审计（输入鲁棒性方向 高 3/4/5）指出三个可由匿名客户端直接触发放大的入口：
//   - POST /api/search 的请求体无上限读入内存（gin 的 GetRawData 就是 io.ReadAll）
//   - channels 只做 Split+TrimSpace，而它被当作工作池的 maxWorkers 使用
//   - /api/check/links 的 items 无数量上限，且响应体与 gzip 解压也无上限
//
// 且认证默认关闭（AUTH_ENABLED 未设置即 false），任何可达客户端都能触发。
//
// 这里取"足够宽松但能护住进程"的硬上限：正常搜索请求只有几百字节、频道数在一个
// 部署里通常几十个，所以下面的值远超合法用法，只在被滥用时生效。超限一律返回 400
// 而不是静默截断——静默截断会让调用方以为请求被完整处理了。
const (
	// maxSearchRequestBodyBytes 是 /api/search POST 请求体的读取上限。
	maxSearchRequestBodyBytes int64 = 1 << 20 // 1 MiB

	// maxRequestChannels 是单次请求允许指定的频道/插件数量上限。
	// 该值同时决定 TG 路径的工作池大小与出站请求数，必须封顶。
	maxRequestChannels = 512

	// maxCheckItems 是 /api/check/links 单次允许检测的链接数量上限。
	maxCheckItems = 256
)

// channelsOverLimit 判断请求指定的频道/插件数量是否超限。
func channelsOverLimit(n int) bool { return n > maxRequestChannels }

// itemsOverLimit 判断单次检测的链接数量是否超限。
func itemsOverLimit(n int) bool { return n > maxCheckItems }
