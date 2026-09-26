package cache

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pansou/model"
	"pansou/util/cpu"
)

// CacheWriteStrategy 缓存写入策略
type CacheWriteStrategy string

const (
	// CacheStrategyImmediate 立即写入策略（当前实现）
	CacheStrategyImmediate CacheWriteStrategy = "immediate"

	// CacheStrategyHybrid 混合智能策略（推荐）
	CacheStrategyHybrid CacheWriteStrategy = "hybrid"
)

// CacheOperation 缓存操作
type CacheOperation struct {
	Key        string
	Data       []model.SearchResult
	TTL        time.Duration
	PluginName string
	Keyword    string
	Timestamp  time.Time
	Priority   int  // 优先级 (1=highest, 4=lowest)
	DataSize   int  // 数据大小（字节）
	IsFinal    bool // 是否为最终结果
}

// CacheWriteConfig 缓存写入配置
type CacheWriteConfig struct {
	// 核心策略
	Strategy CacheWriteStrategy `env:"CACHE_WRITE_STRATEGY" default:"hybrid"`

	// 批量写入参数（自动计算，但可手动覆盖）
	MaxBatchInterval time.Duration `env:"BATCH_MAX_INTERVAL"`  // 0表示自动计算
	MaxBatchSize     int           `env:"BATCH_MAX_SIZE"`      // 0表示自动计算
	MaxBatchDataSize int           `env:"BATCH_MAX_DATA_SIZE"` // 0表示自动计算

	// 行为参数
	HighPriorityRatio float64 `env:"HIGH_PRIORITY_RATIO" default:"0.3"`
	EnableCompression bool    // 默认启用操作合并

	// 内部计算参数（运行时动态调整）
	idleThresholdCPU   float64       // CPU空闲阈值
	idleThresholdDisk  float64       // 磁盘空闲阈值
	forceFlushInterval time.Duration // 强制刷新间隔
	autoTuneInterval   time.Duration // 调优检查间隔

	// 约束边界（硬编码）
	minBatchInterval time.Duration // 最小30秒
	maxBatchInterval time.Duration // 最大10分钟
	minBatchSize     int           // 最小10个
	maxBatchSize     int           // 最大1000个
}

// Initialize 初始化配置
func (c *CacheWriteConfig) Initialize() error {
	// 设置硬编码约束边界
	c.minBatchInterval = 30 * time.Second
	c.maxBatchInterval = 600 * time.Second // 10分钟
	c.minBatchSize = 10
	c.maxBatchSize = 1000

	// 加载环境变量
	c.loadFromEnvironment()

	// 自动计算最优参数（除非手动设置）
	if c.MaxBatchInterval == 0 {
		c.MaxBatchInterval = c.calculateOptimalBatchInterval()
	}
	if c.MaxBatchSize == 0 {
		c.MaxBatchSize = c.calculateOptimalBatchSize()
	}
	if c.MaxBatchDataSize == 0 {
		c.MaxBatchDataSize = c.calculateOptimalDataSize()
	}

	// 内部参数自动设置
	c.forceFlushInterval = c.MaxBatchInterval * 5 // 5倍批量间隔
	c.autoTuneInterval = 300 * time.Second        // 5分钟调优间隔
	c.idleThresholdCPU = 0.3                      // CPU空闲阈值
	c.idleThresholdDisk = 0.5                     // 磁盘空闲阈值

	// 参数验证和约束
	return c.validateAndConstraint()
}

// loadFromEnvironment 从环境变量加载配置
func (c *CacheWriteConfig) loadFromEnvironment() {
	// 策略配置
	if strategy := os.Getenv("CACHE_WRITE_STRATEGY"); strategy != "" {
		c.Strategy = CacheWriteStrategy(strategy)
	}

	// 批量写入参数
	if interval := os.Getenv("BATCH_MAX_INTERVAL"); interval != "" {
		if d, err := time.ParseDuration(interval); err == nil {
			c.MaxBatchInterval = d
		}
	}

	if size := os.Getenv("BATCH_MAX_SIZE"); size != "" {
		if s, err := strconv.Atoi(size); err == nil {
			c.MaxBatchSize = s
		}
	}

	if dataSize := os.Getenv("BATCH_MAX_DATA_SIZE"); dataSize != "" {
		if ds, err := strconv.Atoi(dataSize); err == nil {
			c.MaxBatchDataSize = ds
		}
	}

	// 行为参数
	if ratio := os.Getenv("HIGH_PRIORITY_RATIO"); ratio != "" {
		if r, err := strconv.ParseFloat(ratio, 64); err == nil {
			c.HighPriorityRatio = r
		}
	}
}

