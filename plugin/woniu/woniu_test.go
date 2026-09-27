package woniu

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-gonic/gin"
)

// newTestPlugin 造一个指向假站点的插件，并把账号存储挪到临时目录，
// 避免测试污染真实 cache/woniu_users。
func newTestPlugin(t *testing.T, baseURL string) *WoniuPlugin {
	t.Helper()
	p := NewWoniuPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })
	p.config.BaseURL = baseURL
	return p
}

func mustDoc(t *testing.T, html string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatalf("解析测试 HTML 失败: %v", err)
	}
	return doc
}

// 搜索结果页的真实结构：卡片是 a.video-card，标题在 title 属性里，
// 详情链接形如 /voddetail/{id}/。
func TestParseSearchItems(t *testing.T) {
	const page = `<html><body><div class="video-grid">
	  <a href="/voddetail/35106/" class="video-card" title="凡人修仙传：慕兰之战">
	    <div class="video-poster"><img src="/upload/vod/a.jpg" alt="x"><div class="video-episode">4K高码</div></div>
	    <div class="video-info"><div class="video-title">凡人修仙传：慕兰之战</div><div class="video-meta"> 2026 · 中国大陆 · 动漫 </div></div>
	  </a>
	  <a href="/voddetail/35056/" class="video-card" title="凡人修仙传 年番4">
	    <div class="video-poster"><img src="https://cdn.example.com/b.jpg"></div>
	    <div class="video-info"><div class="video-title">凡人修仙传 年番4</div><div class="video-meta"> 2026 · 中国大陆 · 动漫 </div></div>
	  </a>
	  <a href="/voddetail/35106/" class="video-card" title="重复卡片"></a>
	  <a href="/user/login/" class="video-card" title="非详情链接"></a>
	  <a href="/voddetail/99999/" class="video-card" title=""></a>
	</div></body></html>`

	items := parseSearchItems(mustDoc(t, page), "https://wn4k.com")
	if len(items) != 2 {
		t.Fatalf("期望解析出 2 条（去重 + 丢弃无效卡片），实际 %d 条: %+v", len(items), items)
	}
	if items[0].ID != "35106" || items[0].Title != "凡人修仙传：慕兰之战" {
		t.Errorf("第一条解析错误: %+v", items[0])
	}
	if items[0].URL != "https://wn4k.com/voddetail/35106/" {
		t.Errorf("相对链接未补全为绝对地址: %q", items[0].URL)
	}
	if items[0].Note != "4K高码" {
		t.Errorf("备注解析错误: %q", items[0].Note)
	}
	if items[1].Pic != "https://cdn.example.com/b.jpg" {
		t.Errorf("已是绝对地址的封面不应被改写: %q", items[1].Pic)
	}
}

// 登录态的详情页：链接在 pan-link-meta 文本里，密码是 URL 的 password/pwd 参数。
func TestParsePanLinks(t *testing.T) {
	const page = `<html><body><section class="detail-panel">
	  <div class="pan-group" data-pan-group>
	    <div class="pan-group-title">115离线下载</div>
	    <div class="pan-link-list">
	      <div class="pan-link-item" data-pan-item>
	        <div class="pan-link-info">
	          <div class="pan-link-title">豆豆农场 S01E01-E04 MP4 7.30 GB</div>
	          <div class="pan-link-meta">https://115.com/s/swsqdks3nbi?password=8888</div>
	        </div>
	        <div class="pan-link-actions">
	          <a class="pan-link-btn" href="https://115.com/s/swsqdks3nbi?password=8888">打开</a>
	          <button class="pan-link-btn secondary" data-copy="https://115.com/s/swsqdks3nbi?password=8888">复制</button>
	        </div>
	      </div>
	      <div class="pan-link-item" data-pan-item>
	        <div class="pan-link-info">
	          <div class="pan-link-title">凡人修仙传 4K</div>
	          <div class="pan-link-meta">https://pan.baidu.com/s/1pfS_6nOfutNNxnaKjcUNQA?pwd=xA3R</div>
	        </div>
	      </div>
	      <div class="pan-link-item" data-pan-item>
	        <div class="pan-link-info">
	          <div class="pan-link-title">夸克备份</div>
	          <div class="pan-link-meta">https://pan.quark.cn/s/abcdef123456</div>
	        </div>
	      </div>
	    </div>
	  </div>
	</section></body></html>`

	links := parsePanLinks(mustDoc(t, page), "兜底标题")
	if len(links) != 3 {
		t.Fatalf("期望 3 条链接，实际 %d 条: %+v", len(links), links)
	}

	if links[0].Type != "115" || links[0].Password != "8888" {
		t.Errorf("115 链接解析错误: %+v", links[0])
	}
	if links[0].URL != "https://115.com/s/swsqdks3nbi?password=8888" {
		t.Errorf("URL 应保留原始查询参数: %q", links[0].URL)
	}
	// work_title 必须带上影片名：Service 层只用它做关键词过滤，
	// 而站点的资源备注常常不含作品名（见 TestComposeWorkTitleKeepsMovieTitle）。
	if links[0].WorkTitle != "兜底标题 豆豆农场 S01E01-E04 MP4 7.30 GB" {
		t.Errorf("work_title 应带上影片名: %q", links[0].WorkTitle)
	}

	if links[1].Type != "baidu" || links[1].Password != "xA3R" {
		t.Errorf("百度链接解析错误: %+v", links[1])
	}
	if links[2].Type != "quark" || links[2].Password != "" {
		t.Errorf("夸克链接解析错误: %+v", links[2])
	}
}

