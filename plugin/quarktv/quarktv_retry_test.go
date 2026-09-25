package quarktv

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 重试收敛到 util.DoWithRetry 后必须保持原语义：失败重试、成功返回响应体、
// 每次尝试各自超时。
func TestFetchBodyRetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	body, err := fetchBody(srv.Client(), srv.URL, 5*time.Second, "https://example.com")
	if err != nil {
		t.Fatalf("第三次应当成功: %v", err)
	}
	if string(body) != "<html>ok</html>" {
		t.Errorf("响应体不对: %q", string(body))
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("应请求 3 次，实际 %d 次", got)
	}
}

// 原先这里用手写的 ioReadAll 读体，**没有任何上限**（上游给多大就吃多大）。
// 改用 util.ReadAllLimited 后必须能拒绝超大响应，而不是把它整个读进内存。
func TestFetchBodyRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 不真的写 16MiB，声明超大 Content-Length 后持续写小块即可让封顶读提前失败
		chunk := make([]byte, 1<<20)
		for i := 0; i < 40; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, err := fetchBody(srv.Client(), srv.URL, 10*time.Second, "https://example.com")
	if err == nil {
		t.Fatal("超过 16MiB 的响应必须报错，而不是无上限地读进内存")
	}
}