// calculateOptimalBatchInterval 计算最优批量间隔
func (c *CacheWriteConfig) calculateOptimalBatchInterval() time.Duration {
	// 基于系统性能动态计算
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// 简化实现：根据可用内存量调整
	availableMemoryGB := float64(memStats.Sys) / 1024 / 1024 / 1024

	var interval time.Duration
	switch {
	case availableMemoryGB > 8: // 大内存系统
		interval = 45 * time.Second
	case availableMemoryGB > 4: // 中等内存系统
		interval = 60 * time.Second
	default: // 小内存系统
		interval = 90 * time.Second
	}

	// 应用约束
	if interval < c.minBatchInterval {
		interval = c.minBatchInterval
	}
	if interval > c.maxBatchInterval {
		interval = c.maxBatchInterval
	}

	return interval
}

// calculateOptimalBatchSize 计算最优批量大小
func (c *CacheWriteConfig) calculateOptimalBatchSize() int {
	// 基于CPU核心数和内存动态计算
	numCPU := cpu.SchedulableCount()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	availableMemoryGB := float64(memStats.Sys) / 1024 / 1024 / 1024

	var size int
	switch {
	case numCPU >= 8 && availableMemoryGB > 8: // 高性能系统
		size = 200
	case numCPU >= 4 && availableMemoryGB > 4: // 中等性能系统
		size = 100
	default: // 低性能系统
		size = 50
	}

	// 应用约束
	if size < c.minBatchSize {
		size = c.minBatchSize
	}
	if size > c.maxBatchSize {
		size = c.maxBatchSize
	}

	return size
}

// calculateOptimalDataSize 计算最优数据大小
func (c *CacheWriteConfig) calculateOptimalDataSize() int {
	// 基于可用内存计算
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	availableMemoryGB := float64(memStats.Sys) / 1024 / 1024 / 1024

	var sizeMB int
	switch {
	case availableMemoryGB > 16: // 大内存系统
		sizeMB = 20
	case availableMemoryGB > 8: // 中等内存系统
		sizeMB = 10
	default: // 小内存系统
		sizeMB = 5
	}

	return sizeMB * 1024 * 1024 // 转换为字节
}

// validateAndConstraint 验证和约束配置
func (c *CacheWriteConfig) validateAndConstraint() error {
	// 验证配置合理性
	if c.MaxBatchInterval < c.minBatchInterval {
		return fmt.Errorf("批量间隔配置错误: MaxBatchInterval(%v) < MinBatchInterval(%v)",
			c.MaxBatchInterval, c.minBatchInterval)
	}

	if c.MaxBatchSize < c.minBatchSize {
		return fmt.Errorf("批量大小配置错误: MaxBatchSize(%d) < MinBatchSize(%d)",
			c.MaxBatchSize, c.minBatchSize)
	}

	if c.HighPriorityRatio < 0 || c.HighPriorityRatio > 1 {
		return fmt.Errorf("高优先级比例配置错误: HighPriorityRatio(%f) 应在 [0,1] 范围内",
			c.HighPriorityRatio)
	}

	// 应用最终约束
	if c.MaxBatchInterval > c.maxBatchInterval {
		c.MaxBatchInterval = c.maxBatchInterval
	}
	if c.MaxBatchSize > c.maxBatchSize {
		c.MaxBatchSize = c.maxBatchSize
	}

	// 设置默认策略
	if c.Strategy != CacheStrategyImmediate && c.Strategy != CacheStrategyHybrid {
		c.Strategy = CacheStrategyHybrid
	}

	return nil
}