// 同一条记录在三处来源（data-pan-item / href / data-copy / meta 文本）都写着同一个
// URL，去重后只能算一条——否则单条结果会凭空多出三倍链接。
func TestParsePanLinksDeduplicatesSources(t *testing.T) {
	const page = `<html><body>
	  <div class="pan-link-item" data-pan-item="https://115cdn.com/s/same?password=abcd">
	    <div class="pan-link-title">同一链接</div>
	    <div class="pan-link-meta">https://115cdn.com/s/same?password=abcd</div>
	    <a class="pan-link-btn" href="https://115cdn.com/s/same?password=abcd">打开</a>
	    <button data-copy="https://115cdn.com/s/same?password=abcd">复制</button>
	  </div>
	</body></html>`

	links := parsePanLinks(mustDoc(t, page), "标题")
	if len(links) != 1 {
		t.Fatalf("同一链接被重复计入，期望 1 条实际 %d 条: %+v", len(links), links)
	}
	if links[0].Password != "abcd" {
		t.Errorf("密码解析错误: %+v", links[0])
	}
}

// 未登录的详情页只有锁定预览，链接一律是 https://******（登录后可见）。
// 这类页面必须被识别为锁定态，否则会把占位符当成真链接交出去。
func TestLockedPageDetection(t *testing.T) {
	const locked = `<html><body>
	  <div class="pan-lock-box">
	    <h3 class="pan-lock-title">网盘资源已锁定</h3>
	    <div class="pan-group"><div class="pan-group-title">115离线下载</div>
	      <div class="pan-link-item is-locked">
	        <div class="pan-link-title">某影片</div>
	        <div class="pan-link-meta">https://******（登录后可见）</div>
	      </div>
	    </div>
	  </div></body></html>`

	if !isLockedPage(mustDoc(t, locked)) {
		t.Error("锁定页未被识别")
	}

	// 锁定页上的占位符不能被当成链接。
	if links := parsePanLinks(mustDoc(t, locked), "标题"); len(links) != 0 {
		t.Errorf("锁定页不应解析出链接，实际: %+v", links)
	}

	const unlocked = `<html><body><div class="pan-link-item" data-pan-item>
	  <div class="pan-link-title">某影片</div>
	  <div class="pan-link-meta">https://115cdn.com/s/real?password=abcd</div>
	</div></body></html>`
	if isLockedPage(mustDoc(t, unlocked)) {
		t.Error("正常页被误判为锁定")
	}
}

