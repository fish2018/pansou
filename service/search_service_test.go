package service

import (
	"fmt"
	"pansou/model"
	"testing"
	"time"
)

func TestExtractLinkTitlePairs(t *testing.T) {
	// 测试用例1：标准格式的消息内容
	content1 := `反诈破局（2025）4K 臻彩 更新9集
链接：https://pan.quark.cn/s/95aa77c34147
窃心（2025）4K 臻彩 更新9集
链接：https://pan.quark.cn/s/d961d26892aa
东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集
链接：https://pan.quark.cn/s/e26212a48644`

	result1 := extractLinkTitlePairs(content1)
	
	if result1["https://pan.quark.cn/s/95aa77c34147"] != "反诈破局（2025）4K 臻彩 更新9集" {
		t.Errorf("第一个链接标题匹配错误，期望'反诈破局（2025）4K 臻彩 更新9集'，实际为'%s'", result1["https://pan.quark.cn/s/95aa77c34147"])
	}
	
	if result1["https://pan.quark.cn/s/d961d26892aa"] != "窃心（2025）4K 臻彩 更新9集" {
		t.Errorf("第二个链接标题匹配错误，期望'窃心（2025）4K 臻彩 更新9集'，实际为'%s'", result1["https://pan.quark.cn/s/d961d26892aa"])
	}
	
	if result1["https://pan.quark.cn/s/e26212a48644"] != "东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集" {
		t.Errorf("第三个链接标题匹配错误，期望'东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集'，实际为'%s'", result1["https://pan.quark.cn/s/e26212a48644"])
	}
	
	// 测试用例2：非标准格式的消息内容
	content2 := `今日更新剧集：
	
反诈破局 更新到9集
https://pan.quark.cn/s/95aa77c34147

东北往事之大时代 4K高清版
资源地址：https://pan.quark.cn/s/e26212a48644

窃心：https://pan.quark.cn/s/d961d26892aa`

	result2 := extractLinkTitlePairs(content2)
	
	if result2["https://pan.quark.cn/s/95aa77c34147"] != "反诈破局 更新到9集" {
		t.Errorf("非标准格式第一个链接标题匹配错误，期望'反诈破局 更新到9集'，实际为'%s'", result2["https://pan.quark.cn/s/95aa77c34147"])
	}
	
	if result2["https://pan.quark.cn/s/e26212a48644"] != "东北往事之大时代 4K高清版" {
		t.Errorf("非标准格式第二个链接标题匹配错误，期望'东北往事之大时代 4K高清版'，实际为'%s'", result2["https://pan.quark.cn/s/e26212a48644"])
	}
	
	if result2["https://pan.quark.cn/s/d961d26892aa"] != "窃心" {
		t.Errorf("非标准格式第三个链接标题匹配错误，期望'窃心'，实际为'%s'", result2["https://pan.quark.cn/s/d961d26892aa"])
	}
}

