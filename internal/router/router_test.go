package router

import (
	"testing"
	"time"
)

// 2026-01-01 10:00 UTC == 18:00 CST（白天）；15:00 UTC == 23:00 CST（夜间）。
func TestIsDay(t *testing.T) {
	r := NewRouter(Config{Enabled: true, DayStart: 8, DayEnd: 23})

	cases := []struct {
		utc  time.Time
		want bool
	}{
		{time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), true},   // 18:00 CST 白天
		{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), true},    // 08:00 CST 边界（含）
		{time.Date(2026, 1, 1, 14, 59, 0, 0, time.UTC), true},  // 22:59 CST 白天
		{time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC), false},  // 23:00 CST 夜间
		{time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC), false},  // 07:00 CST 夜间
	}
	for _, c := range cases {
		if got := r.isDay(hourCST(c.utc)); got != c.want {
			t.Errorf("isDay(%v)=%v, want %v", c.utc, got, c.want)
		}
	}
}

func TestIsDayCrossMidnight(t *testing.T) {
	r := NewRouter(Config{Enabled: true, DayStart: 22, DayEnd: 6}) // 跨零点
	cases := []struct {
		utc  time.Time
		want bool
	}{
		{time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC), true},  // 23:00 CST 白天
		{time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC), false},  // 12:00 CST 夜间
		{time.Date(2026, 1, 1, 21, 0, 0, 0, time.UTC), true},   // 05:00 CST 白天（[22:00,06:00) 内）
		{time.Date(2026, 1, 1, 22, 0, 0, 0, time.UTC), false},  // 06:00 CST 夜间（end 不含）
		{time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC), true},  // 22:00 CST 白天（含 start）
	}
	for _, c := range cases {
		if got := r.isDay(hourCST(c.utc)); got != c.want {
			t.Errorf("cross-midnight isDay(%v)=%v, want %v", c.utc, got, c.want)
		}
	}
}

func TestIsDayAllDay(t *testing.T) {
	r := NewRouter(Config{Enabled: true, DayStart: 0, DayEnd: 0}) // 起止相等=全天白天
	if !r.isDay(3) || !r.isDay(23) {
		t.Error("start==end 应全天为白天")
	}
}

func TestChainVirtualDayNight(t *testing.T) {
	r := NewRouter(Config{
		Enabled:      true,
		DayPrimary:   "cn:hy3",
		NightPrimary: "cn:hy4",
		DayStart:     8,
		DayEnd:       23,
		Fallback:     []string{"cn:hy2"},
	})
	day := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)   // 18:00 CST
	night := time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC) // 23:00 CST

	if got := r.Chain("auto", day); !equal(got, []string{"cn:hy3", "cn:hy2"}) {
		t.Errorf("day chain=%v", got)
	}
	if got := r.Chain("auto", night); !equal(got, []string{"cn:hy4", "cn:hy2"}) {
		t.Errorf("night chain=%v", got)
	}
}

func TestChainRealmFilter(t *testing.T) {
	r := NewRouter(Config{
		Enabled:      true,
		DayPrimary:   "cn:hy3",
		NightPrimary: "cn:hy4",
		DayStart:     8,
		DayEnd:       23,
		Fallback:     []string{"cn:hy2", "global:g1"},
	})
	day := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	// cn:auto：global 候选被过滤
	if got := r.Chain("cn:auto", day); !equal(got, []string{"cn:hy3", "cn:hy2"}) {
		t.Errorf("cn:auto chain=%v", got)
	}
}

func TestChainGlobalAutoNoCandidate(t *testing.T) {
	r := NewRouter(Config{
		Enabled:      true,
		DayPrimary:   "cn:hy3",
		NightPrimary: "cn:hy4",
		DayStart:     8,
		DayEnd:       23,
		Fallback:     []string{"cn:hy2"}, // 无国际版候选
	})
	day := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	// 空链→单元素原样
	if got := r.Chain("global:auto", day); !equal(got, []string{"global:auto"}) {
		t.Errorf("global:auto chain=%v", got)
	}
}

