package pool

import (
	"context"
	"sync"
	"time"
)

// Task 表示一个工作任务
type Task func() interface{}

// WorkerPool 工作池结构体
type WorkerPool struct {
	maxWorkers int
	taskQueue  chan Task
	results    chan interface{}
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewWorkerPool 创建一个新的工作池
func NewWorkerPool(maxWorkers int) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	
	pool := &WorkerPool{
		maxWorkers: maxWorkers,
		taskQueue:  make(chan Task, maxWorkers*2), // 任务队列大小为工作者数量的2倍
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
		taskQueue:  make(chan Task, maxWorkers*2), // 任务队列大小为工作者数量的2倍
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
					result := task()
					select {
					case p.results <- result:
					case <-p.ctx.Done():
						// 已超时，没有人再读取结果，直接退出避免阻塞
						return
					}
					
				case <-p.ctx.Done():
					return
				}
			}
		}()
	}
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

// Close 关闭工作池
func (p *WorkerPool) Close() {
	// 取消上下文
	p.cancel()
	
	// 关闭任务队列
	close(p.taskQueue)
	
	// 等待所有工作者完成
	p.wg.Wait()
	
	// 关闭结果队列
	close(p.results)
}

// ExecuteBatch 批量执行任务并返回结果
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

// ExecuteBatchWithTimeout 批量执行任务，带有超时控制，并返回结果
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
			return closeAfterResults(pool, len(tasks))
		}
	}
	
	// 获取所有结果，GetResults方法会处理超时情况
	return closeAfterResults(pool, len(tasks))
}

// closeAfterResults 收集结果后关闭工作池。
// 超时时仍在执行的任务无法被中断，Close 会阻塞到它们结束，
// 因此超时情况下在后台关闭，保证调用方按超时时间拿到已完成的结果。
func closeAfterResults(pool *WorkerPool, count int) []interface{} {
	results := pool.GetResults(count)
	if len(results) < count {
		go pool.Close()
	} else {
		pool.Close()
	}
	return results
} 