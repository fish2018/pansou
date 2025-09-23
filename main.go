package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/netutil"

	"pansou/api"
	"pansou/config"
	"pansou/plugin"
	"pansou/service"
	"pansou/util"
	"pansou/util/cache"

	// 以下是插件的空导入，用于触发各插件的init函数，实现自动注册
	// 添加新插件时，只需在此处添加对应的导入语句即可
	// _ "pansou/plugin/hdr4k"
	// _ "pansou/plugin/pan666"
	_ "pansou/plugin/hunhepan"
	_ "pansou/plugin/jikepan"
	_ "pansou/plugin/panwiki"
	_ "pansou/plugin/pansearch"
	_ "pansou/plugin/panta"
	_ "pansou/plugin/qupansou"
	_ "pansou/plugin/susu"
	_ "pansou/plugin/thepiratebay"
	_ "pansou/plugin/wanou"
	_ "pansou/plugin/xuexizhinan"
	_ "pansou/plugin/panyq"
	_ "pansou/plugin/zhizhen"
	_ "pansou/plugin/labi"
	_ "pansou/plugin/muou"
	_ "pansou/plugin/ouge"
	_ "pansou/plugin/shandian"
	_ "pansou/plugin/duoduo"
	_ "pansou/plugin/huban"
	_ "pansou/plugin/cyg"
	_ "pansou/plugin/erxiao"
	_ "pansou/plugin/miaoso"
	_ "pansou/plugin/fox4k"
	_ "pansou/plugin/pianku"
	_ "pansou/plugin/clmao"
	_ "pansou/plugin/wuji"
	_ "pansou/plugin/cldi"
	_ "pansou/plugin/xiaozhang"
	_ "pansou/plugin/libvio"
	_ "pansou/plugin/leijing"
	_ "pansou/plugin/xb6v"
	_ "pansou/plugin/xys"
	_ "pansou/plugin/ddys"
	_ "pansou/plugin/hdmoli"
	_ "pansou/plugin/yuhuage"
	_ "pansou/plugin/u3c3"
	_ "pansou/plugin/javdb"
	_ "pansou/plugin/clxiong"
	_ "pansou/plugin/jutoushe"
	_ "pansou/plugin/sdso"
	_ "pansou/plugin/xiaoji"
	_ "pansou/plugin/xdyh"
	_ "pansou/plugin/haisou"
	_ "pansou/plugin/bixin"
	_ "pansou/plugin/javdb"
)

// 全局缓存写入管理器
var globalCacheWriteManager *cache.DelayedBatchWriteManager

func main() {
	// 初始化应用
	initApp()

	// 启动服务器
	startServer()
}

// initApp 初始化应用程序
func initApp() {
	// 初始化配置
	config.Init()

	// 初始化HTTP客户端
	util.InitHTTPClient()

	// 初始化缓存写入管理器
	var err error
	globalCacheWriteManager, err = cache.NewDelayedBatchWriteManager()
	if err != nil {
		log.Fatalf("缓存写入管理器创建失败: %v", err)
	}
	if err := globalCacheWriteManager.Initialize(); err != nil {
		log.Fatalf("缓存写入管理器初始化失败: %v", err)
	}
	// 将缓存写入管理器注入到service包
	service.SetGlobalCacheWriteManager(globalCacheWriteManager)

	// 延迟设置主缓存更新函数，确保service初始化完成
	go func() {
		// 等待一小段时间确保service包完全初始化
		time.Sleep(100 * time.Millisecond)
		if mainCache := service.GetEnhancedTwoLevelCache(); mainCache != nil {
			globalCacheWriteManager.SetMainCacheUpdater(func(key string, data []byte, ttl time.Duration) error {
				return mainCache.SetBothLevels(key, data, ttl)
			})
		}
	}()

	// 确保异步插件系统初始化
	plugin.InitAsyncPluginSystem()
}

