package util

import (
	"testing"

	"pansou/model"
)

func linksOf(urls ...string) []model.Link {
	out := make([]model.Link, len(urls))
	for i, u := range urls {
		out[i] = model.Link{URL: u}
	}
	return out
}

// issue #164：多作品汇总帖，每部作品一行标题 + 若干"夸：/度"链接行，
// 每个链接必须拿到自己那部作品的标题，而不是整条消息的首行。
func TestExtractWorkTitlesForLinksMultiWorkDigest(t *testing.T) {
	text := "国剧「十万狂花入梦来 更新07/30集」4K\n" +
		"夸克：https://pan.quark.cn/s/5f9a034b699e\n" +
		"百度：https://pan.baidu.com/s/1F-Q-uuAOloxaDI4F0YROUQ?pwd=8888\n" +
		"\n" +
		"国剧「谷雨 更新26/30集」4K[2026]\n" +
		"夸：https://pan.quark.cn/s/26fa5bb74682\n" +
		"度https://pan.baidu.com/s/1632q8j8NVOgbEKr99ANdRA?pwd=8888\n" +
		"\n" +
		"韩剧：不着边际的你  06/16 集) (2026) \n" +
		"夸：https://pan.quark.cn/s/372c524f01ca\n" +
		"度： https://pan.baidu.com/s/1C2Wy0scH3S3XX3uEl0Aqpg?pwd=8888\n" +
		"\n" +
		"国剧「嫁金钗 30集已完结」4K\n" +
		"夸：https://pan.quark.cn/s/3baf8a0d1dba\n" +
		"度：https://pan.baidu.com/s/1SOYyXNDAe5rdO0tJ1OAIzA?pwd=8888\n"

	links := linksOf(
		"https://pan.quark.cn/s/5f9a034b699e",
		"https://pan.baidu.com/s/1F-Q-uuAOloxaDI4F0YROUQ?pwd=8888",
		"https://pan.quark.cn/s/26fa5bb74682",
		"https://pan.baidu.com/s/1632q8j8NVOgbEKr99ANdRA?pwd=8888",
		"https://pan.quark.cn/s/372c524f01ca",
		"https://pan.baidu.com/s/1C2Wy0scH3S3XX3uEl0Aqpg?pwd=8888",
		"https://pan.quark.cn/s/3baf8a0d1dba",
		"https://pan.baidu.com/s/1SOYyXNDAe5rdO0tJ1OAIzA?pwd=8888",
	)
	want := []string{
		"国剧「十万狂花入梦来 更新07/30集」4K", "国剧「十万狂花入梦来 更新07/30集」4K",
		"国剧「谷雨 更新26/30集」4K[2026]", "国剧「谷雨 更新26/30集」4K[2026]",
		"韩剧：不着边际的你  06/16 集) (2026)", "韩剧：不着边际的你  06/16 集) (2026)",
		"国剧「嫁金钗 30集已完结」4K", "国剧「嫁金钗 30集已完结」4K",
	}

	got := extractWorkTitlesForLinks(links, text, "国剧「十万狂花入梦来 更新07/30集」4K")
	for i := range want {
		if got[i].WorkTitle != want[i] {
			t.Errorf("link %d (%s): WorkTitle=%q, want %q", i, got[i].URL, got[i].WorkTitle, want[i])
		}
	}
}

// 单作品帖链接多于 4 个时，"链接：""🔑 提取码"等行不能被当成作品标题。
func TestExtractWorkTitlesForLinksSingleWorkManyLinks(t *testing.T) {
	text := "名称：嫁金钗（2026）【4K】\n" +
		"\n" +
		"描述：阿丑原是街上倒夜香的低贱少女\n" +
		"\n" +
		"链接：\n" +
		"🔗 夸克：https://pan.quark.cn/s/8da802123507\n" +
		"🔗 百度网盘：https://pan.baidu.com/s/1OKiBSyMnJbj8IwH2ugQs-A?pwd=yptv\n" +
		"🔑 提取码：yptv\n" +
		"🔗 迅雷云盘：https://pan.xunlei.com/s/VP4FCEq23pnrgzQMkKxIqibRA1?pwd=7hk3\n" +
		"🔑 提取码：7hk3\n" +
		"🔗 UC：https://drive.uc.cn/s/abcdef123456\n" +
		"🔗 阿里：https://www.alipan.com/s/abcdefghijk\n"

	links := linksOf(
		"https://pan.quark.cn/s/8da802123507",
		"https://pan.baidu.com/s/1OKiBSyMnJbj8IwH2ugQs-A?pwd=yptv",
		"https://pan.xunlei.com/s/VP4FCEq23pnrgzQMkKxIqibRA1?pwd=7hk3",
		"https://drive.uc.cn/s/abcdef123456",
		"https://www.alipan.com/s/abcdefghijk",
	)
	const def = "嫁金钗（2026）【4K】"
	for _, l := range extractWorkTitlesForLinks(links, text, def) {
		if l.WorkTitle != def {
			t.Errorf("%s: WorkTitle=%q, want default %q", l.URL, l.WorkTitle, def)
		}
	}
}
