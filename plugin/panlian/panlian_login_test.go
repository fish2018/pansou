package panlian

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"pansou/util/json"
)

// 站点自 2026 年起把登录改成三步：取图形验证码 → 提交账号密码+验证码 → 输入账号绑定的邮箱。
// 下面用一个假站点把这三种响应连同边界分支固定下来（响应体逐字取自真实站点实测）。
func newFakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/captcha", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"cap-1","image":"data:image/png;base64,iVBORw0KGgo="}}`))
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		body, _ := url.ParseQuery(readBody(r))
		captchaCode := body.Get("captcha_code")
		w.Header().Set("Content-Type", "application/json")
		switch captchaCode {
		case "":
			// 完全没填验证码：站点要求先输入图形验证码。
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"请先输入图形验证码","error_type":"VALIDATION","details":{"captcha_error":true}}`))
		case "WRONG":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"验证码错误","error_type":"VALIDATION","details":{"captcha_error":true}}`))
		case "NEEDMAIL":
			// 第二步：站点要求输入该账号绑定的邮箱。
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"为了确认是本人操作，请在新页面输入该账号绑定的邮箱（2******3@qq.com）","error_type":"VALIDATION","details":{"confirm_id":"confirm-1","email":"2******3@qq.com","email_verify_required":true}}`))
		case "OLDFLOW":
			// 直接成功、不需要邮箱确认（老账号或站点回退时仍要能登录）。
			http.SetCookie(w, &http.Cookie{Name: "admin_session", Value: "direct-ok", Path: "/"})
			_, _ = w.Write([]byte(`{"success":true,"message":"","data":{"user_id":11200,"role":"user","username":"pansou"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"账号或密码错误","error_type":"VALIDATION"}`))
		}
	})

	mux.HandleFunc("/api/auth/login/confirm-email", func(w http.ResponseWriter, r *http.Request) {
		body, _ := url.ParseQuery(readBody(r))
		w.Header().Set("Content-Type", "application/json")
		if body.Get("email") != "2011820123@qq.com" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"邮箱与该账号绑定的邮箱不一致"}`))
			return
		}
		if body.Get("confirm_id") != "confirm-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"message":"确认标识已过期，请重新登录"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "admin_session", Value: "mail-ok", Path: "/", MaxAge: 2592000})
		_, _ = w.Write([]byte(`{"success":true,"message":"","data":{"user_id":11200,"role":"user","username":"pansou"}}`))
	})

	srv := httptest.NewServer(mux)
	old := DefaultBaseURL
	DefaultBaseURL = srv.URL
	t.Cleanup(func() {
		DefaultBaseURL = old
		srv.Close()
	})
	return srv
}

func readBody(r *http.Request) string {
	defer r.Body.Close()
	buf, _ := io.ReadAll(r.Body)
	return string(buf)
}

func TestFetchCaptcha(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()

	id, image, err := p.fetchCaptcha()
	if err != nil {
		t.Fatalf("取验证码失败: %v", err)
	}
	if id != "cap-1" {
		t.Fatalf("captcha id 应为 cap-1，实际 %q", id)
	}
	if !strings.HasPrefix(image, "data:image/png;base64,") {
		t.Fatalf("验证码图片应为 base64 data URL，实际 %q", image)
	}
}

func TestDoLoginRequiresCaptcha(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()

	// 不传验证码：必须在本地就拦住，不去打站点。
	if _, err := p.doLogin("pansou", "pw", true, "", ""); !errors.Is(err, errCaptchaRequired) {
		t.Fatalf("缺验证码应返回 errCaptchaRequired，实际 %v", err)
	}
	// 传了但站点判错。
	if _, err := p.doLogin("pansou", "pw", true, "cap-1", "WRONG"); !errors.Is(err, errCaptchaInvalid) {
		t.Fatalf("验证码错误应返回 errCaptchaInvalid，实际 %v", err)
	}
}

func TestDoLoginNeedsEmailConfirmation(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()

	attempt, err := p.doLogin("pansou", "pw", true, "cap-1", "NEEDMAIL")
	if err != nil {
		t.Fatalf("需要邮箱确认不应报错: %v", err)
	}
	if !attempt.NeedEmail {
		t.Fatal("应识别为需要邮箱确认")
	}
	if attempt.ConfirmID != "confirm-1" {
		t.Fatalf("confirm_id 应为 confirm-1，实际 %q", attempt.ConfirmID)
	}
	if attempt.EmailHint != "2******3@qq.com" {
		t.Fatalf("邮箱提示应透传掩码邮箱，实际 %q", attempt.EmailHint)
	}
	if attempt.Cookie != "" {
		t.Fatal("邮箱确认前不应拿到 Cookie")
	}
}

func TestDoLoginDirectSuccess(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()

	attempt, err := p.doLogin("pansou", "pw", true, "cap-1", "OLDFLOW")
	if err != nil {
		t.Fatalf("直接成功路径不应报错: %v", err)
	}
	if attempt.NeedEmail {
		t.Fatal("不应要求邮箱确认")
	}
	if !strings.Contains(attempt.Cookie, "admin_session=direct-ok") {
		t.Fatalf("应带回会话 Cookie，实际 %q", attempt.Cookie)
	}
	if attempt.Username != "pansou" {
		t.Fatalf("用户名应为 pansou，实际 %q", attempt.Username)
	}
}

