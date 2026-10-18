package pool

import (
	"errors"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

func mkAccount(id string, credit float64) *types.Account {
	return &types.Account{ID: id, UID: "u-" + id, Weight: 1, CreditTotal: credit}
}

func testCfg() PoolConfig {
	c := DefaultPoolConfig()
	c.RateLimitBase = time.Second
	c.RateLimitMax = 10 * time.Second
	c.CreditExhaustedCooldown = 5 * time.Second
	c.DegradeThreshold = 3
	c.DegradeDuration = 5 * time.Second
	c.SessionTTL = time.Minute
	return c
}

// 成本分层：有额度的账号优先被选中。
func TestPickPrefersCreditTier(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("empty", 0), mkAccount("rich", 100)}, testCfg())
	for i := 0; i < 20; i++ {
		got, err := p.Pick(types.PickRequest{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Account.ID != "rich" {
			t.Fatalf("期望选中 rich，实际 %s", got.Account.ID)
		}
	}
}

// 会话粘性：同一 SessionKey 两次 Pick 命中同一账号。
func TestPickSessionSticky(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10), mkAccount("b", 10)}, testCfg())
	r1, _ := p.Pick(types.PickRequest{Model: "m", SessionKey: "s1"})
	r2, _ := p.Pick(types.PickRequest{Model: "m", SessionKey: "s1"})
	if r1.Account.ID != r2.Account.ID {
		t.Fatalf("会话粘性失效：%s vs %s", r1.Account.ID, r2.Account.ID)
	}
	if r2.Reason != "session-sticky" {
		t.Fatalf("第二次应命中粘性，reason=%s", r2.Reason)
	}
}

// DisabledModels：黑名单命中的模型跳过该账号。
func TestPickSkipsDisabledModel(t *testing.T) {
	a := mkAccount("a", 10)
	a.DisabledModels = []string{"m1"}
	p := NewPool([]*types.Account{a, mkAccount("b", 10)}, testCfg())
	got, err := p.Pick(types.PickRequest{Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.ID != "b" {
		t.Fatalf("应跳过黑名单账号，实际选中 %s", got.Account.ID)
	}
}

// 无可用账号：全部 Disabled 时返回明确错误。
func TestPickNoAccount(t *testing.T) {
	a := mkAccount("a", 10)
	a.Disabled = true
	p := NewPool([]*types.Account{a}, testCfg())
	_, err := p.Pick(types.PickRequest{Model: "m"})
	if !errors.Is(err, ErrNoAccountAvailable) {
		t.Fatalf("期望 ErrNoAccountAvailable，实际 %v", err)
	}
}

// ReportRateLimited：指数退避冷却，且 Pick 会跳过。
func TestReportRateLimitedBackoff(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10), mkAccount("b", 10)}, testCfg())
	p.Report(types.CallReport{AccountID: "a", Model: "m", Status: types.ReportRateLimited})
	snap := p.Snapshot()
	if snap[0].CooldownUntil.IsZero() || snap[0].SoftStreak != 1 {
		t.Fatalf("限流后应进入冷却，streak=%d", snap[0].SoftStreak)
	}
	got, err := p.Pick(types.PickRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Account.ID != "b" {
		t.Fatalf("冷却中的账号应被跳过，实际选中 %s", got.Account.ID)
	}
}

// ReportCreditExhausted：分钟级软冷却（不硬冷到次日）。
func TestReportCreditExhaustedSoft(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10)}, testCfg())
	before := time.Now()
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportCreditExhausted})
	until := p.Snapshot()[0].CooldownUntil
	if d := until.Sub(before); d < 4*time.Second || d > 6*time.Second {
		t.Fatalf("软冷却应在 ~5s 左右，实际 %v", d)
	}
}

// ReportUpstreamError：达阈值后降级。
func TestReportUpstreamDegrade(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10)}, testCfg())
	for i := 0; i < 3; i++ {
		p.Report(types.CallReport{AccountID: "a", Status: types.ReportUpstreamError})
	}
	if p.Snapshot()[0].DegradeUntil.IsZero() {
		t.Fatal("连续失败达阈值后应降级")
	}
}

// ReportAuthFailed：标记 Disabled。
func TestReportAuthFailedDisables(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10)}, testCfg())
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportAuthFailed})
	if !p.Snapshot()[0].Disabled {
		t.Fatal("401 后账号应被标记 Disabled")
	}
}

// ReportOK：衰减失败计数 + 更新 EMA。
func TestReportOKDecaysAndEMA(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10)}, testCfg())
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportUpstreamError})
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportUpstreamError})
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportOK, CreditCost: 100})
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportOK, CreditCost: 200})
	snap := p.Snapshot()[0]
	if snap.Fails != 0 { // 2 -> 1 -> 0（减半衰减）
		t.Fatalf("成功后失败计数应衰减，实际 %d", snap.Fails)
	}
	if snap.EMACost != 130 { // 0.3*200 + 0.7*100
		t.Fatalf("EMA 期望 130，实际 %v", snap.EMACost)
	}
}

// ReportBadRequest：不惩罚账号。
func TestReportBadRequestNoPenalty(t *testing.T) {
	p := NewPool([]*types.Account{mkAccount("a", 10)}, testCfg())
	p.Report(types.CallReport{AccountID: "a", Status: types.ReportBadRequest})
	snap := p.Snapshot()[0]
	if snap.Fails != 0 || !snap.CooldownUntil.IsZero() || !snap.DegradeUntil.IsZero() {
		t.Fatal("400 不应惩罚账号")
	}
}

func TestPoolAccountOps(t *testing.T) {
	p := NewPool([]*types.Account{
		{ID: "a1", UID: "a1", Region: "cn"},
		{ID: "a2", UID: "a2", Region: "cn"},
	}, DefaultPoolConfig())

	if got := p.Get("a1"); got == nil || got.ID != "a1" {
		t.Fatal("Get(a1) 失败")
	}
	if p.Get("nope") != nil {
		t.Fatal("Get 不存在的应返回 nil")
	}
	if len(p.All()) != 2 {
		t.Fatal("All 应返回 2 个")
	}
	// 停用后不再参与选号
	if !p.SetDisabled("a1", true) {
		t.Fatal("SetDisabled 失败")
	}
	if !p.Get("a1").Disabled {
		t.Fatal("Disabled 未生效")
	}
	if p.SetDisabled("nope", true) {
		t.Fatal("不存在的账号 SetDisabled 应返回 false")
	}
	// 启用恢复
	if !p.SetDisabled("a1", false) || p.Get("a1").Disabled {
		t.Fatal("启用失败")
	}
	// 删除
	removed := p.Remove("a2")
	if removed == nil || removed.ID != "a2" {
		t.Fatal("Remove 失败")
	}
	if p.Get("a2") != nil || p.Len() != 1 {
		t.Fatal("Remove 后仍能查到")
	}
	if p.Remove("a2") != nil {
		t.Fatal("重复 Remove 应返回 nil")
	}
}