// 没有 data-pan-item / meta 时，应能从"打开"按钮的 href 兜底取到链接。
func TestParsePanLinksFromButtonHref(t *testing.T) {
	const page = `<html><body>
	  <div class="pan-link-item">
	    <div class="pan-link-title">只有按钮</div>
	    <a class="pan-link-btn" href="https://drive.uc.cn/s/uc123456?pwd=zzzz">打开</a>
	  </div>
	</body></html>`

	links := parsePanLinks(mustDoc(t, page), "标题")
	if len(links) != 1 {
		t.Fatalf("未从按钮 href 取到链接: %+v", links)
	}
	if links[0].Type != "uc" || links[0].Password != "zzzz" {
		t.Errorf("UC 链接解析错误: %+v", links[0])
	}
}

func TestDetectPanType(t *testing.T) {
	cases := []struct {
		url  string
		typ  string
		want bool
	}{
		{"https://115cdn.com/s/abc", "115", true},
		{"https://115.com/s/abc", "115", true},
		{"https://anxia.com/s/abc", "115", true},
		{"https://pan.baidu.com/s/abc", "baidu", true},
		{"https://pan.quark.cn/s/abc", "quark", true},
		{"https://drive.uc.cn/s/abc", "uc", true},
		{"https://www.aliyundrive.com/s/abc", "aliyun", true},
		{"https://www.alipan.com/s/abc", "aliyun", true},
		{"https://cloud.189.cn/t/abc", "tianyi", true},
		{"https://pan.xunlei.com/s/abc", "xunlei", true},
		{"https://www.123pan.com/s/abc", "123", true},
		{"magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01", "magnet", true},
		{"ed2k://|file|a.mkv|123|abcdef0123456789abcdef0123456789|/", "ed2k", true},
		{"https://example.com/something", "others", true},
		{"not-a-link", "", false},
		{"", "", false},
	}

	for _, c := range cases {
		got, ok := detectPanType(c.url)
		if ok != c.want {
			t.Errorf("detectPanType(%q) 可用性 = %v，期望 %v", c.url, ok, c.want)
			continue
		}
		if ok && got != c.typ {
			t.Errorf("detectPanType(%q) = %q，期望 %q", c.url, got, c.typ)
		}
	}
}

// 链接后面常跟中文说明或标点，规整时必须截断，否则拼出的 URL 打不开。
func TestNormalizePanLinkStripsTrailingText(t *testing.T) {
	link, ok := normalizePanLink("https://115cdn.com/s/abc?password=1234 提取码：1234", "标题")
	if !ok {
		t.Fatal("应识别为有效链接")
	}
	if link.URL != "https://115cdn.com/s/abc?password=1234" {
		t.Errorf("URL 未正确截断: %q", link.URL)
	}
	if link.Password != "1234" {
		t.Errorf("密码解析错误: %q", link.Password)
	}
}

// 站点结构变化时 isLockedPage 的文本兜底：没有 pan-lock-box，但链接是占位符。
func TestLockedPageTextFallback(t *testing.T) {
	const page = `<html><body><div class="pan-link-meta">https://******（登录后可见）</div></body></html>`
	if !isLockedPage(mustDoc(t, page)) {
		t.Error("缺少 pan-lock-box 时未按文本兜底识别锁定态")
	}
}

// 站点把详情页正文里的简介放在 #detailDescText，内容里应带上备注与简介。
func TestBuildContentAndTags(t *testing.T) {
	const page = `<html><body>
	  <h1 class="premium-title">某影片</h1>
	  <div class="premium-tags-top">
	    <span class="p-tag score">★ 8.0</span>
	    <span class="p-tag">2026</span>
	    <span class="p-tag">中国大陆</span>
	    <span class="p-tag">动漫</span>
	  </div>
	  <div class="premium-meta-grid">
	    <div class="meta-item"><span class="m-label">更新时间</span><span class="m-val">2026-09-26 17:24</span></div>
	  </div>
	  <div id="detailDescText"> 平凡少年韩立出生贫困 </div>
	</body></html>`

	doc := mustDoc(t, page)
	content := buildContent(doc, searchItem{Note: "4K高码", Meta: "2026 · 中国大陆 · 动漫"})
	if !strings.Contains(content, "4K高码") || !strings.Contains(content, "平凡少年韩立") {
		t.Errorf("内容拼接不完整: %q", content)
	}

	tags := parseTags(doc)
	if len(tags) != 3 {
		t.Fatalf("评分标签应被剔除，期望 3 个标签实际 %d 个: %+v", len(tags), tags)
	}

	got := parseDetailTime(doc)
	want := time.Date(2026, 9, 26, 17, 24, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("更新时间解析 = %v，期望 %v", got, want)
	}
}

