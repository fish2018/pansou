package cache

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 原子写的基本性质：整体替换，且不留临时文件。
func TestWriteFileAtomicReplacesWholesale(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "entry")

	if err := writeFileAtomic(target, []byte("旧内容"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("新内容"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "新内容" {
		t.Errorf("内容未整体替换: %q", got)
	}
	assertNoTempFiles(t, dir)
}

// rename 失败时必须返回错误、保留原内容、且不留临时文件。
// 目标路径是一个非空目录，rename 到它必然失败。
func TestWriteFileAtomicKeepsOldContentOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0755); err != nil {
		t.Fatal(err)
	}

	err := writeFileAtomic(target, []byte("新内容"), 0644)
	if err == nil {
		t.Fatal("目标是被占用的目录，rename 应当失败并返回错误")
	}
	if !strings.Contains(err.Error(), "重命名") {
		t.Errorf("错误信息应指明失败环节: %v", err)
	}
	// 目录还在，没被破坏
	if _, statErr := os.Stat(filepath.Join(target, "child")); statErr != nil {
		t.Errorf("失败路径破坏了原有内容: %v", statErr)
	}
	assertNoTempFiles(t, dir)
}

// 目录不存在时应当报错而不是 panic 或静默成功。
func TestWriteFileAtomicFailsOnMissingDir(t *testing.T) {
	dir := t.TempDir()
	err := writeFileAtomic(filepath.Join(dir, "不存在", "entry"), []byte("x"), 0644)
	if err == nil {
		t.Fatal("目录不存在时应报错")
	}
	if !strings.Contains(err.Error(), "临时文件") {
		t.Errorf("错误信息应指明失败环节: %v", err)
	}
}

// Set/Delete 之后缓存目录里不该留下临时文件。
func TestDiskCacheLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, 10)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		if err := c.Set(fmt.Sprintf("键%d", i), []byte(fmt.Sprintf("值%d", i)), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if err := c.Delete(fmt.Sprintf("键%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	assertNoTempFiles(t, dir)
}

// 并发读写时，任何一次命中读到的都必须是完整值，绝不是半截。
//
// 这条对应修复前的故障形态：直接写最终路径，被中断后元数据仍有效，
// 于是 Get 会把截断的内容交给反序列化，表现为"结果残缺"而不是未命中。
func TestDiskCacheConcurrentReadSeesOnlyCompleteValues(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, 50)
	if err != nil {
		t.Fatal(err)
	}

	payloads := [][]byte{
		bytes.Repeat([]byte("A"), 200*1024),
		bytes.Repeat([]byte("B"), 200*1024),
	}
	const key = "并发读写键"

	if err := c.Set(key, payloads[0], time.Minute); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := c.Set(key, payloads[i%len(payloads)], time.Minute); err != nil {
				t.Errorf("Set 失败: %v", err)
				return
			}
		}
	}()

	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, hit, err := c.Get(key)
				if err != nil || !hit {
					continue
				}
				if !bytes.Equal(data, payloads[0]) && !bytes.Equal(data, payloads[1]) {
					t.Errorf("读到了残缺值：长度 %d，首字节 %q", len(data), data[:1])
					return
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	assertNoTempFiles(t, dir)
}

// 文件被外部删掉后 Delete 不该报错（过期清理会走到这条路径）。
func TestDiskCacheDeleteToleratesMissingFile(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set("键", []byte("值"), time.Minute); err != nil {
		t.Fatal(err)
	}

	// 模拟文件被外部删除（或重复删除）
	for _, name := range []string{c.getFilename("键"), c.getFilename("键") + ".meta"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Delete("键"); err != nil {
		t.Errorf("文件已不存在时 Delete 不该报错: %v", err)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("缓存目录里留下了临时文件: %s", e.Name())
		}
	}
}

// 上次被 kill 留下的临时文件要在启动加载时清掉：它们不计入 currSize、也不被 LRU 清理，
// 不清就是永久占着磁盘。
func TestLoadMetadataSweepsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set("键", []byte("值"), time.Minute); err != nil {
		t.Fatal(err)
	}

	// 伪造两个残留临时文件（一个数据、一个元数据）
	stale := []string{
		filepath.Join(dir, "deadbeef.tmp123456"),
		filepath.Join(dir, "deadbeef.meta.tmp654321"),
	}
	for _, p := range stale {
		if err := os.WriteFile(p, []byte("半截内容"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 重新打开（构造时会 loadMetadata）应把残留清掉
	if _, err := NewDiskCache(dir, 10); err != nil {
		t.Fatal(err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("残留临时文件未被清理: %s", filepath.Base(p))
		}
	}
	// 正常缓存项不受影响
	if _, hit, _ := c.Get("键"); !hit {
		t.Error("清理临时文件误伤了正常缓存项")
	}
}

// 磁盘写失败原先被 _ = 完全吞掉：内存里还有值，看起来一切正常，直到重启后缓存全空、
// 且没有任何线索。改为限流日志后，这里锁住"限流期间失败次数不丢"。
func TestDiskWriteFailureLogIsThrottled(t *testing.T) {
	atomic.StoreInt64(&diskWriteFailureLastLog, 0)
	atomic.StoreInt64(&diskWriteFailureSkipped, 0)

	// 第一次：打印
	logDiskWriteFailure(errors.New("磁盘写满"))
	if got := atomic.LoadInt64(&diskWriteFailureSkipped); got != 0 {
		t.Errorf("首次失败不该被跳过，实际 skipped=%d", got)
	}
	if got := atomic.LoadInt64(&diskWriteFailureLastLog); got == 0 {
		t.Error("首次失败应记录时间戳")
	}

	// 后续 3 次在限流窗口内：跳过但计数
	for i := 0; i < 3; i++ {
		logDiskWriteFailure(errors.New("磁盘写满"))
	}
	if got := atomic.LoadInt64(&diskWriteFailureSkipped); got != 3 {
		t.Errorf("限流期间应累计 3 次，实际 %d", got)
	}

	// 窗口外的失败：打印并把累计数一并报出（数量信息不丢）
	atomic.StoreInt64(&diskWriteFailureLastLog, time.Now().Add(-2*diskWriteFailureLogInterval).UnixNano())
	logDiskWriteFailure(errors.New("磁盘写满"))
	if got := atomic.LoadInt64(&diskWriteFailureSkipped); got != 0 {
		t.Errorf("窗口外打印后应清零累计，实际 %d", got)
	}
}
