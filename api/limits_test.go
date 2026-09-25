package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	utiljson "pansou/util/json"
)

// api 层原先没有测试。这三个上限是"匿名客户端可直接触发资源放大"的入口，
// 且认证默认关闭（AUTH_ENABLED 未设置即 false），所以必须有用例锁住。
func newLimitsTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/search", SearchHandler)
	r.POST("/api/check/links", CheckHandler)
	return r
}

func TestSearchHandlerRejectsOversizedBody(t *testing.T) {
	r := newLimitsTestRouter()

	// 略超上限的请求体
	big := bytes.Repeat([]byte("a"), int(maxSearchRequestBodyBytes)+1024)
	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge && w.Code != http.StatusBadRequest {
		t.Errorf("超大请求体应被拒绝，实际状态码 %d，响应 %s", w.Code, w.Body.String())
	}
}

// 边界：恰好等于上限必须放行（判断是 > 而不是 >=）。
// 这里直接测判断函数：走完整 handler 会进到全局 searchService 与真实上游调用，
// 不适合放进单测，而边界语义恰恰是这个判断函数唯一的风险点。
func TestLimitBoundariesAreInclusive(t *testing.T) {
	if channelsOverLimit(maxRequestChannels) {
		t.Errorf("恰好 %d 个频道不应算超限", maxRequestChannels)
	}
	if !channelsOverLimit(maxRequestChannels + 1) {
		t.Errorf("%d 个频道应算超限", maxRequestChannels+1)
	}
	if itemsOverLimit(maxCheckItems) {
		t.Errorf("恰好 %d 个 item 不应算超限", maxCheckItems)
	}
	if !itemsOverLimit(maxCheckItems + 1) {
		t.Errorf("%d 个 item 应算超限", maxCheckItems+1)
	}
	// 0 与负数一律放行，交给各自的"不能为空"逻辑处理
	if channelsOverLimit(0) || channelsOverLimit(-1) || itemsOverLimit(0) || itemsOverLimit(-1) {
		t.Error("0 与负数不应被上限拦截")
	}
}

func TestSearchHandlerRejectsTooManyChannels(t *testing.T) {
	r := newLimitsTestRouter()

	channels := make([]string, maxRequestChannels+1)
	for i := range channels {
		channels[i] = fmt.Sprintf("ch%d", i)
	}
	body, _ := utiljson.Marshal(map[string]interface{}{"kw": "仙逆", "channels": channels})

	req := httptest.NewRequest(http.MethodPost, "/api/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("频道数超限应返回 400，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "超过上限") {
		t.Errorf("应给出超限提示而不是静默截断: %s", w.Body.String())
	}
}

func TestCheckHandlerRejectsTooManyItems(t *testing.T) {
	r := newLimitsTestRouter()

	items := make([]map[string]string, maxCheckItems+1)
	for i := range items {
		items[i] = map[string]string{"url": fmt.Sprintf("https://pan.quark.cn/s/%d", i)}
	}
	body, _ := utiljson.Marshal(map[string]interface{}{"items": items})

	req := httptest.NewRequest(http.MethodPost, "/api/check/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("items 超限应返回 400，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "超过上限") {
		t.Errorf("应给出超限提示: %s", w.Body.String())
	}
}

// 空 items 的错误必须先于数量上限触发，保持既有语义。
func TestCheckHandlerStillRejectsEmptyItems(t *testing.T) {
	r := newLimitsTestRouter()
	body, _ := utiljson.Marshal(map[string]interface{}{"items": []interface{}{}})
	req := httptest.NewRequest(http.MethodPost, "/api/check/links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不能为空") {
		t.Errorf("空 items 应报\"不能为空\"，实际 %d %s", w.Code, w.Body.String())
	}
}
