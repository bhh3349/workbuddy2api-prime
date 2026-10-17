// Package router 实现模型编排：虚拟模型（auto/cn:auto）的昼夜路由与降级链展开。
//
// 移植自 panel-plus 的 internal/autoroute 设计（见分析文档
// ~/workspace/workbuddy2api-analysis/docs/04-workbuddy2api-panel-plus-jacky.md §4.2）：
//   - 虚拟模型按北京时间判定昼夜，选 day_primary / night_primary；
//   - 降级链递归展开（去重、最多 8 个、递归 3 层防环）；
//   - 只有「换号解决不了、换模型可能解决」的错误类别才触发降级。
package router

import (
	"strings"
	"time"
)

// 可降级错误类别。判据哲学：「换号解决不了、换模型可能解决」才降级；
// 内容拦截/参数错误/上下文超长等请求自身问题应 fail-fast，不进降级。
const (
	KindSoftRate       = "soft_rate"        // 限流
	KindHardCredit     = "hard_credit"      // 额度耗尽
	KindModelBlocked   = "model_blocked"    // 后端无此模型
	KindServer         = "server"           // 上游故障
	KindAccountFault   = "account_fault"    // 账号授权故障
	KindNotFound       = "not_found"        // 模型不存在
	KindNoHealthyAcct  = "no_healthy_account" // 无可用账号
)

// defaultKinds 是 FallbackOn 为空时的默认可降级集合。
var defaultKinds = []string{
	KindSoftRate,
	KindHardCredit,
	KindModelBlocked,
	KindServer,
	KindAccountFault,
	KindNotFound,
	KindNoHealthyAcct,
}

// 降级链展开的上限：防配置成环或过长把单请求放大成风暴。
const (
	maxChainLen   = 8
	maxExpandDepth = 3
)

// Config 是模型编排的配置，对应 config.json 的 auto_model 段 + 顶层 model_fallback。
type Config struct {
	Enabled      bool              // 默认 false：关闭时零回归，行为与未启用编排完全一致
	DayPrimary   string            // 白天主模型，如 "cn:hy3"
	NightPrimary string            // 夜间主模型，如 "cn:hy4-preview"
	DayStart     int               // 白天起始小时（北京时间），默认 8
	DayEnd       int               // 白天结束小时（北京时间），默认 23；区间 [start,end)
	Fallback     []string          // auto 的降级链
	ModelFallback map[string][]string // 具名模型→降级链，键须与请求 model 逐字匹配
	VirtualID    string            // 虚拟模型名，默认 "auto"
	Override     bool              // 上游有同名真实模型时仍接管（默认让位不劫持）
	OnEmpty      bool              // 上游 200 但正文空视为失败并降级，默认 true
	FallbackOn   []string          // 可触发降级的错误类别，空=默认集合
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:      false,
		DayStart:     8,
		DayEnd:       23,
		VirtualID:    "auto",
		Override:     false,
		OnEmpty:      true,
		FallbackOn:   nil,
	}
}

// Router 是模型编排器。
type Router struct {
	cfg Config
	// realModels 是上游实际下发的模型集合（用于 IsVirtual 的让位判断）；
	// 为 nil 表示未知，此时只要裸名命中虚拟名即视为虚拟模型。
	realModels map[string]bool
}

// NewRouter 创建 Router。
func NewRouter(cfg Config) *Router {
	if cfg.VirtualID == "" {
		cfg.VirtualID = "auto"
	}
	return &Router{cfg: cfg}
}

// SetRealModels 设置上游实际模型集合（用于虚拟名让位判断）。
func (r *Router) SetRealModels(models map[string]bool) {
	r.realModels = models
}

// cstZone 是北京时间时区：固定 +8，不依赖进程 TZ。
var cstZone = time.FixedZone("CST", 8*3600)

// hourCST 返回 now 的北京时间小时（0-23）。
func hourCST(now time.Time) int {
	return now.In(cstZone).Hour()
}