// startServer 启动Web服务器
func startServer() {
	// 初始化插件管理器
	pluginManager := plugin.NewPluginManager()

	// 注册全局插件（根据配置过滤）
	if config.AppConfig.AsyncPluginEnabled {
		pluginManager.RegisterGlobalPluginsWithFilter(config.AppConfig.EnabledPlugins)
	}

	// 更新默认并发数（如果插件被禁用则使用0）
	pluginCount := 0
	if config.AppConfig.AsyncPluginEnabled {
		pluginCount = len(pluginManager.GetPlugins())
	}
	config.UpdateDefaultConcurrency(pluginCount)

	// 初始化搜索服务
	searchService := service.NewSearchService(pluginManager)

	// 设置路由
	router := api.SetupRouter(searchService)

	// 获取端口配置
	port := config.AppConfig.Port

	// 输出服务信息
	printServiceInfo(port, pluginManager)

	// 创建HTTP服务器
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      router,
		ReadTimeout:  config.AppConfig.HTTPReadTimeout,
		WriteTimeout: config.AppConfig.HTTPWriteTimeout,
		IdleTimeout:  config.AppConfig.HTTPIdleTimeout,
	}

	// 创建通道来接收操作系统信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// 在单独的goroutine中启动服务器
	go func() {
		// 如果设置了最大连接数，使用限制监听器
		if config.AppConfig.HTTPMaxConns > 0 {
			// 创建监听器
			listener, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				log.Fatalf("创建监听器失败: %v", err)
			}

			// 创建限制连接数的监听器
			limitListener := netutil.LimitListener(listener, config.AppConfig.HTTPMaxConns)

			// 使用限制监听器启动服务器
			if err := srv.Serve(limitListener); err != nil && err != http.ErrServerClosed {
				log.Fatalf("启动服务器失败: %v", err)
			}
		} else {
			// 使用默认方式启动服务器（不限制连接数）
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("启动服务器失败: %v", err)
			}
		}
	}()

	// 等待中断信号
	<-quit
	fmt.Println("正在关闭服务器...")

	// 优先保存缓存数据到磁盘（数据安全第一）
	// 增加关闭超时时间，确保数据有足够时间保存
	shutdownTimeout := 10 * time.Second
	
	if globalCacheWriteManager != nil {
		if err := globalCacheWriteManager.Shutdown(shutdownTimeout); err != nil {
			log.Printf("缓存数据保存失败: %v", err)
		}
	}
	
	// 额外确保内存缓存也被保存（双重保障）
	if mainCache := service.GetEnhancedTwoLevelCache(); mainCache != nil {
		if err := mainCache.FlushMemoryToDisk(); err != nil {
			log.Printf("内存缓存同步失败: %v", err)
		} 
	}

	// 设置关闭超时时间
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 优雅关闭服务器
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("服务器关闭异常: %v", err)
	}

	fmt.Println("服务器已安全关闭")
}

