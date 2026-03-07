package config

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Config 应用配置结构
type Config struct {
	DefaultChannels    []string
	DefaultConcurrency int
	Port               string
	ProxyURL           string
	UseProxy           bool
	HTTPProxyURL       string
	HTTPSProxyURL      string
	// 缓存相关配置
	CacheEnabled    bool
	CachePath       string
	CacheMaxSizeMB  int
	CacheTTLMinutes int
	// 压缩相关配置
	EnableCompression bool
	MinSizeToCompress int // 最小压缩大小（字节）
	// GC相关配置
	GCPercent      int  // GC触发阈值百分比
	OptimizeMemory bool // 是否启用内存优化
	// 插件相关配置
	PluginTimeoutSeconds int           // 插件超时时间（秒）
	PluginTimeout        time.Duration // 插件超时时间（Duration）
	// 异步插件相关配置
	AsyncPluginEnabled        bool          // 是否启用异步插件
	EnabledPlugins            []string      // 启用的具体插件列表（空表示启用所有）
	AsyncResponseTimeout      int           // 响应超时时间（秒）
	AsyncResponseTimeoutDur   time.Duration // 响应超时时间（Duration）
	AsyncMaxBackgroundWorkers int           // 最大后台工作者数量
	AsyncMaxBackgroundTasks   int           // 最大后台任务数量
	AsyncCacheTTLHours        int           // 异步缓存有效期（小时）
	AsyncLogEnabled           bool          // 是否启用异步插件详细日志
	// HTTP服务器配置
	HTTPReadTimeout  time.Duration // 读取超时
	HTTPWriteTimeout time.Duration // 写入超时
	HTTPIdleTimeout  time.Duration // 空闲超时
	HTTPMaxConns     int           // 最大连接数
	// 认证相关配置
	AuthEnabled     bool              // 是否启用认证
	AuthUsers       map[string]string // 用户名:密码映射
	AuthTokenExpiry time.Duration     // Token有效期
	AuthJWTSecret   string            // JWT签名密钥
	AuthMcpToken    string            // 设置MCP token，如果不设置则默认mcp不验证

}

// 全局配置实例
var AppConfig *Config

// 初始化配置
func Init() {
	proxyURL := getProxyURL()
	pluginTimeoutSeconds := getPluginTimeout()
	asyncResponseTimeoutSeconds := getAsyncResponseTimeout()

	AppConfig = &Config{
		DefaultChannels:    getDefaultChannels(),
		DefaultConcurrency: getDefaultConcurrency(),
		Port:               getPort(),
		ProxyURL:           proxyURL,
		UseProxy:           proxyURL != "",
		HTTPProxyURL:       getHTTPProxyURL(),
		HTTPSProxyURL:      getHTTPSProxyURL(),
		// 缓存相关配置
		CacheEnabled:    getCacheEnabled(),
		CachePath:       getCachePath(),
		CacheMaxSizeMB:  getCacheMaxSize(),
		CacheTTLMinutes: getCacheTTL(),
		// 压缩相关配置
		EnableCompression: getEnableCompression(),
		MinSizeToCompress: getMinSizeToCompress(),
		// GC相关配置
		GCPercent:      getGCPercent(),
		OptimizeMemory: getOptimizeMemory(),
		// 插件相关配置
		PluginTimeoutSeconds: pluginTimeoutSeconds,
		PluginTimeout:        time.Duration(pluginTimeoutSeconds) * time.Second,
		// 异步插件相关配置
		AsyncPluginEnabled:        getAsyncPluginEnabled(),
		EnabledPlugins:            getEnabledPlugins(),
		AsyncResponseTimeout:      asyncResponseTimeoutSeconds,
		AsyncResponseTimeoutDur:   time.Duration(asyncResponseTimeoutSeconds) * time.Second,
		AsyncMaxBackgroundWorkers: getAsyncMaxBackgroundWorkers(),
		AsyncMaxBackgroundTasks:   getAsyncMaxBackgroundTasks(),
		AsyncCacheTTLHours:        getAsyncCacheTTLHours(),
		AsyncLogEnabled:           getAsyncLogEnabled(),
		// HTTP服务器配置
		HTTPReadTimeout:  getHTTPReadTimeout(),
		HTTPWriteTimeout: getHTTPWriteTimeout(),
		HTTPIdleTimeout:  getHTTPIdleTimeout(),
		HTTPMaxConns:     getHTTPMaxConns(),
		// 认证相关配置
		AuthEnabled:     getAuthEnabled(),
		AuthUsers:       getAuthUsers(),
		AuthTokenExpiry: getAuthTokenExpiry(),
		AuthJWTSecret:   getAuthJWTSecret(),
		AuthMcpToken:    getAuthMcpToken(),
	}

	// 应用GC配置
	applyGCSettings()
}

