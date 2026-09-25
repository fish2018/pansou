package gying

import (
	"testing"

	"pansou/model"
	"pansou/plugin"
)

func newTestPlugin() *GyingPlugin {
	return &GyingPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPluginWithFilter("gying", 3, true),
	}
}

func seedCache(p *GyingPlugin, keyword string, results []model.SearchResult) {
	p.searchCache.Store(keyword, model.PluginSearchResult{Results: results, IsFinal: true})
}

// 用户看到的现象是"一开 refresh 结果就没了"：因为刷新会跳过插件缓存，
// 而抓取失败时插件返回空结果，把缓存里的有效数据挡在了外面。
// 这个用例锁定修复后的行为：刷新拿不到新数据时回退缓存，而不是返回空。
func TestRefreshFallsBackToCacheWhenFetchUnavailable(t *testing.T) {
	p := newTestPlugin()
	cached := []model.SearchResult{{UniqueID: "gying-1", Title: "仙逆 缓存结果"}}
	seedCache(p, "仙逆", cached)

	// 无可用账号，等价于抓取不可用
	got, err := p.SearchWithResult("仙逆", map[string]interface{}{"refresh": true})
	if err != nil {
		t.Fatalf("刷新失败时不应直接报错: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].UniqueID != "gying-1" {
		t.Fatalf("刷新拿不到新数据时应回退缓存, 实际: %#v", got.Results)
	}
}

// 非刷新路径的缓存命中行为不能被改动。
func TestNonRefreshStillServesCache(t *testing.T) {
	p := newTestPlugin()
	seedCache(p, "遮天", []model.SearchResult{{UniqueID: "gying-2", Title: "遮天 缓存结果"}})

	got, err := p.SearchWithResult("遮天", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].UniqueID != "gying-2" {
		t.Fatalf("缓存命中异常: %#v", got.Results)
	}
}

// 没有任何缓存可用时，保持"空结果且不报错"——关键词本身没有匹配是合法情况，
// 不能当成失败上报。
func TestNoCacheAndNoFetchReturnsEmptyWithoutError(t *testing.T) {
	p := newTestPlugin()

	got, err := p.SearchWithResult("不存在的关键词", map[string]interface{}{"refresh": true})
	if err != nil {
		t.Fatalf("无缓存可回退时不应报错: %v", err)
	}
	if len(got.Results) != 0 {
		t.Fatalf("期望空结果, 实际: %#v", got.Results)
	}
}
