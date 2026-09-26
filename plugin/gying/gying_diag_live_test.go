package gying

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pansou/plugin"
	"pansou/util/json"
)

// 真实站点诊断：默认跳过，设置 GYING_DIAG=1 才运行。
//
//	GYING_DIAG=1 GYING_TEST_USERNAME=... GYING_TEST_PASSWORD=... \
//	  go test ./plugin/gying/ -run TestGyingDiagnoseSearch -v
//
// 站点自 2026 起加了两层门：先过 browser_pow 计算验证（24 小时 browser_verified），
// 搜索还必须带登录会话。这个用例把每一层的真实响应落盘，用来定位断在哪一层。
func TestGyingDiagnoseSearch(t *testing.T) {
	if os.Getenv("GYING_DIAG") == "" {
		t.Skip("未设置 GYING_DIAG=1，跳过 gying 站点诊断")
	}
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("需要 GYING_TEST_USERNAME / GYING_TEST_PASSWORD")
	}

	dir := t.TempDir()
	oldStorage := StorageDir
	StorageDir = dir
	t.Cleanup(func() { StorageDir = oldStorage })

	p := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}

	t.Logf("站点: %s", p.getBaseURL())

	// 1) 登录：确认 PoW 求解与会话获取是否正常
	scraper, cookie, err := p.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin 失败: %v", err)
	}
	t.Logf("登录成功: cookie 长度=%d", len(cookie))
	for _, c := range strings.Split(cookie, ";") {
		if name := strings.TrimSpace(strings.SplitN(c, "=", 2)[0]); name != "" {
			t.Logf("  会话 Cookie: %s", name)
		}
	}
	if client, cerr := getScraperClient(scraper); cerr == nil && client.Jar != nil {
		u, _ := url.Parse(p.getBaseURL())
		var names []string
		for _, ck := range client.Jar.Cookies(u) {
			names = append(names, ck.Name)
		}
		t.Logf("登录后 jar 内 Cookie: %s", strings.Join(names, ", "))
	}

	// 2) 直接取一次搜索页，把真实响应落盘（这一层能看到是挑战页、登录壳还是真结果）
	searchURL := p.getBaseURL() + "/search?q=" + "一人之下" + "&type=0&mode=2"
	body, status, _, err := p.requestWithChallengeRetry(scraper, "GET", searchURL, "", "")
	if err != nil {
		t.Errorf("requestWithChallengeRetry 失败: %v", err)
	} else {
		t.Logf("搜索页: status=%d len=%d challenge=%v loginShell=%v",
			status, len(body), isBotChallengePage(body), isLoginShell(body))
		out := filepath.Join(os.TempDir(), "gying-diag-search.html")
		if werr := os.WriteFile(out, body, 0o644); werr == nil {
			t.Logf("响应已落盘: %s", out)
		}
		t.Logf("  含 _obj.search: %v", strings.Contains(string(body), "_obj.search"))
		if i := strings.Index(string(body), "_obj.search"); i >= 0 {
			end := i + 400
			if end > len(body) {
				end = len(body)
			}
			t.Logf("  片段: %s", string(body)[i:end])
		}
		if title := extractTitleForDiag(string(body)); title != "" {
			t.Logf("  标题: %s", title)
		}
	}

	// 3) 走完整的搜索路径，看最终返回
	results, err := p.searchWithScraper("一人之下", scraper)
	if err != nil {
		t.Errorf("searchWithScraper 失败: %v", err)
	}
	t.Logf("searchWithScraper 返回 %d 条", len(results))
	for i, r := range results {
		if i >= 3 {
			break
		}
		t.Logf("  [%d] %s（%d 链接）", i+1, r.Title, len(r.Links))
	}
}

