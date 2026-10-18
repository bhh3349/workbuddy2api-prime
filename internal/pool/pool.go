// Package pool 实现 workbuddy2api-prime 的账号池调度模块。
//
// 融合两家优点：
//   - HanawaBanana：三层选号（会话粘性→成本分层→加权随机）+ EMA 成本账本，
//     选号过程零新增上游探测请求（WAF 友好）；积分耗尽走分钟级软冷却
//     （cooldown.credit_exhausted），不硬冷却到次日。
//   - linguo：积分"最早到期优先"温和权重（preferExpiring 思路，权重加成而非硬排序）、
//     429 指数退避软冷却、模型级独立冷却、连续失败降级。
//
// 并发安全：全部运行态由单一 sync.Mutex 保护；只用标准库。
package pool

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
)

// ErrNoAccountAvailable 在没有任何账号可供选择时由 Pick 返回。
// 可用 errors.Is 判定；错误文本内附带各过滤原因的计数，便于排查。
var ErrNoAccountAvailable = errors.New("pool: 没有可用账号")

// PoolConfig 是账号池的可调参数。零值字段会被 withDefaults 填上默认值。
type PoolConfig struct {
	RateLimitBase           time.Duration // 429 指数退避基数，默认 10m
	RateLimitMax            time.Duration // 指数退避封顶，默认 2h
	CreditExhaustedCooldown time.Duration // 积分耗尽分钟级软冷却，默认 10m（不指数堆加）
	DegradeThreshold        int           // 连续上游失败降级阈值，默认 5
	DegradeDuration         time.Duration // 降级时长（固定，不指数升级），默认 10m
	ExpirySoonThreshold     time.Duration // "额度快到期"判定窗口，默认 120h（5 天）
	ExpiryFactor            float64       // 快到期账号权重加成倍数，默认 3
	CostEMAAlpha            float64       // EMA 成本账本平滑系数，默认 0.3
	SessionTTL              time.Duration // 会话粘性 TTL（滚动续期），默认 30m
}

// DefaultPoolConfig 返回一套开箱即用的默认参数。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		RateLimitBase:           10 * time.Minute,
		RateLimitMax:            2 * time.Hour,
		CreditExhaustedCooldown: 10 * time.Minute,
		DegradeThreshold:        5,
		DegradeDuration:         10 * time.Minute,
		ExpirySoonThreshold:     120 * time.Hour,
		ExpiryFactor:            3,
		CostEMAAlpha:            0.3,
		SessionTTL:              30 * time.Minute,
	}
}

// withDefaults 把零值/非法字段填上默认值，返回可用配置。
func (c PoolConfig) withDefaults() PoolConfig {
	d := DefaultPoolConfig()
	if c.RateLimitBase > 0 {
		d.RateLimitBase = c.RateLimitBase
	}
	if c.RateLimitMax > 0 {
		d.RateLimitMax = c.RateLimitMax
	}
	if c.CreditExhaustedCooldown > 0 {
		d.CreditExhaustedCooldown = c.CreditExhaustedCooldown
	}
	if c.DegradeThreshold > 0 {
		d.DegradeThreshold = c.DegradeThreshold
	}
	if c.DegradeDuration > 0 {
		d.DegradeDuration = c.DegradeDuration
	}
	if c.ExpirySoonThreshold > 0 {
		d.ExpirySoonThreshold = c.ExpirySoonThreshold
	}
	if c.ExpiryFactor > 0 {
		d.ExpiryFactor = c.ExpiryFactor
	}
	if c.CostEMAAlpha > 0 && c.CostEMAAlpha < 1 {
		d.CostEMAAlpha = c.CostEMAAlpha
	}
	if c.SessionTTL > 0 {
		d.SessionTTL = c.SessionTTL
	}
	return d
}

// accountState 是单个账号的运行态（不落盘，内存态）。
type accountState struct {
	acc           *types.Account
	fails         int                  // 连续上游失败计数（ReportUpstreamError）
	softStreak    int                  // 连续被限流计数（指数退避用）
	cooldownUntil time.Time            // 账号级冷却截止时间
	degradeUntil  time.Time            // 连败降级截止时间
	modelCooldown map[string]time.Time // 模型级独立冷却：model -> 截止时间
	emaCost       float64              // EMA 成本账本（单次调用平均消耗积分）
	emaSamples    int                  // EMA 观测次数
	lastPick      time.Time            // 上次被选中时间
}

// sessionBinding 记录会话键到上次成功账号的绑定（滚动续期）。
type sessionBinding struct {
	accountID string
	expiresAt time.Time
}

// Pool 是账号池：持有账号列表与全部运行态，并发安全。
type Pool struct {
	mu       sync.Mutex
	accounts []*accountState          // 保持插入顺序
	byID     map[string]*accountState // 按 Account.ID 索引
	sessions map[string]*sessionBinding
	cfg      PoolConfig
}