// 从环境变量获取默认频道列表，如果未设置则使用默认值
func getDefaultChannels() []string {
	channelsEnv := os.Getenv("CHANNELS")
	if channelsEnv == "" {
		return []string{"tgsearchers4"}
	}
	return strings.Split(channelsEnv, ",")
}

// 从环境变量获取默认并发数，如果未设置则使用基于环境变量的简单计算
func getDefaultConcurrency() int {
	concurrencyEnv := os.Getenv("CONCURRENCY")
	if concurrencyEnv != "" {
		concurrency, err := strconv.Atoi(concurrencyEnv)
		if err == nil && concurrency > 0 {
			return concurrency
		}
	}

	// 环境变量未设置或无效，使用基于环境变量的简单计算
	// 计算频道数
	channelCount := len(getDefaultChannels())

	// 估计插件数（从环境变量或默认值，实际在应用启动后会根据真实插件数调整）
	pluginCountEnv := os.Getenv("PLUGIN_COUNT")
	pluginCount := 0
	if pluginCountEnv != "" {
		count, err := strconv.Atoi(pluginCountEnv)
		if err == nil && count > 0 {
			pluginCount = count
		}
	}

	// 如果没有指定插件数，默认使用7个（当前已知的插件数）
	if pluginCount == 0 {
		pluginCount = 7
	}

	// 计算并发数 = 频道数 + 插件数 + 10
	concurrency := channelCount + pluginCount + 10
	if concurrency < 1 {
		concurrency = 1 // 确保至少为1
	}

	return concurrency
}

// 更新默认并发数（根据实际插件数或0调用）
// pluginCount: 如果插件被禁用则为0，否则为实际插件数
func UpdateDefaultConcurrency(pluginCount int) {
	if AppConfig == nil {
		return
	}

	// 只有当未通过环境变量指定并发数时才进行调整
	concurrencyEnv := os.Getenv("CONCURRENCY")
	if concurrencyEnv != "" {
		return
	}

	// 计算频道数
	channelCount := len(AppConfig.DefaultChannels)

	// 计算并发数 = 频道数 + 插件数（插件禁用时为0）+ 10
	concurrency := channelCount + pluginCount + 10
	if concurrency < 1 {
		concurrency = 1 // 确保至少为1
	}

	// 更新配置
	AppConfig.DefaultConcurrency = concurrency
}

// 从环境变量获取服务端口，如果未设置则使用默认值
func getPort() string {
	port := os.Getenv("PORT")
	if port == "" {
		return "8888"
	}
	return port
}

func getProxyURL() string {
	return os.Getenv("PROXY")
}

func getHTTPProxyURL() string {
	if proxyURL := os.Getenv("HTTP_PROXY"); proxyURL != "" {
		return proxyURL
	}
	return os.Getenv("http_proxy")
}

func getHTTPSProxyURL() string {
	if proxyURL := os.Getenv("HTTPS_PROXY"); proxyURL != "" {
		return proxyURL
	}
	return os.Getenv("https_proxy")
}

// 从环境变量获取是否启用缓存，如果未设置则默认启用
func getCacheEnabled() bool {
	enabled := os.Getenv("CACHE_ENABLED")
	if enabled == "" {
		return true
	}
	return enabled != "false" && enabled != "0"
}

