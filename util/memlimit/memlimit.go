// Package memlimit 让 Go 堆的软上限（GOMEMLIMIT）跟随容器内存配额。
//
// 动机来自实测：本进程在 100MB 缓存配置下，突发 20 次搜索 RSS 峰值到 212MB，而缓存本身只占
// 60MB（内存缓存 = CACHE_MAX_SIZE × 3/5）。余下部分是搜索期间的分配脉冲与 GC 尚未归还的内存，
// 也就是说 RSS 的峰值由"堆软上限"而非缓存大小决定。在 256MB 的容器里 212MB 已经贴近上限，
// 这份风险不该靠运维去猜 GOGC/GOMEMLIMIT 来兜。
//
// 因此按 Uber automemlimit 的做法：读到 cgroup 内存配额后取 90% 作为堆软上限
// （留 10% 给 goroutine 栈、运行时结构、堆外内存与 GC 自身的余量）。容器外的环境没有 cgroup
// 文件，本函数是 no-op；部署方若已显式设置 GOMEMLIMIT，则完全不干预。
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	cgroupV2Path = "/sys/fs/cgroup/memory.max"
	cgroupV1Path = "/sys/fs/cgroup/memory/memory.limit_in_bytes"

	limitNumerator   = 9
	limitDenominator = 10

	// 低于此值视为配额写错（例如把 MiB 当字节写），不设限制，避免把进程锁进 GC 抖动。
	minSaneBytes = 16 << 20
	// cgroup v1 用 9223372036854771712 表示"无限"；超过 1TiB 一律视为无限。
	maxSaneBytes = 1 << 40
)

// ApplyFromCgroup 按容器内存配额设置堆软上限。返回生效的字节数与说明；
// 字节数为 0 表示未设置，说明里给出原因（会写进启动日志，便于核对）。
func ApplyFromCgroup() (int64, string) {
	return apply([]string{cgroupV2Path, cgroupV1Path}, os.Getenv("GOMEMLIMIT"))
}

// apply 是 ApplyFromCgroup 的纯逻辑部分，把文件路径与已有环境变量作为输入，便于测试。
func apply(paths []string, envLimit string) (int64, string) {
	if strings.TrimSpace(envLimit) != "" {
		return 0, "GOMEMLIMIT 已显式设置为 " + strings.TrimSpace(envLimit) + "，不覆盖"
	}

	raw, src := readFirstLimit(paths)
	if raw == 0 {
		return 0, "未发现 cgroup 内存配额（容器外或配额为无限），保持 Go 默认"
	}
	if raw < minSaneBytes {
		return 0, "cgroup 内存配额 " + humanMB(raw) + " 过小，疑为配置错误，保持 Go 默认"
	}
	if raw > maxSaneBytes {
		return 0, "cgroup 内存配额视为无限（" + humanMB(raw) + "），保持 Go 默认"
	}

	limit := raw * limitNumerator / limitDenominator
	debug.SetMemoryLimit(limit)
	return limit, "cgroup " + src + " 配额 " + humanMB(raw) + " × 9/10 = " + humanMB(limit)
}

// readFirstLimit 返回第一个可用配额及其来源路径。0 表示没有可用配额。
// "max" 与非数字内容（旧内核的 -1 写法）都视为无限。
func readFirstLimit(paths []string) (int64, string) {
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" || s == "max" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		return n, p
	}
	return 0, ""
}

func humanMB(n int64) string {
	return strconv.FormatInt(n/(1<<20), 10) + "MB"
}
