package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pansou/config"
)

func newTestAdaptive(dir string, taskCount, ceiling int) *adaptiveConcurrency {
	old := config.AppConfig
	config.AppConfig = &config.Config{CachePath: dir}
	defer func() { config.AppConfig = old }()
	ac := newAdaptiveConcurrency(taskCount, ceiling)
	ac.persistPath = filepath.Join(dir, "adaptive_concurrency.json")
	return ac
}

func TestAdaptiveStartsConservativeAndGrows(t *testing.T) {
	dir := t.TempDir()
	ac := newTestAdaptive(dir, 71, 128)
	// 初始取任务数的四分之一：对未知部署先保守起步
	if got := ac.limitValue(); got != 17 {
		t.Fatalf("初始并发 = %d, 期望 17（71/4 取整）", got)
	}
	// 稳定在 2 秒（地板），无丢弃：应逐步放宽
	for i := 0; i < 12; i++ {
		for j := 0; j < 20; j++ {
			ac.observeTask(2 * time.Second)
		}
		ac.adjust()
	}
	if got := ac.limitValue(); got <= 17 {
		t.Fatalf("无排队无丢弃时应逐步放宽, 实际 %d", got)
	}
	if got := ac.limitValue(); got > 128 {
		t.Fatalf("不得超过天花板, 实际 %d", got)
	}
}

func TestAdaptiveBacksOffWhenQueued(t *testing.T) {
	dir := t.TempDir()
	ac := newTestAdaptive(dir, 71, 128)
	// 先建立地板值 1 秒
	for j := 0; j < 20; j++ {
		ac.observeTask(1 * time.Second)
	}
	ac.adjust()
	before := ac.limitValue()
	// 再出现明显排队（5 秒 = 5 倍地板）
	for j := 0; j < 20; j++ {
		ac.observeTask(5 * time.Second)
	}
	ac.adjust()
	if after := ac.limitValue(); after >= before {
		t.Fatalf("观察到排队时应退回安全水位: %d -> %d", before, after)
	}
}

func TestAdaptiveBacksOffWhenDropped(t *testing.T) {
	dir := t.TempDir()
	ac := newTestAdaptive(dir, 71, 128)
	for j := 0; j < 20; j++ {
		ac.observeTask(1 * time.Second)
	}
	ac.adjust()
	before := ac.limitValue()
	for j := 0; j < 20; j++ {
		ac.observeTask(1 * time.Second)
	}
	ac.observeDropped()
	ac.adjust()
	if after := ac.limitValue(); after >= before {
		t.Fatalf("出现未取到槽位的任务时应退回: %d -> %d", before, after)
	}
}

func TestAdaptiveFloorRelaxesWhenEnvironmentSlows(t *testing.T) {
	dir := t.TempDir()
	ac := newTestAdaptive(dir, 71, 128)
	// 线路整体变慢：一直是 4 秒且不下降
	for i := 0; i < 40; i++ {
		for j := 0; j < 20; j++ {
			ac.observeTask(4 * time.Second)
		}
		ac.adjust()
	}
	ac.mu.Lock()
	floor := ac.floor
	ac.mu.Unlock()
	// 地板必须从 4 秒抬起来（否则会被永久误判为排队并压到下限）
	if floor < 2*time.Second {
		t.Fatalf("线路变慢后地板应上抬, 实际 %v", floor)
	}
	if got := ac.limitValue(); got < adaptiveMinLimit {
		t.Fatalf("并发不得低于下限 %d, 实际 %d", adaptiveMinLimit, got)
	}
}

func TestAdaptivePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ac := newTestAdaptive(dir, 71, 128)
	for i := 0; i < 10; i++ {
		for j := 0; j < 20; j++ {
			ac.observeTask(2 * time.Second)
		}
		ac.adjust()
	}
	converged := ac.limitValue()
	if _, err := os.Stat(filepath.Join(dir, "adaptive_concurrency.json")); err != nil {
		t.Fatalf("应落盘状态文件: %v", err)
	}
	ac2 := newTestAdaptive(dir, 71, 128)
	if got := ac2.limitValue(); got != converged {
		t.Fatalf("重启后应载入上次收敛值 %d, 实际 %d", converged, got)
	}
}

func TestAdaptiveIgnoresCorruptState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "adaptive_concurrency.json"), []byte("{坏文件"), 0o644); err != nil {
		t.Fatal(err)
	}
	ac := newTestAdaptive(dir, 71, 128)
	if got := ac.limitValue(); got != 17 {
		t.Fatalf("损坏的状态文件应降级为初始值 17, 实际 %d", got)
	}
}
