// workbuddy2api-prime 网关入口：组装各模块并启动 HTTP 服务。
//
// 请求链路：withAuth（子钥匙/管理员鉴权）→ orchestrator（模型编排+降级）
// → upstream.Backend（选号→上游→上报）→ billing（记账）
package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy2api-prime/internal/apikeys"
	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/billing"
	"workbuddy2api-prime/internal/config"
	"workbuddy2api-prime/internal/logbuf"
	"workbuddy2api-prime/internal/modelgroups"
	"workbuddy2api-prime/internal/panel"
	"workbuddy2api-prime/internal/pool"
	"workbuddy2api-prime/internal/protocol"
	"workbuddy2api-prime/internal/router"
	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
)

// ctxKey 是请求上下文键类型。
type ctxKey string

const ctxAPIKey ctxKey = "api_key"

// orchestrator 实现 protocol.ChatBackend：模型编排（降级链）+ 用量记账。
type orchestrator struct {
	inner  *upstream.Backend
	router *router.Router
	ledger *billing.Ledger
}

func (o *orchestrator) Chat(ctx context.Context, req *protocol.ChatRequest) (*protocol.ChatResponse, error) {
	chain := o.router.Chain(req.Model, time.Now())
	if len(chain) == 0 {
		chain = []string{req.Model}
	}
	var lastErr error
	for _, model := range chain {
		r := *req
		r.Model = model
		resp, err := o.inner.Chat(ctx, &r)
		if err == nil {
			o.recordUsage(ctx, &r, resp, true)
			return resp, nil
		}
		lastErr = err
		if !o.shouldFallback(err) {
			o.recordUsage(ctx, &r, nil, false)
			return nil, err
		}
		// 可降级：继续下一个候选模型。
	}
	o.recordUsage(ctx, req, nil, false)
	return nil, lastErr
}

// shouldFallback 把上游错误映射为编排降级类别。
func (o *orchestrator) shouldFallback(err error) bool {
	be, ok := err.(*protocol.BackendError)
	if !ok {
		return o.router.ShouldFallback(router.KindServer)
	}
	switch {
	case be.StatusCode == http.StatusTooManyRequests:
		return o.router.ShouldFallback(router.KindSoftRate)
	case be.StatusCode == http.StatusPaymentRequired:
		return o.router.ShouldFallback(router.KindHardCredit)
	case be.StatusCode == http.StatusNotFound:
		return o.router.ShouldFallback(router.KindNotFound)
	case be.StatusCode >= 500:
		return o.router.ShouldFallback(router.KindServer)
	default:
		return false // 400 等请求自身问题 fail-fast
	}
}

// recordUsage 写用量账本。
func (o *orchestrator) recordUsage(ctx context.Context, req *protocol.ChatRequest, resp *protocol.ChatResponse, ok bool) {
	if o.ledger == nil {
		return
	}
	rec := types.UsageRecord{
		Time:  time.Now(),
		Model: req.Model,
		OK:    ok,
	}
	if k, _ := ctx.Value(ctxAPIKey).(*types.APIKey); k != nil {
		rec.APIKeyID = k.ID
	}
	if resp != nil && resp.Usage != nil {
		rec.PromptTok = resp.Usage.PromptTokens
		rec.CompTok = resp.Usage.CompletionTokens
	}
	_ = o.ledger.Record(rec) // 记账失败不影响主链路
}

