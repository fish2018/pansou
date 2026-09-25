package plugin

import (
	"testing"

	"pansou/util"
)

// util.ExtContextKeyName 是 util 侧为避免导入环而重复声明的常量，
// 必须与 plugin.ExtContextKey 保持一致，否则 ext 摘要会把本次请求的
// context 对象算进去，导致缓存永不命中。
func TestExtContextKeyNameMatchesPluginConstant(t *testing.T) {
	if util.ExtContextKeyName != ExtContextKey {
		t.Errorf("util.ExtContextKeyName = %q 与 plugin.ExtContextKey = %q 不一致",
			util.ExtContextKeyName, ExtContextKey)
	}
}
