package service

import (
	"errors"
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
				o.observe(id, err)
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
				o.observe(id, err)
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
	o.observe("c3", nil)
	o.observe("c1", nil)
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
	o.observe("a", nil)
	o.observe("b", errors.New("failed"))
	o.finalize(submitted)

	// 失败是常态（站点改版、单站限流），不应被当成"这次批量搜索没跑完"
	if o.complete() {
		t.Error("存在失败项时 complete() 应为 false")
	}
	if o.timedOut() != 0 {
		t.Errorf("timedOut() = %d, 期望 0", o.timedOut())
	}
}
