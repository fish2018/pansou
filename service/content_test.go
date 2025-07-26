package service

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// 处理没有换行符的情况
func extractLinkTitlePairsNoNewlines(content string) map[string]string {
	// 结果映射：链接URL -> 对应标题
	linkTitleMap := make(map[string]string)
	
	// 链接正则表达式
	linkRegex := regexp.MustCompile(`(https?://[^\s"']+)`)
	
	// 尝试识别标题-链接对的模式
	// 格式1：标题链接：URL
	pattern1 := regexp.MustCompile(`([^链地资网]+?)链接：(https?://[^\s"']+)`)
	matches1 := pattern1.FindAllStringSubmatch(content, -1)
	for _, match := range matches1 {
		if len(match) >= 3 {
			title := strings.TrimSpace(match[1])
			url := match[2]
			linkTitleMap[url] = title
		}
	}
	
	// 如果没有找到任何匹配，尝试使用更宽松的模式
	if len(linkTitleMap) == 0 {
		// 尝试从内容中提取所有可能的标题和链接
		// 这个正则表达式匹配类似"电影名(年份)4K 更新X集"的模式
		titlePattern := regexp.MustCompile(`([^链地资网\s]+?(?:\([^)]+\))?(?:\s*\d+K)?(?:\s*臻彩)?(?:\s*MAX)?(?:\s*HDR)?(?:\s*更(?:新)?\d+集))`)
		titles := titlePattern.FindAllString(content, -1)
		
		// 提取所有链接
		links := linkRegex.FindAllString(content, -1)
		
		// 尝试根据位置关系匹配标题和链接
		for _, link := range links {
			// 找到链接在内容中的位置
			linkPos := strings.Index(content, link)
			
			// 找到最接近链接位置的前一个标题
			closestTitle := ""
			closestDist := -1
			
			for _, title := range titles {
				titlePos := strings.Index(content, title)
				if titlePos < linkPos {
					dist := linkPos - titlePos
					if closestDist == -1 || dist < closestDist {
						closestDist = dist
						closestTitle = title
					}
				}
			}
			
			if closestTitle != "" {
				linkTitleMap[link] = strings.TrimSpace(closestTitle)
			}
		}
	}
	
	return linkTitleMap
}

func TestNoNewlineExtraction(t *testing.T) {
	// API返回的content格式，没有换行符
	content := "反诈破局（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/95aa77c34147窃心（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/d961d26892aa东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集链接：https://pan.quark.cn/s/e26212a48644迎凤归  (2025)4K  更13集链接：https://pan.quark.cn/s/9a1275b86cc1千禧风云 (2025)4K  爱情 / 奇幻 更20集链接：https://pan.quark.cn/s/c382b51d9895我本是高峰 4K.臻彩MAX HDR    更新16集链接：https://pan.quark.cn/s/7c3adabd129e恋爱潜伏（2025）4K 臻彩  更新17集链接：https://pan.quark.cn/s/6c31133cc6ec将军家的小儿子 (2025) 更新24集链接：https://pan.quark.cn/s/2493ac41c98a野火  2025 4K 更新21集链接：https://pan.quark.cn/s/552f753851e0"

	// 使用我们的算法提取链接-标题对
	linkTitleMap := extractLinkTitlePairsNoNewlines(content)
	
	// 打印结果
	fmt.Println("链接-标题对应关系:")
	for link, title := range linkTitleMap {
		fmt.Printf("%s -> %s\n", link, title)
	}
	
	// 验证特定链接
	expectedLinks := []string{
		"https://pan.quark.cn/s/95aa77c34147窃心（2025）4K",
		"https://pan.quark.cn/s/d961d26892aa东北往事之大时代(2025)剧情",
		"https://pan.quark.cn/s/e26212a48644迎凤归",
		"https://pan.quark.cn/s/9a1275b86cc1千禧风云",
		"https://pan.quark.cn/s/c382b51d9895我本是高峰",
		"https://pan.quark.cn/s/7c3adabd129e恋爱潜伏（2025）4K",
		"https://pan.quark.cn/s/6c31133cc6ec将军家的小儿子",
		"https://pan.quark.cn/s/2493ac41c98a野火",
		"https://pan.quark.cn/s/552f753851e0",
	}
	
	expectedTitles := []string{
		"反诈破局（2025）4K 臻彩 更新9集",
		"臻彩 更新9集",
		"徐洋 安冬 4K更新14集",
		"(2025)4K  更13集",
		"(2025)4K  爱情 / 奇幻 更20集",
		"4K.臻彩MAX HDR    更新16集",
		"臻彩  更新17集",
		"(2025) 更新24集",
		"2025 4K 更新21集",
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