// printServiceInfo 打印服务信息
func printServiceInfo(port string, pluginManager *plugin.PluginManager) {
	// 启动服务器
	fmt.Printf("服务器启动在 http://localhost:%s\n", port)

	// 输出代理信息
	if config.AppConfig.UseProxy {
		fmt.Printf("使用SOCKS5代理: %s\n", config.AppConfig.ProxyURL)
	} else {
		fmt.Println("未使用代理")
	}

	// 输出并发信息
	if os.Getenv("CONCURRENCY") != "" {
		fmt.Printf("默认并发数: %d (由环境变量CONCURRENCY指定)\n", config.AppConfig.DefaultConcurrency)
	} else {
		channelCount := len(config.AppConfig.DefaultChannels)
		pluginCount := 0
		// 只有插件启用时才计算插件数
		if config.AppConfig.AsyncPluginEnabled && pluginManager != nil {
			pluginCount = len(pluginManager.GetPlugins())
		}
		fmt.Printf("默认并发数: %d (= 频道数%d + 插件数%d + 10)\n",
			config.AppConfig.DefaultConcurrency, channelCount, pluginCount)
	}

	// 输出缓存信息
	if config.AppConfig.CacheEnabled {
		fmt.Printf("缓存已启用: 路径=%s, 最大大小=%dMB, TTL=%d分钟\n",
			config.AppConfig.CachePath,
			config.AppConfig.CacheMaxSizeMB,
			config.AppConfig.CacheTTLMinutes)
	} else {
		fmt.Println("缓存已禁用")
	}

	// 输出压缩信息
	if config.AppConfig.EnableCompression {
		fmt.Printf("响应压缩已启用: 最小压缩大小=%d字节\n",
			config.AppConfig.MinSizeToCompress)
	} 

	// 输出GC配置信息
	fmt.Printf("GC配置: 触发阈值=%d%%, 内存优化=%v\n",
		config.AppConfig.GCPercent,
		config.AppConfig.OptimizeMemory)

	// 输出HTTP服务器配置信息
	readTimeoutMsg := ""
	if os.Getenv("HTTP_READ_TIMEOUT") != "" {
		readTimeoutMsg = "(由环境变量指定)"
	} else {
		readTimeoutMsg = "(自动计算)"
	}

	writeTimeoutMsg := ""
	if os.Getenv("HTTP_WRITE_TIMEOUT") != "" {
		writeTimeoutMsg = "(由环境变量指定)"
	} else {
		writeTimeoutMsg = "(自动计算)"
	}

	maxConnsMsg := ""
	if os.Getenv("HTTP_MAX_CONNS") != "" {
		maxConnsMsg = "(由环境变量指定)"
	} else {
		cpuCount := runtime.NumCPU()
		maxConnsMsg = fmt.Sprintf("(自动计算: CPU核心数%d × 200)", cpuCount)
	}

	fmt.Printf("HTTP服务器配置: 读取超时=%v %s, 写入超时=%v %s, 空闲超时=%v, 最大连接数=%d %s\n",
		config.AppConfig.HTTPReadTimeout, readTimeoutMsg,
		config.AppConfig.HTTPWriteTimeout, writeTimeoutMsg,
		config.AppConfig.HTTPIdleTimeout,
		config.AppConfig.HTTPMaxConns, maxConnsMsg)

	// 输出异步插件配置信息
	if config.AppConfig.AsyncPluginEnabled {
		// 检查工作者数量是否由环境变量指定
		workersMsg := ""
		if os.Getenv("ASYNC_MAX_BACKGROUND_WORKERS") != "" {
			workersMsg = "(由环境变量指定)"
		} else {
			cpuCount := runtime.NumCPU()
			workersMsg = fmt.Sprintf("(自动计算: CPU核心数%d × 5)", cpuCount)
		}

		// 检查任务数量是否由环境变量指定
		tasksMsg := ""
		if os.Getenv("ASYNC_MAX_BACKGROUND_TASKS") != "" {
			tasksMsg = "(由环境变量指定)"
		} else {
			tasksMsg = "(自动计算: 工作者数量 × 5)"
		}

		fmt.Printf("异步插件已启用: 响应超时=%d秒, 最大工作者=%d %s, 最大任务=%d %s, 缓存TTL=%d小时\n",
			config.AppConfig.AsyncResponseTimeout,
			config.AppConfig.AsyncMaxBackgroundWorkers, workersMsg,
			config.AppConfig.AsyncMaxBackgroundTasks, tasksMsg,
			config.AppConfig.AsyncCacheTTLHours)
	} else {
		fmt.Println("异步插件已禁用")
	}

	// 只有当插件功能启用时才输出插件信息
	if config.AppConfig.AsyncPluginEnabled {
		plugins := pluginManager.GetPlugins()
		if len(plugins) > 0 {
			// 根据新逻辑，只有指定了具体插件才会加载插件
			fmt.Printf("已启用指定插件 (%d个):\n", len(plugins))

			// 按优先级排序（优先级数字越小越靠前）
			sort.Slice(plugins, func(i, j int) bool {
				// 优先级相同时按名称排序
				if plugins[i].Priority() == plugins[j].Priority() {
					return plugins[i].Name() < plugins[j].Name()
				}
				return plugins[i].Priority() < plugins[j].Priority()
			})

			for _, p := range plugins {
				fmt.Printf("  - %s (优先级: %d)\n", p.Name(), p.Priority())
			}
		} else {
			// 区分不同的情况
			if config.AppConfig.EnabledPlugins == nil {
				fmt.Println("未设置插件列表 (ENABLED_PLUGINS)，未加载任何插件")
			} else if len(config.AppConfig.EnabledPlugins) > 0 {
				fmt.Printf("未找到指定的插件: %s\n", strings.Join(config.AppConfig.EnabledPlugins, ", "))
			} else {
				fmt.Println("插件列表为空 (ENABLED_PLUGINS=\"\")，未加载任何插件")
			}
		}
	}
}
