package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBatchSearchOutcomeCacheTTL(t *testing.T) {
	full := 60 * time.Minute
	partial := 3 * time.Minute
	errBoom := errors.New("boom")

	cases := []struct {
		name        string
		submitted   []string
		observed    map[string]error
		wantTTL     time.Duration
		wantWrite   bool
		wantMissing int
	}{
		{
			name:      "全部成功写完整TTL",
			submitted: []string{"a", "b"},
			observed:  map[string]error{"a": nil, "b": nil},
			wantTTL:   full,
			wantWrite: true,
		},
		{
			name:        "有任务超时未返回则写短TTL",
			submitted:   []string{"a", "b", "c"},
			observed:    map[string]error{"a": nil, "b": nil},
			wantTTL:     partial,
			wantWrite:   true,
			wantMissing: 1,
		},
		{
			name:      "部分失败但无超时仍写完整TTL",
			submitted: []string{"a", "b"},
			observed:  map[string]error{"a": nil, "b": errBoom},
			wantTTL:   full,
			wantWrite: true,
		},
		{
			name:      "全部失败不写缓存",
			submitted: []string{"a", "b"},
			observed:  map[string]error{"a": errBoom, "b": errBoom},
			wantTTL:   0,
			wantWrite: false,
		},
		{
			name:        "全部超时未返回不写缓存",
			submitted:   []string{"a", "b"},
			observed:    map[string]error{},
			wantTTL:     0,
			wantWrite:   false,
			wantMissing: 2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newBatchSearchOutcome(len(c.submitted))
			for id, err := range c.observed {
				o.observe(id, err, 10*time.Millisecond)
			}
			o.finalize(c.submitted)

			if got := o.timedOut(); got != c.wantMissing {
				t.Errorf("timedOut() = %d, 期望 %d", got, c.wantMissing)
			}
			ttl, write := o.cacheTTL(full, partial)
			if write != c.wantWrite {
				t.Errorf("cacheTTL 写缓存 = %v, 期望 %v", write, c.wantWrite)
			}
			if write && ttl != c.wantTTL {
				t.Errorf("cacheTTL TTL = %v, 期望 %v", ttl, c.wantTTL)
			}
		})
	}
}

func TestBatchSearchOutcomeShouldBackfill(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		submitted []string
		observed  map[string]error
		want      bool
	}{
		{
			name:      "少量超时值得补齐",
			enabled:   true,
			submitted: []string{"a", "b", "c", "d", "e", "f"},
			observed:  map[string]error{"a": nil, "b": nil, "c": nil, "d": nil, "e": nil},
			want:      true,
		},
		{
			name:      "超时占比超过三分之一则放弃",
			enabled:   true,
			submitted: []string{"a", "b", "c"},
			observed:  map[string]error{"a": nil},
			want:      false,
		},
		{
			name:      "开关关闭时不补齐",
			enabled:   false,
			submitted: []string{"a", "b", "c", "d"},
			observed:  map[string]error{"a": nil, "b": nil, "c": nil},
			want:      false,
		},
		{
			name:      "没有成功项时不补齐",
			enabled:   true,
			submitted: []string{"a", "b"},
			observed:  map[string]error{},
			want:      false,
		},
		{
			name:      "全部成功时不需要补齐",
			enabled:   true,
			submitted: []string{"a", "b"},
			observed:  map[string]error{"a": nil, "b": nil},
			want:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newBatchSearchOutcome(len(c.submitted))
			for id, err := range c.observed {
				o.observe(id, err, 10*time.Millisecond)
			}
			o.finalize(c.submitted)

			if got := o.shouldBackfill(c.enabled); got != c.want {
				t.Errorf("shouldBackfill() = %v, 期望 %v", got, c.want)
			}
		})
	}
}

func TestBatchSearchOutcomeFinalizeKeepsSubmittedOrder(t *testing.T) {
	submitted := []string{"c1", "c2", "c3", "c4"}
	o := newBatchSearchOutcome(len(submitted))
	o.observe("c3", nil, 10*time.Millisecond)
	o.observe("c1", nil, 10*time.Millisecond)
	o.finalize(submitted)

	missing := o.missingIDs()
	if len(missing) != 2 || missing[0] != "c2" || missing[1] != "c4" {
		t.Errorf("missingIDs() = %v, 期望 [c2 c4]", missing)
	}
	if o.complete() {
		t.Error("有超时未完成时 complete() 应为 false")
	}
}

func TestBatchSearchOutcomeComplete(t *testing.T) {
	submitted := []string{"a", "b"}
	o := newBatchSearchOutcome(len(submitted))
	o.observe("a", nil, 10*time.Millisecond)
	o.observe("b", errors.New("failed"), 10*time.Millisecond)
	o.finalize(submitted)

	// 失败是常态（站点改版、单站限流），不应被当成"这次批量搜索没跑完"
	if o.complete() {
		t.Error("存在失败项时 complete() 应为 false")
	}
	if o.timedOut() != 0 {
		t.Errorf("timedOut() = %d, 期望 0", o.timedOut())
	}
}

func TestFailureClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"限流", &httpStatusError{channel: "c", code: 429}, "限流429"},
		{"禁止访问", &httpStatusError{channel: "c", code: 403}, "禁止403"},
		{"频道不存在", &httpStatusError{channel: "c", code: 404}, "不存在404"},
		{"服务端错误", &httpStatusError{channel: "c", code: 503}, "服务端5xx"},
		{"其它状态码", &httpStatusError{channel: "c", code: 302}, "状态码302"},
		{"包装后的超时仍可识别", fmt.Errorf("请求失败: %w", context.DeadlineExceeded), "超时"},
		{"普通错误", errors.New("boom"), "其它错误"},
		{"无错误", nil, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureClass(c.err); got != c.want {
				t.Errorf("failureClass() = %q, 期望 %q", got, c.want)
			}
		})
	}
}

func TestBatchSearchOutcomeSlowestAndFailureSummary(t *testing.T) {
	o := newBatchSearchOutcome(4)
	o.observe("fast", nil, 5*time.Millisecond)
	o.observe("slow", nil, 900*time.Millisecond)
	o.observe("rate-limited", &httpStatusError{channel: "rate-limited", code: 429}, 20*time.Millisecond)
	o.observe("also-limited", &httpStatusError{channel: "also-limited", code: 429}, 20*time.Millisecond)
	o.finalize([]string{"fast", "slow", "rate-limited", "also-limited"})

	slow := o.slowestTasks(2)
	if len(slow) != 2 || slow[0].id != "slow" {
		t.Fatalf("slowestTasks() = %+v, 期望首位是 slow", slow)
	}
	if slow[1].id != "rate-limited" && slow[1].id != "also-limited" {
		t.Errorf("slowestTasks() 次位 = %s, 期望是两个限流项之一", slow[1].id)
	}

	summary := o.failureSummary(5)
	if !strings.HasPrefix(summary, "限流429×2") {
		t.Errorf("failureSummary() = %q, 期望以 \"限流429×2\" 开头", summary)
	}
	// 摘要必须带一条示例报错，否则"其它错误×N"这类归类无从定位
	if !strings.Contains(summary, "429") || !strings.Contains(summary, "示例") {
		t.Errorf("failureSummary() = %q, 期望包含示例错误", summary)
	}
	// 全部成功时不应产生失败摘要
	clean := newBatchSearchOutcome(1)
	clean.observe("ok", nil, time.Millisecond)
	clean.finalize([]string{"ok"})
	if got := clean.failureSummary(5); got != "" {
		t.Errorf("无失败时 failureSummary() = %q, 期望空串", got)
	}
}
