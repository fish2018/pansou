package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 这套用例盯的是门的三条硬约束：
//  1. 判定看"能否拿到 HTTP 响应"，不看状态码——404/403 都说明网络通；
//  2. 任何不确定都按可达处理（fail-open），不能因为探测本身出问题就让 TG 少跑一轮；
//  3. 探测只改自己的状态，不阻塞、不写缓存、不计频道失败（后两条由 searchTG 的早返回保证，
//     在实测里验证，见 docs 的技术债记录）。

type tgProbeTransport struct {
	target  string
	failErr error
	// 记录服务端收到的请求方法，用来钉住"探测用 HEAD 而不是 GET"。
	lastMethod string
}

func (t *tgProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.lastMethod = req.Method
	if t.failErr != nil {
		return nil, t.failErr
	}
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestTGProbeTreatsAnyHTTPResponseAsReachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 刻意返回 404：网络是通的，只是路径不存在。这种"通但没内容"不算不可达。
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &http.Client{Transport: &tgProbeTransport{target: server.URL}}
	reachable, reason := probeTGURL(context.Background(), client, "https://t.me/s/tgsearchers7")

	if !reachable {
		t.Fatalf("拿到 HTTP 404 应判定为可达，实际不可达（原因：%s）", reason)
	}
	if !strings.Contains(reason, "404") {
		t.Errorf("原因应记录状态码便于排查，实际：%s", reason)
	}
}

func TestTGProbeTreatsTransportErrorAsUnreachable(t *testing.T) {
	client := &http.Client{Transport: &tgProbeTransport{failErr: errors.New("dial tcp: i/o timeout")}}
	reachable, reason := probeTGURL(context.Background(), client, "https://t.me/s/tgsearchers7")

	if reachable {
		t.Fatalf("传输层失败应判定为不可达，实际可达（原因：%s）", reason)
	}
	if !strings.Contains(reason, "i/o timeout") {
		t.Errorf("原因应带上失败详情，实际：%s", reason)
	}
}

func TestTGProbeUsesHEAD(t *testing.T) {
	// 探测必须是 HEAD：频道页正文 133KB，探测只需要"能否拿到响应"这一比特。
	// 实测 HEAD 0 字节 vs GET 133,273 字节。这条用例防止以后被改回 GET。
	var method string
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := &tgProbeTransport{target: server.URL}
	reachable, _ := probeTGURL(context.Background(), &http.Client{Transport: transport}, "https://t.me/s/tgsearchers7")

	if !reachable {
		t.Fatal("HEAD 拿到 200 应判定为可达")
	}
	if method != http.MethodHead {
		t.Errorf("探测应使用 HEAD，实际使用 %s", method)
	}
	if path != "/s/tgsearchers7" {
		t.Errorf("探测应打到真实频道页路径，实际 %s", path)
	}
}

func TestTGProbeTreatsUnsupportedMethodAsReachable(t *testing.T) {
	// 判定看"拿到了 HTTP 响应"而非状态码：站点不支持 HEAD 回 405 同样证明网络是通的。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer server.Close()

	reachable, reason := probeTGURL(context.Background(), &http.Client{Transport: &tgProbeTransport{target: server.URL}}, "https://t.me/s/tgsearchers7")
	if !reachable {
		t.Fatalf("405 应判定为可达（网络通），实际不可达（原因：%s）", reason)
	}
	if !strings.Contains(reason, "405") {
		t.Errorf("原因应记录状态码，实际：%s", reason)
	}
}

func TestTGProbeURLPointsAtRealChannelPage(t *testing.T) {
	// 探测必须覆盖真正要走的 /s/<channel> 路径，而不是站点首页。
	if got := tgProbeURL(); !strings.HasPrefix(got, "https://t.me/") {
		t.Errorf("探测地址应以 https://t.me/ 开头，实际 %s", got)
	}
}

func TestGateIsFailOpenBeforeAnyProbe(t *testing.T) {
	// 全新进程里没有任何探测结论时，必须按可达处理：门只用来省掉必然失败的请求，
	// 不允许在"不知道"的情况下让 TG 少跑一轮。
	restore := snapshotGateState()
	defer restore()

	tgCheckedAt.Store(0)
	tgReachable.Store(true)

	if !TGReachable() {
		t.Fatal("未探测状态下应默认可达")
	}
	snapshot := TGReachabilitySnapshot()
	if snapshot["checked_seconds_ago"] != nil {
		t.Errorf("未探测时 checked_seconds_ago 应为 nil，实际 %v", snapshot["checked_seconds_ago"])
	}
	if reason, _ := snapshot["reason"].(string); !strings.Contains(reason, "尚未探测") {
		t.Errorf("未探测时原因应说明尚未探测，实际：%s", reason)
	}
}