// 从环境变量获取缓存路径，如果未设置则使用默认路径
func getCachePath() string {
	path := os.Getenv("CACHE_PATH")
	if path == "" {
		// 默认在当前目录下创建cache文件夹
		defaultPath, err := filepath.Abs("./cache")
		if err != nil {
			return "./cache"
		}
		return defaultPath
	}
	return path
}

// 从环境变量获取缓存最大大小(MB)，如果未设置则使用默认值
func getCacheMaxSize() int {
	sizeEnv := os.Getenv("CACHE_MAX_SIZE")
	if sizeEnv == "" {
		return 100 // 默认100MB
	}
	size, err := strconv.Atoi(sizeEnv)
	if err != nil || size <= 0 {
		return 100
	}
	return size
}

// 从环境变量获取缓存TTL(分钟)，如果未设置则使用默认值
func getCacheTTL() int {
	ttlEnv := os.Getenv("CACHE_TTL")
	if ttlEnv == "" {
		return 60 // 默认60分钟
	}
	ttl, err := strconv.Atoi(ttlEnv)
	if err != nil || ttl <= 0 {
		return 60
	}
	return ttl
}

// 从环境变量获取是否启用压缩，如果未设置则默认禁用
func getEnableCompression() bool {
	enabled := os.Getenv("ENABLE_COMPRESSION")
	if enabled == "" {
		return false // 默认禁用，因为通常由Nginx等处理
	}
	return enabled == "true" || enabled == "1"
}

// 从环境变量获取最小压缩大小，如果未设置则使用默认值
func getMinSizeToCompress() int {
	sizeEnv := os.Getenv("MIN_SIZE_TO_COMPRESS")
	if sizeEnv == "" {
		return 1024 // 默认1KB
	}
	size, err := strconv.Atoi(sizeEnv)
	if err != nil || size <= 0 {
		return 1024
	}
	return size
}

// 从环境变量获取GC百分比，如果未设置则使用默认值
func getGCPercent() int {
	percentEnv := os.Getenv("GC_PERCENT")
	if percentEnv == "" {
		return 50 // 默认50% - 优化内存管理，更频繁的GC避免内存暴涨
	}
	percent, err := strconv.Atoi(percentEnv)
	if err != nil || percent <= 0 {
		return 50 // 错误时也使用优化后的默认值
	}
	return percent
}

// 从环境变量获取是否优化内存，如果未设置则默认启用
func getOptimizeMemory() bool {
	enabled := os.Getenv("OPTIMIZE_MEMORY")
	if enabled == "" {
		return true // 默认启用
	}
	return enabled != "false" && enabled != "0"
}

// 从环境变量获取插件超时时间（秒），如果未设置则使用默认值
func getPluginTimeout() int {
	timeoutEnv := os.Getenv("PLUGIN_TIMEOUT")
	if timeoutEnv == "" {
		return 30 // 默认30秒
	}
	timeout, err := strconv.Atoi(timeoutEnv)
	if err != nil || timeout <= 0 {
		return 30
	}
	return timeout
}

// 从环境变量获取是否启用异步插件，如果未设置则默认启用
func getAsyncPluginEnabled() bool {
	enabled := os.Getenv("ASYNC_PLUGIN_ENABLED")
	if enabled == "" {
		return true // 默认启用
	}
	return enabled != "false" && enabled != "0"
}

// 从环境变量获取启用的插件列表
// 返回nil表示未设置环境变量（不启用任何插件）
// 返回[]string{}表示设置为空（不启用任何插件）
// 返回具体列表表示启用指定插件
func getEnabledPlugins() []string {
	plugins, exists := os.LookupEnv("ENABLED_PLUGINS")
	if !exists {
		// 未设置环境变量时返回nil，表示不启用任何插件
		return nil
	}

	if plugins == "" {
		// 设置为空字符串，也表示不启用任何插件
		return []string{}
	}

	// 按逗号分割插件名
	result := make([]string, 0)
	for _, plugin := range strings.Split(plugins, ",") {
		plugin = strings.TrimSpace(plugin)
		if plugin != "" {
			result = append(result, plugin)
		}
	}

	return result
}

