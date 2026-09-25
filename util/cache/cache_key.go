package cache

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"pansou/plugin"
)

// tgSearchParserVersion 用于在 Telegram 解析规则变化时主动使旧缓存失效。
// v2 开始纳入 inline keyboard 按钮中的网盘链接。
const tgSearchParserVersion = "v2"

// 预计算的哈希值映射
var (
	channelHashCache sync.Map // 存储频道列表哈希
	pluginHashCache  sync.Map // 存储插件列表哈希

	// 预先计算的常用列表哈希值
	precomputedHashes sync.Map

	// 所有插件名称的哈希值
	allPluginsHash string
	// 所有频道名称的哈希值
	allChannelsHash string

	// 上面两个哈希缓存的条目计数，用于给它们加上限（见 storeListHash）
	channelHashCacheEntries int64
	pluginHashCacheEntries  int64
)

// listHashCacheLimit 是 channelHashCache / pluginHashCache 的条目上限。
// 这两个缓存原先只增不删，而键是用户请求参数拼出来的（channels、plugins 都来自
// 查询串或 POST body），任意一个新组合都会常驻，进程生命周期内单调增长。
// 到达上限后整体重置：它只是哈希值的旁路缓存，重建代价远小于无界增长。
const listHashCacheLimit = 4096

// storeListHash 带上限地写入列表哈希缓存。
func storeListHash(c *sync.Map, counter *int64, key, value string) {
	if atomic.LoadInt64(counter) >= listHashCacheLimit {
		c.Range(func(k, _ interface{}) bool {
			c.Delete(k)
			return true
		})
		atomic.StoreInt64(counter, 0)
	}
	if _, loaded := c.LoadOrStore(key, value); !loaded {
		atomic.AddInt64(counter, 1)
	}
}

// 初始化预计算的哈希值
func init() {
	// 预计算空列表的哈希值
	precomputedHashes.Store("empty_channels", "all")

	// 预计算所有插件的哈希值
	allPlugins := plugin.GetRegisteredPlugins()
	allPluginNames := make([]string, 0, len(allPlugins))
	for _, p := range allPlugins {
		allPluginNames = append(allPluginNames, p.Name())
	}
	sort.Strings(allPluginNames)
	allPluginsHash = calculateListHash(allPluginNames)
	precomputedHashes.Store("all_plugins", allPluginsHash)

	// 预计算所有频道的哈希值（这里假设有一个全局频道列表）
	// 注意：如果没有全局频道列表，可以使用一个默认值
	allChannelsHash = "all"
	precomputedHashes.Store("all_channels", allChannelsHash)
}

// GenerateTGCacheKey 为TG搜索生成缓存键
func GenerateTGCacheKey(keyword string, channels []string) string {
	// 关键词标准化
	normalizedKeyword := strings.ToLower(strings.TrimSpace(keyword))

	// 获取频道列表哈希
	channelsHash := getChannelsHash(channels)

	// 生成TG搜索特定的缓存键
	keyStr := fmt.Sprintf("tg:%s:%s:%s", tgSearchParserVersion, normalizedKeyword, channelsHash)
	hash := md5.Sum([]byte(keyStr))
	return hex.EncodeToString(hash[:])
}

// GeneratePluginCacheKey 为插件搜索生成缓存键。
//
// extDigest 是影响结果形状的 ext 参数摘要（util.ExtDigest）。此前该键只有
// keyword + plugins，而 ext 同样改变结果，于是 ext 不同的两个请求会共用一条缓存。
func GeneratePluginCacheKey(keyword string, plugins []string, extDigest string) string {
	// 关键词标准化
	normalizedKeyword := strings.ToLower(strings.TrimSpace(keyword))

	// 获取插件列表哈希
	pluginsHash := getPluginsHash(plugins)

	// 生成插件搜索特定的缓存键
	keyStr := fmt.Sprintf("plugin:%s:%s:%s", normalizedKeyword, pluginsHash, extDigest)
	hash := md5.Sum([]byte(keyStr))
	return hex.EncodeToString(hash[:])
}

// GenerateCacheKey 根据所有影响搜索结果的参数生成缓存键
func GenerateCacheKey(keyword string, channels []string, sourceType string, plugins []string) string {
	// 关键词标准化
	normalizedKeyword := strings.ToLower(strings.TrimSpace(keyword))

	// 获取频道列表哈希
	channelsHash := getChannelsHash(channels)

	// 源类型处理
	if sourceType == "" {
		sourceType = "all"
	}

	// 插件参数规范化处理
	var pluginsHash string
	if sourceType == "tg" {
		// 对于只搜索Telegram的请求，忽略插件参数
		pluginsHash = "none"
	} else {
		// 获取插件列表哈希
		pluginsHash = getPluginsHash(plugins)
	}

	// 生成最终缓存键
	keyStr := fmt.Sprintf("%s:%s:%s:%s", normalizedKeyword, channelsHash, sourceType, pluginsHash)
	hash := md5.Sum([]byte(keyStr))
	return hex.EncodeToString(hash[:])
}

// 获取或计算频道哈希
func getChannelsHash(channels []string) string {
	if channels == nil || len(channels) == 0 {
		// 使用预计算的所有频道哈希
		if hash, ok := precomputedHashes.Load("all_channels"); ok {
			return hash.(string)
		}
		return allChannelsHash
	}

	// 对于小型列表，直接使用字符串连接
	if len(channels) < 5 {
		channelsCopy := make([]string, len(channels))
		copy(channelsCopy, channels)
		sort.Strings(channelsCopy)

		// 直接返回排序后的规范化编码
		return encodeList(channelsCopy)
	}

	// 生成排序后的字符串用作键
	channelsCopy := make([]string, len(channels))
	copy(channelsCopy, channels)
	sort.Strings(channelsCopy)
	key := encodeList(channelsCopy)

	// 尝试从缓存获取
	if hash, ok := channelHashCache.Load(key); ok {
		return hash.(string)
	}

	// 计算哈希
	hash := calculateListHash(channelsCopy)

	// 存入缓存
	storeListHash(&channelHashCache, &channelHashCacheEntries, key, hash)
	return hash
}

