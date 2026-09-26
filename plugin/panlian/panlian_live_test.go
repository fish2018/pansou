package panlian

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"pansou/util/json"
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

// 走真实 HTTP 动作层联调：请求形状与前端完全一致（action 名、参数名、响应信封），
// 覆盖"前端能不能真的登录成功"，比只测内部方法更贴近线上。
//
//	PANLIAN_LIVE=http1 go test ./plugin/panlian/ -run TestLivePanlianHTTPFlow -v
//	PANLIAN_LIVE=http2 PANLIAN_LIVE_CAPTCHA_ID=... PANLIAN_LIVE_CAPTCHA_CODE=... \
//	  PANLIAN_LIVE_USER=... PANLIAN_LIVE_PASS=... PANLIAN_LIVE_EMAIL=... go test ... -v
func TestLivePanlianHTTPFlow(t *testing.T) {
	mode := strings.TrimSpace(os.Getenv("PANLIAN_LIVE"))
	if mode != "http1" && mode != "http2" {
		t.Skip("未设置 PANLIAN_LIVE=http1/http2，跳过真实站点动作层联调")
	}

	p := NewPanlianPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 与线上一致：api/router.go 里是 RegisterWebRoutes(r.Group(""))，所以路径是 /panlian/:hash
	p.RegisterWebRoutes(r.Group(""))
	hash := strings.Repeat("d", 64)

	post := func(payload map[string]interface{}) map[string]interface{} {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求失败: %v", err)
		}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panlian/"+hash, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		var out map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析响应失败: %v (%s)", err, w.Body.String())
		}
		return out
	}
	dataOf := func(m map[string]interface{}, step string) map[string]interface{} {
		d, ok := m["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s 响应缺少 data: %v", step, m)
		}
		return d
	}

	switch mode {
	case "http1":
		out := post(map[string]interface{}{"action": "captcha"})
		if out["success"] != true {
			t.Fatalf("action=captcha 失败: %v", out)
		}
		d := dataOf(out, "captcha")
		id, _ := d["captcha_id"].(string)
		img, _ := d["image"].(string)
		if id == "" || !strings.HasPrefix(img, "data:image/png;base64,") {
			t.Fatalf("验证码响应不完整: %v", d)
		}
		b64 := img[strings.Index(img, ",")+1:]
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("解码验证码失败: %v", err)
		}
		outPath := filepath.Join(os.TempDir(), "panlian-http-captcha.png")
		if err := os.WriteFile(outPath, raw, 0o644); err != nil {
			t.Fatalf("写验证码失败: %v", err)
		}
		t.Logf("captcha_id=%s", id)
		t.Logf("验证码图片=%s（%d 字节）", outPath, len(raw))

	case "http2":
		id := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_CAPTCHA_ID"))
		code := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_CAPTCHA_CODE"))
		username := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_USER"))
		password := os.Getenv("PANLIAN_LIVE_PASS")
		email := strings.TrimSpace(os.Getenv("PANLIAN_LIVE_EMAIL"))
		if id == "" || code == "" || username == "" || password == "" || email == "" {
			t.Fatal("缺少 PANLIAN_LIVE_CAPTCHA_ID / _CODE / _USER / _PASS / _EMAIL")
		}

		step1 := post(map[string]interface{}{
			"action": "login", "username": username, "password": password,
			"captcha_id": id, "captcha_code": code,
		})
		if step1["success"] != true {
			t.Fatalf("第一步 action=login 应成功: %v", step1)
		}
		d1 := dataOf(step1, "login")
		if d1["need_email"] != true {
			t.Fatalf("真实站点应要求邮箱确认: %v", d1)
		}
		confirmID, _ := d1["confirm_id"].(string)
		if confirmID == "" {
			t.Fatalf("缺少 confirm_id: %v", d1)
		}
		t.Logf("第一步通过：need_email=true confirm_id=%s email_hint=%v", confirmID, d1["email_hint"])

		step2 := post(map[string]interface{}{
			"action": "confirm_email", "username": username, "password": password,
			"confirm_id": confirmID, "email": email,
		})
		if step2["success"] != true {
			t.Fatalf("第二步 action=confirm_email 应成功: %v", step2)
		}
		d2 := dataOf(step2, "confirm_email")
		if d2["status"] != "active" {
			t.Fatalf("应落为 active: %v", d2)
		}
		t.Logf("第二步通过：username=%v status=%v", d2["username"], d2["status"])

		// 前端登录成功后立刻会查状态，这条必须显示已登录。
		st := post(map[string]interface{}{"action": "get_status"})
		d3 := dataOf(st, "get_status")
		if d3["logged_in"] != true {
			t.Fatalf("get_status 应显示已登录: %v", d3)
		}
		t.Logf("get_status：logged_in=%v username=%v", d3["logged_in"], d3["username"])

		// 再走一次前端的管理页测试搜索，确认这条会话真能出结果。
		ts := post(map[string]interface{}{"action": "test_search", "keyword": "仙逆"})
		if ts["success"] != true {
			t.Fatalf("test_search 失败: %v", ts)
		}
		d4 := dataOf(ts, "test_search")
		results, _ := d4["results"].([]interface{})
		t.Logf("test_search：返回 %d 条", len(results))
		if len(results) == 0 {
			t.Fatalf("test_search 无结果，会话可能无效: %v", d4)
		}
	}
}
