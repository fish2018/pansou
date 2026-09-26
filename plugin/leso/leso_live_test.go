package leso

import (
	"os"
	"testing"
	"time"
)

// 联网实测：默认跳过，设 LESO_LIVE=1 运行。
//
// 这个站点的搜索链路比较特殊：POST search.php 会 302 跳到带 searchid 的结果页，
// 必须带着同一份 cookie 再取一次才拿得到列表。链路一旦断掉，插件只会静默返回 0 条，
// 从外面看不出是"没资源"还是"接口变了"，所以留一个能直接验的入口。
//
// 直接调 searchImpl 而不是 SearchWithResult：后者走异步层，测到的是响应窗口而不是插件真实耗时。
func TestLiveSearch(t *testing.T) {
	if os.Getenv("LESO_LIVE") != "1" {
		t.Skip("默认跳过联网测试，需要时设 LESO_LIVE=1")
	}
	keywords := []string{"凡人修仙传", "遮天", "庆余年"}
	if custom := os.Getenv("LESO_LIVE_KEYWORD"); custom != "" {
		keywords = []string{custom}
	}
	p := NewPlugin()
	for _, keyword := range keywords {
		// 先单独看一眼搜索页本身返回多少条目：这个数决定详情页的抓取量，
		// 也用来分辨"站点真的只有几条"和"被频率限制掐了"。
		doc, err := p.fetchSearch(p.client, keyword)
		if err != nil {
			t.Fatalf("%s 搜索页请求失败: %v", keyword, err)
		}
		items := doc.Find(".slst li.pbw").Length()
		start := time.Now()
		results, err := p.searchImpl(p.client, keyword, nil)
		cost := time.Since(start)
		if err != nil {
			t.Fatalf("%s 搜索失败: %v", keyword, err)
		}
		links := 0
		for _, r := range results {
			links += len(r.Links)
		}
		t.Logf("%s: 搜索页 %d 条 -> 产出帖子 %d 条, 链接 %d 条, 插件耗时 %s",
			keyword, items, len(results), links, cost.Round(time.Millisecond))
		if len(results) == 0 {
			t.Errorf("%s 返回 0 条，搜索链路可能已失效", keyword)
		}
	}
}
