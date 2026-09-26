package cache

import (
	"sync"
	"testing"
	"time"
)

// stats 的元信息字段（LastFlushTime/LastFlushTrigger/LastBatchSize/TotalOperationsWritten）
// 此前是裸读写：写来自 flushGlobalBuffer（handler goroutine 与监控 goroutine 都会调）
// 与 executeBatchWrite，读来自批处理 goroutine 的 shouldTriggerBatchWrite，
// 以及 GetStats/GetWriteManagerStats 的整结构复制。
//
// 本用例在 -race 下跑：
//
//	go test -race ./util/cache/ -run TestFlushMetaConcurrentAccess
func TestFlushMetaConcurrentAccess(t *testing.T) {
	m, err := NewDelayedBatchWriteManager()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写侧：模拟两个 flush 发起方
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.recordFlushMeta("并发触发", id)
					m.addOperationsWritten(id)
				}
			}
		}(i)
	}

	// 读侧：模拟批处理 goroutine 与统计读取
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.lastFlushTime()
					_ = m.GetStats()
					_ = m.GetWriteManagerStats()
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 元信息写入后必须能被读到，锁不能把语义改坏。
func TestFlushMetaRoundTrip(t *testing.T) {
	m, err := NewDelayedBatchWriteManager()
	if err != nil {
		t.Fatal(err)
	}

	before := m.lastFlushTime()
	time.Sleep(2 * time.Millisecond)
	m.recordFlushMeta("单测触发", 7)
	m.addOperationsWritten(7)

	if got := m.lastFlushTime(); !got.After(before) {
		t.Errorf("LastFlushTime 未被更新: before=%v got=%v", before, got)
	}
	stats := m.snapshotStats()
	if stats.LastFlushTrigger != "单测触发" {
		t.Errorf("LastFlushTrigger = %q", stats.LastFlushTrigger)
	}
	if stats.LastBatchSize != 7 {
		t.Errorf("LastBatchSize = %d", stats.LastBatchSize)
	}
	if stats.TotalOperationsWritten != 7 {
		t.Errorf("TotalOperationsWritten = %d", stats.TotalOperationsWritten)
	}
}