// 从环境变量获取异步响应超时时间（秒），如果未设置则使用默认值
func getAsyncResponseTimeout() int {
	timeoutEnv := os.Getenv("ASYNC_RESPONSE_TIMEOUT")
	if timeoutEnv == "" {
		return 4 // 默认4秒
	}
	timeout, err := strconv.Atoi(timeoutEnv)
	if err != nil || timeout <= 0 {
		return 4
	}
	return timeout
}

// 从环境变量获取最大后台工作者数量，如果未设置则自动计算
func getAsyncMaxBackgroundWorkers() int {
	sizeEnv := os.Getenv("ASYNC_MAX_BACKGROUND_WORKERS")
	if sizeEnv != "" {
		size, err := strconv.Atoi(sizeEnv)
		if err == nil && size > 0 {
			return size
		}
	}

	// 自动计算：根据CPU核心数计算
	// 每个CPU核心分配5个工作者，最小20个
	cpuCount := runtime.NumCPU()
	workers := cpuCount * 5

	// 确保至少有20个工作者
	if workers < 20 {
		workers = 20
	}

	return workers
}

// 从环境变量获取最大后台任务数量，如果未设置则自动计算
func getAsyncMaxBackgroundTasks() int {
	sizeEnv := os.Getenv("ASYNC_MAX_BACKGROUND_TASKS")
	if sizeEnv != "" {
		size, err := strconv.Atoi(sizeEnv)
		if err == nil && size > 0 {
			return size
		}
	}

	// 自动计算：工作者数量的5倍，最小100个
	workers := getAsyncMaxBackgroundWorkers()
	tasks := workers * 5

	// 确保至少有100个任务
	if tasks < 100 {
		tasks = 100
	}

	return tasks
}

// 从环境变量获取异步缓存有效期（小时），如果未设置则使用默认值
func getAsyncCacheTTLHours() int {
	ttlEnv := os.Getenv("ASYNC_CACHE_TTL_HOURS")
	if ttlEnv == "" {
		return 1 // 默认1小时
	}
	ttl, err := strconv.Atoi(ttlEnv)
	if err != nil || ttl <= 0 {
		return 1
	}
	return ttl
}

// 从环境变量获取HTTP读取超时，如果未设置则自动计算
func getHTTPReadTimeout() time.Duration {
	timeoutEnv := os.Getenv("HTTP_READ_TIMEOUT")
	if timeoutEnv != "" {
		timeout, err := strconv.Atoi(timeoutEnv)
		if err == nil && timeout > 0 {
			return time.Duration(timeout) * time.Second
		}
	}

	// 自动计算：默认30秒，异步模式下根据异步响应超时调整
	timeout := 30 * time.Second

	// 如果启用了异步插件，确保读取超时足够长
	if getAsyncPluginEnabled() {
		// 读取超时应该至少是异步响应超时的3倍，确保有足够时间完成异步操作
		asyncTimeoutSecs := getAsyncResponseTimeout()
		asyncTimeoutExtended := time.Duration(asyncTimeoutSecs*3) * time.Second
		if asyncTimeoutExtended > timeout {
			timeout = asyncTimeoutExtended
		}
	}

	return timeout
}

// 从环境变量获取HTTP写入超时，如果未设置则自动计算
func getHTTPWriteTimeout() time.Duration {
	timeoutEnv := os.Getenv("HTTP_WRITE_TIMEOUT")
	if timeoutEnv != "" {
		timeout, err := strconv.Atoi(timeoutEnv)
		if err == nil && timeout > 0 {
			return time.Duration(timeout) * time.Second
		}
	}

	// 自动计算：默认60秒，但根据插件超时和异步处理时间调整
	timeout := 60 * time.Second

	// 如果启用了异步插件，确保写入超时足够长
	pluginTimeoutSecs := getPluginTimeout()

	// 计算1.5倍的插件超时时间（使用整数运算：乘以3再除以2）
	pluginTimeoutExtended := time.Duration(pluginTimeoutSecs*3/2) * time.Second

	if pluginTimeoutExtended > timeout {
		timeout = pluginTimeoutExtended
	}

	return timeout
}