// 端到端：假站点上跑一次完整搜索，验证登录态详情解析、链接聚合与结果形状。
func TestSearchImplEndToEnd(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const searchPage = `<html><body><div class="video-grid">
	  <a href="/voddetail/1001/" class="video-card" title="测试影片A">
	    <div class="video-poster"><img src="/upload/a.jpg"></div>
	    <div class="video-info"><div class="video-title">测试影片A</div><div class="video-meta"> 2026 · 中国大陆 · 电影 </div></div>
	  </a>
	</div></body></html>`

	const detailPage = `<html><body>
	  <h1 class="premium-title">测试影片A</h1>
	  <div class="premium-tags-top"><span class="p-tag">2026</span><span class="p-tag">电影</span></div>
	  <div id="detailDescText">这是测试简介</div>
	  <div class="pan-link-item" data-pan-item>
	    <div class="pan-link-title">测试影片A 4K</div>
	    <div class="pan-link-meta">https://115cdn.com/s/testlink?password=abcd</div>
	  </div>
	</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/vodsearch/"):
			if !strings.Contains(r.URL.RawQuery, "wd=") {
				t.Errorf("搜索请求缺少 wd 参数: %s", r.URL.String())
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(searchPage))
		case strings.HasPrefix(r.URL.Path, "/voddetail/"):
			// 详情页必须带登录 Cookie 才给出真实链接。
			if !strings.Contains(r.Header.Get("Cookie"), "user_check=") {
				t.Errorf("详情请求未携带登录 Cookie")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(detailPage))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	user := &User{Hash: "h", Username: "tester", Cookie: "user_id=1; user_check=ok", Status: "active"}
	p.users.Store(user.Hash, user)

	results, err := p.searchImpl(srv.Client(), "测试影片", nil)
	if err != nil {
		t.Fatalf("searchImpl() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条: %+v", len(results), results)
	}

	got := results[0]
	if got.UniqueID != "woniu-1001" {
		t.Errorf("UniqueID = %q，期望 woniu-1001", got.UniqueID)
	}
	if got.Channel != "" {
		t.Errorf("插件结果的 Channel 必须为空，实际 %q", got.Channel)
	}
	if got.Title != "测试影片A" {
		t.Errorf("Title = %q", got.Title)
	}
	if len(got.Links) != 1 {
		t.Fatalf("期望 1 条链接，实际 %d 条: %+v", len(got.Links), got.Links)
	}
	if got.Links[0].Type != "115" || got.Links[0].Password != "abcd" {
		t.Errorf("链接解析错误: %+v", got.Links[0])
	}
	if len(got.Images) != 1 {
		t.Errorf("封面未带出: %+v", got.Images)
	}
}

// 没有可用账号时必须明确报错。站点详情页对游客只给锁定预览，
// 静默返回空结果会让部署方以为"关键词没资源"，实际是没配账号。
func TestSearchWithoutAccountFailsLoudly(t *testing.T) {
	p := newTestPlugin(t, "https://example.invalid")

	_, err := p.searchImpl(&http.Client{Timeout: time.Second}, "任意关键词", nil)
	if err == nil {
		t.Fatal("无账号时应当报错，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "未配置可用账号") {
		t.Errorf("错误信息未说明缺少账号: %v", err)
	}
}

// 搜索页有影片但详情全失败时（会话失效 / 站点改版）不能静默返回 0 条。
func TestSearchReportsAllDetailsFailed(t *testing.T) {
	const searchPage = `<html><body>
	  <a href="/voddetail/2001/" class="video-card" title="失效影片"></a>
	</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/vodsearch/") {
			w.Write([]byte(searchPage))
			return
		}
		// 详情页一律返回锁定态，模拟登录态失效。
		w.Write([]byte(`<html><body><div class="pan-lock-box">网盘资源已锁定</div></body></html>`))
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	p.users.Store("h", &User{Hash: "h", Username: "tester", Cookie: "user_check=stale", Status: "active"})

	_, err := p.searchImpl(srv.Client(), "失效影片", nil)
	if err == nil {
		t.Fatal("详情全部失败时应当报错，实际返回 nil")
	}
	if !strings.Contains(err.Error(), "未取到网盘链接") {
		t.Errorf("错误信息未说明原因: %v", err)
	}
}

