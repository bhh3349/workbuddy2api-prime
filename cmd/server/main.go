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
	// /v1/models：从模型目录缓存转换，带 group 分组元数据（discount/full_price）。
	protocol.RegisterModelsRoute(mux, func() []protocol.ModelInfo {
		entries, _, _, err := sched.ModelDirSnapshot()
		if err != nil {
			return nil
		}
		out := make([]protocol.ModelInfo, 0, len(entries))
		for _, e := range entries {
			group := "full_price"
			if e.PromoFactor != nil || e.PromoLabel != "" {
				group = "discount"
			}
			out = append(out, protocol.ModelInfo{
				ID:         e.ID,
				Group:      group,
				PromoLabel: e.PromoLabel,
			})
		}
		return out
	})
	pn := panel.New(p, sched, ledger, keyStore, authStore)
	pn.SetAdminKey(cfg.AdminKey)
	pn.SetConfig(cfg)
	pn.RegisterRoutes(mux)

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
