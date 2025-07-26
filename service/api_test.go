package service

import (
	"pansou/model"
	"testing"
	"time"
)

func TestRealAPIContent(t *testing.T) {
	// 模拟API返回的content格式
	content := "反诈破局（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/95aa77c34147窃心（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/d961d26892aa东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集链接：https://pan.quark.cn/s/e26212a48644迎凤归  (2025)4K  更13集链接：https://pan.quark.cn/s/9a1275b86cc1千禧风云 (2025)4K  爱情 / 奇幻 更20集链接：https://pan.quark.cn/s/c382b51d9895我本是高峰 4K.臻彩MAX HDR    更新16集链接：https://pan.quark.cn/s/7c3adabd129e恋爱潜伏（2025）4K 臻彩  更新17集链接：https://pan.quark.cn/s/6c31133cc6ec将军家的小儿子 (2025) 更新24集链接：https://pan.quark.cn/s/2493ac41c98a野火  2025 4K 更新21集链接：https://pan.quark.cn/s/552f753851e0"
	
	// 创建一个模拟的搜索结果
	now := time.Now()
	results := []model.SearchResult{
		{
			UniqueID: "tgsearchers2_55539",
			Channel:  "tgsearchers2",
			Title:    "反诈破局（2025）4K 臻彩 更新9集",
			Content:  content,
			Datetime: now,
			Links: []model.Link{
				{Type: "quark", URL: "https://pan.quark.cn/s/95aa77c34147", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/d961d26892aa", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/e26212a48644", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/9a1275b86cc1", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/c382b51d9895", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/7c3adabd129e", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/6c31133cc6ec", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/2493ac41c98a", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/552f753851e0", Password: ""},
			},
		},
	}
	
	// 使用mergeResultsByType处理结果
	merged := mergeResultsByType(results)
	
	// 验证合并后的结果
	quarkLinks := merged["quark"]
	if len(quarkLinks) != 9 {
		t.Errorf("期望9个夸克链接，实际获得%d个", len(quarkLinks))
	}
	
	// 期望的标题映射
	expectedTitles := map[string]string{
		"https://pan.quark.cn/s/95aa77c34147": "反诈破局（2025）4K 臻彩 更新9集",
		"https://pan.quark.cn/s/d961d26892aa": "窃心（2025）4K 臻彩 更新9集",
		"https://pan.quark.cn/s/e26212a48644": "东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集",
		"https://pan.quark.cn/s/9a1275b86cc1": "迎凤归  (2025)4K  更13集",
		"https://pan.quark.cn/s/c382b51d9895": "千禧风云 (2025)4K  爱情 / 奇幻 更20集",
		"https://pan.quark.cn/s/7c3adabd129e": "我本是高峰 4K.臻彩MAX HDR    更新16集",
		"https://pan.quark.cn/s/6c31133cc6ec": "恋爱潜伏（2025）4K 臻彩  更新17集",
		"https://pan.quark.cn/s/2493ac41c98a": "将军家的小儿子 (2025) 更新24集",
		"https://pan.quark.cn/s/552f753851e0": "野火  2025 4K 更新21集",
	}
	
	// 检查每个链接的标题是否正确
	for _, link := range quarkLinks {
		expectedTitle, exists := expectedTitles[link.URL]
		if !exists {
			t.Errorf("未预期的链接: %s", link.URL)
			continue
		}
		
		if link.Note != expectedTitle {
			t.Errorf("链接 %s 标题不匹配: 期望 '%s', 实际 '%s'", link.URL, expectedTitle, link.Note)
		} else {
			t.Logf("链接 %s 标题正确匹配: '%s'", link.URL, link.Note)
		}
	}
} 