package util

import "testing"

// 本文件锁定解析路径上被优化过的两处对外行为：
// 1) extractTitle 用字符串处理替代"每条消息建一个 goquery 文档"；
// 2) ExtractPassword 的四个网盘分支正则由函数内编译提到包级。
// 断言只依赖包内函数，不需要网络或样本文件，可作为长期回归契约。

func TestExtractTitleContract(t *testing.T) {
	cases := []struct {
		name        string
		htmlContent string
		textContent string
		want        string
	}{
		{
			name:        "跳过日期头取真正的作品名",
			htmlContent: "<b>📅 9月9日</b><br/>名称：仙逆 4K 合集<br/>简介：xxxx",
			textContent: "📅 9月9日\n名称：仙逆 4K 合集\n简介：xxxx",
			want:        "仙逆 4K 合集",
		},
		{
			name:        "以话题标签开头时跳到下一行",
			htmlContent: "#影视<br/>遮天 全季<br/>描述：yyy",
			textContent: "#影视\n遮天 全季\n描述：yyy",
			want:        "遮天 全季",
		},
		{
			name:        "遇到简介关键字只保留前半段",
			htmlContent: "凡人修仙传 全集 简介：一个普通少年的修仙路",
			textContent: "凡人修仙传 全集 简介：一个普通少年的修仙路",
			want:        "凡人修仙传 全集",
		},
		{
			name:        "链接文本参与取行且HTML实体被解码",
			htmlContent: "<a href=\"https://pan.quark.cn/s/abc\">兰香如故 &amp; 番外</a><br/>资源说明：zzz",
			textContent: "兰香如故 & 番外\n资源说明：zzz",
			want:        "兰香如故 & 番外",
		},
		{
			name:        "注释内容不计入文本",
			htmlContent: "<!-- 隐藏注释 -->漫长的季节<br/>其它：q",
			textContent: "漫长的季节\n其它：q",
			want:        "漫长的季节",
		},
		{
			name:        "HTML为空时回落到纯文本内容",
			htmlContent: "",
			textContent: "#标签\n名称：备用标题",
			want:        "备用标题",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractTitle(c.htmlContent, c.textContent); got != c.want {
				t.Errorf("extractTitle() = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestExtractPasswordContract(t *testing.T) {
	cases := []struct {
		name    string
		content string
		url     string
		want    string
	}{
		{
			name: "天翼云盘访问码（中文括号）",
			url:  "https://cloud.189.cn/t/abc（访问码：x7k9）",
			want: "x7k9",
		},
		{
			name: "迅雷网盘pwd参数",
			url:  "https://pan.xunlei.com/s/VNabc?pwd=8f3q",
			want: "8f3q",
		},
		{
			name: "115网盘password参数",
			url:  "https://115.com/s/abc?password=9m2x",
			want: "9m2x",
		},
		{
			name: "123网盘URL编码提取码",
			url:  "https://www.123pan.com/s/abc?%E6%8F%90%E5%8F%96%E7%A0%81:ab12",
			want: "ab12",
		},
		{
			name: "百度网盘pwd参数",
			url:  "https://pan.baidu.com/s/1abc?pwd=1234",
			want: "1234",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractPassword(c.content, c.url); got != c.want {
				t.Errorf("ExtractPassword() = %q, 期望 %q", got, c.want)
			}
		})
	}
}
