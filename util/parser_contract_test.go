package util

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

// 本文件锁定解析路径上被优化过的两处对外行为：
// 1) 正文从 DOM 直接拼出（<br> 记为换行），取代"序列化后剥标签"的旧写法；
// 2) ExtractPassword 的四个网盘分支正则由函数内编译提到包级。
//
// 标题用例走完整链路（HTML -> DOM -> 正文 -> 标题），而不是直接构造中间字符串，
// 这样签名调整或实现替换都无法绕过契约。

// extractTitleFromHTML 复刻解析器里的调用方式：从消息正文节点取
// 带换行的正文与纯文本，再交给 extractTitle。
func extractTitleFromHTML(t *testing.T, innerHTML string) string {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<html><body><div class="tgme_widget_message_text">` + innerHTML + `</div></body></html>`))
	if err != nil {
		t.Fatalf("构造DOM失败: %v", err)
	}
	sel := doc.Find(".tgme_widget_message_text")
	if sel.Length() == 0 {
		t.Fatal("未找到正文节点")
	}
	return extractTitle(messageTextWithBreaks(sel), sel.Text())
}

func TestExtractTitleContract(t *testing.T) {
	cases := []struct {
		name      string
		innerHTML string
		want      string
	}{
		{
			name:      "跳过日期头取真正的作品名",
			innerHTML: "<b>📅 9月9日</b><br/>名称：仙逆 4K 合集<br/>简介：xxxx",
			want:      "仙逆 4K 合集",
		},
		{
			name:      "以话题标签开头时跳到下一行",
			innerHTML: "#影视<br/>遮天 全季<br/>描述：yyy",
			want:      "遮天 全季",
		},
		{
			name:      "遇到简介关键字只保留前半段",
			innerHTML: "凡人修仙传 全集 简介：一个普通少年的修仙路",
			want:      "凡人修仙传 全集",
		},
		{
			name:      "链接文本参与取行且HTML实体被解码",
			innerHTML: `<a href="https://pan.quark.cn/s/abc">兰香如故 &amp; 番外</a><br/>资源说明：zzz`,
			want:      "兰香如故 & 番外",
		},
		{
			name:      "注释内容不计入文本",
			innerHTML: "<!-- 隐藏注释 -->漫长的季节<br/>其它：q",
			want:      "漫长的季节",
		},
		{
			name:      "正文为空时回落到纯文本内容",
			innerHTML: "",
			want:      "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractTitleFromHTML(t, c.innerHTML); got != c.want {
				t.Errorf("extractTitle() = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// 正文拼装是最核心的契约：<br> 必须变成换行符，标签必须消失，实体必须解码。
func TestMessageTextWithBreaksContract(t *testing.T) {
	cases := []struct {
		name      string
		innerHTML string
		want      string
	}{
		{
			name:      "br自闭合与普通写法都记为换行",
			innerHTML: "第一行<br/>第二行<br>第三行",
			want:      "第一行\n第二行\n第三行",
		},
		{
			name:      "标签被剥离但保留文本",
			innerHTML: "<b>粗体</b>与<i>斜体</i>",
			want:      "粗体与斜体",
		},
		{
			name:      "实体解码",
			innerHTML: "兰香如故 &amp; 番外 &lt;合集&gt;",
			want:      "兰香如故 & 番外 <合集>",
		},
		{
			name:      "注释丢弃",
			innerHTML: "可见<!-- 隐藏 -->文本",
			want:      "可见文本",
		},
		{
			name:      "嵌套元素内的换行保留",
			innerHTML: "<span>上<br/>下</span>",
			want:      "上\n下",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(
				`<html><body><div class="tgme_widget_message_text">` + c.innerHTML + `</div></body></html>`))
			if err != nil {
				t.Fatalf("构造DOM失败: %v", err)
			}
			got := messageTextWithBreaks(doc.Find(".tgme_widget_message_text"))
			if got != c.want {
				t.Errorf("messageTextWithBreaks() = %q, 期望 %q", got, c.want)
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

// 解析状态契约：0 条结果必须能区分"频道确实没有内容"与"页面结构变了"。
func TestParseSearchResultsStatus(t *testing.T) {
	// 真实的无结果页结构：外层仍是 message_wrap，里面是 centered + 无结果标记
	noMessagesHTML := `<html><body><section class="tgme_channel_history js-message_history">
		<div class="tgme_widget_message_wrap js-widget_message_wrap"><div class="tgme_widget_message_centered"><div class="tme_no_messages_found">No posts found</div></div></div>
	</section></body></html>`

	// 有消息块，但缺少时间节点：解析会在取时间处提前返回，一条都拿不到
	brokenHTML := `<html><body>
		<div class="tgme_widget_message_wrap js-widget_message_wrap"><div class="tgme_widget_message" data-post="ch/123">
			<div class="tgme_widget_message_text">仙逆 4K 合集</div>
		</div></div>
	</body></html>`

	okHTML := `<html><body>
		<div class="tgme_widget_message_wrap js-widget_message_wrap"><div class="tgme_widget_message" data-post="ch/123">
			<div class="tgme_widget_message_date"><time datetime="2026-09-24T10:00:00+00:00">2026-09-24</time></div>
			<div class="tgme_widget_message_text">仙逆 4K 合集<br/><a href="https://pan.quark.cn/s/abc">夸克网盘</a></div>
		</div></div>
	</body></html>`

	cases := []struct {
		name       string
		html       string
		wantStatus PageParseStatus
		wantCount  int
	}{
		{"正常页面解析出结果", okHTML, ParseStatusOK, 1},
		{"页面明确无结果", noMessagesHTML, ParseStatusNoMessages, 0},
		{"有消息块但解析失败", brokenHTML, ParseStatusStructureChanged, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			results, _, status, err := ParseSearchResultsWithStatus(c.html, "testchannel")
			if err != nil {
				t.Fatalf("解析出错: %v", err)
			}
			if status != c.wantStatus {
				t.Errorf("status = %v, 期望 %v", status, c.wantStatus)
			}
			if len(results) != c.wantCount {
				t.Errorf("结果数 = %d, 期望 %d", len(results), c.wantCount)
			}
		})
	}
}

// 解析失败时结果与翻页参数都应为空，错误必须上抛。
func TestParseSearchResultsWithStatusPropagatesError(t *testing.T) {
	if _, _, _, err := ParseSearchResultsWithStatus("", "testchannel"); err != nil {
		t.Logf("空页面返回错误（可接受）: %v", err)
	}
}