// 登录：解析站点 {"code":1} 响应并收集 Cookie。
func TestDoLoginCollectsCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/login.html" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析登录表单失败: %v", err)
		}
		if r.Form.Get("user_name") != "tester" || r.Form.Get("user_pwd") != "secret" {
			t.Errorf("登录表单字段不符: %v", r.Form)
		}
		http.SetCookie(w, &http.Cookie{Name: "user_id", Value: "1703", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "user_check", Value: "abc123", Path: "/", Expires: time.Now().Add(720 * time.Hour)})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":1,"msg":"登录成功","url":"\/user\/index.html"}`))
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	user, err := p.doLogin("tester", "secret")
	if err != nil {
		t.Fatalf("doLogin() error = %v", err)
	}
	if !strings.Contains(user.Cookie, "user_check=abc123") {
		t.Errorf("Cookie 未收集: %q", user.Cookie)
	}
	if user.Status != "active" {
		t.Errorf("Status = %q，期望 active", user.Status)
	}
	if user.Username != "tester" {
		t.Errorf("Username = %q", user.Username)
	}
	if user.ExpireAt.IsZero() || time.Until(user.ExpireAt) < 24*time.Hour {
		t.Errorf("过期时间应按站点 Cookie 的 Expires 计算: %v", user.ExpireAt)
	}
}

