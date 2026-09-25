package util

import "testing"

// 各网盘类型的行为冻结用例（合成页面）。
//
// 为什么合成夹具也必须要有：真实页面夹具（testdata/tg_channel_shareAliyun_real.html）只覆盖
// 阿里云盘一种类型，其余类型的密码归并逻辑没有基线。这份合成页面把六种类型和"链接只出现在
// inline keyboard 按钮里"这个边界一起覆盖。
//
// 下面断言的数字全是**实测当前行为**，不是理想值。
func TestParseAllNetdiskTypesFixture(t *testing.T) {
	html := `<html><body>
<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="chan/1">
  <div class="tgme_widget_message_date"><time datetime="2024-01-02T03:04:05+00:00"></time></div>
  <div class="tgme_widget_message_bubble"><div class="tgme_widget_message_text">
    百度网盘：https://pan.baidu.com/s/1AbCdEf 提取码：1234<br>天翼云盘：https://cloud.189.cn/t/xyzabc 访问码：abcd<br>
    UC网盘：https://drive.uc.cn/s/uc123 提取码：uc99<br>123网盘：https://www.123pan.com/s/pan123 提取码：p123<br>
    115网盘：https://115.com/s/abc115 访问码：p115<br>阿里云盘：https://www.aliyundrive.com/s/ali999
  </div></div>
</div></div>
<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="chan/2">
  <div class="tgme_widget_message_date"><time datetime="2024-01-02T03:04:05+00:00"></time></div>
  <div class="tgme_widget_message_bubble">
    <div class="tgme_widget_message_text">只有按钮里有链接</div>
    <div class="tgme_widget_message_inline_keyboard"><a href="https://www.alipan.com/s/btn888">按钮文字 提取码 bt01</a></div>
  </div>
</div></div>
<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="chan/3">
  <div class="tgme_widget_message_date"><time datetime="2024-01-02T03:04:05+00:00"></time></div>
  <div class="tgme_widget_message_bubble"><div class="tgme_widget_message_text">这条没有任何网盘链接，应被丢弃</div></div>
</div></div>
</body></html>`

	results, _, status, err := ParseSearchResultsWithStatus(html, "chan")
	if err != nil || status != ParseStatusOK {
		t.Fatalf("err=%v status=%v", err, status)
	}

	// 三个消息块，其中第三条不含任何网盘链接，被正常丢弃——但状态仍是 OK
	if len(results) != 2 {
		t.Fatalf("应为 2 条（第三条无链接被丢弃），实际 %d", len(results))
	}

	// 类型顺序固定：百度 → 天翼 → UC → 123 → 115 → 阿里云
	wantType := []string{"baidu", "tianyi", "uc", "123", "115", "aliyun"}
	if len(results[0].Links) != len(wantType) {
		t.Fatalf("第一条应有 6 个链接，实际 %d", len(results[0].Links))
	}
	for i, want := range wantType {
		if got := results[0].Links[i].Type; got != want {
			t.Errorf("链接[%d] 类型应为 %q，实际 %q", i, want, got)
		}
	}

	// 百度链接要把密码拼回 URL
	if got := results[0].Links[0].URL; got != "https://pan.baidu.com/s/1AbCdEf?pwd=1234" {
		t.Errorf("百度链接形态变了: %q", got)
	}

	// 冻结当前密码行为：一条消息里多个链接各带自己的提取码时，**全部取到第一个码**。
	//
	// 这是实测行为，不是理想行为——成因在 ExtractPassword 的正文回退分支：它把整条正文按
	// "提取码" 切分后返回第一个合法码，与链接本身没有关联。真实页面里 .Text() 还会把 <br>
	// 折叠掉，所以即便上游把每个码写在自己那行也救不回来。
	//
	// 这里先冻结现状，避免重构顺手改掉它；这个正确性问题需要单独决策后修复。
	for i, l := range results[0].Links {
		if l.Password != "1234" {
			t.Errorf("链接[%d] 密码实测为 1234（整条消息取首个码），实际 %q", i, l.Password)
		}
	}

	// 链接只出现在 inline keyboard 按钮里时也必须被解析出来（否则按钮里的盘链接会被整批忽略）
	btn := results[1].Links
	if len(btn) != 1 || btn[0].Type != "aliyun" || btn[0].URL != "https://www.alipan.com/s/btn888" {
		t.Errorf("按钮链接解析异常: %+v", btn)
	}
}
