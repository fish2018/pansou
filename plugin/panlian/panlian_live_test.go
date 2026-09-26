package panlian

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 真实站点联调测试：默认跳过，只有显式设置 PANLIAN_LIVE 时才运行。
//
// 站点登录必须有人肉识别图形验证码，所以拆成两步，验证码图片落盘后由人看图再回填：
//
//	PANLIAN_LIVE=step1 go test ./plugin/panlian/ -run TestLivePanlianLogin -v
//	  → 打印 captcha_id，并把图片写到 /tmp/panlian-live-captcha.png
//
//	PANLIAN_LIVE=step2 \
//	  PANLIAN_LIVE_CAPTCHA_ID=<上一步的 id> PANLIAN_LIVE_CAPTCHA_CODE=<图中字符> \
//	  PANLIAN_LIVE_USER=<账号> PANLIAN_LIVE_PASS=<密码> PANLIAN_LIVE_EMAIL=<绑定邮箱> \
//	  go test ./plugin/panlian/ -run TestLivePanlianLogin -v
//	  → 走完登录+邮箱确认，并用拿到的会话真搜一次
//
// 凭据只从环境变量读，不写进仓库。
func TestLivePanlianLogin(t *testing.T) {
	mode := strings.TrimSpace(os.Getenv("PANLIAN_LIVE"))
	if mode == "" {
		t.Skip("未设置 PANLIAN_LIVE，跳过真实站点联调")
	}

	p := NewPanlianPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })

	switch mode {
	case "step1":
		id, image, err := p.fetchCaptcha()
		if err != nil {
			t.Fatalf("取验证码失败: %v", err)
		}
		b64 := image
		if i := strings.Index(b64, ","); i >= 0 {
			b64 = b64[i+1:]
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("解码验证码图片失败: %v", err)
		}
		out := filepath.Join(os.TempDir(), "panlian-live-captcha.png")
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			t.Fatalf("写验证码图片失败: %v", err)
		}
		t.Logf("captcha_id=%s", id)
		t.Logf("验证码图片=%s（%d 字节）", out, len(raw))

	case "step2":
		id := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_CAPTCHA_ID"))
		code := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_CAPTCHA_CODE"))
		username := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_USER"))
		password := os.Getenv("PANLIAN_LIVE_PASS")
		email := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_EMAIL"))
		if id == "" || code == "" || username == "" || password == "" || email == "" {
			t.Fatal("缺少 PANLIAN_LIVE_CAPTCHA_ID / _CODE / _USER / _PASS / _EMAIL")
		}

		attempt, err := p.doLogin(username, password, true, id, code)
		if err != nil {
			t.Fatalf("第一步登录失败: %v", err)
		}
		if attempt.NeedEmail {
			t.Logf("第一步通过，站点要求邮箱确认：confirm_id=%s 掩码邮箱=%s", attempt.ConfirmID, attempt.EmailHint)
			cookie, uname, err := p.confirmEmailLogin(attempt.ConfirmID, email, true)
			if err != nil {
				t.Fatalf("邮箱确认失败: %v", err)
			}
			t.Logf("邮箱确认通过：username=%s cookie 长度=%d", uname, len(cookie))
			liveSearchCheck(t, p, uname, cookie)
			return
		}

		t.Logf("站点未要求邮箱确认，直接登录成功：cookie 长度=%d", len(attempt.Cookie))
		liveSearchCheck(t, p, attempt.Username, attempt.Cookie)

	default:
		t.Fatalf("PANLIAN_LIVE 只能是 step1 或 step2，实际 %q", mode)
	}
}

// liveSearchCheck 用刚拿到的会话调用插件自己的搜索路径，确认 Cookie 真能出结果。
func liveSearchCheck(t *testing.T, p *PanlianPlugin, username, cookie string) {
	t.Helper()
	user := &User{
		Hash:      strings.Repeat("c", 64),
		Username:  username,
		Cookie:    cookie,
		Status:    "active",
		LoginAt:   time.Now(),
		ExpireAt:  time.Now().Add(30 * 24 * time.Hour),
		CreatedAt: time.Now(),
	}
	client := &http.Client{Timeout: RequestTimeout}
	results, err := p.searchWithUser(client, user, "仙逆")
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	t.Logf("搜索返回 %d 条结果", len(results))
	if len(results) == 0 {
		t.Fatal("搜索无结果，会话可能无效")
	}
	for i, r := range results {
		if i >= 3 {
			break
		}
		t.Logf("  [%d] %s（%d 个链接）", i+1, r.Title, len(r.Links))
	}
}