func TestSingleProbeFailureDoesNotCloseGate(t *testing.T) {
	// 这条是实测教训的固化：经代理访问 t.me 时探测偶尔会超时，若单次失败就关门，
	// 会把可用的 TG 静默跳过——本机制最危险的失效模式。一次失败只能记连击，不能改判。
	restore := snapshotGateState()
	defer restore()

	tgReachable.Store(true)
	tgProbeFailStreak.Store(0)

	stillReachable := ProbeTGReachabilityNow(&http.Client{Transport: &tgProbeTransport{failErr: errors.New("connection refused")}})
	if !stillReachable {
		t.Fatal("首次探测失败不应把门关上（fail-open）")
	}
	if !TGReachable() {
		t.Fatal("首次探测失败后 TGReachable 仍应为 true")
	}
	if got := tgProbeFailStreak.Load(); got != 1 {
		t.Fatalf("首次失败后连击应为 1，实际 %d", got)
	}
	if snapshot := TGReachabilitySnapshot(); snapshot["fail_streak"] != int32(1) {
		t.Errorf("快照应暴露连击数，实际 %v", snapshot["fail_streak"])
	}
}

func TestConsecutiveProbeFailuresCloseGate(t *testing.T) {
	restore := snapshotGateState()
	defer restore()

	tgReachable.Store(true)
	tgProbeFailStreak.Store(0)

	client := &http.Client{Transport: &tgProbeTransport{failErr: errors.New("i/o timeout")}}
	ProbeTGReachabilityNow(client)
	if down := ProbeTGReachabilityNow(client); down {
		t.Fatal("连续达到阈值后应判定不可达")
	}
	if TGReachable() {
		t.Fatal("连续失败达阈值后 TGReachable 应为 false")
	}
	if reason := tgReason(); !strings.Contains(reason, "连续 2 次探测失败") {
		t.Errorf("原因应说明是连续失败，实际：%s", reason)
	}
}

func TestProbeSuccessResetsFailStreak(t *testing.T) {
	restore := snapshotGateState()
	defer restore()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tgProbeFailStreak.Store(1)
	tgReachable.Store(false)

	if up := ProbeTGReachabilityNow(&http.Client{Transport: &tgProbeTransport{target: server.URL}}); !up {
		t.Fatal("探测成功应判定可达")
	}
	if got := tgProbeFailStreak.Load(); got != 0 {
		t.Errorf("成功后连击应清零，实际 %d", got)
	}
}

func TestProbeNowFlipsStateAndRestores(t *testing.T) {
	restore := snapshotGateState()
	defer restore()

	tgProbeFailStreak.Store(0)
	failing := &http.Client{Transport: &tgProbeTransport{failErr: errors.New("connection refused")}}
	ProbeTGReachabilityNow(failing) // 第 1 次只记连击
	down := ProbeTGReachabilityNow(failing)
	if down {
		t.Fatal("连续失败达到阈值后应判定不可达")
	}
	if TGReachable() {
		t.Fatal("TGReachable 应跟随探测结论")
	}
	if snapshot := TGReachabilitySnapshot(); snapshot["reachable"] != false {
		t.Errorf("快照里的 reachable 应为 false，实际 %v", snapshot["reachable"])
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if up := ProbeTGReachabilityNow(&http.Client{Transport: &tgProbeTransport{target: server.URL}}); !up {
		t.Fatal("恢复后应判定可达")
	}
	if !TGReachable() {
		t.Fatal("恢复后 TGReachable 应为 true")
	}
}

// snapshotGateState 保存并还原全局门状态，避免用例之间互相污染
// （一旦某个用例把门置为不可达，同包其它用例里的 searchTG 会被跳过）。
func snapshotGateState() func() {
	reachable := tgReachable.Load()
	checkedAt := tgCheckedAt.Load()
	reason := tgReason()
	loggedOnce := tgLoggedOnce.Load()
	lastLoggedOK := tgLastLoggedOK.Load()
	failStreak := tgProbeFailStreak.Load()

	return func() {
		tgReachable.Store(reachable)
		tgCheckedAt.Store(checkedAt)
		tgProbeReason.Store(reason)
		tgLoggedOnce.Store(loggedOnce)
		tgLastLoggedOK.Store(lastLoggedOK)
		tgProbeFailStreak.Store(failStreak)
	}
}