// 登录失败时把站点原文带出来，便于区分"密码错"和"站点异常"。
func TestDoLoginFailureSurfacesMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"code":1003,"msg":"获取用户信息失败"}`))
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	_, err := p.doLogin("tester", "wrong")
	if err == nil {
		t.Fatal("登录失败时应当返回错误")
	}
	if !strings.Contains(err.Error(), "获取用户信息失败") {
		t.Errorf("错误信息未带出站点原文: %v", err)
	}
}

// 校验登录态：站点用 302 到 /user/login.html 表示未登录。
func TestVerifyUserRedirectsToLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "user_check=good") {
			http.Redirect(w, r, "/user/login.html", http.StatusFound)
			return
		}
		w.Write([]byte(`<html><body>会员中心</body></html>`))
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)

	valid := &User{Hash: "a", Cookie: "user_id=1; user_check=good", Status: "active"}
	if !p.verifyUser(valid) {
		t.Error("有效 Cookie 被判定为失效")
	}

	stale := &User{Hash: "b", Cookie: "user_id=1; user_check=bad", Status: "active"}
	if p.verifyUser(stale) {
		t.Error("失效 Cookie 被判定为有效")
	}
}

// 账号落盘后应能被重新加载，且文件权限为 0600（Cookie 等同于密码）。
func TestSaveAndLoadUsers(t *testing.T) {
	p := newTestPlugin(t, "https://wn4k.com")
	user := &User{
		Hash:         "deadbeef",
		Username:     "tester",
		Cookie:       "user_id=1; user_check=xyz",
		Status:       "active",
		CreatedAt:    time.Now(),
		LastAccessAt: time.Now(),
	}
	if err := p.saveUser(user); err != nil {
		t.Fatalf("saveUser() error = %v", err)
	}

	info, err := os.Stat(filepath.Join(storageDir, "deadbeef.json"))
	if err != nil {
		t.Fatalf("账号文件未落盘: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("账号文件权限 = %o，期望 600", perm)
	}

	fresh := NewWoniuPlugin()
	fresh.loadAllUsers()
	loaded, ok := fresh.getUserByHash("deadbeef")
	if !ok {
		t.Fatal("重新加载后找不到账号")
	}
	if loaded.Cookie != user.Cookie || loaded.Username != user.Username {
		t.Errorf("加载的账号内容不符: %+v", loaded)
	}
}

// 只有 active 且有 Cookie 的账号才参与搜索。
func TestGetActiveUsersFilters(t *testing.T) {
	p := newTestPlugin(t, "https://wn4k.com")
	p.users.Store("a", &User{Hash: "a", Status: "active", Cookie: "c=1", LastAccessAt: time.Now()})
	p.users.Store("b", &User{Hash: "b", Status: "expired", Cookie: "c=2", LastAccessAt: time.Now()})
	p.users.Store("c", &User{Hash: "c", Status: "active", Cookie: "", LastAccessAt: time.Now()})

	users := p.getActiveUsers()
	if len(users) != 1 || users[0].Hash != "a" {
		t.Fatalf("可用账号筛选错误: %+v", users)
	}
}

// 站点地址规范化：补协议、去尾斜杠。
func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"wn4k.com":          "https://wn4k.com",
		"https://wn4k.com/": "https://wn4k.com",
		"http://a.com":      "http://a.com",
		"  https://b.com//": "https://b.com",
		"":                  "",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 换站后旧 Cookie 不再适用，必须全部标记为待重登，否则会拿旧站会话去撞新站。
func TestUpdateBaseURLInvalidatesSessions(t *testing.T) {
	p := newTestPlugin(t, "https://old.example.com")
	p.users.Store("a", &User{Hash: "a", Status: "active", Cookie: "c=1"})

	normalized, err := p.updateBaseURL("new.example.com")
	if err != nil {
		t.Fatalf("updateBaseURL() error = %v", err)
	}
	if normalized != "https://new.example.com" {
		t.Errorf("规范化结果 = %q", normalized)
	}
	user, _ := p.getUserByHash("a")
	if user.Status != "expired" {
		t.Errorf("换站后账号状态 = %q，期望 expired", user.Status)
	}
	if len(p.getActiveUsers()) != 0 {
		t.Error("换站后不应再有可用账号")
	}
}

func TestUpdateBaseURLRejectsEmpty(t *testing.T) {
	p := newTestPlugin(t, "https://wn4k.com")
	if _, err := p.updateBaseURL("   "); err == nil {
		t.Error("空站点地址应当被拒绝")
	}
}

// 专属 hash 必须稳定：管理页链接靠它定位账号，变了就等于账号丢失。
func TestGenerateHashStable(t *testing.T) {
	p := NewWoniuPlugin()
	first := p.generateHash("pansou")
	second := p.generateHash("pansou")
	if first != second {
		t.Errorf("同一用户名生成了不同 hash: %q vs %q", first, second)
	}
	if len(first) != 64 {
		t.Errorf("hash 长度 = %d，期望 64", len(first))
	}
	if p.generateHash("other") == first {
		t.Error("不同用户名生成了相同 hash")
	}
	if !p.isHexString(first) {
		t.Errorf("hash 不是十六进制: %q", first)
	}
}

// 管理接口的端到端形状：状态查询 / 登录 / 退出。
func TestManageActions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/login.html":
			http.SetCookie(w, &http.Cookie{Name: "user_check", Value: "ok", Path: "/"})
			w.Write([]byte(`{"code":1,"msg":"登录成功"}`))
		case "/user/logout.html":
			http.Redirect(w, r, "/", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newTestPlugin(t, srv.URL)
	hash := p.generateHash("tester")

	router := gin.New()
	group := router.Group("")
	p.RegisterWebRoutes(group)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/woniu/"+hash, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. 初始状态：未登录。
	w := post(`{"action":"get_status"}`)
	if !strings.Contains(w.Body.String(), `"logged_in":false`) {
		t.Errorf("初始状态应为未登录: %s", w.Body.String())
	}

	// 2. 登录。
	w = post(`{"action":"login","username":"tester","password":"secret"}`)
	if !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("登录失败: %s", w.Body.String())
	}
	user, ok := p.getUserByHash(hash)
	if !ok || user.Status != "active" {
		t.Fatalf("登录后账号未激活: %+v", user)
	}

	// 3. 登录后状态。
	w = post(`{"action":"get_status"}`)
	if !strings.Contains(w.Body.String(), `"logged_in":true`) {
		t.Errorf("登录后状态应为已登录: %s", w.Body.String())
	}

	// 4. 配置查询。
	w = post(`{"action":"get_config"}`)
	if !strings.Contains(w.Body.String(), srv.URL) {
		t.Errorf("配置未返回当前站点地址: %s", w.Body.String())
	}

	// 5. 退出。
	w = post(`{"action":"logout"}`)
	if !strings.Contains(w.Body.String(), `"success":true`) {
		t.Fatalf("退出失败: %s", w.Body.String())
	}
	if _, ok := p.getUserByHash(hash); ok {
		t.Error("退出后账号仍存在")
	}

	// 6. 未知动作要报错而不是静默成功。
	w = post(`{"action":"nonsense"}`)
	if !strings.Contains(w.Body.String(), `"success":false`) {
		t.Errorf("未知动作应报错: %s", w.Body.String())
	}
}

// 测试搜索接口在未登录时必须拒绝，而不是返回空结果。
func TestTestSearchRequiresLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	p := newTestPlugin(t, "https://wn4k.com")
	hash := p.generateHash("tester")

	router := gin.New()
	p.RegisterWebRoutes(router.Group(""))

	req := httptest.NewRequest(http.MethodPost, "/woniu/"+hash, strings.NewReader(`{"action":"test_search","keyword":"测试"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "请先登录") {
		t.Errorf("未登录时测试搜索应被拒绝: %s", w.Body.String())
	}
}

