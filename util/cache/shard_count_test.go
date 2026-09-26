package cache

import "testing"

// README 一直把 SHARD_COUNT 列在环境变量表里，代码却从没读过它——文档承诺了不存在的开关。
// 这组用例把承诺固定下来。
func TestResolveShardCountHonoursEnv(t *testing.T) {
	t.Setenv("SHARD_COUNT", "32")
	if got := resolveShardCount(4); got != 32 {
		t.Errorf("SHARD_COUNT=32 应生效，实际 %d", got)
	}

	// 非 2 的幂要向上取整（分片按掩码取模，必须是 2 的幂）
	t.Setenv("SHARD_COUNT", "17")
	if got := resolveShardCount(4); got != 32 {
		t.Errorf("17 应向上取到 32，实际 %d", got)
	}
}

// 非法/越界值必须回落或夹紧，不能让一个手滑的 SHARD_COUNT=0 变成零分片缓存。
func TestResolveShardCountRejectsBadValues(t *testing.T) {
	for _, bad := range []string{"0", "-3", "abc", "  "} {
		t.Setenv("SHARD_COUNT", bad)
		got := resolveShardCount(4) // CPU 推算 = 8
		if got != 8 {
			t.Errorf("SHARD_COUNT=%q 应回落到 CPU 推算值 8，实际 %d", bad, got)
		}
	}

	t.Setenv("SHARD_COUNT", "100000")
	if got := resolveShardCount(4); got != 64 {
		t.Errorf("超大值应夹到 64，实际 %d", got)
	}
	t.Setenv("SHARD_COUNT", "1")
	if got := resolveShardCount(4); got != 4 {
		t.Errorf("过小值应抬到 4，实际 %d", got)
	}
}

func TestResolveShardCountDefaultsToCPU(t *testing.T) {
	t.Setenv("SHARD_COUNT", "")
	if got := resolveShardCount(4); got != 8 {
		t.Errorf("未设置时应按 CPU*2 推算，实际 %d", got)
	}
	// 极多核时封顶
	if got := resolveShardCount(256); got != 64 {
		t.Errorf("多核应封顶 64，实际 %d", got)
	}
}
