package cache

import (
	"strings"
	"testing"
)

// channels / plugins 都来自请求参数（POST body 直接反序列化成 []string），
// 元素里含逗号是可以构造的。原实现用 strings.Join(items, ",") 作键，
// ["a","b"] 与 ["a,b"] 会得到同一个键，两个语义完全不同的请求因此共用一条
// 缓存、互相拿到对方的结果。
func TestCacheKeyNoCollisionAcrossCommaInElement(t *testing.T) {
	// 对照组：旧编码确实会碰撞，说明这个用例针对的是真实缺陷
	if strings.Join([]string{"a", "b"}, ",") != strings.Join([]string{"a,b"}, ",") {
		t.Fatal("对照前提不成立：旧编码并未碰撞，本用例无意义")
	}

	// 修复后必须区分开（小列表走直接编码分支）
	if GenerateTGCacheKey("kw", []string{"a", "b"}) == GenerateTGCacheKey("kw", []string{"a,b"}) {
		t.Error("频道小列表仍碰撞: [\"a\",\"b\"] 与 [\"a,b\"] 得到同一缓存键")
	}
	if GeneratePluginCacheKey("kw", []string{"a", "b"}, "noext") == GeneratePluginCacheKey("kw", []string{"a,b"}, "noext") {
		t.Error("插件小列表仍碰撞: [\"a\",\"b\"] 与 [\"a,b\"] 得到同一缓存键")
	}
}

// 大列表（>=5）走的是 md5 摘要分支，原实现逐项 h.Write 却不写元素边界，
// ["ab","c","d","e","f"] 与 ["a","bc","d","e","f"] 会得到同一摘要。
func TestCacheKeyNoCollisionAcrossElementBoundary(t *testing.T) {
	left := []string{"ab", "c", "d", "e", "f"}
	right := []string{"a", "bc", "d", "e", "f"}
	if len(left) < 5 || len(right) < 5 {
		t.Fatal("本用例针对 >=5 元素的摘要分支，样例必须够长")
	}
	if calculateListHash(left) == calculateListHash(right) {
		t.Error("元素边界不同却得到同一摘要: 摘要未包含元素边界")
	}
	if GenerateTGCacheKey("kw", left) == GenerateTGCacheKey("kw", right) {
		t.Error("频道大列表仍碰撞")
	}
	if GeneratePluginCacheKey("kw", left, "noext") == GeneratePluginCacheKey("kw", right, "noext") {
		t.Error("插件大列表仍碰撞")
	}
}

// 编码必须无歧义：不同列表一定得到不同编码。
func TestEncodeListIsUnambiguous(t *testing.T) {
	groups := [][]string{
		{"a", "b"},
		{"a,b"},
		{"ab"},
		{"a", "b,c"},
		{"a,", "b"},
	}
	seen := map[string][]string{}
	for _, g := range groups {
		enc := encodeList(g)
		if prev, ok := seen[enc]; ok {
			t.Errorf("编码歧义: %v 与 %v 都编码为 %q", prev, g, enc)
			continue
		}
		seen[enc] = g
	}
}

// 元素顺序不影响结果（列表在编码前会排序），这点必须保持，
// 否则同一次搜索因参数顺序不同会重复打上游。
func TestCacheKeyOrderInsensitive(t *testing.T) {
	if GenerateTGCacheKey("kw", []string{"b", "a"}) != GenerateTGCacheKey("kw", []string{"a", "b"}) {
		t.Error("频道顺序不同导致缓存键不同")
	}
	if GeneratePluginCacheKey("kw", []string{"b", "a"}, "noext") != GeneratePluginCacheKey("kw", []string{"a", "b"}, "noext") {
		t.Error("插件顺序不同导致缓存键不同")
	}
}

// 插件缓存键原先只有 keyword + plugins，完全没有 ext。而 ext 确实改变结果形状
// （sdso 的 pages_per_type、cyg 的 per_page 等），于是 ext 不同的两个请求会共用
// 一条缓存。本用例同时给出对照：只要摘要相同，键就会塌缩成同一个——这正是修复前
// 所有请求的处境。
func TestPluginCacheKeyIncludesExtDigest(t *testing.T) {
	plugins := []string{"sdso"}
	base := plugins
	withPages := plugins

	noExt := "noext"
	pagesDigest := "d41d8cd98f00b204e9800998ecf8427e"

	// 对照：摘要相同 → 键相同（等价于修复前"键里没有 ext"的状态）
	if GeneratePluginCacheKey("X", base, noExt) != GeneratePluginCacheKey("X", withPages, noExt) {
		t.Fatal("对照前提不成立：摘要相同却得到不同键")
	}

	// 修复后：ext 不同 → 摘要不同 → 键必须不同
	if GeneratePluginCacheKey("X", base, noExt) == GeneratePluginCacheKey("X", withPages, pagesDigest) {
		t.Error("ext 摘要不同却得到同一缓存键，不同 ext 的请求仍会互相拿到对方结果")
	}
}
