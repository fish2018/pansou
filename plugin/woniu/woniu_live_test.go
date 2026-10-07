package woniu

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"pansou/plugin"
)

// 真实站点联调测试：默认跳过，只有显式设置 WONIU_LIVE 时才运行。
// 凭据只从环境变量读，不写进仓库。
//
//	WONIU_LIVE=1 WONIU_LIVE_USER=<账号> WONIU_LIVE_PASS=<密码> \
//	  go test ./plugin/woniu/ -run TestLiveWoniu -v
func TestLiveWoniuLoginAndSearch(t *testing.T) {
	if strings.TrimSpace(os.Getenv("WONIU_LIVE")) == "" {
		t.Skip("未设置 WONIU_LIVE，跳过真实站点联调")
	}
	username := strings.TrimSpace(os.Getenv("WONIU_LIVE_USER"))
	password := os.Getenv("WONIU_LIVE_PASS")
	if username == "" || password == "" {
		t.Skip("未设置 WONIU_LIVE_USER / WONIU_LIVE_PASS，跳过真实站点联调")
	}

	p := NewWoniuPlugin()
	oldStorage := storageDir
	storageDir = t.TempDir()
	t.Cleanup(func() { storageDir = oldStorage })

	user, err := p.doLogin(username, password)
	if err != nil {
		t.Fatalf("doLogin() error = %v", err)
	}
	if user.Cookie == "" {
		t.Fatal("doLogin() 未返回 Cookie")
	}
	t.Logf("登录成功: username=%s cookieLen=%d expire=%s", user.Username, len(user.Cookie), formatTime(user.ExpireAt))

	if !p.verifyUser(user) {
		t.Fatal("verifyUser() 判定刚登录的会话无效")
	}
	// searchImpl 从插件账号池里取账号，联调时把刚登录的会话放进去。
	p.users.Store(user.Hash, user)

	// 详情页必须登录才给链接，这里直接跑完整 searchImpl。
	keyword := strings.TrimSpace(os.Getenv("WONIU_LIVE_KEYWORD"))
	if keyword == "" {
		keyword = "凡人修仙传"
	}

	start := time.Now()
	results, err := p.searchImpl(p.httpClient(), keyword, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("searchImpl() error = %v", err)
	}
	t.Logf("关键词 %q 命中 %d 条结果，耗时 %v", keyword, len(results), elapsed)

	if len(results) == 0 {
		t.Fatalf("关键词 %q 未返回结果", keyword)
	}

	// 发布预算：超过窗口的结果会被框架整批丢弃，实测必须落在预算内。
	budget := plugin.PublishBudget()
	if elapsed > budget {
		t.Errorf("搜索耗时 %v 超出发布预算 %v，结果会被框架丢弃", elapsed, budget)
	}

	totalLinks := 0
	for _, result := range results {
		if len(result.Links) == 0 {
			t.Errorf("结果 %q 没有链接", result.Title)
		}
		if result.Channel != "" {
			t.Errorf("结果 %q 的 Channel 应为空，实际 %q", result.Title, result.Channel)
		}
		if !strings.HasPrefix(result.UniqueID, "woniu-") {
			t.Errorf("结果 %q 的 UniqueID 前缀错误: %q", result.Title, result.UniqueID)
		}
		for _, link := range result.Links {
			if link.URL == "" || strings.Contains(link.URL, "******") {
				t.Errorf("结果 %q 含无效链接: %+v", result.Title, link)
			}
			if link.Type == "" {
				t.Errorf("结果 %q 的链接缺少类型: %+v", result.Title, link)
			}
		}
		totalLinks += len(result.Links)
		t.Logf("  - %s [%d 条链接]", result.Title, len(result.Links))
	}
	t.Logf("合计 %d 条链接", totalLinks)
}

// TestLiveWoniuGuestIsLocked 验证一个关键前提：不带登录态时详情页只给锁定预览。
// 这条假设一旦不成立（站点放开游客访问），插件的"必须有账号"策略就该重新评估。
func TestLiveWoniuGuestIsLocked(t *testing.T) {
	if strings.TrimSpace(os.Getenv("WONIU_LIVE")) == "" {
		t.Skip("未设置 WONIU_LIVE，跳过真实站点联调")
	}

	p := NewWoniuPlugin()
	client := p.newHTTPClient(RequestTimeout, nil)

	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.getBaseURL()+"/voddetail/7128/", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	p.setHeaders(req, p.getBaseURL()+"/", "")

	doc, err := p.loadDocument(ctx, client, req)
	if err != nil {
		t.Fatalf("loadDocument() error = %v", err)
	}
	if !isLockedPage(doc) {
		t.Error("游客访问详情页未呈现锁定态，插件的账号策略需要重新评估")
	}
	if links := parsePanLinks(doc, "标题"); len(links) != 0 {
		t.Errorf("游客详情页解析出了链接，说明站点已放开访问: %+v", links)
	}
}
