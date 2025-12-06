package plugin

import (
	"fmt"

	"pansou/model"
)

// ============================================================
// 缓存管理函数
// ============================================================

// updateMainCache 更新主缓存系统（兼容性方法，默认IsFinal=true）
func (p *BaseAsyncPlugin) updateMainCache(cacheKey string, results []model.SearchResult) {
	p.updateMainCacheWithFinal(cacheKey, results, true)
}

// updateMainCacheWithFinal 更新主缓存系统，支持IsFinal参数
func (p *BaseAsyncPlugin) updateMainCacheWithFinal(cacheKey string, results []model.SearchResult, isFinal bool) {
	// 如果主缓存更新函数为空或缓存键为空，直接返回
	if p.mainCacheUpdater == nil || cacheKey == "" {
		return
	}

	// 🚀 优化：如果新结果为空，跳过缓存更新（避免无效操作）
	if len(results) == 0 {
		return
	}

	// 🔥 增强防重复更新机制 - 使用数据哈希确保真正的去重
	// 生成结果数据的简单哈希标识
	dataHash := fmt.Sprintf("%d_%d", len(results), results[0].UniqueID)
	if len(results) > 1 {
		dataHash += fmt.Sprintf("_%d", results[len(results)-1].UniqueID)
	}
	updateKey := fmt.Sprintf("final_%s_%s_%s_%t", p.name, cacheKey, dataHash, isFinal)

	// 检查是否已经处理过相同的数据
	if p.hasUpdatedFinalCache(updateKey) {
		return
	}

	// 标记已更新
	p.markFinalCacheUpdated(updateKey)

	// 🔧 恢复异步插件缓存更新，使用修复后的统一序列化
	// 传递原始数据，由主程序负责GOB序列化
	if p.mainCacheUpdater != nil {
		err := p.mainCacheUpdater(cacheKey, results, p.cacheTTL, isFinal, p.currentKeyword)
		if err != nil {
			fmt.Printf("❌ [%s] 主缓存更新失败: %s | 错误: %v\n", p.name, cacheKey, err)
		}
	}
}

// hasUpdatedFinalCache 检查是否已经更新过指定的最终结果缓存
func (p *BaseAsyncPlugin) hasUpdatedFinalCache(updateKey string) bool {
	p.finalUpdateMutex.RLock()
	defer p.finalUpdateMutex.RUnlock()
	return p.finalUpdateTracker[updateKey]
}

// markFinalCacheUpdated 标记已更新指定的最终结果缓存
func (p *BaseAsyncPlugin) markFinalCacheUpdated(updateKey string) {
	p.finalUpdateMutex.Lock()
	defer p.finalUpdateMutex.Unlock()
	p.finalUpdateTracker[updateKey] = true
}