// 获取或计算插件哈希
func getPluginsHash(plugins []string) string {
	// 检查是否为空列表
	if plugins == nil || len(plugins) == 0 {
		// 使用预计算的所有插件哈希
		if hash, ok := precomputedHashes.Load("all_plugins"); ok {
			return hash.(string)
		}
		return allPluginsHash
	}

	// 检查是否有空字符串元素
	hasNonEmptyPlugin := false
	for _, p := range plugins {
		if p != "" {
			hasNonEmptyPlugin = true
			break
		}
	}

	// 如果全是空字符串，也视为空列表
	if !hasNonEmptyPlugin {
		if hash, ok := precomputedHashes.Load("all_plugins"); ok {
			return hash.(string)
		}
		return allPluginsHash
	}

	// 对于小型列表，直接使用字符串连接
	if len(plugins) < 5 {
		pluginsCopy := make([]string, 0, len(plugins))
		for _, p := range plugins {
			if p != "" { // 忽略空字符串
				pluginsCopy = append(pluginsCopy, p)
			}
		}
		sort.Strings(pluginsCopy)

		// 直接返回排序后的规范化编码
		return encodeList(pluginsCopy)
	}

	// 生成排序后的字符串用作键，忽略空字符串
	pluginsCopy := make([]string, 0, len(plugins))
	for _, p := range plugins {
		if p != "" { // 忽略空字符串
			pluginsCopy = append(pluginsCopy, p)
		}
	}
	sort.Strings(pluginsCopy)
	key := encodeList(pluginsCopy)

	// 尝试从缓存获取
	if hash, ok := pluginHashCache.Load(key); ok {
		return hash.(string)
	}

	// 计算哈希
	hash := calculateListHash(pluginsCopy)

	// 存入缓存
	storeListHash(&pluginHashCache, &pluginHashCacheEntries, key, hash)
	return hash
}

// encodeList 把列表编码成无歧义的规范化字符串。
//
// 原实现是 strings.Join(items, ",")，元素里含逗号就会与"多个元素"混同：
//
//	["a","b"] 与 ["a,b"] 的编码都是 "a,b"
//
// 而 channels / plugins 都来自请求参数（POST body 会直接反序列化成 []string），
// 含逗号的元素是可以构造的，于是两个语义完全不同的请求会命中同一条缓存、
// 互相拿到对方的结果。原 calculateListHash 逐项 h.Write 却不写元素边界，
// 有同样的问题（["ab","c"] 与 ["a","bc"] 的摘要相同）。
//
// 改成"长度:内容,"的逐项编码后，任一编码结果都能唯一还原出原列表。
func encodeList(items []string) string {
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(strconv.Itoa(len(item)))
		sb.WriteByte(':')
		sb.WriteString(item)
		sb.WriteByte(',')
	}
	return sb.String()
}

// 计算列表的哈希值
func calculateListHash(items []string) string {
	sum := md5.Sum([]byte(encodeList(items)))
	return hex.EncodeToString(sum[:])
}

// GenerateCacheKeyV2 根据所有影响搜索结果的参数生成缓存键
// 为保持向后兼容，保留原函数，但标记为已弃用
func GenerateCacheKeyV2(keyword string, channels []string, sourceType string, plugins []string) string {
	// 关键词标准化：去除首尾空格，转为小写
	normalizedKeyword := strings.ToLower(strings.TrimSpace(keyword))

	// 频道处理
	var channelsStr string
	if channels != nil && len(channels) > 0 {
		channelsCopy := make([]string, len(channels))
		copy(channelsCopy, channels)
		sort.Strings(channelsCopy)
		channelsStr = strings.Join(channelsCopy, ",")
	} else {
		channelsStr = "all"
	}

	// 插件处理
	var pluginsStr string
	if plugins != nil && len(plugins) > 0 {
		pluginsCopy := make([]string, len(plugins))
		copy(pluginsCopy, plugins)
		sort.Strings(pluginsCopy)
		pluginsStr = strings.Join(pluginsCopy, ",")
	} else {
		pluginsStr = "all"
	}

	// 源类型处理
	if sourceType == "" {
		sourceType = "all"
	}

	// 生成缓存键字符串
	keyStr := fmt.Sprintf("v2:%s:%s:%s:%s", normalizedKeyword, channelsStr, sourceType, pluginsStr)

	// 计算MD5哈希
	hash := md5.Sum([]byte(keyStr))
	return hex.EncodeToString(hash[:])
}

// GenerateCacheKeyLegacy 根据查询和过滤器生成缓存键
// 为保持向后兼容，保留原函数，但重命名为更清晰的名称
func GenerateCacheKeyLegacy(query string, filters map[string]string) string {
	// 如果只需要基于关键词的缓存，不考虑过滤器，调用新函数
	if filters == nil || len(filters) == 0 {
		return GenerateCacheKey(query, nil, "", nil)
	}

	// 创建包含查询和所有过滤器的字符串
	keyStr := query

	// 按字母顺序排序过滤器键，确保相同的过滤器集合总是产生相同的键
	var keys []string
	for k := range filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 添加过滤器到键字符串
	for _, k := range keys {
		keyStr += "|" + k + "=" + filters[k]
	}

	// 计算MD5哈希
	hash := md5.Sum([]byte(keyStr))
	return hex.EncodeToString(hash[:])
}
