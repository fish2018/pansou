package util

import (
	"context"
	"testing"
)

// ext 是请求可控参数且确实改变结果形状（sdso 的 pages_per_type、cyg 的 per_page 等），
// 所以不同 ext 必须得到不同摘要——否则两个结果不同的请求会共用一条插件缓存。
func TestExtDigestDistinguishesResultShapingParams(t *testing.T) {
	base := map[string]interface{}{}
	withPages := map[string]interface{}{"pages": 5}
	withPagesPerType := map[string]interface{}{"pages_per_type": 3}

	a, b, c := ExtDigest(base), ExtDigest(withPages), ExtDigest(withPagesPerType)
	if a == b {
		t.Error("空 ext 与带 pages 的 ext 得到同一摘要")
	}
	if b == c {
		t.Error("pages 与 pages_per_type 得到同一摘要")
	}

	// 同一份 ext 必须稳定（这是缓存能命中的前提）
	if ExtDigest(withPages) != b {
		t.Error("同一份 ext 两次摘要不一致")
	}
}

// 键序不稳定会让缓存永远命中不了：必须与 map 迭代顺序无关。
func TestExtDigestIsOrderIndependent(t *testing.T) {
	m1 := map[string]interface{}{"per_page": 20, "order_by": "time", "order": "desc"}
	m2 := map[string]interface{}{"order": "desc", "per_page": 20, "order_by": "time"}
	if ExtDigest(m1) != ExtDigest(m2) {
		t.Error("键序不同的等价 ext 得到不同摘要，缓存将无法命中")
	}
}

// 拼接必须有边界：{"a":"bc"} 与 {"ab":"c"} 不能撞车。
func TestExtDigestNoBoundaryCollision(t *testing.T) {
	if ExtDigest(map[string]interface{}{"a": "bc"}) == ExtDigest(map[string]interface{}{"ab": "c"}) {
		t.Error("键值边界不同却得到同一摘要")
	}
}

// _ctx 承载的是本次请求的 context 对象（随请求而变、不可序列化），
// refresh 是"绕过缓存"开关而非结果形状参数——两者都不该影响摘要，
// 否则一次 refresh 就会让同一份结果多占一个缓存槽，且 ctx 的变化会让缓存永不命中。
func TestExtDigestIgnoresContextAndRefresh(t *testing.T) {
	plain := ExtDigest(map[string]interface{}{"pages": 2})
	withCtx := ExtDigest(map[string]interface{}{
		"pages":           2,
		ExtContextKeyName: context.Background(),
		"refresh":         true,
	})
	if plain != withCtx {
		t.Errorf("_ctx/refresh 不应改变摘要: %s vs %s", plain, withCtx)
	}
	if ExtDigest(map[string]interface{}{"_ctx": context.Background()}) != "noext" {
		t.Error("只含被忽略键的 ext 应退化为 noext")
	}
}

// 无 ext 与 nil ext 必须等价，且给出固定的 noext 标记。
func TestExtDigestNilAndEmpty(t *testing.T) {
	if ExtDigest(nil) != "noext" || ExtDigest(map[string]interface{}{}) != "noext" {
		t.Error("nil / 空 ext 应为 noext")
	}
}

// ExtContextKeyName 与 plugin.ExtContextKey 的一致性由 plugin 包中的用例锁定：
// util 不能 import plugin（会成环），所以这里只能靠常量对齐，需要断言防漂移。
func TestExtContextKeyNameValue(t *testing.T) {
	if ExtContextKeyName != "_ctx" {
		t.Errorf("ExtContextKeyName = %q，必须与 plugin.ExtContextKey 一致", ExtContextKeyName)
	}
}
