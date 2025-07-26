package service

import (
	"fmt"
	"testing"
)

func TestNoNewlineContent(t *testing.T) {
	// API返回的content格式，没有换行符
	content := "反诈破局（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/95aa77c34147窃心（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/d961d26892aa东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集链接：https://pan.quark.cn/s/e26212a48644迎凤归  (2025)4K  更13集链接：https://pan.quark.cn/s/9a1275b86cc1千禧风云 (2025)4K  爱情 / 奇幻 更20集链接：https://pan.quark.cn/s/c382b51d9895我本是高峰 4K.臻彩MAX HDR    更新16集链接：https://pan.quark.cn/s/7c3adabd129e恋爱潜伏（2025）4K 臻彩  更新17集链接：https://pan.quark.cn/s/6c31133cc6ec将军家的小儿子 (2025) 更新24集链接：https://pan.quark.cn/s/2493ac41c98a野火  2025 4K 更新21集链接：https://pan.quark.cn/s/552f753851e0"

	// 使用我们的算法提取链接-标题对
	linkTitleMap := extractLinkTitlePairs(content)
	
	// 打印结果
	fmt.Println("链接-标题对应关系:")
	for link, title := range linkTitleMap {
		fmt.Printf("%s -> %s\n", link, title)
	}
	
	// 验证特定链接
	expectedLinks := []string{
		"https://pan.quark.cn/s/95aa77c34147",
		"https://pan.quark.cn/s/d961d26892aa",
		"https://pan.quark.cn/s/e26212a48644",
		"https://pan.quark.cn/s/9a1275b86cc1",
		"https://pan.quark.cn/s/c382b51d9895",
		"https://pan.quark.cn/s/7c3adabd129e",
		"https://pan.quark.cn/s/6c31133cc6ec",
		"https://pan.quark.cn/s/2493ac41c98a",
		"https://pan.quark.cn/s/552f753851e0",
	}
	
	expectedTitles := []string{
		"反诈破局（2025）4K 臻彩 更新9集",
		"窃心（2025）4K 臻彩 更新9集",
		"东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集",
		"迎凤归  (2025)4K  更13集",
		"千禧风云 (2025)4K  爱情 / 奇幻 更20集",
		"我本是高峰 4K.臻彩MAX HDR    更新16集",
		"恋爱潜伏（2025）4K 臻彩  更新17集",
		"将军家的小儿子 (2025) 更新24集",
		"野火  2025 4K 更新21集",
	}
	
	// 检查是否所有链接都有对应的标题
	for i, link := range expectedLinks {
		title, exists := linkTitleMap[link]
		expectedTitle := expectedTitles[i]
		
		if !exists {
			t.Errorf("链接 %s 没有找到对应标题", link)
		} else if title != expectedTitle {
			t.Errorf("链接 %s 标题不匹配: 期望 '%s', 实际 '%s'", link, expectedTitle, title)
		} else {
			t.Logf("链接 %s 标题正确匹配: '%s'", link, title)
		}
	}
} 