func extractTitleForDiag(html string) string {
	i := strings.Index(html, "<title>")
	if i < 0 {
		return ""
	}
	rest := html[i+len("<title>"):]
	j := strings.Index(rest, "</title>")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// 复现生产路径：应用重启后从磁盘恢复会话，再走插件入口 Search。
// 三种状态都要测：正常会话、缺 browser_verified、会话彻底失效（要能自动重登恢复）。
func TestGyingDiagnoseProductionPath(t *testing.T) {
	if os.Getenv("GYING_DIAG") != "" && os.Getenv("GYING_DIAG_PROD") == "" {
		t.Skip("设置 GYING_DIAG_PROD=1 才跑生产路径诊断")
	}
	if os.Getenv("GYING_DIAG_PROD") == "" {
		t.Skip("未设置 GYING_DIAG_PROD=1，跳过生产路径诊断")
	}
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("需要 GYING_TEST_USERNAME / GYING_TEST_PASSWORD")
	}

	dir := t.TempDir()
	oldStorage := StorageDir
	StorageDir = dir
	t.Cleanup(func() { StorageDir = oldStorage })

	newPlugin := func() *GyingPlugin {
		q := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}
		q.loadAllUsers()
		return q
	}

	// 阶段一：真实登录一次，落盘（等价于应用里点一次登录）
	p1 := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}
	scraper, cookie, err := p1.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin 失败: %v", err)
	}
	_ = scraper
	hash := p1.generateHash(username)
	enc, err := p1.encryptPassword(password)
	if err != nil {
		t.Fatalf("加密密码失败: %v", err)
	}
	user := &User{
		Hash: hash, Username: username, EncryptedPassword: enc, Cookie: cookie,
		Status: "active", LoginAt: time.Now(),
		ExpireAt: time.Now().Add(30 * 24 * time.Hour), LastAccessAt: time.Now(), CreatedAt: time.Now(),
	}
	if err := p1.saveUser(user); err != nil {
		t.Fatalf("保存用户失败: %v", err)
	}
	t.Logf("阶段一：登录并落盘完成，cookie 长度=%d", len(cookie))

	search := func(p *GyingPlugin, label string) {
		users := p.getActiveUsers()
		t.Logf("%s：有效用户 %d 个", label, len(users))
		results, err := p.Search("一人之下", map[string]interface{}{})
		if err != nil {
			t.Errorf("%s：Search 返回错误: %v", label, err)
			return
		}
		t.Logf("%s：Search 返回 %d 条", label, len(results))
		if len(results) > 0 {
			t.Logf("  首条: %s（%d 链接）", results[0].Title, len(results[0].Links))
		}
	}

	// 阶段二：模拟应用重启（新实例 + 从磁盘恢复），直接搜索
	p2 := newPlugin()
	search(p2, "阶段二 重启恢复后搜索")

	// 阶段三：模拟 browser_verified 过期（24 小时）——从落盘 cookie 里去掉它
	stripped := cookie
	var parts []string
	for _, kv := range strings.Split(cookie, ";") {
		if !strings.Contains(kv, "browser_verified") && strings.TrimSpace(kv) != "" {
			parts = append(parts, strings.TrimSpace(kv))
		}
	}
	stripped = strings.Join(parts, "; ")
	t.Logf("阶段三：剥掉 browser_verified 后 cookie=%s", stripped)
	user.Cookie = stripped
	if err := p2.saveUser(user); err != nil {
		t.Fatalf("保存剥离后的用户失败: %v", err)
	}
	p3 := newPlugin()
	search(p3, "阶段三 缺 browser_verified 时搜索")

	// 阶段四：模拟会话彻底失效（伪造坏 cookie），要求能自动重登恢复
	user.Cookie = "PHPSESSID=deadbeefdeadbeefdeadbeefdeadbeef; app_auth=dead"
	if err := p3.saveUser(user); err != nil {
		t.Fatalf("保存坏 cookie 失败: %v", err)
	}
	p4 := newPlugin()
	search(p4, "阶段四 会话失效时搜索（应自动重登）")
}

// 计时诊断：站点加 PoW 后单次搜索要多久，与默认 PLUGIN_TIMEOUT=10 秒对比。
func TestGyingDiagnoseLatency(t *testing.T) {
	if os.Getenv("GYING_DIAG_LATENCY") == "" {
		t.Skip("未设置 GYING_DIAG_LATENCY=1，跳过计时诊断")
	}
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("需要 GYING_TEST_USERNAME / GYING_TEST_PASSWORD")
	}

	oldStorage := StorageDir
	StorageDir = t.TempDir()
	t.Cleanup(func() { StorageDir = oldStorage })

	p := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}

	t0 := time.Now()
	scraper, _, err := p.doLogin(username, password)
	loginCost := time.Since(t0)
	if err != nil {
		t.Fatalf("doLogin 失败: %v", err)
	}
	t.Logf("登录（含一次 PoW）耗时: %s", loginCost.Round(time.Millisecond))

	t1 := time.Now()
	results, err := p.searchWithScraper("一人之下", scraper)
	firstCost := time.Since(t1)
	if err != nil {
		t.Fatalf("首次搜索失败: %v", err)
	}
	t.Logf("首次搜索（热会话）耗时: %s，返回 %d 条", firstCost.Round(time.Millisecond), len(results))

	// 模拟 browser_verified 过期：剥离后必须重新算 PoW，这是"每天都可能发生的一次慢搜索"
	client, cerr := getScraperClient(scraper)
	if cerr != nil || client.Jar == nil {
		t.Fatalf("取 scraper 客户端失败: %v", cerr)
	}
	u, _ := url.Parse(p.getBaseURL())
	var kept []*http.Cookie
	for _, ck := range client.Jar.Cookies(u) {
		if ck.Name != "browser_verified" {
			kept = append(kept, ck)
		}
	}
	client.Jar.SetCookies(u, kept)
	t.Logf("剥离 browser_verified 后 jar 内剩余: %d 个 Cookie", len(kept))

	t2 := time.Now()
	results2, err := p.searchWithScraper("一人之下", scraper)
	coldCost := time.Since(t2)
	if err != nil {
		t.Fatalf("冷搜索失败: %v", err)
	}
	t.Logf("冷搜索（需重算 PoW）耗时: %s，返回 %d 条", coldCost.Round(time.Millisecond), len(results2))

	t.Logf("对比：默认 PLUGIN_TIMEOUT=10s")
	if coldCost > 10*time.Second {
		t.Logf("结论：冷搜索 %s 超过默认超时，管理器会拿不到 gying 的结果", coldCost.Round(time.Second))
	} else {
		t.Logf("结论：冷搜索 %s 未超过默认超时", coldCost.Round(time.Second))
	}
}

