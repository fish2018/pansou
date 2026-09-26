package config

import (
	"reflect"
	"testing"
)

// 无 CHANNELS 环境变量时的默认频道。
//
// 这个值曾经与部署配置和 README 三方不一致（代码 tgsearchers6、README 表格 tgsearchers3、
// supervisor 示例 tgsearchers4），而且没有任何测试盯着它。用一个用例把它钉住：
// 改默认值就必须同时改这里和 README 的默认值表格。
func TestDefaultChannelsWhenEnvUnset(t *testing.T) {
	t.Setenv("CHANNELS", "")
	if got, want := getDefaultChannels(), []string{"tgsearchers7"}; !reflect.DeepEqual(got, want) {
		t.Errorf("无 CHANNELS 时默认频道应为 %v，实际 %v", want, got)
	}
}

func TestChannelsFromEnvAreSplit(t *testing.T) {
	t.Setenv("CHANNELS", "a,b,c")
	if got, want := getDefaultChannels(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CHANNELS 应按逗号切分，期望 %v，实际 %v", want, got)
	}
}