// 用户名入口应重定向到专属 hash，且该 hash 可逆推出账号。
func TestManagePageRedirectsToHash(t *testing.T) {
	gin.SetMode(gin.TestMode)

	p := newTestPlugin(t, "https://wn4k.com")
	router := gin.New()
	p.RegisterWebRoutes(router.Group(""))

	req := httptest.NewRequest(http.MethodGet, "/woniu/pansou", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("状态码 = %d，期望 302", w.Code)
	}
	want := "/woniu/" + p.generateHash("pansou")
	if loc := w.Header().Get("Location"); loc != want {
		t.Errorf("重定向到 %q，期望 %q", loc, want)
	}
}

// 回归：站点给的资源备注常常只是规格描述，不含作品名。
// Service 层合并链接时只拿 WorkTitle 做关键词过滤，所以 WorkTitle 必须带上影片名，
// 否则会出现"results 里有结果、merged_by_type 却是空的"。
// 实测"庆余年"的备注是 "S02 4K WEB-DL DV HiveWeb-173.26GB"。
func TestComposeWorkTitleKeepsMovieTitle(t *testing.T) {
	cases := []struct {
		movie string
		note  string
		want  string
	}{
		{"庆余年 第二季", "S02 4K WEB-DL DV HiveWeb-173.26GB", "庆余年 第二季 S02 4K WEB-DL DV HiveWeb-173.26GB"},
		{"凡人修仙传", "凡人修仙传 4K WEB[364.19G]", "凡人修仙传 4K WEB[364.19G]"},
		{"某影片", "", "某影片"},
		{"某影片", "某影片", "某影片"},
		{"", "只有备注", "只有备注"},
		{"  某影片  ", "  备注  ", "某影片 备注"},
	}

	for _, c := range cases {
		got := composeWorkTitle(c.movie, c.note)
		if got != c.want {
			t.Errorf("composeWorkTitle(%q, %q) = %q，期望 %q", c.movie, c.note, got, c.want)
		}
		// 关键不变量：结果里必须包含影片名（影片名非空时）。
		if c.movie != "" && !strings.Contains(got, strings.TrimSpace(c.movie)) {
			t.Errorf("composeWorkTitle(%q, %q) = %q 丢失了影片名", c.movie, c.note, got)
		}
	}
}

// 回归：备注不含作品名时，解析出的链接仍要能被关键词过滤保住。
func TestParsePanLinksKeepsKeywordInWorkTitle(t *testing.T) {
	const page = `<html><body>
	  <div class="pan-link-item" data-pan-item>
	    <div class="pan-link-title">S02 4K WEB-DL DV HiveWeb-173.26GB</div>
	    <div class="pan-link-meta">https://115cdn.com/s/qingyu?password=d008</div>
	  </div>
	</body></html>`

	links := parsePanLinks(mustDoc(t, page), "庆余年 第二季")
	if len(links) != 1 {
		t.Fatalf("期望 1 条链接，实际 %d 条", len(links))
	}
	if !strings.Contains(links[0].WorkTitle, "庆余年") {
		t.Errorf("WorkTitle 丢了作品名，会被 Service 层关键词过滤丢弃: %q", links[0].WorkTitle)
	}
}