// DelayedBatchWriteManager 延迟批量写入管理器
type DelayedBatchWriteManager struct {
	strategy CacheWriteStrategy
	config   *CacheWriteConfig

	// 延迟写入队列
	writeQueue  chan *CacheOperation
	queueBuffer []*CacheOperation
	queueMutex  sync.Mutex

	// flushMetaMutex 保护 stats 里的元信息字段（时间/触发原因/批量大小/已写操作数）。
	// 计数器用 atomic 是对的，但这几个是 time.Time/string/int，无法直接用 atomic，
	// 此前是裸读写：写来自 flushGlobalBuffer（handler goroutine 与监控 goroutine 都会
	// 调）与 executeBatchWrite，读来自批处理 goroutine 的 shouldTriggerBatchWrite。
	// 用专用锁收口，不与 queueMutex 混用（避免把 flush 路径也串行化）。
	flushMetaMutex sync.RWMutex

	// 全局缓冲区管理器
	globalBufferManager *GlobalBufferManager

	// 统计信息
	stats *WriteManagerStats

	// 控制通道
	shutdownChan chan struct{}
	flushTicker  *time.Ticker

	// 数据压缩（操作合并）
	operationMap map[string]*CacheOperation // key -> latest operation (去重合并)
	mapMutex     sync.RWMutex

	// 主缓存更新函数
	mainCacheUpdater func(string, []byte, time.Duration) error

	// 序列化器
	serializer *GobSerializer

	// 初始化标志
	initialized int32
	initMutex   sync.Mutex
}

// WriteManagerStats 写入管理器统计信息
type WriteManagerStats struct {
	// 基础统计
	TotalWrites      int64 // 总写入次数
	TotalOperations  int64 // 总操作次数
	BatchWrites      int64 // 批量写入次数
	ImmediateWrites  int64 // 立即写入次数
	MergedOperations int64 // 合并操作次数
	FailedWrites     int64 // 失败写入次数
	SuccessfulWrites int64 // 成功写入次数

	// 性能统计
	LastFlushTime          time.Time // 上次刷新时间
	LastFlushTrigger       string    // 上次刷新触发原因
	LastBatchSize          int       // 上次批量大小
	TotalOperationsWritten int       // 已写入操作总数

	// 时间窗口
	WindowStart time.Time // 统计窗口开始时间
	WindowEnd   time.Time // 统计窗口结束时间

	// 运行时状态
	CurrentQueueSize   int32   // 当前队列大小
	CurrentMemoryUsage int64   // 当前内存使用量
	SystemLoadAverage  float64 // 系统负载均值
}

// NewDelayedBatchWriteManager 创建新的延迟批量写入管理器
func NewDelayedBatchWriteManager() (*DelayedBatchWriteManager, error) {
	config := &CacheWriteConfig{
		Strategy:          CacheStrategyHybrid,
		EnableCompression: true,
	}

	// 初始化配置
	if err := config.Initialize(); err != nil {
		return nil, fmt.Errorf("配置初始化失败: %v", err)
	}

	// 创建全局缓冲区管理器
	globalBufferManager := NewGlobalBufferManager(BufferHybrid)

	manager := &DelayedBatchWriteManager{
		strategy:            config.Strategy,
		config:              config,
		writeQueue:          make(chan *CacheOperation, 1000), // 队列容量1000
		queueBuffer:         make([]*CacheOperation, 0, config.MaxBatchSize),
		globalBufferManager: globalBufferManager,
		operationMap:        make(map[string]*CacheOperation),
		shutdownChan:        make(chan struct{}),
		stats: &WriteManagerStats{
			WindowStart: time.Now(),
		},
		serializer: NewGobSerializer(),
	}

	return manager, nil
}

// Initialize 初始化管理器
func (m *DelayedBatchWriteManager) Initialize() error {
	if !atomic.CompareAndSwapInt32(&m.initialized, 0, 1) {
		return nil // 已经初始化
	}

	m.initMutex.Lock()
	defer m.initMutex.Unlock()

	// 初始化全局缓冲区管理器
	if err := m.globalBufferManager.Initialize(); err != nil {
		return fmt.Errorf("全局缓冲区管理器初始化失败: %v", err)
	}

	// 启动后台处理goroutine
	go m.backgroundProcessor()

	// 启动定时刷新goroutine
	m.flushTicker = time.NewTicker(m.config.MaxBatchInterval)
	go m.timerFlushProcessor()

	// 启动自动调优goroutine
	go m.autoTuningProcessor()

	// 启动全局缓冲区监控
	go m.globalBufferMonitor()

	fmt.Printf("缓存写入策略: %s\n", m.strategy)
	return nil
}

// SetMainCacheUpdater 设置主缓存更新函数
func (m *DelayedBatchWriteManager) SetMainCacheUpdater(updater func(string, []byte, time.Duration) error) {
	m.mainCacheUpdater = updater
}

