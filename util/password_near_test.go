package util

import (
	"strings"
	"testing"
)

const passwordFixtureBody = "百度网盘：https://pan.baidu.com/s/1AbCdEf 提取码：1234\n" +
	"天翼云盘：https://cloud.189.cn/t/xyzabc 访问码：abcd\n" +
	"UC网盘：https://drive.uc.cn/s/uc123 提取码：uc99\n" +
	"123网盘：https://www.123pan.com/s/pan123 提取码：p123\n" +
	"115网盘：https://115.com/s/abc115 访问码：p115\n" +
	"阿里云盘：https://www.aliyundrive.com/s/ali999"

// 对照组：证明"按链接就近取值"之前的行为确实是"整条消息取首个码"。
//
// 没有这条对照，上面的修复就只是在断言里改数字——看不出探针到底测没测到问题。
func TestExtractCodeNearControlOldBehaviorWasFirstMatch(t *testing.T) {
	for _, linkURL := range ExtractNetDiskLinks(passwordFixtureBody) {
		if got := ExtractPassword(passwordFixtureBody, linkURL); got != "1234" {
			t.Fatalf("对照实验失效：旧逻辑对 %q 竟返回 %q（预期所有链接都是首个码 1234）", linkURL, got)
		}
	}
	t.Log("对照实验符合预期：旧逻辑对六个链接一律返回首个码 1234")
}

// 修复后的行为：每条链接拿到自己附近的码。
func TestExtractCodeNearPerLink(t *testing.T) {
	want := map[string]string{
		"https://pan.baidu.com/s/1AbCdEf":      "1234",
		"https://cloud.189.cn/t/xyzabc":        "abcd",
		"https://drive.uc.cn/s/uc123":          "uc99",
		"https://123pan.com/s/pan123":          "p123",
		"https://115.com/s/abc115":             "p115",
		"https://www.aliyundrive.com/s/ali999": "",
	}
	for linkURL, wantPW := range want {
		if got := extractCodeNear(linkURL, passwordFixtureBody); got != wantPW {
			t.Errorf("%q 的就近提取码应为 %q，实际 %q", linkURL, wantPW, got)
		}
	}
}

// 提取码写在下一行也必须取到——真实帖子里很常见。
func TestExtractCodeNearCrossesNewline(t *testing.T) {
	body := "链接：https://pan.baidu.com/s/1NextLine\n提取码：9xyz\n"
	if got := extractCodeNear("https://pan.baidu.com/s/1NextLine", body); got != "9xyz" {
		t.Errorf("换行后的提取码没取到，实际 %q", got)
	}
}

// 两条链接挨得很近时不能串码：窗口必须在下一个链接处截断。
func TestExtractCodeNearDoesNotCrossIntoNextLink(t *testing.T) {
	// 第一条链接自己**没有**码，紧跟着的第二条有；第一条绝不能拿到第二条的码
	body := "https://pan.baidu.com/s/1First https://drive.uc.cn/s/ucSecond 提取码：abcd"
	if got := extractCodeNear("https://pan.baidu.com/s/1First", body); got != "" {
		t.Errorf("第一条链接不该拿到下一条的码，实际 %q", got)
	}
	if got := extractCodeNear("https://drive.uc.cn/s/ucSecond", body); got != "abcd" {
		t.Errorf("第二条链接应拿到自己的码 abcd，实际 %q", got)
	}
}

// 锚点被规范化过（去 www.、去 scheme）时仍然要能定位。
func TestExtractCodeNearHandlesNormalizedAnchor(t *testing.T) {
	body := "网盘：https://www.123pan.com/s/pan123 提取码：p123"
	for _, anchor := range []string{
		"https://www.123pan.com/s/pan123",
		"https://123pan.com/s/pan123",
		"123pan.com/s/pan123",
		"/s/pan123",
	} {
		if got := extractCodeNear(anchor, body); got != "p123" {
			t.Errorf("锚点形态 %q 未取到提取码，实际 %q", anchor, got)
		}
	}
}

// 长上下文里定位不到链接时返回空，让调用方回退旧逻辑——不猜、也不返回半截结果。
func TestExtractCodeNearUnanchoredLongContextReturnsEmpty(t *testing.T) {
	long := strings.Repeat("这是一段很长的正文，提到很多别的东西。", 20) + "提取码：1234"
	if got := extractCodeNear("https://pan.baidu.com/s/1NotPresent", long); got != "" {
		t.Errorf("定位不到锚点时应返回空，实际 %q", got)
	}
}

// passwordFor 的分工：就近取到就用它，取不到必须回退（不能因为附近没有就返回空）。
func TestPasswordForFallsBackInsteadOfEmpty(t *testing.T) {
	// 链接在正文里定位不到，但整条消息里确实有一个码写在别处（末尾），旧逻辑找得到
	body := strings.Repeat("正文内容。", 30) + "\n全网盘列表：https://pan.baidu.com/s/1Far 提取码：7788"
	if got := passwordFor("https://pan.baidu.com/s/1Far", "另一个完全不同的短上下文", body); got != "7788" {
		t.Errorf("回退逻辑应取到 7788，实际 %q", got)
	}

	// 短上下文（按钮标签）里能就近取到
	if got := passwordFor("https://www.alipan.com/s/btn", "按钮 提取码：bt01", body); got != "bt01" {
		t.Errorf("短上下文应取到 bt01，实际 %q", got)
	}
}

// 窗口末尾不能切出半个多字节字符。
func TestExtractCodeNearDoesNotSplitRune(t *testing.T) {
	body := "网盘：https://pan.baidu.com/s/1Rune 提取码" + strings.Repeat("字", 40) + "：zz99"
	// 只要能正常返回（不 panic、不产生非法 UTF-8 结果）即可
	got := extractCodeNear("https://pan.baidu.com/s/1Rune", body)
	if !utf8Valid(got) {
		t.Errorf("返回值不是合法 UTF-8: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
