/**
 * 活动监控器 - 跟踪MCP工具调用活动
 */
export class ActivityMonitor {
  private lastActivityTime: number;
  private idleTimeout: number;
  private enableIdleShutdown: boolean;
  private idleTimer: NodeJS.Timeout | null = null;
  private onIdleCallback: (() => void) | null = null;

  constructor(idleTimeout: number = 300000, enableIdleShutdown: boolean = true) {
    this.lastActivityTime = Date.now();
    this.idleTimeout = idleTimeout;
    this.enableIdleShutdown = enableIdleShutdown;
  }

  /**
   * 记录活动
   */
  recordActivity(): void {
    this.lastActivityTime = Date.now();
    this.resetIdleTimer();
  }

  /**
   * 获取最后活动时间
   */
  getLastActivityTime(): number {
    return this.lastActivityTime;
  }

  /**
   * 获取空闲时间（毫秒）
   */
  getIdleTime(): number {
    return Date.now() - this.lastActivityTime;
  }

  /**
   * 检查是否空闲超时
   */
  isIdleTimeout(): boolean {
    return this.getIdleTime() >= this.idleTimeout;
  }

  /**
   * 设置空闲回调函数
   */
  setOnIdleCallback(callback: () => void): void {
    this.onIdleCallback = callback;
    this.resetIdleTimer();
  }

  /**
   * 重置空闲计时器
   */
  private resetIdleTimer(): void {
    if (!this.enableIdleShutdown || !this.onIdleCallback) {
      return;
    }

    // 清除现有计时器
    if (this.idleTimer) {
      clearTimeout(this.idleTimer);
    }

    // 设置新的计时器
    this.idleTimer = setTimeout(() => {
      if (this.onIdleCallback) {
        console.log(`[ActivityMonitor] 检测到空闲超时 (${this.idleTimeout}ms)，触发空闲回调`);
        this.onIdleCallback();
      }
    }, this.idleTimeout);
  }

  /**
   * 停止监控
   */
  stop(): void {
    if (this.idleTimer) {
      clearTimeout(this.idleTimer);
      this.idleTimer = null;
    }
    this.onIdleCallback = null;
  }

  /**
   * 更新配置
   */
  updateConfig(idleTimeout: number, enableIdleShutdown: boolean): void {
    this.idleTimeout = idleTimeout;
    this.enableIdleShutdown = enableIdleShutdown;
    this.resetIdleTimer();
  }

  /**
   * 获取状态信息
   */
  getStatus(): {
    lastActivityTime: number;
    idleTime: number;
    idleTimeout: number;
    enableIdleShutdown: boolean;
    isIdleTimeout: boolean;
  } {
    return {
      lastActivityTime: this.lastActivityTime,
      idleTime: this.getIdleTime(),
      idleTimeout: this.idleTimeout,
      enableIdleShutdown: this.enableIdleShutdown,
      isIdleTimeout: this.isIdleTimeout()
    };
  }
}

// 全局活动监控器实例
let globalActivityMonitor: ActivityMonitor | null = null;

/**
 * 获取全局活动监控器实例
 */
export function getActivityMonitor(): ActivityMonitor {
  if (!globalActivityMonitor) {
    throw new Error('活动监控器未初始化，请先调用 initializeActivityMonitor');
  }
  return globalActivityMonitor;
}

/**
 * 初始化全局活动监控器
 */
export function initializeActivityMonitor(idleTimeout: number, enableIdleShutdown: boolean): ActivityMonitor {
  if (globalActivityMonitor) {
    globalActivityMonitor.stop();
  }
  globalActivityMonitor = new ActivityMonitor(idleTimeout, enableIdleShutdown);
  return globalActivityMonitor;
}

/**
 * 停止全局活动监控器
 */
export function stopActivityMonitor(): void {
  if (globalActivityMonitor) {
    globalActivityMonitor.stop();
    globalActivityMonitor = null;
  }
}