// HandleCacheOperation 处理缓存操作
func (m *DelayedBatchWriteManager) HandleCacheOperation(op *CacheOperation) error {
	// 确保管理器已初始化
	if err := m.Initialize(); err != nil {
		return err
	}

	// 关键：无论什么策略，都立即更新内存缓存
	if err := m.updateMemoryCache(op); err != nil {
		return fmt.Errorf("内存缓存更新失败: %v", err)
	}

	// 根据策略处理磁盘写入
	if m.strategy == CacheStrategyImmediate {
		return m.immediateWriteToDisk(op)
	}

	// 使用全局缓冲区管理器进行智能缓冲
	return m.handleWithGlobalBuffer(op)
}

// handleWithGlobalBuffer 使用全局缓冲区处理操作
func (m *DelayedBatchWriteManager) handleWithGlobalBuffer(op *CacheOperation) error {
	// 尝试添加到全局缓冲区
	buffer, shouldFlush, err := m.globalBufferManager.AddOperation(op)
	if err != nil {
		// 全局缓冲区失败，降级到本地队列
		return m.enqueueForBatchWrite(op)
	}

	// 如果需要刷新缓冲区
	if shouldFlush {
		return m.flushGlobalBuffer(buffer.ID)
	}

	return nil
}

// flushGlobalBuffer 刷新全局缓冲区
func (m *DelayedBatchWriteManager) flushGlobalBuffer(bufferID string) error {
	operations, err := m.globalBufferManager.FlushBuffer(bufferID)
	if err != nil {
		return fmt.Errorf("刷新全局缓冲区失败: %v", err)
	}

	if len(operations) == 0 {
		return nil
	}

	// 按优先级排序操作
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Priority != operations[j].Priority {
			return operations[i].Priority < operations[j].Priority
		}
		return operations[i].Timestamp.Before(operations[j].Timestamp)
	})

	// 统计信息更新
	atomic.AddInt64(&m.stats.BatchWrites, 1)
	atomic.AddInt64(&m.stats.TotalWrites, 1)
	m.recordFlushMeta("全局缓冲区触发", len(operations))

	// 批量写入磁盘
	err = m.batchWriteToDisk(operations)
	if err != nil {
		atomic.AddInt64(&m.stats.FailedWrites, 1)
		return fmt.Errorf("全局缓冲区批量写入失败: %v", err)
	}

	// 📈 成功统计
	atomic.AddInt64(&m.stats.SuccessfulWrites, 1)
	m.addOperationsWritten(len(operations))

	return nil
}

// globalBufferMonitor 全局缓冲区监控
func (m *DelayedBatchWriteManager) globalBufferMonitor() {
	ticker := time.NewTicker(2 * time.Minute) // 每2分钟检查一次
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 检查是否有过期的缓冲区需要刷新
			m.checkAndFlushExpiredBuffers()

		case <-m.shutdownChan:
			return
		}
	}
}

// checkAndFlushExpiredBuffers 检查并刷新过期缓冲区
func (m *DelayedBatchWriteManager) checkAndFlushExpiredBuffers() {
	// 使用原子操作获取需要刷新的缓冲区列表
	expiredBuffers := m.globalBufferManager.GetExpiredBuffersForFlush()

	flushedCount := 0
	for _, bufferID := range expiredBuffers {
		if err := m.flushGlobalBuffer(bufferID); err != nil {
			// 区分错误类型，缓冲区不存在是正常情况
			if isBufferNotExistError(err) {
				// 静默处理：缓冲区已被其他线程清理，这是正常的
				continue
			}
			// 只有真正的错误才打印警告
			fmt.Printf("[全局缓冲区] 刷新缓冲区失败 %s: %v\n", bufferID, err)
		} else {
			flushedCount++
		}
	}

	if flushedCount > 0 {
		fmt.Printf("[全局缓冲区] 刷新完成，处理 %d 个过期缓冲区\n", flushedCount)
	}
}

// isBufferNotExistError 检查是否为缓冲区不存在错误
func isBufferNotExistError(err error) bool {
	return err != nil && (err.Error() == "缓冲区不存在: "+err.Error()[strings.LastIndex(err.Error(), ": ")+2:] ||
		strings.Contains(err.Error(), "缓冲区不存在"))
}

