package leso

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// rewriteTransport 把发往站点域名的请求改投到本地测试服务器，
// 这样整条搜索链路（POST search.php -> 302 结果页 -> 并发抓详情 -> 解析链接）
// 都能离线跑，不依赖外网，也不受站点频率策略影响。
type rewriteTransport struct{ target string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	parsed, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme = parsed.Scheme
	clone.URL.Host = parsed.Host
	clone.Host = parsed.Host
	return http.DefaultTransport.RoundTrip(clone)
}

const threadListPage = `<html><body><div class="slst">
<li class="pbw"><h3><a href="forum.php?mod=viewthread&amp;tid=101">凡人修仙传 年番4</a></h3><span>2026-9-20</span></li>
</div></body></html>`

func newTestServer(t *testing.T, detailHTML string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case r.URL.Query().Get("searchid") != "":
			// 结果页
			io.WriteString(w, threadListPage)
		case strings.HasPrefix(r.URL.Path, "/search.php"):
			// Discuz 的真实行为：POST 搜索后 302 到带 searchid 的结果页
			http.Redirect(w, r, "/search.php?mod=forum&searchid=9001&kw=x", http.StatusFound)
		default:
			io.WriteString(w, detailHTML)
		}
	}))
}

func newTestPlugin(target string) *Plugin {
	p := NewPlugin()
	p.client = &http.Client{Transport: rewriteTransport{target: target}}
	return p
}

// 走通完整链路：POST 被 302、结果页解析、详情页提取链接。
func TestSearchImplFollowsRedirectAndExtractsLinks(t *testing.T) {
	server := newTestServer(t, `<html><body><td class="t_f">
凡人修仙传 全集 链接: https://pan.quark.cn/s/abcdefg 提取码: 1234
</td></body></html>`)
	defer server.Close()

	results, err := newTestPlugin(server.URL).searchImpl(nil, "凡人修仙传", nil)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条", len(results))
	}
	links := results[0].Links
	if len(links) != 1 || links[0].Type != "quark" || links[0].Password != "1234" {
		t.Fatalf("链接解析不符合预期: %+v", links)
	}
}

// 搜索页给出了条目、详情页却一条链接都提取不到时，必须报错。
// 否则插件返回"0 条且无错误"，和"关键词确实没结果"完全分不清——
// 全量 65 插件同跑时实测出现过这个状态，日志里只留一个 leso=0。
func TestAllDetailsFailedReturnsError(t *testing.T) {
	server := newTestServer(t, `<html><body><td class="t_f">这个帖子什么链接都没有</td></body></html>`)
	defer server.Close()

	_, err := newTestPlugin(server.URL).searchImpl(nil, "凡人修仙传", nil)
	if err == nil {
		t.Fatal("详情页全部不可用时应当报错，实际返回了 nil")
	}
	if !strings.Contains(err.Error(), "抓取失败") {
		t.Fatalf("错误信息应说明详情页抓取失败，实际: %v", err)
	}
}

// 搜索页本身就没有条目：这是正常的"无结果"，不应报错。
func TestEmptySearchResultIsNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<html><body><div class="slst">没有找到相关结果</div></body></html>`)
	}))
	defer server.Close()

	results, err := newTestPlugin(server.URL).searchImpl(nil, "不存在的关键词", nil)
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("期望 0 条结果，实际 %d 条", len(results))
	}
}

// 部分条目失败不影响整体，成功的照样返回。
func TestPartialDetailFailureStillReturnsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case r.URL.Query().Get("searchid") != "":
			io.WriteString(w, `<html><body><div class="slst">
<li class="pbw"><h3><a href="forum.php?mod=viewthread&amp;tid=101">凡人修仙传 有链接</a></h3></li>
<li class="pbw"><h3><a href="forum.php?mod=viewthread&amp;tid=102">凡人修仙传 无链接</a></h3></li>
</div></body></html>`)
		case r.URL.Query().Get("tid") == "101":
			io.WriteString(w, `<html><body><td class="t_f">凡人修仙传 链接: https://pan.baidu.com/s/xyz123</td></body></html>`)
		case strings.HasPrefix(r.URL.Path, "/search.php"):
			http.Redirect(w, r, "/search.php?mod=forum&searchid=9002&kw=x", http.StatusFound)
		default:
			io.WriteString(w, `<html><body><td class="t_f">没有链接</td></body></html>`)
		}
	}))
	defer server.Close()

	results, err := newTestPlugin(server.URL).searchImpl(nil, "凡人修仙传", nil)
	if err != nil {
		t.Fatalf("部分失败不应导致整体报错: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望保留 1 条成功结果，实际 %d 条", len(results))
	}
}