// NewPool 创建账号池。cfg 零值字段自动填默认参数。
// accounts 切片内的 *types.Account 会被池直接引用（运行态 Disabled 等字段会回写）。
func NewPool(accounts []*types.Account, cfg PoolConfig) *Pool {
	p := &Pool{
		byID:     make(map[string]*accountState, len(accounts)),
		sessions: make(map[string]*sessionBinding),
		cfg:      cfg.withDefaults(),
	}
	for _, acc := range accounts {
		p.addLocked(acc)
	}
	return p
}

// addLocked 在持有锁的前提下追加账号。
func (p *Pool) addLocked(acc *types.Account) {
	if acc == nil || acc.ID == "" {
		return
	}
	if st, ok := p.byID[acc.ID]; ok {
		// ID 已存在：只换账号指针（凭证可能刷新了），保留运行态。
		st.acc = acc
		return
	}
	st := &accountState{acc: acc, modelCooldown: make(map[string]time.Time)}
	p.accounts = append(p.accounts, st)
	p.byID[acc.ID] = st
}

// AddAccount 向池中追加账号。ID 已存在时只更新账号指针（保留运行态），
// 返回 true 表示新加入，false 表示已存在。
func (p *Pool) AddAccount(acc *types.Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acc == nil || acc.ID == "" {
		return false
	}
	if _, ok := p.byID[acc.ID]; ok {
		p.addLocked(acc)
		return false
	}
	p.addLocked(acc)
	return true
}

// Len 返回池中账号数量。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// eligibleLocked 判断账号在当前时刻是否可被选中（持有锁）。
// 返回不可用时的原因标签，供 Pick 统计与错误信息使用。
func (p *Pool) eligibleLocked(st *accountState, model string, now time.Time) (bool, string) {
	acc := st.acc
	if acc.Disabled {
		return false, "disabled"
	}
	if now.Before(st.cooldownUntil) {
		return false, "cooling"
	}
	if now.Before(st.degradeUntil) {
		return false, "degraded"
	}
	if model != "" {
		if until, ok := st.modelCooldown[model]; ok && now.Before(until) {
			return false, "modelcool"
		}
		for _, m := range acc.DisabledModels {
			if m == model {
				return false, "modelblocked"
			}
		}
	}
	return true, ""
}

// expiryFactorLocked 计算"最早到期优先"权重因子（持有锁）。
// 参考 linguo preferExpiring 的温和实现：快到期账号权重加成，不做硬排序。
func (p *Pool) expiryFactorLocked(acc *types.Account, now time.Time) float64 {
	if acc.CreditExpiresAt.IsZero() || !acc.CreditExpiresAt.After(now) {
		return 1
	}
	if acc.CreditExpiresAt.Sub(now) <= p.cfg.ExpirySoonThreshold {
		return p.cfg.ExpiryFactor
	}
	return 1
}

// weightLocked 计算账号的选号权重：手动权重 × 到期优先因子（持有锁）。
func (p *Pool) weightLocked(st *accountState, now time.Time) float64 {
	w := st.acc.Weight
	if w <= 0 {
		w = 1
	}
	return w * p.expiryFactorLocked(st.acc, now)
}

// sweepSessionsLocked 清理过期的会话绑定（持有锁），防止无限增长。
func (p *Pool) sweepSessionsLocked(now time.Time) {
	for k, b := range p.sessions {
		if !b.expiresAt.After(now) {
			delete(p.sessions, k)
		}
	}
}

// dropSessionsLocked 删除指向指定账号的全部会话绑定（持有锁）。
// 账号进入冷却/降级/禁用时调用，避免粘性把后续请求钉在不可用账号上。
func (p *Pool) dropSessionsLocked(accountID string) {
	for k, b := range p.sessions {
		if b.accountID == accountID {
			delete(p.sessions, k)
		}
	}
}