// updateMemoryCache 更新内存缓存（立即执行）
func (m *DelayedBatchWriteManager) updateMemoryCache(op *CacheOperation) error {
	// 如果有主缓存更新函数，立即更新内存层
	if m.mainCacheUpdater != nil {
		// 序列化数据
		_, err := m.serializer.Serialize(op.Data)
		if err != nil {
			return fmt.Errorf("内存缓存数据序列化失败: %v", err)
		}

		// 这里只更新内存，不写磁盘（磁盘由批量写入处理）
		// 注意：mainCacheUpdater实际上是SetBothLevels，会同时更新内存和磁盘
	}
	return nil
}

// immediateWriteToDisk 立即写入磁盘
func (m *DelayedBatchWriteManager) immediateWriteToDisk(op *CacheOperation) error {
	if m.mainCacheUpdater == nil {
		return fmt.Errorf("主缓存更新函数未设置")
	}

	// 序列化数据
	data, err := m.serializer.Serialize(op.Data)
	if err != nil {
		return fmt.Errorf("数据序列化失败: %v", err)
	}

	// 更新统计
	atomic.AddInt64(&m.stats.TotalWrites, 1)
	atomic.AddInt64(&m.stats.TotalOperations, 1)
	atomic.AddInt64(&m.stats.ImmediateWrites, 1)

	return m.mainCacheUpdater(op.Key, data, op.TTL)
}

// enqueueForBatchWrite 加入批量写入队列
func (m *DelayedBatchWriteManager) enqueueForBatchWrite(op *CacheOperation) error {
	// 🚀 操作合并优化：相同key的操作只保留最新的
	if m.config.EnableCompression {
		m.mapMutex.Lock()
		existing, exists := m.operationMap[op.Key]
		if exists {
			// 合并操作：保留最新数据，累计统计信息
			op.DataSize += existing.DataSize
			atomic.AddInt64(&m.stats.MergedOperations, 1)
		}
		m.operationMap[op.Key] = op
		m.mapMutex.Unlock()
	}

	// 加入延迟写入队列
	select {
	case m.writeQueue <- op:
		atomic.AddInt64(&m.stats.TotalOperations, 1)
		atomic.AddInt32(&m.stats.CurrentQueueSize, 1)
		return nil
	default:
		// 队列满时，触发紧急刷新
		return m.emergencyFlush()
	}
}

// backgroundProcessor 后台处理器
func (m *DelayedBatchWriteManager) backgroundProcessor() {
	for {
		select {
		case op := <-m.writeQueue:
			m.queueMutex.Lock()
			m.queueBuffer = append(m.queueBuffer, op)
			atomic.AddInt32(&m.stats.CurrentQueueSize, -1)

			// 检查是否应该触发批量写入
			if shouldFlush, trigger := m.shouldTriggerBatchWrite(); shouldFlush {
				m.executeBatchWrite(trigger)
			}
			m.queueMutex.Unlock()

		case <-m.shutdownChan:
			// 优雅关闭：处理剩余操作
			m.flushAllPendingData()
			return
		}
	}
}

// timerFlushProcessor 定时刷新处理器
func (m *DelayedBatchWriteManager) timerFlushProcessor() {
	for {
		select {
		case <-m.flushTicker.C:
			m.queueMutex.Lock()
			if len(m.queueBuffer) > 0 {
				m.executeBatchWrite("定时触发")
			}
			m.queueMutex.Unlock()

		case <-m.shutdownChan:
			m.flushTicker.Stop()
			return
		}
	}
}

// autoTuningProcessor 自动调优处理器
func (m *DelayedBatchWriteManager) autoTuningProcessor() {
	ticker := time.NewTicker(m.config.autoTuneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.autoTuneParameters()

		case <-m.shutdownChan:
			return
		}
	}
}

