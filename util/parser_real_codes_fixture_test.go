package util

import (
	"os"
	"testing"
)

// 第二份真实夹具：带提取码的频道页（与 testdata/tg_channel_shareAliyun_real.html 互补）。
//
// 第一份夹具只覆盖阿里云盘、正文里没有提取码；这一份是百度/夸克/迅雷混排、正文里带码的真实页面，
// 用来兜住"按链接就近取码"这条路径在真实布局上的行为。
//
// 冻结的是实测值。**注意**：这张页面上新旧逻辑结果相同——码多数直接写在 URL 的 ?pwd= 里，
// 两条路径都走 URL 分支。所以这份夹具证明的是"没有退化"，不是"修复有效"；
// 修复本身的对照实验在 password_near_test.go。
func TestParseRealCodesPageFixture(t *testing.T) {
	html, err := os.ReadFile("testdata/tg_channel_leoziyuan_with_codes.html")
	if err != nil {
		t.Fatal(err)
	}

	results, _, status, err := ParseSearchResultsWithStatus(string(html), "leoziyuan")
	if err != nil || status != ParseStatusOK {
		t.Fatalf("err=%v status=%v", err, status)
	}
	if len(results) != 20 {
		t.Fatalf("消息块 20 个，实测解析出 %d 条", len(results))
	}

	links, coded := 0, 0
	for _, r := range results {
		if len(r.Links) == 0 {
			t.Errorf("消息 %q 没有任何链接", r.MessageID)
		}
		for _, l := range r.Links {
			links++
			if l.Password != "" {
				coded++
			}
			if l.Type == "" {
				t.Errorf("链接缺少类型: %+v", l)
			}
		}
	}

	// 实测 502 条链接、其中 19 条带密码。数量变化说明解析或取码行为变了，需要先解释再改数字。
	if links != 502 {
		t.Errorf("链接总数变了（实测基线 502），实际 %d", links)
	}
	if coded != 19 {
		t.Errorf("带密码链接数变了（实测基线 19），实际 %d", coded)
	}
}