func TestChainDisabled(t *testing.T) {
	r := NewRouter(Config{Enabled: false})
	now := time.Now()
	if got := r.Chain("auto", now); !equal(got, []string{"auto"}) {
		t.Errorf("disabled chain=%v", got)
	}
}

func TestChainRealModelYield(t *testing.T) {
	day := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC) // 18:00 CST 白天
	r := NewRouter(Config{Enabled: true, DayPrimary: "cn:hy3", DayStart: 8, DayEnd: 23})
	r.SetRealModels(map[string]bool{"auto": true})
	// 上游有同名真实模型且未 override：让位不劫持
	if got := r.Chain("auto", day); !equal(got, []string{"auto"}) {
		t.Errorf("yield chain=%v", got)
	}
	r2 := NewRouter(Config{Enabled: true, DayPrimary: "cn:hy3", DayStart: 8, DayEnd: 23, Override: true})
	r2.SetRealModels(map[string]bool{"auto": true})
	if got := r2.Chain("auto", day); len(got) == 0 || got[0] != "cn:hy3" {
		t.Errorf("override chain=%v", got)
	}
}

func TestChainNamedModelFallback(t *testing.T) {
	r := NewRouter(Config{
		Enabled: false, // 未启用编排时具名 fallback 仍生效
		ModelFallback: map[string][]string{
			"gpt-x": {"gpt-y", "gpt-z"},
		},
	})
	now := time.Now()
	if got := r.Chain("gpt-x", now); !equal(got, []string{"gpt-x", "gpt-y", "gpt-z"}) {
		t.Errorf("named chain=%v", got)
	}
	// 键须逐字匹配
	if got := r.Chain("gpt-x2", now); !equal(got, []string{"gpt-x2"}) {
		t.Errorf("non-match chain=%v", got)
	}
}

func TestChainCycleAndDedup(t *testing.T) {
	r := NewRouter(Config{
		ModelFallback: map[string][]string{
			"a": {"b"},
			"b": {"a", "b"}, // 环 + 重复
		},
	})
	if got := r.Chain("a", time.Now()); !equal(got, []string{"a", "b"}) {
		t.Errorf("cycle chain=%v", got)
	}
}

func TestChainMaxLen(t *testing.T) {
	fb := make([]string, 12)
	for i := range fb {
		fb[i] = string(rune('b' + i))
	}
	r := NewRouter(Config{ModelFallback: map[string][]string{"a": fb}})
	got := r.Chain("a", time.Now())
	if len(got) != maxChainLen {
		t.Errorf("chain len=%d, want %d", len(got), maxChainLen)
	}
}

func TestChainMaxDepth(t *testing.T) {
	r := NewRouter(Config{ModelFallback: map[string][]string{
		"a": {"b"}, "b": {"c"}, "c": {"d"}, "d": {"e"}, "e": {"f"},
	}})
	// 初始 + 3 层递归 = a,b,c,d
	if got := r.Chain("a", time.Now()); !equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("depth chain=%v", got)
	}
}

func TestShouldFallback(t *testing.T) {
	r := NewRouter(Config{})
	for _, k := range []string{"soft_rate", "hard_credit", "model_blocked", "server", "account_fault", "not_found", "no_healthy_account"} {
		if !r.ShouldFallback(k) {
			t.Errorf("default should fallback on %s", k)
		}
	}
	// 请求自身问题 fail-fast
	for _, k := range []string{"content_filter", "bad_request", "context_too_long"} {
		if r.ShouldFallback(k) {
			t.Errorf("should NOT fallback on %s", k)
		}
	}
	// 自定义集合覆盖默认
	r2 := NewRouter(Config{FallbackOn: []string{"custom_kind"}})
	if !r2.ShouldFallback("custom_kind") || r2.ShouldFallback("soft_rate") {
		t.Error("custom FallbackOn 未生效")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