// withAuth 是鉴权中间件：wbk_ 子钥匙优先，否则比对管理员 key。
// adminKeyFn 用函数传入，支持向导页设置密钥后热更新。
func withAuth(keys *apikeys.Store, adminKeyFn func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
			return
		}
		if apikeys.IsSubKey(token) {
			k, err := keys.Verify(token, clientIP(r))
			if err != nil {
				http.Error(w, `{"error":"subkey rejected: `+err.Error()+`"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxAPIKey, k)))
			return
		}
		if adminKeyFn() == "" || token != adminKeyFn() {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// clientIP 只取 TCP 对端（默认不信任反代，防 XFF 伪造绕过 IP 白名单）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// promoLabelCode 把中文优惠标签映射为 ASCII 安全的英文编码。
// /v1/models 的 promo_label 可能被 agent 拿去做 HTTP header 值，中文会触发
// "Cannot convert argument to a ByteString" 错误。
func promoLabelCode(label string) string {
	switch label {
	case "限时免费", "Free now":
		return "free"
	case "独家限免":
		return "exclusive_free"
	case "夜间折扣":
		return "night_discount"
	case "错峰使用":
		return "off_peak"
	default:
		// 未知标签：转小写+下划线，保底 ASCII 安全
		out := make([]byte, 0, len(label))
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				out = append(out, byte(r))
			case r >= 'A' && r <= 'Z':
				out = append(out, byte(r+('a'-'A')))
			case r == ' ', r == '-', r == '_':
				out = append(out, '_')
			}
		}
		if len(out) == 0 {
			return "promo"
		}
		return string(out)
	}
}

func main() {
	// 日志同时 tee 到内存环形缓冲，供面板「运行日志」视图查询。
	log.SetOutput(io.MultiWriter(os.Stderr, logbuf.Writer()))

	cfg, err := config.Load("config.json")
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	authStore, err := auth.NewStore(cfg.AuthDir)
	if err != nil {
		log.Fatalf("初始化 auth store 失败: %v", err)
	}
	accounts, err := authStore.LoadAll()
	if err != nil {
		log.Fatalf("加载账号失败: %v", err)
	}
	log.Printf("已加载 %d 个账号", len(accounts))

	p := pool.NewPool(accounts, pool.DefaultPoolConfig())
	backend := upstream.NewBackend(p, authStore)

	keyStore, err := apikeys.NewStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化 apikeys store 失败: %v", err)
	}
	groupStore, err := modelgroups.NewStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化模型分组存储失败: %v", err)
	}
	ledger, err := billing.NewLedger(cfg.DataDir)
	if err != nil {
		log.Fatalf("初始化账本失败: %v", err)
	}
	rt := router.NewRouter(router.DefaultConfig())

	orch := &orchestrator{inner: backend, router: rt, ledger: ledger}

	// 定时任务：签到/保活/成长任务
	// 账号快照直接取池内 live 指针：面板新绑定的账号 AddAccount 进池后，
	// 定时任务下一轮即能看到，无需重启。
	sched, err := scheduler.New(func() []*types.Account { return p.All() }, cfg.DataDir,
		func(f string, a ...any) { log.Printf("[scheduler] "+f, a...) })
	if err != nil {
		log.Fatalf("初始化 scheduler 失败: %v", err)
	}
	sched.SetBilling(upstream.NewBilling(), authStore.Save)
	sched.SetModelDir(upstream.NewModelDirClient(), cfg.DataDir)
	sched.SetGrowth(upstream.NewGrowth()) // 成长任务引擎上游客户端
	sched.RegisterBuiltinTasks()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sched.Start(ctx)
	defer sched.Stop()

	mux := http.NewServeMux()
	protocol.RegisterRoutes(mux, orch)
	pn := panel.New(p, sched, ledger, keyStore, authStore)
	pn.SetAdminKey(cfg.AdminKey)
	pn.SetConfig(cfg)
	pn.SetGroups(groupStore)
	pn.RegisterRoutes(mux)
	// /v1/models：从模型目录缓存转换，带 group 分组元数据（discount/full_price）。
	// 分组开关（pn.ModelsGroup）：all 返回全部；discount/full_price 只返回该组——
	// agent 不理解 group 字段时，手动切组让接口只吐该组的模型。
	protocol.RegisterModelsRoute(mux, func(r *http.Request) []protocol.ModelInfo {
		entries, promos, _, err := sched.ModelDirSnapshot()
		if err != nil {
			return nil
		}
		// 子钥匙绑定了自定义分组时，只返回组内模型。
		var groupModels map[string]bool
		if k, _ := r.Context().Value(ctxAPIKey).(*types.APIKey); k != nil && k.ModelGroupID != "" {
			if g := groupStore.Get(k.ModelGroupID); g != nil {
				groupModels = make(map[string]bool, len(g.Models))
				for _, mid := range g.Models {
					groupModels[mid] = true
				}
			}
		}
		// 优惠不在条目里（抓取层故意不写死），用 promotions 规则实时判定——
		// 与面板 mdApplyPromos 同逻辑：promo 启用 + 时间窗口生效 + 模型 ID 命中。
		now := time.Now()
		activePromo := map[string]*upstream.ModelPromotion{} // entryID -> promo
		for i := range promos {
			p := &promos[i]
			if !p.Enabled || !upstream.PromoActiveAt(p, now) {
				continue
			}
			for _, mid := range p.ModelIDs {
				if _, ok := activePromo[mid]; !ok {
					activePromo[mid] = p
				}
			}
		}
		wantGroup := pn.ModelsGroup()
		out := make([]protocol.ModelInfo, 0, len(entries))
		for _, e := range entries {
			// 子钥匙绑定了自定义分组：只保留组内模型（优先级高于全局分组开关）。
			if groupModels != nil && !groupModels[e.ID] {
				continue
			}
			group := "full_price"
			label := ""
			if p, ok := activePromo[e.ID]; ok {
				group = "discount"
				if p.Badge != nil {
					// promo_label 只给 ASCII 安全的英文编码：agent 可能拿去做 header 值，
					// 中文会导致 "Cannot convert argument to a ByteString"。
					label = promoLabelCode(p.Badge.Label)
				}
			}
			if groupModels == nil && wantGroup != "all" && group != wantGroup {
				continue
			}
			out = append(out, protocol.ModelInfo{
				ID:         e.ID,
				Group:      group,
				PromoLabel: label,
			})
		}
		return out
	})

	// /healthz 公开（健康检查），其余走鉴权。
	root := http.NewServeMux()
	root.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// 首次运行向导：admin key 为空时，/panel/setup 走公开路由，设好即生效。
	if pn.SetupNeeded() {
		pn.SetupRoutes(root, func(k string) error {
			cfg.AdminKey = k
			pn.SetAdminKey(k)
			return config.Save("config.json", cfg)
		})
	}
	root.Handle("/", withAuth(keyStore, func() string { return cfg.AdminKey }, mux))

	handler := root
	log.Printf("workbuddy2api-prime listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, handler))
}
