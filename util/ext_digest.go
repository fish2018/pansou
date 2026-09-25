package util

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// extDigestExcludedKeys 是不参与摘要的 ext 键。
//
// 放在 util 而不是 util/cache：pansou/util 不依赖 pansou/plugin 与 pansou/util/cache，
// 而基础插件包（pansou/plugin）与键生成（pansou/util/cache）都需要这个摘要，
// 放这里两边都能用且不构成导入环。
var extDigestExcludedKeys = map[string]bool{
	// 本次搜索的上下文对象，随请求而变且不可序列化，与结果内容无关
	"_ctx": true,
	// 强制刷新是"绕过缓存"的开关，不是改变结果形状的参数；
	// 把它算进键会让同一份结果因为一次 refresh 而多占一个缓存槽
	"refresh": true,
	// 递归标记，插件内部透传用
	"_depth": true,
	// service 层注入的主缓存键。它是"结果写到哪"的地址而不是结果形状参数，
	// 且其值本身由 keyword+plugins+ext 推出，算进摘要纯属冗余。
	"_main_cache_key": true,
}

// ExtDigest 计算影响搜索结果形状的 ext 参数的摘要。
//
// 起因：插件级缓存键原先是 name:keyword，完全没有 ext。而 ext 是请求可控参数且
// 确实改变结果——sdso 用 ext["pages_per_type"]/["pages"] 决定抓取页数，cyg 用
// ["per_page"]/["page"]/["order_by"]/["order"]，miaoso 用 ["title_en"]。
// 于是 {"kw":"X","plugins":["sdso"]} 与同一个请求再加 {"pages":5} 会命中同一条缓存，
// 后者直接拿到前者的少页结果。
//
// 实现要点：键排序后逐项用"长度:内容,"编码。之所以不直接 Marshal 整个 map，
// 是因为 pansou/util/json 的 sonic 配置里 SortMapKeys 为 false，序列化 map 的
// 键序不稳定，同样的 ext 会算出不同摘要（那会让缓存永远命中不了）。
func ExtDigest(ext map[string]interface{}) string {
	if len(ext) == 0 {
		return "noext"
	}
	keys := make([]string, 0, len(ext))
	for k := range ext {
		if extDigestExcludedKeys[k] || k == ExtContextKeyName {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "noext"
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		val := fmt.Sprintf("%v", ext[k])
		sb.WriteString(strconv.Itoa(len(k)))
		sb.WriteByte(':')
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(strconv.Itoa(len(val)))
		sb.WriteByte(':')
		sb.WriteString(val)
		sb.WriteByte(',')
	}
	sum := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

// ExtContextKeyName 必须与 plugin.ExtContextKey 保持一致。
//
// 这里不能直接引用 plugin.ExtContextKey：pansou/util 不能依赖 pansou/plugin
// （plugin 依赖 util，会成环）。plugin 包内有一条断言用例锁住两者一致。
const ExtContextKeyName = "_ctx"
