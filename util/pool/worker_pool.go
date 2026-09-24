package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Task 表示一个工作任务。
// 执行时会收到本次批次的上下文：超时后调用方可以据此提前返回，
// 而不必等不可中断的任务自己结束。
type Task func(ctx context.Context) interface{}

// WorkerPool 工作池结构体
type WorkerPool struct {
	maxWorkers int
	taskQueue  chan Task
	results    chan interface{}
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// backgroundReclaims 统计因超时而转入后台回收的批次数（用于监控）
var backgroundReclaims int64

// BackgroundReclaims 返回因超时而转入后台回收的批次数。
// 该值持续增长说明有任务的耗时明显超过配置的超时时间。
func BackgroundReclaims() int64 {
	return atomic.LoadInt64(&backgroundReclaims)
}

// NewWorkerPool 创建一个新的工作池
func NewWorkerPool(maxWorkers int) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())

	pool := &WorkerPool{
		maxWorkers: maxWorkers,
		taskQueue:  make(chan Task, maxWorkers*2),        // 任务队列大小为工作者数量的2倍
		results:    make(chan interface{}, maxWorkers*2), // 结果队列大小为工作者数量的2倍
		ctx:        ctx,
		cancel:     cancel,
	}

	// 启动工作者
	pool.startWorkers()

	return pool
}

// NewWorkerPoolWithContext 创建一个带有指定上下文的新工作池
func NewWorkerPoolWithContext(ctx context.Context, maxWorkers int) *WorkerPool {
	ctx, cancel := context.WithCancel(ctx)

	pool := &WorkerPool{
		maxWorkers: maxWorkers,
		taskQueue:  make(chan Task, maxWorkers*2),        // 任务队列大小为工作者数量的2倍
		results:    make(chan interface{}, maxWorkers*2), // 结果队列大小为工作者数量的2倍
		ctx:        ctx,
		cancel:     cancel,
	}

	// 启动工作者
	pool.startWorkers()

	return pool
}

// startWorkers 启动工作者协程
func (p *WorkerPool) startWorkers() {
	for i := 0; i < p.maxWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()

			for {
				select {
				case task, ok := <-p.taskQueue:
					if !ok {
						return
					}

					// 执行任务并发送结果
					result := p.runTask(task)

					// 发送同样要响应取消：超时后已经没有人读结果，
					// 若无条件写入，工作者会在结果队列写满后永久阻塞，
					// Close 的 wg.Wait 就再也返回不了。
					select {
					case p.results <- result:
					case <-p.ctx.Done():
						return
					}

				case <-p.ctx.Done():
					return
				}
			}
		}()
	}
}

// runTask 执行单个任务，并把 panic 收敛为本轮的"无结果"。
// 插件解析代码属于外部输入处理，单个插件 panic 不应拖垮整个服务进程。
func (p *WorkerPool) runTask(task Task) (result interface{}) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[worker_pool] 任务 panic 已收敛: %v\n", r)
			result = nil
		}
	}()

	return task(p.ctx)
}

// Submit 提交一个任务到工作池
func (p *WorkerPool) Submit(task Task) {
	p.taskQueue <- task
}

// GetResults 获取所有任务的结果
func (p *WorkerPool) GetResults(count int) []interface{} {
	results := make([]interface{}, 0, count)

	// 收集指定数量的结果
	for i := 0; i < count; i++ {
		select {
		case result := <-p.results:
			results = append(results, result)
		case <-p.ctx.Done():
			// 上下文取消，返回已收集的结果
			return results
		}
	}

	return results
}

// Close 关闭工作池：等待所有工作者退出后关闭结果通道。
func (p *WorkerPool) Close() {
	p.Shutdown(true)
}

// Shutdown 关闭工作池。
// wait=true 时等待所有工作者退出并关闭结果通道，与原 Close 行为一致。
// wait=false 用于超时路径：调用方不等不可中断的任务，回收在后台完成；
// 此时不再关闭 results —— 调用方已停止读取，通道交给 GC 即可，
// 否则后台仍要等任务结束才能关闭通道，goroutine 白白延长存活。
func (p *WorkerPool) Shutdown(wait bool) {
	// 取消上下文
	p.cancel()

	// 关闭任务队列
	close(p.taskQueue)

	if !wait {
		go func() {
			p.wg.Wait()
		}()
		return
	}

	// 等待所有工作者完成
	p.wg.Wait()

	// 关闭结果队列
	close(p.results)
}

// ExecuteBatch 批量执行任务并返回结果（无超时控制）
func ExecuteBatch(tasks []Task, maxWorkers int) []interface{} {
	if len(tasks) == 0 {
		return []interface{}{}
	}

	// 如果任务数量少于工作者数量，调整工作者数量
	if len(tasks) < maxWorkers {
		maxWorkers = len(tasks)
	}

	// 创建工作池
	pool := NewWorkerPool(maxWorkers)
	defer pool.Close()

	// 提交所有任务
	for _, task := range tasks {
		pool.Submit(task)
	}

	// 获取所有结果
	return pool.GetResults(len(tasks))
}

// ExecuteBatchWithTimeout 批量执行任务，带有超时控制，并返回结果。
// 到点后按已完成的结果返回；未完成的任务无法被中断，其回收转入后台。
func ExecuteBatchWithTimeout(tasks []Task, maxWorkers int, timeout time.Duration) []interface{} {
	if len(tasks) == 0 {
		return []interface{}{}
	}

	// 如果任务数量少于工作者数量，调整工作者数量
	if len(tasks) < maxWorkers {
		maxWorkers = len(tasks)
	}

	// 创建带超时的上下文
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// 创建工作池
	pool := NewWorkerPoolWithContext(ctx, maxWorkers)

	// 提交所有任务
	for _, task := range tasks {
		select {
		case pool.taskQueue <- task:
			// 任务提交成功
		case <-ctx.Done():
			// 超时或取消，停止提交更多任务
			return closeAfterResults(ctx, pool, len(tasks))
		}
	}

	// 获取所有结果，GetResults方法会处理超时情况
	return closeAfterResults(ctx, pool, len(tasks))
}

// closeAfterResults 收集结果后关闭工作池。
// 超时时仍在执行的任务无法被中断，回收转入后台完成，
// 保证调用方按超时时间拿到已完成的结果。
func closeAfterResults(ctx context.Context, pool *WorkerPool, count int) []interface{} {
	results := pool.GetResults(count)

	// 用上下文状态判断是否超时，而不是结果数量：
	// "结果没凑齐"与"已经超时"是两件事，后者才是调用方需要立刻返回的信号。
	if ctx.Err() != nil {
		atomic.AddInt64(&backgroundReclaims, 1)
		fmt.Printf("[worker_pool] 批次超时：已收到 %d/%d 个结果，其余任务在后台执行\n",
			len(results), count)
		go pool.Shutdown(false)
	} else {
		pool.Shutdown(true)
	}

	return results
}