// Shutdown 优雅关闭
func (m *DelayedBatchWriteManager) Shutdown(timeout time.Duration) error {
	if !atomic.CompareAndSwapInt32(&m.initialized, 1, 0) {
		return nil // 已经关闭
	}

	// 正在保存缓存数据（静默）

	// 关闭后台处理器
	close(m.shutdownChan)

	// 等待所有数据保存完成，但有超时保护
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		var lastErr error

		// 第一步：强制刷新全局缓冲区（优先级最高）
		if err := m.flushAllGlobalBuffers(); err != nil {
			fmt.Printf("[数据保护] 全局缓冲区刷新失败: %v\n", err)
			lastErr = err
		}

		// 第二步：刷新本地队列
		if err := m.flushAllPendingData(); err != nil {
			fmt.Printf("[数据保护] 本地队列刷新失败: %v\n", err)
			lastErr = err
		}

		// 第三步：关闭全局缓冲区管理器
		if err := m.globalBufferManager.Shutdown(); err != nil {
			fmt.Printf("[数据保护] 全局缓冲区管理器关闭失败: %v\n", err)
			lastErr = err
		}

		done <- lastErr
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("数据保存失败: %v", err)
		}
		// 缓存数据已安全保存（静默）
		return nil
	case <-ctx.Done():
		return fmt.Errorf("数据保存超时")
	}
}

// flushAllGlobalBuffers 刷新所有全局缓冲区
func (m *DelayedBatchWriteManager) flushAllGlobalBuffers() error {
	allBuffers := m.globalBufferManager.FlushAllBuffers()

	var lastErr error

	for bufferID, operations := range allBuffers {
		if len(operations) > 0 {
			if err := m.batchWriteToDisk(operations); err != nil {
				fmt.Printf("[全局缓冲区] 缓冲区 %s 刷新失败: %v\n", bufferID, err)
				lastErr = fmt.Errorf("刷新全局缓冲区 %s 失败: %v", bufferID, err)
				continue
			}
		}
	}

	return lastErr
}

// flushAllPendingData 刷新所有待处理数据
func (m *DelayedBatchWriteManager) flushAllPendingData() error {
	m.queueMutex.Lock()
	defer m.queueMutex.Unlock()

	// 处理队列缓冲区中的数据
	if len(m.queueBuffer) > 0 {
		if err := m.executeBatchWrite("程序关闭"); err != nil {
			return err
		}
	}

	// 处理操作映射中的数据（如果启用了压缩）
	if m.config.EnableCompression && len(m.operationMap) > 0 {
		operations := m.getCompressedOperations()
		if len(operations) > 0 {
			return m.batchWriteToDisk(operations)
		}
	}

	return nil
}

// shouldTriggerBatchWrite 检查是否应该触发批量写入
func (m *DelayedBatchWriteManager) shouldTriggerBatchWrite() (bool, string) {
	now := time.Now()

	// 条件1：时间间隔达到阈值
	lastFlush := m.lastFlushTime()
	if now.Sub(lastFlush) >= m.config.MaxBatchInterval {
		return true, "时间间隔触发"
	}

	// 条件2：操作数量达到阈值
	if len(m.queueBuffer) >= m.config.MaxBatchSize {
		return true, "数量阈值触发"
	}

	// 条件3：数据大小达到阈值
	totalSize := m.calculateBufferSize()
	if totalSize >= m.config.MaxBatchDataSize {
		return true, "大小阈值触发"
	}

	// 条件4：高优先级数据比例达到阈值
	highPriorityRatio := m.calculateHighPriorityRatio()
	if highPriorityRatio >= m.config.HighPriorityRatio {
		return true, "高优先级触发"
	}

	// 条件5：系统空闲（CPU和磁盘使用率都较低）
	if m.isSystemIdle() {
		return true, "系统空闲触发"
	}

	// 条件6：强制刷新间隔（兜底机制）
	if now.Sub(m.lastFlushTime()) >= m.config.forceFlushInterval {
		return true, "强制刷新触发"
	}

	return false, ""
}

// calculateBufferSize 计算缓冲区数据大小
func (m *DelayedBatchWriteManager) calculateBufferSize() int {
	totalSize := 0
	for _, op := range m.queueBuffer {
		totalSize += op.DataSize
	}
	return totalSize
}

// calculateHighPriorityRatio 计算高优先级数据比例
func (m *DelayedBatchWriteManager) calculateHighPriorityRatio() float64 {
	if len(m.queueBuffer) == 0 {
		return 0
	}

	highPriorityCount := 0
	for _, op := range m.queueBuffer {
		if op.Priority <= 2 { // 等级1和等级2插件
			highPriorityCount++
		}
	}

	return float64(highPriorityCount) / float64(len(m.queueBuffer))
}

// isSystemIdle 检查系统是否空闲
func (m *DelayedBatchWriteManager) isSystemIdle() bool {
	// 简化实现：基于CPU使用率
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// 如果GC频率较低，认为系统相对空闲
	return memStats.NumGC%10 == 0
}