func TestConfirmEmailLogin(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()

	cookie, username, err := p.confirmEmailLogin("confirm-1", "2011820123@qq.com", true)
	if err != nil {
		t.Fatalf("邮箱确认应成功: %v", err)
	}
	if !strings.Contains(cookie, "admin_session=mail-ok") {
		t.Fatalf("应带回会话 Cookie，实际 %q", cookie)
	}
	if username != "pansou" {
		t.Fatalf("用户名应为 pansou，实际 %q", username)
	}

	// 邮箱不匹配：站点会拒绝，必须原样把消息带出来。
	if _, _, err := p.confirmEmailLogin("confirm-1", "someone@else.com", true); err == nil {
		t.Fatal("邮箱不匹配应报错")
	}
	// 确认标识过期。
	if _, _, err := p.confirmEmailLogin("stale", "2011820123@qq.com", true); err == nil {
		t.Fatal("过期 confirm_id 应报错")
	}
	// 参数缺失在本地拦住。
	if _, _, err := p.confirmEmailLogin("", "2011820123@qq.com", true); err == nil {
		t.Fatal("缺 confirm_id 应报错")
	}
}

// 站点会话 Cookie 带 Secure，浏览器/Go 只在 https 下保存；真实站点是 https，
// 这里用 http 假站点时不能带 Secure，否则 CookieJar 会丢弃、测试假失败。
func TestSecureCookieNotUsedInFakeSite(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()
	attempt, err := p.doLogin("pansou", "pw", true, "cap-1", "OLDFLOW")
	if err != nil || attempt.Cookie == "" {
		t.Fatalf("假站点必须能拿到 Cookie（不要设 Secure）: %v", err)
	}
}

func TestHandleCaptchaAndLoginFlow(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	p.RegisterWebRoutes(r.Group("/api"))

	hash := strings.Repeat("a", 64)
	post := func(payload string) map[string]interface{} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/panlian/"+hash, strings.NewReader(payload))
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

	// 1) 取验证码
	captcha := post(`{"action":"captcha"}`)
	if captcha["success"] != true {
		t.Fatalf("取验证码失败: %v", captcha)
	}
	data := captcha["data"].(map[string]interface{})
	if data["captcha_id"] != "cap-1" || data["image"] == "" {
		t.Fatalf("验证码响应不完整: %v", data)
	}

	// 2) 直接登录：缺验证码要被挡住并带上 captcha_required
	blocked := post(`{"action":"login","username":"pansou","password":"pw"}`)
	if blocked["success"] != false {
		t.Fatalf("缺验证码应失败: %v", blocked)
	}
	if bd := blocked["data"].(map[string]interface{}); bd["captcha_required"] != true {
		t.Fatalf("应带 captcha_required 标记: %v", bd)
	}

	// 3) 验证码错：同样带标记，前端据此自动换一张
	invalid := post(`{"action":"login","username":"pansou","password":"pw","captcha_id":"cap-1","captcha_code":"WRONG"}`)
	if id := invalid["data"].(map[string]interface{}); id["captcha_invalid"] != true {
		t.Fatalf("应带 captcha_invalid 标记: %v", id)
	}

	// 4) 登录通过但要求邮箱确认
	needMail := post(`{"action":"login","username":"pansou","password":"pw","captcha_id":"cap-1","captcha_code":"NEEDMAIL"}`)
	if needMail["success"] != true {
		t.Fatalf("需要邮箱确认时 success 应为 true: %v", needMail)
	}
	nd := needMail["data"].(map[string]interface{})
	if nd["need_email"] != true || nd["confirm_id"] != "confirm-1" || nd["email_hint"] != "2******3@qq.com" {
		t.Fatalf("邮箱确认响应不完整: %v", nd)
	}
	// 此时还不应该落盘成 active
	if u, ok := p.getUserByHash(hash); ok && u.Status == "active" {
		t.Fatal("邮箱确认前不应标记为 active")
	}

	// 5) 提交邮箱完成登录
	done := post(`{"action":"confirm_email","username":"pansou","password":"pw","confirm_id":"confirm-1","email":"2011820123@qq.com"}`)
	if done["success"] != true {
		t.Fatalf("邮箱确认应成功: %v", done)
	}
	dd := done["data"].(map[string]interface{})
	if dd["status"] != "active" || dd["username"] != "pansou" {
		t.Fatalf("完成响应不完整: %v", dd)
	}
	user, ok := p.getUserByHash(hash)
	if !ok || user.Status != "active" {
		t.Fatalf("用户应已落盘且为 active: %+v", user)
	}
	if !strings.Contains(user.Cookie, "admin_session=mail-ok") {
		t.Fatalf("应保存会话 Cookie，实际 %q", user.Cookie)
	}
	if user.ExpireAt.Before(time.Now().Add(29 * 24 * time.Hour)) {
		t.Fatalf("有效期应约 30 天，实际 %v", user.ExpireAt)
	}
}

// 自动续期在站点加验证码后已不可行：必须标记过期并给出可读原因，而不是静默失败。
func TestReloginUserMarksExpired(t *testing.T) {
	newFakeSite(t)
	p := NewPanlianPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })

	hash := strings.Repeat("b", 64)
	enc, err := p.encryptPassword("pw")
	if err != nil {
		t.Fatalf("加密密码失败: %v", err)
	}
	user := &User{Hash: hash, Username: "pansou", EncryptedPassword: enc, Cookie: "old-cookie", Status: "active"}
	p.users.Store(hash, user)

	if err := p.reloginUser(user); err == nil {
		t.Fatal("自动续期应返回错误说明原因")
	}
	if user.Status != "expired" {
		t.Fatalf("应标记为 expired，实际 %q", user.Status)
	}
	if user.Cookie != "" {
		t.Fatalf("过期后应清空 Cookie，实际 %q", user.Cookie)
	}
	if got, ok := p.getUserByHash(hash); !ok || got.Status != "expired" {
		t.Fatalf("落盘状态应为 expired: %+v", got)
	}
}