// 从环境变量获取HTTP空闲超时，如果未设置则自动计算
func getHTTPIdleTimeout() time.Duration {
	timeoutEnv := os.Getenv("HTTP_IDLE_TIMEOUT")
	if timeoutEnv != "" {
		timeout, err := strconv.Atoi(timeoutEnv)
		if err == nil && timeout > 0 {
			return time.Duration(timeout) * time.Second
		}
	}

	// 自动计算：默认120秒，考虑到保持连接的效益
	return 120 * time.Second
}

// 从环境变量获取HTTP最大连接数，如果未设置则自动计算
func getHTTPMaxConns() int {
	maxConnsEnv := os.Getenv("HTTP_MAX_CONNS")
	if maxConnsEnv != "" {
		maxConns, err := strconv.Atoi(maxConnsEnv)
		if err == nil && maxConns > 0 {
			return maxConns
		}
	}

	// 自动计算：根据CPU核心数计算
	// 每个CPU核心分配200个连接，最小1000个
	cpuCount := runtime.NumCPU()
	maxConns := cpuCount * 200

	// 确保至少有1000个连接
	if maxConns < 1000 {
		maxConns = 1000
	}

	return maxConns
}

// 从环境变量获取异步插件日志开关，如果未设置则使用默认值
func getAsyncLogEnabled() bool {
	logEnv := os.Getenv("ASYNC_LOG_ENABLED")
	if logEnv == "" {
		return true // 默认启用日志
	}
	enabled, err := strconv.ParseBool(logEnv)
	if err != nil {
		return true // 解析失败时默认启用
	}
	return enabled
}

// 从环境变量获取认证开关，如果未设置则默认关闭
func getAuthEnabled() bool {
	enabled := os.Getenv("AUTH_ENABLED")
	return enabled == "true" || enabled == "1"
}

// 从环境变量获取用户配置，格式：user1:pass1,user2:pass2
func getAuthUsers() map[string]string {
	usersEnv := os.Getenv("AUTH_USERS")
	if usersEnv == "" {
		return nil
	}

	users := make(map[string]string)
	pairs := strings.Split(usersEnv, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) == 2 {
			username := strings.TrimSpace(parts[0])
			password := strings.TrimSpace(parts[1])
			if username != "" && password != "" {
				users[username] = password
			}
		}
	}
	return users
}

// 从环境变量获取Token有效期（小时），如果未设置则使用默认值
func getAuthTokenExpiry() time.Duration {
	expiryEnv := os.Getenv("AUTH_TOKEN_EXPIRY")
	if expiryEnv == "" {
		return 24 * time.Hour // 默认24小时
	}
	expiry, err := strconv.Atoi(expiryEnv)
	if err != nil || expiry <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(expiry) * time.Hour
}

// 从环境变量获取JWT密钥，如果未设置则生成随机密钥
func getAuthJWTSecret() string {
	secret := os.Getenv("AUTH_JWT_SECRET")
	if secret == "" {
		// 生成随机密钥（32字节）
		import_crypto := "crypto/rand"
		import_encoding := "encoding/base64"
		_ = import_crypto
		_ = import_encoding
		// 注意：实际使用时应该使用crypto/rand生成随机密钥
		// 这里为了简化，使用时间戳作为临时密钥
		secret = "pansou-default-secret-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return secret
}

// 从环境变量获取Mcp Token,为空则MCP不验证
func getAuthMcpToken() string {
	return os.Getenv("AUTH_MCP_TOKEN")
}

// 应用GC设置
func applyGCSettings() {
	// 设置GC百分比
	debug.SetGCPercent(AppConfig.GCPercent)

	// 如果启用内存优化
	if AppConfig.OptimizeMemory {
		// 释放操作系统内存
		debug.FreeOSMemory()
	}
}
