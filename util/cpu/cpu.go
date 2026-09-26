// Package cpu 提供"这个进程实际能用多少 CPU"这唯一一个口径。
//
// 放在独立叶子包而不是 pansou/util：pansou/util 反向依赖 pansou/config（util/compression.go），
// 而 config 需要本函数，放在 util 会形成 config → util → config 的导入环。本包只依赖 runtime，
// config、util/cache、main 都可以安全引用。
package cpu

import "runtime"

// SchedulableCount 返回运行时真正能并行使用的 CPU 数量。
//
// 为什么用 GOMAXPROCS(0) 而不是 NumCPU()：Go 1.25 起 GOMAXPROCS 的默认值由 cgroup CPU 配额推导
// （runtime/cgroup_linux.go：ceil(quota/period)，配额小于 2 时抬到 2），因此在 --cpus=1 这类容器里
// 它会变小；而 NumCPU() 返回进程启动时向操作系统查到的宿主核数（runtime/debug.go 的 numCPUStartup），
// 读不到 cgroup 配额。
//
// 后果很实际：1 核配额的 NAS 容器里按宿主核数推导"核数 × 5 个后台工作者、核数 × 200 个连接"，
// 会凭空放大数倍；GOMAXPROCS 若高于配额还会触发 CFS 限流——automaxprocs 实测同一负载下
// RPS 从 44715 掉到 22191、P99.9 从 26.38ms 涨到 76.19ms。
//
// 非容器环境（或未显式设置 GOMAXPROCS）下两者取值完全相同，所以这个替换对常规部署是 no-op；
// 部署方显式设置 GOMAXPROCS 时按它推导也更贴合"这是给这个进程的 CPU 预算"的本意。
func SchedulableCount() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return 1
}