func TestMergeResultsByType(t *testing.T) {
	// 创建测试数据
	now := time.Now()
	results := []model.SearchResult{
		{
			UniqueID: "test_1",
			Title:    "测试消息1",
			Content:  `反诈破局（2025）4K 臻彩 更新9集
链接：https://pan.quark.cn/s/95aa77c34147
东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集
链接：https://pan.quark.cn/s/e26212a48644`,
			Datetime: now,
			Links: []model.Link{
				{Type: "quark", URL: "https://pan.quark.cn/s/95aa77c34147", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/e26212a48644", Password: ""},
			},
		},
	}
	
	merged := mergeResultsByType(results)
	
	// 验证结果
	quarkLinks := merged["quark"]
	if len(quarkLinks) != 2 {
		t.Errorf("期望2个夸克链接，实际获得%d个", len(quarkLinks))
	}
	
	// 找到对应链接的索引
	var idx1, idx2 int
	for i, link := range quarkLinks {
		if link.URL == "https://pan.quark.cn/s/95aa77c34147" {
			idx1 = i
		} else if link.URL == "https://pan.quark.cn/s/e26212a48644" {
			idx2 = i
		}
	}
	
	// 验证链接标题
	if quarkLinks[idx1].Note != "反诈破局（2025）4K 臻彩 更新9集" {
		t.Errorf("第一个链接标题错误，期望'反诈破局（2025）4K 臻彩 更新9集'，实际为'%s'", quarkLinks[idx1].Note)
	}
	
	if quarkLinks[idx2].Note != "东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集" {
		t.Errorf("第二个链接标题错误，期望'东北往事之大时代(2025)剧情 徐洋 安冬 4K更新14集'，实际为'%s'", quarkLinks[idx2].Note)
	}
	
	// 测试边缘情况：空消息
	emptyResults := []model.SearchResult{
		{
			UniqueID: "empty_1",
			Title:    "空消息",
			Content:  "",
			Datetime: now,
			Links: []model.Link{
				{Type: "quark", URL: "https://pan.quark.cn/s/empty123", Password: ""},
			},
		},
	}
	
	emptyMerged := mergeResultsByType(emptyResults)
	emptyQuarkLinks := emptyMerged["quark"]
	
	// 验证空消息的结果
	if len(emptyQuarkLinks) != 1 {
		t.Errorf("期望1个空消息链接，实际获得%d个", len(emptyQuarkLinks))
	}
	
	if emptyQuarkLinks[0].Note != "空消息" {
		t.Errorf("空消息链接标题错误，期望'空消息'，实际为'%s'", emptyQuarkLinks[0].Note)
	}
} 

func TestMergeResultsByTypeNoNewlines(t *testing.T) {
	// 创建测试数据 - 没有换行符的情况
	now := time.Now()
	results := []model.SearchResult{
		{
			UniqueID: "test_1",
			Title:    "测试消息1",
			Content:  `反诈破局（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/95aa77c34147窃心（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/d961d26892aa`,
			Datetime: now,
			Links: []model.Link{
				{Type: "quark", URL: "https://pan.quark.cn/s/95aa77c34147", Password: ""},
				{Type: "quark", URL: "https://pan.quark.cn/s/d961d26892aa", Password: ""},
			},
		},
	}
	
	merged := mergeResultsByType(results)
	
	// 验证结果
	quarkLinks := merged["quark"]
	if len(quarkLinks) != 2 {
		t.Errorf("期望2个夸克链接，实际获得%d个", len(quarkLinks))
	}
	
	// 检查链接标题
	for _, link := range quarkLinks {
		if link.URL == "https://pan.quark.cn/s/95aa77c34147" && link.Note != "反诈破局（2025）4K 臻彩 更新9集" {
			t.Errorf("第一个链接标题错误，期望'反诈破局（2025）4K 臻彩 更新9集'，实际为'%s'", link.Note)
		}
		if link.URL == "https://pan.quark.cn/s/d961d26892aa" && link.Note != "窃心（2025）4K 臻彩 更新9集" {
			t.Errorf("第二个链接标题错误，期望'窃心（2025）4K 臻彩 更新9集'，实际为'%s'", link.Note)
		}
	}
} 

func TestExtractLinkTitlePairsNoNewlines(t *testing.T) {
	// API返回的content格式，没有换行符
	content := "反诈破局（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/95aa77c34147窃心（2025）4K 臻彩 更新9集链接：https://pan.quark.cn/s/d961d26892aa"

	// 使用我们的算法提取链接-标题对
	linkTitleMap := extractLinkTitlePairs(content)
	
	// 打印结果
	fmt.Println("链接-标题对应关系:")
	for link, title := range linkTitleMap {
		fmt.Printf("%s -> %s\n", link, title)
	}
	
	// 验证特定链接
	if title, found := linkTitleMap["https://pan.quark.cn/s/95aa77c34147"]; !found {
		t.Errorf("链接 https://pan.quark.cn/s/95aa77c34147 没有找到对应标题")
	} else if title != "反诈破局（2025）4K 臻彩 更新9集" {
		t.Errorf("链接 https://pan.quark.cn/s/95aa77c34147 标题不匹配: 期望 '反诈破局（2025）4K 臻彩 更新9集', 实际 '%s'", title)
	}
	
	if title, found := linkTitleMap["https://pan.quark.cn/s/d961d26892aa"]; !found {
		t.Errorf("链接 https://pan.quark.cn/s/d961d26892aa 没有找到对应标题")
	} else if title != "窃心（2025）4K 臻彩 更新9集" {
		t.Errorf("链接 https://pan.quark.cn/s/d961d26892aa 标题不匹配: 期望 '窃心（2025）4K 臻彩 更新9集', 实际 '%s'", title)
	}
} 