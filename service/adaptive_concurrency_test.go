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

// 起始并发是并发控制里唯一"拍"出来的数，用 CPU 核数给它上界。
// 重点验证两件事：① 小机器不再开局就打满；② 8 核及以上与加此上界之前完全一致（no-op）。
func TestAdaptiveInitialLimitByCores(t *testing.T) {
	cases := []struct {
		name      string
		taskCount int
		cores     int
		want      float64
	}{
		{"71 任务 / 8 核：与旧实现一致（71/4）", 71, 8, 17.75},
		{"71 任务 / 2 核：被核数上界压到 8", 71, 2, 8},
		{"71 任务 / 1 核：下限 8 保护，不降到 4", 71, 1, 8},
		{"400 任务 / 32 核：任务数仍是主约束", 400, 32, 100},
		{"400 任务 / 2 核：被核数上界压到 8", 400, 2, 8},
		{"4 任务 / 8 核：下限 8 保护", 4, 8, 8},
		{"核数未知（0）：退回按任务数计算", 71, 0, 17.75},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adaptiveInitialLimit(tc.taskCount, tc.cores); got != tc.want {
				t.Errorf("自适应起始并发 = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

// 常见部署（核数充裕）下起始值必须与旧实现逐位一致，否则此前所有实测都要重跑。
func TestAdaptiveInitialLimitIsNoopOnWideMachines(t *testing.T) {
	for _, taskCount := range []int{20, 50, 69, 71, 111, 128} {
		legacy := float64(taskCount) / 4
		if legacy < adaptiveMinLimit {
			legacy = adaptiveMinLimit
		}
		if got := adaptiveInitialLimit(taskCount, 8); got != legacy {
			t.Errorf("任务数 %d / 8 核：起始值 %v 与旧实现 %v 不一致（有用例会因此改变）", taskCount, got, legacy)
		}
	}
}
