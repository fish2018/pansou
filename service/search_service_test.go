package service

import (
	"strings"
	"testing"
	"time"

	"pansou/model"
)

func TestMergeResultsByTypeKeepsTelegramContentKeywordMatch(t *testing.T) {
	results := []model.SearchResult{{
		UniqueID: "testchannel_1",
		Channel:  "testchannel",
		Datetime: time.Now(),
		Title:    "📅 9月10日",
		Content:  "🎬【目标作品】简介中包含目标关键词",
		Links: []model.Link{{
			Type: "baidu",
			URL:  "https://pan.baidu.com/s/test-link",
		}},
	}}

	merged := mergeResultsByType(results, "目标关键词", nil)
	if got := len(merged["baidu"]); got != 1 {
		t.Fatalf("expected Telegram content keyword match to retain one link, got %d: %#v", got, merged)
	}
}

// issue #164：一条 TG 消息汇总了多部作品，每部作品有自己的标题和链接。
// 关键词只属于其中一部作品时，其他作品的链接不能因为"整条消息正文含关键词"被放行。
func TestMergeResultsByTypeDropsOtherWorksInMultiWorkTelegramMessage(t *testing.T) {
	content := "国剧「谷雨 更新26/30集」4K[2026]\n" +
		"夸：https://pan.quark.cn/s/26fa5bb74682\n" +
		"度https://pan.baidu.com/s/1632q8j8NVOgbEKr99ANdRA?pwd=8888\n" +
		"\n" +
		"国剧「嫁金钗 30集已完结」4K\n" +
		"夸：https://pan.quark.cn/s/3baf8a0d1dba\n" +
		"度：https://pan.baidu.com/s/1SOYyXNDAe5rdO0tJ1OAIzA?pwd=8888\n" +
		"\n" +
		"国剧「小城良方 29集已完结」4K\n" +
		"夸：https://pan.quark.cn/s/5da369d418f7\n" +
		"度： https://pan.baidu.com/s/1wM_AXd66_4xVnvBC2VWSeQ?pwd=8888\n"

	results := []model.SearchResult{{
		UniqueID: "leoziyuan_1",
		Channel:  "leoziyuan",
		Datetime: time.Now(),
		Title:    "国剧「谷雨 更新26/30集」4K[2026]",
		Content:  content,
		Links: []model.Link{
			{Type: "quark", URL: "https://pan.quark.cn/s/26fa5bb74682"},
			{Type: "baidu", URL: "https://pan.baidu.com/s/1632q8j8NVOgbEKr99ANdRA?pwd=8888"},
			{Type: "quark", URL: "https://pan.quark.cn/s/3baf8a0d1dba"},
			{Type: "baidu", URL: "https://pan.baidu.com/s/1SOYyXNDAe5rdO0tJ1OAIzA?pwd=8888"},
			{Type: "quark", URL: "https://pan.quark.cn/s/5da369d418f7"},
			{Type: "baidu", URL: "https://pan.baidu.com/s/1wM_AXd66_4xVnvBC2VWSeQ?pwd=8888"},
		},
	}}

	merged := mergeResultsByType(results, "嫁金钗", nil)
	for _, typ := range []string{"quark", "baidu"} {
		links := merged[typ]
		if len(links) != 1 {
			t.Fatalf("%s: expected only the 嫁金钗 link, got %d: %#v", typ, len(links), links)
		}
		if !strings.Contains(links[0].Note, "嫁金钗") {
			t.Fatalf("%s: kept link belongs to another work: %#v", typ, links[0])
		}
	}
}