// executeBatchWrite 执行批量写入
func (m *DelayedBatchWriteManager) executeBatchWrite(trigger string) error {
	if len(m.queueBuffer) == 0 {
		return nil
	}

	// 操作合并：如果启用压缩，使用合并后的操作
	var operations []*CacheOperation
	if m.config.EnableCompression {
		operations = m.getCompressedOperations()
	} else {
		operations = make([]*CacheOperation, len(m.queueBuffer))
		copy(operations, m.queueBuffer)
	}

	if len(operations) == 0 {
		return nil
	}

	// 按优先级排序：确保重要数据优先写入
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Priority != operations[j].Priority {
			return operations[i].Priority < operations[j].Priority // 数字越小优先级越高
		}
		return operations[i].Timestamp.Before(operations[j].Timestamp)
	})

	// 统计信息更新
	atomic.AddInt64(&m.stats.BatchWrites, 1)
	m.recordFlushMeta(trigger, len(operations))

	// 批量写入磁盘
	err := m.batchWriteToDisk(operations)
	if err != nil {
		atomic.AddInt64(&m.stats.FailedWrites, 1)
		return fmt.Errorf("批量写入失败: %v", err)
	}

	// 清空缓冲区
	m.queueBuffer = m.queueBuffer[:0]
	if m.config.EnableCompression {
		m.mapMutex.Lock()
		m.operationMap = make(map[string]*CacheOperation)
		m.mapMutex.Unlock()
	}

	// 成功统计
	atomic.AddInt64(&m.stats.SuccessfulWrites, 1)
	atomic.AddInt64(&m.stats.TotalWrites, 1)
	m.addOperationsWritten(len(operations))

	return nil
}

// getCompressedOperations 获取压缩后的操作列表
func (m *DelayedBatchWriteManager) getCompressedOperations() []*CacheOperation {
	m.mapMutex.RLock()
	defer m.mapMutex.RUnlock()

	operations := make([]*CacheOperation, 0, len(m.operationMap))
	for _, op := range m.operationMap {
		operations = append(operations, op)
	}

	return operations
}

// batchWriteToDisk 批量写入磁盘
func (m *DelayedBatchWriteManager) batchWriteToDisk(operations []*CacheOperation) error {
	if m.mainCacheUpdater == nil {
		return fmt.Errorf("主缓存更新函数未设置")
	}

	// 批量处理所有操作
	for _, op := range operations {
		// 序列化数据
		data, err := m.serializer.Serialize(op.Data)
		if err != nil {
			return fmt.Errorf("数据序列化失败: %v", err)
		}

		// 写入磁盘
		if err := m.mainCacheUpdater(op.Key, data, op.TTL); err != nil {
			return fmt.Errorf("磁盘写入失败: %v", err)
		}
	}

	return nil
}

// emergencyFlush 紧急刷新
func (m *DelayedBatchWriteManager) emergencyFlush() error {
	m.queueMutex.Lock()
	defer m.queueMutex.Unlock()

	return m.executeBatchWrite("紧急刷新")
}

// autoTuneParameters 自适应参数调优
func (m *DelayedBatchWriteManager) autoTuneParameters() {
	// 完全自动调优，无需配置开关
	stats := m.collectRecentStats()

	// 调优批量间隔：基于系统负载动态调整
	avgSystemLoad := stats.SystemLoadAverage
	switch {
	case avgSystemLoad > 0.8: // 高负载：延长间隔，减少干扰
		m.config.MaxBatchInterval = m.minDuration(m.config.MaxBatchInterval*12/10, m.config.maxBatchInterval)
	case avgSystemLoad < 0.3: // 低负载：缩短间隔，及时持久化
		m.config.MaxBatchInterval = m.maxDuration(m.config.MaxBatchInterval*8/10, m.config.minBatchInterval)
	}

	// 调优批量大小：基于写入频率动态调整
	queueSize := int(atomic.LoadInt32(&m.stats.CurrentQueueSize))
	switch {
	case queueSize > 200: // 高频：增大批量，提高效率
		m.config.MaxBatchSize = m.minInt(m.config.MaxBatchSize*12/10, m.config.maxBatchSize)
	case queueSize < 50: // 低频：减小批量，降低延迟
		m.config.MaxBatchSize = m.maxInt(m.config.MaxBatchSize*8/10, m.config.minBatchSize)
	}
}