// 验证 PoW 提交前的 3 秒人工等待是否必要：置 0 后冷路径是否仍能通过验证。
func TestGyingDiagnosePowFloor(t *testing.T) {
	if os.Getenv("GYING_DIAG_FLOOR") == "" {
		t.Skip("未设置 GYING_DIAG_FLOOR=1，跳过 PoW 下限验证")
	}
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("需要 GYING_TEST_USERNAME / GYING_TEST_PASSWORD")
	}

	oldStorage := StorageDir
	StorageDir = t.TempDir()
	oldFloor := powMinSolveTime
	powMinSolveTime = 0
	t.Cleanup(func() { StorageDir = oldStorage; powMinSolveTime = oldFloor })

	p := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}
	scraper, _, err := p.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin 失败: %v", err)
	}
	t.Log("登录完成（本次登录阶段用的是默认下限，先还原再测冷搜索）")

	powMinSolveTime = 0
	client, cerr := getScraperClient(scraper)
	if cerr != nil || client.Jar == nil {
		t.Fatalf("取 scraper 客户端失败: %v", cerr)
	}
	u, _ := url.Parse(p.getBaseURL())
	var kept []*http.Cookie
	for _, ck := range client.Jar.Cookies(u) {
		if ck.Name != "browser_verified" {
			kept = append(kept, ck)
		}
	}
	client.Jar.SetCookies(u, kept)

	start := time.Now()
	results, err := p.searchWithScraper("一人之下", scraper)
	cost := time.Since(start)
	if err != nil {
		t.Fatalf("下限置 0 后冷搜索失败: %v（说明服务端确实要求等待，需保留下限）", err)
	}
	t.Logf("下限置 0 冷搜索: %s，返回 %d 条结论=服务端不校验提交时刻，可省掉这 3 秒", cost.Round(time.Millisecond), len(results))
}

// 阶段计时：会话预热完成后，搜索页请求与详情并发各占多久。
// 框架的响应观察窗口默认 4 秒，冷路径每多一秒都在压缩余量。
func TestGyingDiagnosePhases(t *testing.T) {
	if os.Getenv("GYING_DIAG_PHASES") == "" {
		t.Skip("未设置 GYING_DIAG_PHASES=1，跳过阶段计时")
	}
	username := os.Getenv("GYING_TEST_USERNAME")
	password := os.Getenv("GYING_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("需要 GYING_TEST_USERNAME / GYING_TEST_PASSWORD")
	}

	oldStorage := StorageDir
	StorageDir = t.TempDir()
	t.Cleanup(func() { StorageDir = oldStorage })

	p := &GyingPlugin{BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3), baseURL: DefaultGyingBaseURL}
	scraper, _, err := p.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin 失败: %v", err)
	}

	// 模拟启动预热：把 PoW 与防爬 cookie 提前做掉
	t0 := time.Now()
	if err := p.warmupSession(scraper, nil); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	t.Logf("阶段0 预热: %s", time.Since(t0).Round(time.Millisecond))

	// 阶段1：搜索页
	searchURL := fmt.Sprintf("%s/search?q=%s&type=0&mode=2", p.getBaseURL(), url.QueryEscape("一人之下"))
	t1 := time.Now()
	body, status, _, err := p.requestWithChallengeRetry(scraper, http.MethodGet, searchURL, "", "")
	d1 := time.Since(t1)
	if err != nil {
		t.Fatalf("搜索页请求失败: %v", err)
	}
	t.Logf("阶段1 搜索页请求: %s (status=%d, len=%d)", d1.Round(time.Millisecond), status, len(body))

	matches := searchDataPattern.FindSubmatch(body)
	if len(matches) < 2 {
		t.Fatalf("未匹配到 _obj.search")
	}
	var searchData SearchData
	if err := json.Unmarshal(matches[1], &searchData); err != nil {
		t.Fatalf("解析搜索数据失败: %v", err)
	}
	t.Logf("搜索页候选条目数: %d", len(searchData.L.I))

	// 阶段2：详情并发
	t2 := time.Now()
	results, err := p.fetchAllDetails(&searchData, scraper, "一人之下")
	d2 := time.Since(t2)
	if err != nil {
		t.Fatalf("fetchAllDetails 失败: %v", err)
	}
	t.Logf("阶段2 详情并发: %s，返回 %d 条", d2.Round(time.Millisecond), len(results))
	t.Logf("合计（不含预热）: %s；若超过 4 秒，框架观察窗口会截断", (d1 + d2).Round(time.Millisecond))
}
