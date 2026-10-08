package libvio

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func TestExtractDirectPanLinks(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`
		<div class="playlist-panel netdisk-panel">
			<a class="netdisk-item" href="https://pan.quark.cn/s/quark123"><span class="netdisk-name">夸克</span></a>
			<a class="netdisk-item" href="https://drive.uc.cn/s/uc123?public=1"><span class="netdisk-name">UC</span></a>
			<a class="netdisk-item" href="https://pan.baidu.com/s/baidu123?pwd=a1b2"><span class="netdisk-name">百度</span></a>
			<a class="netdisk-item" href="https://example.com/ignored">忽略</a>
		</div>`))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	p := NewLibvioPlugin()
	links := p.extractDirectPanLinks(doc, "仙逆")
	if len(links) != 3 {
		t.Fatalf("links = %+v", links)
	}
	types := map[string]string{}
	for _, link := range links {
		if link.WorkTitle != "仙逆" {
			t.Fatalf("work title = %q", link.WorkTitle)
		}
		types[link.Type] = link.Password
	}
	if types["quark"] != "" || types["uc"] != "" || types["baidu"] != "a1b2" {
		t.Fatalf("types = %+v", types)
	}
}

// 站点真实挑战页的参数形状（2026-10 实测抓取）。
const challengePageFixture = `<script>
(function () {
  "use strict";
  var TS = "1791465295", SIG = "0a8db1316598f1f8da9fe22470e654ffdf1c988ad685026f3ab86860222c383e", DIFF = "0000", MODE = "auto";
  var POW = "__cdn_pow";
  var EXPECT = 65536;
})();
</script>`

func TestPowParamRegexExtractsRealChallenge(t *testing.T) {
	params := powParamRegex.FindStringSubmatch(challengePageFixture)
	if params == nil {
		t.Fatal("未匹配到挑战参数")
	}
	ts, sig, diff, mode := params[1], params[2], params[3], params[4]
	if ts != "1791465295" {
		t.Fatalf("TS = %q", ts)
	}
	if sig != "0a8db1316598f1f8da9fe22470e654ffdf1c988ad685026f3ab86860222c383e" {
		t.Fatalf("SIG = %q", sig)
	}
	if diff != "0000" || mode != "auto" {
		t.Fatalf("DIFF = %q, MODE = %q", diff, mode)
	}
}

// TestSolvePowNonceHitsKnownAnswer 用抓取到的真实挑战做已知答案回归。
// nonce=1657 是该 SIG 的最小解，1656 不满足，所以同时也是"最小解"的约束。
func TestSolvePowNonceHitsKnownAnswer(t *testing.T) {
	const sig = "0a8db1316598f1f8da9fe22470e654ffdf1c988ad685026f3ab86860222c383e"

	nonce, err := solvePowNonce(sig, "0000")
	if err != nil {
		t.Fatalf("求解失败: %v", err)
	}
	if nonce != 1657 {
		t.Fatalf("nonce = %d, 期望 1657", nonce)
	}

	sum := sha256.Sum256([]byte(sig + strconv.Itoa(nonce)))
	if !strings.HasPrefix(hex.EncodeToString(sum[:]), "0000") {
		t.Fatalf("解出的 nonce 未通过自校验: %s", hex.EncodeToString(sum[:]))
	}
}

func TestSolvePowNonceReturnsMinimum(t *testing.T) {
	const sig = "18f3ad6e47ebd6e44704eab02137"

	nonce, err := solvePowNonce(sig, "0000")
	if err != nil {
		t.Fatalf("求解失败: %v", err)
	}
	if nonce != 133839 {
		t.Fatalf("nonce = %d, 期望最小解 133839", nonce)
	}
}

func TestSolvePowNonceRejectsImpossibleDifficulty(t *testing.T) {
	if testing.Short() {
		t.Skip("熔断路径要跑满尝试上限，短模式跳过")
	}
	// 8 个 hex 前导零超出熔断上限，必须报错而不是空转。
	if _, err := solvePowNonce("abc123", "00000000"); err == nil {
		t.Fatal("不可达难度应当返回错误")
	}
}

func TestIsChallengeBody(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		body   string
		want   bool
	}{
		{
			name:   "响应头直接标明",
			header: http.Header{"X-Cdn-Challenge": []string{"required"}},
			body:   "403 Forbidden: browser verification required",
			want:   true,
		},
		{
			name:   "只有正文（部分节点不给响应头）",
			header: http.Header{},
			body:   challengePageFixture,
			want:   true,
		},
		{
			name:   "地域封禁不是验证",
			header: http.Header{},
			body:   "Access denied by geographic restriction",
			want:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isChallengeBody(test.header, []byte(test.body)); got != test.want {
				t.Fatalf("isChallengeBody = %v, 期望 %v", got, test.want)
			}
		})
	}
}

func TestCdnGuardExpiry(t *testing.T) {
	var g cdnGuard
	if g.get() != "" {
		t.Fatal("未验证时应返回空串")
	}

	g.set("token-1")
	if g.get() != "token-1" {
		t.Fatal("刚写入的 cookie 应当可用")
	}

	// 手动把过期时间推到过去，模拟 cookie 到期。
	g.mu.Lock()
	g.expireAt = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if g.get() != "" {
		t.Fatal("过期 cookie 必须返回空串，否则会拿着死 cookie 反复撞 403")
	}

	// 作废（服务端否掉 cookie）后同样应立即失效。
	g.set("token-2")
	g.set("")
	if g.get() != "" {
		t.Fatal("被作废的 cookie 不应继续使用")
	}
}
