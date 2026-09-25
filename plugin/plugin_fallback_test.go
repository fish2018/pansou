package plugin

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"pansou/model"
)

// 插件抓取失败时，框架此前直接返回错误，调用方（service.searchPlugins）在
// err != nil 分支会 continue 丢弃结果，于是插件从最终结果里凭空消失。
// refresh=true 时最明显：刷新跳过缓存，失败就直接返回空。
// 这里锁定修复后的行为：失败但有缓存时回退缓存，并返回 nil error。
func TestAsyncSearchFallsBackToCacheOnError(t *testing.T) {
	p := NewBaseAsyncPlugin("test-fallback", 1)
	keyword := "仙逆"
	// 插件级缓存键的格式只有 pluginCacheKey 一处定义，这里跟着它走，
	// 不再手拼，避免键格式一变测试就失配
	cacheKey := pluginCacheKey(p, keyword, nil)

	cached := []model.SearchResult{{UniqueID: "test-cached-1", Title: "缓存结果"}}
	apiResponseCache.Store(cacheKey, cachedResponse{
		Results:     cached,
		Timestamp:   time.Now(),
		Complete:    true,
		LastAccess:  time.Now(),
		AccessCount: 1,
	})
	t.Cleanup(func() { apiResponseCache.Delete(cacheKey) })

	failing := func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error) {
		return nil, errors.New("站点不可达")
	}

	results, err := p.AsyncSearch(keyword, failing, cacheKey, map[string]interface{}{"refresh": true})
	if err != nil {
		t.Fatalf("失败时应回退缓存而不是返回错误: %v", err)
	}
	if len(results) != 1 || results[0].UniqueID != "test-cached-1" {
		t.Fatalf("未回退到缓存结果: %#v", results)
	}
}

// 没有缓存可回退时，必须如实返回错误，不能把失败伪装成空结果。
func TestAsyncSearchReportsErrorWithoutCache(t *testing.T) {
	p := NewBaseAsyncPlugin("test-no-cache", 1)
	keyword := "不存在的关键词"
	// 插件级缓存键的格式只有 pluginCacheKey 一处定义，这里跟着它走，
	// 不再手拼，避免键格式一变测试就失配
	cacheKey := pluginCacheKey(p, keyword, nil)
	apiResponseCache.Delete(cacheKey)

	failing := func(*http.Client, string, map[string]interface{}) ([]model.SearchResult, error) {
		return nil, errors.New("站点不可达")
	}

	_, err := p.AsyncSearch(keyword, failing, cacheKey, map[string]interface{}{"refresh": true})
	if err == nil {
		t.Fatal("无缓存可回退时应返回错误，让上层计入失败统计")
	}
}