// isDay 按配置判定给定北京时间小时是否为白天。
// start==end 视为全天白天；start<end 为 [start,end)；start>end 为跨零点。
func (r *Router) isDay(hour int) bool {
	start, end := r.cfg.DayStart, r.cfg.DayEnd
	if start == end {
		return true
	}
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

// splitRealm 把 "cn:hy3" 拆成 realm="cn"、bare="hy3"；无前缀时 realm 为空。
func splitRealm(model string) (realm, bare string) {
	if i := strings.Index(model, ":"); i > 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

// IsVirtual 判定 model 是否为本网关接管的虚拟模型。
// 条件：编排已启用 且 裸名==虚拟名 且（上游无同名真实模型 或 Override）。
// 上游 cn 侧曾下发过 "auto"，默认让位不劫持。
func (r *Router) IsVirtual(model string) bool {
	if !r.cfg.Enabled {
		return false
	}
	_, bare := splitRealm(model)
	if bare != r.cfg.VirtualID {
		return false
	}
	if r.realModels != nil && r.realModels[bare] && !r.cfg.Override {
		return false
	}
	return true
}

// primary 按昼夜返回虚拟模型的主模型。
func (r *Router) primary(now time.Time) string {
	if r.isDay(hourCST(now)) {
		return r.cfg.DayPrimary
	}
	return r.cfg.NightPrimary
}

// filterRealm 按 realm 过滤候选：保留同 realm 或无 realm 前缀（realm 无关）的条目。
func filterRealm(models []string, realm string) []string {
	if realm == "" {
		return models
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		mr, _ := splitRealm(m)
		if mr == "" || mr == realm {
			out = append(out, m)
		}
	}
	return out
}

// expand 递归展开降级链：去重、最多 maxChainLen 个、递归 maxExpandDepth 层，防成环。
func (r *Router) expand(seeds []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, maxChainLen)
	var walk func(models []string, depth int)
	walk = func(models []string, depth int) {
		if len(out) >= maxChainLen || depth > maxExpandDepth {
			return
		}
		for _, m := range models {
			if len(out) >= maxChainLen {
				return
			}
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
			// 递归展开该模型的具名降级链
			if next, ok := r.cfg.ModelFallback[m]; ok && depth < maxExpandDepth {
				walk(next, depth+1)
			}
		}
	}
	walk(seeds, 0)
	return out
}

// Chain 为一次请求计算候选模型链（按优先级排序）。
//
//   - 虚拟模型（auto/cn:auto/global:auto）：[昼夜主模型] + realm 过滤后的 Fallback，
//     再递归展开。global:auto 在无国际版候选时返回 [model] 原样（单元素）。
//   - 具名模型：[自身] + ModelFallback[自身] 递归展开；无 fallback 时返回 [model]，
//     行为与未启用编排完全一致。
func (r *Router) Chain(model string, now time.Time) []string {
	if r.IsVirtual(model) {
		realm, _ := splitRealm(model)
		// 主模型与降级链都要按 realm 过滤：cn:auto/global:auto 只在该 realm 候选里挑
		seeds := filterRealm(append([]string{r.primary(now)}, r.cfg.Fallback...), realm)
		chain := r.expand(seeds)
		if len(chain) == 0 {
			// 如 global:auto 在默认配置下无国际版候选：空链→单元素原样
			return []string{model}
		}
		return chain
	}
	// 具名模型：键与请求 model 逐字匹配
	if fb, ok := r.cfg.ModelFallback[model]; ok {
		return r.expand(append([]string{model}, fb...))
	}
	return []string{model}
}

// ShouldFallback 判定给定错误类别是否可触发降级（换候选模型重试）。
func (r *Router) ShouldFallback(kind string) bool {
	kinds := r.cfg.FallbackOn
	if len(kinds) == 0 {
		kinds = defaultKinds
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
