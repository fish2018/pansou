package service

import (
	"fmt"
	"testing"
)

var benchPluginNames = func() []string {
	n := make([]string, 71)
	for i := range n {
		n[i] = fmt.Sprintf("plugin-%d", i)
	}
	return n
}()

var benchChannelNames = func() []string {
	n := make([]string, 110)
	for i := range n {
		n[i] = fmt.Sprintf("channel-%d", i)
	}
	return n
}()

var benchErrs = func() []error {
	e := make([]error, 71)
	for i := range e {
		e[i] = errStub{}
	}
	return e
}()

// 模拟一轮搜索的存活观测开销：71 个插件 + 110 个频道。
// 名字与错误对象预先构造，避免把基准自身的 fmt 开销算进来。
func benchRound(r *livenessRegistry) {
	for i, name := range benchPluginNames {
		switch {
		case i%3 == 0:
			r.record(r.plugins, name, 5, nil)
		case i%7 == 0:
			r.record(r.plugins, name, 0, benchErrs[i])
		default:
			r.record(r.plugins, name, 0, nil)
		}
	}
	for _, name := range benchChannelNames {
		r.record(r.channels, name, 1, nil)
	}
}

func BenchmarkLivenessRecordOneRound(b *testing.B) {
	r := newTestLiveness()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchRound(r)
	}
}

func BenchmarkLivenessSnapshot(b *testing.B) {
	r := newTestLiveness()
	for i := 0; i < 30; i++ {
		benchRound(r)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.snapshot()
	}
}