// collectRecentStats 收集最近的统计数据
func (m *DelayedBatchWriteManager) collectRecentStats() *WriteManagerStats {
	return m.GetWriteManagerStats()
}

// 辅助函数
func (m *DelayedBatchWriteManager) minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (m *DelayedBatchWriteManager) maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func (m *DelayedBatchWriteManager) minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *DelayedBatchWriteManager) maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// GetStats 获取统计信息
// recordFlushMeta 记录本次刷新的元信息（并发安全）。
func (m *DelayedBatchWriteManager) recordFlushMeta(trigger string, batchSize int) {
	m.flushMetaMutex.Lock()
	m.stats.LastFlushTime = time.Now()
	m.stats.LastFlushTrigger = trigger
	m.stats.LastBatchSize = batchSize
	m.flushMetaMutex.Unlock()
}

// addOperationsWritten 累加已写入操作数（并发安全）。
func (m *DelayedBatchWriteManager) addOperationsWritten(n int) {
	m.flushMetaMutex.Lock()
	m.stats.TotalOperationsWritten += n
	m.flushMetaMutex.Unlock()
}

// lastFlushTime 是 LastFlushTime 的并发安全读取口。
func (m *DelayedBatchWriteManager) lastFlushTime() time.Time {
	m.flushMetaMutex.RLock()
	defer m.flushMetaMutex.RUnlock()
	return m.stats.LastFlushTime
}

// snapshotStats 逐字段读取统计快照。
//
// 不能写 `stats := *m.stats`：整结构复制会与 atomic.AddInt64 的写入竞争
// （复制不是原子的），这是比裸字段更隐蔽的一处。计数器逐个原子读，元信息走锁。
func (m *DelayedBatchWriteManager) snapshotStats() WriteManagerStats {
	m.flushMetaMutex.RLock()
	lastFlush := m.stats.LastFlushTime
	lastTrigger := m.stats.LastFlushTrigger
	lastBatch := m.stats.LastBatchSize
	totalWritten := m.stats.TotalOperationsWritten
	windowStart := m.stats.WindowStart
	m.flushMetaMutex.RUnlock()

	return WriteManagerStats{
		TotalWrites:            atomic.LoadInt64(&m.stats.TotalWrites),
		TotalOperations:        atomic.LoadInt64(&m.stats.TotalOperations),
		BatchWrites:            atomic.LoadInt64(&m.stats.BatchWrites),
		ImmediateWrites:        atomic.LoadInt64(&m.stats.ImmediateWrites),
		MergedOperations:       atomic.LoadInt64(&m.stats.MergedOperations),
		FailedWrites:           atomic.LoadInt64(&m.stats.FailedWrites),
		SuccessfulWrites:       atomic.LoadInt64(&m.stats.SuccessfulWrites),
		CurrentQueueSize:       atomic.LoadInt32(&m.stats.CurrentQueueSize),
		LastFlushTime:          lastFlush,
		LastFlushTrigger:       lastTrigger,
		LastBatchSize:          lastBatch,
		TotalOperationsWritten: totalWritten,
		WindowStart:            windowStart,
	}
}

func (m *DelayedBatchWriteManager) GetStats() map[string]interface{} {
	stats := m.snapshotStats()
	stats.WindowEnd = time.Now()

	// 计算压缩比例
	if stats.TotalOperations > 0 {
		stats.SystemLoadAverage = float64(stats.TotalWrites) / float64(stats.TotalOperations)
	}

	// 获取全局缓冲区统计
	globalBufferStats := m.globalBufferManager.GetStats()

	// 合并所有统计信息
	combinedStats := map[string]interface{}{
		"write_manager": &stats,
		"global_buffer": globalBufferStats,
		"buffer_info":   m.globalBufferManager.GetBufferInfo(),
	}

	return combinedStats
}

// GetWriteManagerStats 获取写入管理器统计（兼容性方法）
func (m *DelayedBatchWriteManager) GetWriteManagerStats() *WriteManagerStats {
	stats := m.snapshotStats()
	stats.WindowEnd = time.Now()

	// 计算压缩比例
	if stats.TotalOperations > 0 {
		stats.SystemLoadAverage = float64(stats.TotalWrites) / float64(stats.TotalOperations)
	}

	return &stats
}