// Pick 从池中挑选一个账号，完整实现三层选号：
//  1. 会话粘性：SessionKey 非空且绑定的账号仍可用时，直接命中；
//  2. 成本分层：剩余额度 > 0 的账号优先（tier0），无额度账号只在 tier0 为空时参与；
//  3. 加权随机：权重 = 手动权重 × 到期优先因子，按权重概率抽取。
//
// 过滤跳过：Disabled、账号级冷却中、降级中、模型级冷却中、
// DisabledModels 命中目标模型的账号。无可用账号时返回 ErrNoAccountAvailable。
func (p *Pool) Pick(req types.PickRequest) (types.PickResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	// —— 第 1 层：会话粘性 ——
	if req.SessionKey != "" {
		if b, ok := p.sessions[req.SessionKey]; ok {
			if b.expiresAt.After(now) {
				if st, ok := p.byID[b.accountID]; ok {
					if eligible, _ := p.eligibleLocked(st, req.Model, now); eligible {
						b.expiresAt = now.Add(p.cfg.SessionTTL) // 滚动续期
						st.lastPick = now
						return types.PickResult{Account: st.acc, Reason: "session-sticky"}, nil
					}
				}
			}
			delete(p.sessions, req.SessionKey) // 绑定过期或账号不可用，解除绑定
		}
	}
	p.sweepSessionsLocked(now)

	// —— 第 2 层：过滤 + 成本分层 ——
	var tierCredit, tierEmpty []*accountState
	var nDisabled, nCooling, nDegraded, nModelCool, nModelBlocked int
	for _, st := range p.accounts {
		ok, reason := p.eligibleLocked(st, req.Model, now)
		if !ok {
			switch reason {
			case "disabled":
				nDisabled++
			case "cooling":
				nCooling++
			case "degraded":
				nDegraded++
			case "modelcool":
				nModelCool++
			case "modelblocked":
				nModelBlocked++
			}
			continue
		}
		if st.acc.CreditTotal > 0 {
			tierCredit = append(tierCredit, st)
		} else {
			tierEmpty = append(tierEmpty, st)
		}
	}
	candidates := tierCredit
	tierName := "credit>0"
	if len(candidates) == 0 {
		candidates = tierEmpty // 有额度的全不可用时，零额度账号顶班
		tierName = "credit=0"
	}
	if len(candidates) == 0 {
		return types.PickResult{}, fmt.Errorf(
			"%w（disabled=%d, 冷却中=%d, 降级中=%d, 模型冷却=%d, 模型黑名单=%d)",
			ErrNoAccountAvailable, nDisabled, nCooling, nDegraded, nModelCool, nModelBlocked)
	}

	// —— 第 3 层：加权随机 ——
	weights := make([]float64, len(candidates))
	var total float64
	for i, st := range candidates {
		weights[i] = p.weightLocked(st, now)
		total += weights[i]
	}
	var picked *accountState
	if total <= 0 {
		picked = candidates[0] // 兜底：权重全零时取首个
	} else {
		r := rand.Float64() * total
		picked = candidates[len(candidates)-1]
		for i, w := range weights {
			r -= w
			if r < 0 {
				picked = candidates[i]
				break
			}
		}
	}
	picked.lastPick = now

	// 绑定会话（Report 侧无 SessionKey，只能在选号时绑定；
	// 若后续调用失败导致账号不可用，Report 会清除其绑定）。
	if req.SessionKey != "" {
		p.sessions[req.SessionKey] = &sessionBinding{
			accountID: picked.acc.ID,
			expiresAt: now.Add(p.cfg.SessionTTL),
		}
	}

	return types.PickResult{
		Account: picked.acc,
		Reason:  fmt.Sprintf("weighted(tier=%s,weight=%.2f)", tierName, p.weightLocked(picked, now)),
	}, nil
}

