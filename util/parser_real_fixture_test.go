package util

import (
	"os"
	"strings"
	"testing"
)

// 真实页面夹具的**行为冻结**用例。
//
// 夹具是真实抓取的 Telegram 频道搜索页（t.me/s/shareAliyun?q=...，152KB、20 个消息块），
// 不是手写的合成 HTML——手写的只能证明"我理解的页面结构能被解析"，证明不了真实页面能被解析。
//
// 冻结的是**当前行为**，不是"理想值"：下面这些数字是实测出来的。改动解析器时若这条用例挂了，
// 先判断是"改坏了"还是"确实有意改进"，不要顺手改数字——这个解析器是全量 TG 频道的共同入口，
// 一次改错影响所有频道结果。
func TestParseRealChannelPageFixture(t *testing.T) {
	html, err := os.ReadFile("testdata/tg_channel_shareAliyun_real.html")
	if err != nil {
		t.Fatal(err)
	}

	results, nextPage, status, err := ParseSearchResultsWithStatus(string(html), "shareAliyun")
	if err != nil {
		t.Fatalf("真实页面不该解析失败: %v", err)
	}
	if status != ParseStatusOK {
		t.Fatalf("真实页面状态应为 ParseStatusOK，实际 %v", status)
	}
	if nextPage != "" {
		t.Errorf("首页不该有翻页参数，实际 %q", nextPage)
	}

	// 20 个 tgme_widget_message_wrap 块，全部被识别为消息
	if len(results) != 20 {
		t.Fatalf("消息块数=20，实测解析出 %d 条", len(results))
	}

	// 阿里云盘有新旧两个域名，都要算 aliyun。写这条断言时我一开始只认 aliyundrive.com，
	// 结果 4 条 alipan.com 被判成异常——是**断言写窄了，解析器是对的**。
	domainCount := map[string]int{}
	for i, r := range results {
		if !strings.HasPrefix(r.UniqueID, "shareAliyun_") {
			t.Errorf("第 %d 条 UniqueID 缺少频道前缀: %q", i, r.UniqueID)
		}
		if r.Datetime.IsZero() {
			t.Errorf("第 %d 条时间未解析出来: %q", i, r.UniqueID)
		}
		if len(r.Links) == 0 {
			t.Errorf("第 %d 条没有任何链接: %q", i, r.UniqueID)
		}
		for _, l := range r.Links {
			if l.Type != "aliyun" {
				t.Errorf("第 %d 条链接类型应为 aliyun，实际 %q (%s)", i, l.Type, l.URL)
			}
			switch {
			case strings.HasPrefix(l.URL, "https://www.aliyundrive.com/s/"):
				domainCount["aliyundrive"]++
			case strings.HasPrefix(l.URL, "https://www.alipan.com/s/"):
				domainCount["alipan"]++
			default:
				t.Errorf("第 %d 条链接形态异常: %q", i, l.URL)
			}
		}
	}

	// 冻结域名分布：16 条 aliyundrive + 4 条 alipan（实测值）。哪天上游全换域名，这里会先响。
	if domainCount["aliyundrive"] != 16 || domainCount["alipan"] != 4 {
		t.Errorf("域名分布变了（实测基线 16/4）: %v", domainCount)
	}

	// 冻结首条的关键字段：三个字段一起变才说明结构真的变了，比"数量对得上"更敏感
	first := results[0]
	if first.UniqueID != "shareAliyun_45740" {
		t.Errorf("首条 UniqueID 变了: %q", first.UniqueID)
	}
	if first.Title != "骄阳伴我(2023) S01E01-E05 4K" {
		t.Errorf("首条标题变了: %q", first.Title)
	}
	if len(first.Links) != 1 || first.Links[0].URL != "https://www.aliyundrive.com/s/7H1P3TqeXey" {
		t.Errorf("首条链接变了: %+v", first.Links)
	}
}

// 同一个夹具换一个频道名，UniqueID 前缀必须跟着变——否则不同频道的结果会在合并时撞车。
func TestParseRealChannelPageFixtureUsesChannelInID(t *testing.T) {
	html, err := os.ReadFile("testdata/tg_channel_shareAliyun_real.html")
	if err != nil {
		t.Fatal(err)
	}

	for _, channel := range []string{"shareAliyun", "另一个频道"} {
		results, _, status, err := ParseSearchResultsWithStatus(string(html), channel)
		if err != nil || status != ParseStatusOK {
			t.Fatalf("频道 %q 解析失败: err=%v status=%v", channel, err, status)
		}
		if len(results) == 0 {
			t.Fatalf("频道 %q 没有结果", channel)
		}
		want := channel + "_"
		if !strings.HasPrefix(results[0].UniqueID, want) {
			t.Errorf("频道 %q 的 UniqueID 前缀应为 %q，实际 %q", channel, want, results[0].UniqueID)
		}
	}
}
