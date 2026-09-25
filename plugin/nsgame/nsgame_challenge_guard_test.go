package nsgame

import "testing"

// difficultyBits 直接来自第三方响应 JSON，是"摘要前缀零位数"，而 sum 是 [32]byte。
// 修复前 fullBytes = difficultyBits/8 无上界：difficultyBits >= 256 时 sum[i] 越界、
// 257..263 时 sum[fullBytes] 越界；这两处都在没有 recover 的 goroutine 里，会导致
// 整个进程退出而不是单次请求失败。
func TestSolveChallengeRejectsOutOfRangeDifficulty(t *testing.T) {
	// 这些值在修复前会 panic（进程级），修复后必须安全返回空串
	for _, bits := range []int{257, 260, 263, 264, 512, 4096, -1, -8} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("difficultyBits=%d 触发 panic: %v", bits, r)
				}
			}()
			if got := solveChallenge("chal", bits); got != "" {
				t.Errorf("difficultyBits=%d 应返回空串，实际 %q", bits, got)
			}
		}()
	}
}

// 合法范围内的最小难度必须仍能正常求解，避免把守卫做成"一律拒绝"。
func TestSolveChallengeAcceptsZeroDifficulty(t *testing.T) {
	got := solveChallenge("chal", 0)
	if got == "" {
		t.Fatal("难度 0 应立即可解，实际返回空串")
	}
}