// Report 上报一次上游调用结果，按 Status 更新账号运行态：
//   - ReportOK：衰减连续失败计数（减半），更新 EMA 成本账本；
//   - ReportRateLimited：指数退避冷却（基数 << (连续次数-1)，封顶），
//     同时写模型级独立冷却；
//   - ReportCreditExhausted：分钟级软冷却（固定时长，不指数堆加），
//     到期即回候选池，不硬冷却到次日；
//   - ReportBadRequest：客户端请求问题，不惩罚账号；
//   - ReportUpstreamError：连续失败计数，达阈值后降级（固定时长出池）；
//   - ReportAuthFailed：标记账号 Disabled（需人工/刷新恢复），清冷却域。
//
// 账号进入不可用状态时，其会话粘性绑定一并清除。
func (p *Pool) Report(r types.CallReport) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.byID[r.AccountID]
	if !ok {
		return // 未知账号，上报忽略
	}
	now := time.Now()

	switch r.Status {
	case types.ReportOK:
		st.softStreak = 0
		if st.fails > 0 {
			st.fails /= 2 // 衰减而非清零：历史失败仍有半衰记忆
		}
		if r.CreditCost > 0 {
			if st.emaSamples == 0 {
				st.emaCost = r.CreditCost
			} else {
				st.emaCost = p.cfg.CostEMAAlpha*r.CreditCost + (1-p.cfg.CostEMAAlpha)*st.emaCost
			}
			st.emaSamples++
		}
		// 注意：不碰 cooldownUntil / modelCooldown（参考 linguo NoteSuccess），
		// 冷却一律按时到期，避免成功一次就提前解限流。

	case types.ReportRateLimited:
		st.softStreak++
		shift := st.softStreak - 1
		if shift > 10 {
			shift = 10 // 10m<<10 已远超封顶，防位移溢出
		}
		backoff := p.cfg.RateLimitBase << shift
		if backoff > p.cfg.RateLimitMax || backoff <= 0 {
			backoff = p.cfg.RateLimitMax
		}
		until := now.Add(backoff)
		if until.After(st.cooldownUntil) {
			st.cooldownUntil = until
		}
		if r.Model != "" {
			// 模型级独立冷却：该模型被限流不影响账号上其他模型
			// （参考 linguo 6004 只冷却触发模型）。
			st.modelCooldown[r.Model] = until
		}
		p.dropSessionsLocked(st.acc.ID)

	case types.ReportCreditExhausted:
		// 分钟级软冷却：固定时长，不做指数堆加（参考 HanawaBanana
		// cooldown.credit_exhausted）。签到恢复余额后由外部刷新 CreditTotal，
		// 冷却到期账号自动回到候选池。
		until := now.Add(p.cfg.CreditExhaustedCooldown)
		if until.After(st.cooldownUntil) {
			st.cooldownUntil = until
		}
		p.dropSessionsLocked(st.acc.ID)

	case types.ReportBadRequest:
		// 400 类是客户端请求问题，不惩罚账号，只记日志（由调用方处理）。

	case types.ReportUpstreamError:
		st.fails++
		if st.fails >= p.cfg.DegradeThreshold {
			// 连败降级：固定时长临时出池（参考 linguo degrade.go，不指数升级）
			st.degradeUntil = now.Add(p.cfg.DegradeDuration)
			st.fails = 0
			p.dropSessionsLocked(st.acc.ID)
		}

	case types.ReportAuthFailed:
		// 401：凭证失效，标记停用等待人工处理/刷新；清冷却域
		// （参考 linguo：禁用时清冷却域但保留熔断器语义）。
		st.acc.Disabled = true
		st.cooldownUntil = time.Time{}
		st.degradeUntil = time.Time{}
		st.modelCooldown = make(map[string]time.Time)
		st.softStreak = 0
		st.fails = 0
		p.dropSessionsLocked(st.acc.ID)
	}
}

// AccountStat 是单个账号运行态的只读快照（面板/调试用）。
type AccountStat struct {
	AccountID     string
	Disabled      bool
	Fails         int                  // 连续上游失败计数
	SoftStreak    int                  // 连续被限流计数
	CooldownUntil time.Time            // 账号级冷却截止
	DegradeUntil  time.Time            // 降级截止
	ModelCooldown map[string]time.Time // 模型级冷却（副本）
	EMACost       float64              // EMA 成本账本
	LastPick      time.Time            // 上次被选中时间
}

// Snapshot 返回所有账号运行态的快照（深拷贝 map，调用方可安全读取）。
func (p *Pool) Snapshot() []AccountStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]AccountStat, 0, len(p.accounts))
	for _, st := range p.accounts {
		mc := make(map[string]time.Time, len(st.modelCooldown))
		for m, t := range st.modelCooldown {
			mc[m] = t
		}
		out = append(out, AccountStat{
			AccountID:     st.acc.ID,
			Disabled:      st.acc.Disabled,
			Fails:         st.fails,
			SoftStreak:    st.softStreak,
			CooldownUntil: st.cooldownUntil,
			DegradeUntil:  st.degradeUntil,
			ModelCooldown: mc,
			EMACost:       st.emaCost,
			LastPick:      st.lastPick,
		})
	}
	return out
}

// Get 按账号 ID 返回池内账号指针（池直接引用的 live 对象，调用方可修改运行态字段）。
// 不存在时返回 nil。
func (p *Pool) Get(id string) *types.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.byID[id]; ok {
		return st.acc
	}
	return nil
}

// All 返回池内全部账号指针（live 对象，保持插入顺序）。
// 调用方只应读或修改 Account 字段，不得改动返回切片本身。
func (p *Pool) All() []*types.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*types.Account, 0, len(p.accounts))
	for _, st := range p.accounts {
		out = append(out, st.acc)
	}
	return out
}

// SetDisabled 设置账号停用/启用状态（停用后不再参与选号，但照常参与签到等维护任务）。
// 不存在时返回 false。调用方负责持久化（auth.Save）。
func (p *Pool) SetDisabled(id string, disabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.byID[id]
	if !ok {
		return false
	}
	st.acc.Disabled = disabled
	return true
}

// Remove 把账号移出池子，返回被移除的账号（不存在时返回 nil）。
// 调用方负责删除凭证文件（auth.Delete）。
func (p *Pool) Remove(id string) *types.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.byID[id]
	if !ok {
		return nil
	}
	delete(p.byID, id)
	for i, s := range p.accounts {
		if s == st {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	p.dropSessionsLocked(id)
	return st.acc
}
