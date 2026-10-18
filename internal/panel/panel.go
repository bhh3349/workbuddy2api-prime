// Package panel 提供 Web 管理面板：账号池可视化、用量明细、签到历史、
// 子钥匙管理、定时任务手动触发。
//
// 鉴权由 cmd/server 的 withAuth 统一做（admin key），本包内不做鉴权。
// 页面为单文件 HTML（go:embed），无前端框架，原生 JS + fetch。
package panel

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"workbuddy2api-prime/internal/apikeys"
	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/billing"
	"workbuddy2api-prime/internal/pool"
	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// Panel 聚合各后端模块，为面板 API 提供数据。
type Panel struct {
	pool   *pool.Pool
	sched  *scheduler.Scheduler
	ledger *billing.Ledger
	keys   *apikeys.Store
	auth   *auth.Store

	mu       sync.Mutex
	adminKey string       // 当前管理员密钥（SetAdminKey 设置，供 SetupNeeded 判断）
	devices  *deviceStore // 进行中的设备授权会话
	billing  *upstream.Billing
}

// New 创建 Panel。各参数可为 nil（对应接口返回空数据），但正常使用应全传。
func New(p *pool.Pool, sched *scheduler.Scheduler, ledger *billing.Ledger, keys *apikeys.Store, auth *auth.Store) *Panel {
	return &Panel{pool: p, sched: sched, ledger: ledger, keys: keys, auth: auth, billing: upstream.NewBilling()}
}

// RegisterRoutes 把面板页面与 API 挂到 mux 上。
func (p *Panel) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/", p.serveIndex)
	mux.HandleFunc("GET /panel/app.js", p.serveAppJS)
	mux.HandleFunc("GET /panel/api/accounts", p.handleAccounts)
	mux.HandleFunc("GET /panel/api/usage", p.handleUsage)
	mux.HandleFunc("GET /panel/api/usage/series", p.handleUsageSeries)
	mux.HandleFunc("GET /panel/api/usage/recent", p.handleUsageRecent)
	mux.HandleFunc("GET /panel/api/checkin", p.handleCheckin)
	mux.HandleFunc("POST /panel/api/tasks/{name}/run", p.handleTaskRun)
	mux.HandleFunc("GET /panel/api/keys", p.handleKeysList)
	mux.HandleFunc("POST /panel/api/keys", p.handleKeyCreate)
	mux.HandleFunc("DELETE /panel/api/keys/{id}", p.handleKeyDelete)
	mux.HandleFunc("POST /panel/api/accounts/device/start", p.handleDeviceStart)
	mux.HandleFunc("GET /panel/api/accounts/device/poll", p.handleDevicePoll)
	mux.HandleFunc("POST /panel/api/accounts/import", p.handleAccountImport)
	// 账号运维：单号签到/余额、批量操作、停用启用、删除
	mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.handleAccountCheckin)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.handleAccountBalance)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.handleAccountDisable)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/enable", p.handleAccountEnable)
	mux.HandleFunc("DELETE /panel/api/accounts/{uid}", p.handleAccountRemove)
	mux.HandleFunc("POST /panel/api/checkin_all", p.handleCheckinAll)
	mux.HandleFunc("POST /panel/api/balance_all", p.handleBalanceAll)
	mux.HandleFunc("POST /panel/api/keepalive_all", p.handleKeepaliveAll)
	// 模型目录：读缓存展示 + 手动立即刷新（定时任务每天 03:00 自动抓取）
	mux.HandleFunc("GET /panel/api/models", p.handleModels)
	mux.HandleFunc("POST /panel/api/models/refresh", p.handleModelsRefresh)

	// 成长任务（任务中心后端 API）
	mux.HandleFunc("GET /panel/api/growth/overview", p.handleGrowthOverview)
	mux.HandleFunc("POST /panel/api/growth/scan", p.handleGrowthScan)
	mux.HandleFunc("POST /panel/api/growth/run", p.handleGrowthRun)
	mux.HandleFunc("GET /panel/api/growth/queue", p.handleGrowthQueue)
	// 任务中心：队列扫描/执行 + 单账号任务操作（对齐前端调用点）
	mux.HandleFunc("POST /panel/api/tasks/scan_all", p.handleTasksScanAll)
	mux.HandleFunc("POST /panel/api/tasks/run_queue", p.handleTasksRunQueue)
	mux.HandleFunc("GET /panel/api/tasks/queue", p.handleTasksQueue)
	mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.handleAccountTasks)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.handleAccountTasksAccept)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.handleAccountTasksAcceptAll)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.handleAccountTasksClaim)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.handleAccountTasksAuto)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.handleAccountTasksAutoAll)
}

// serveIndex 返回单页 HTML。
func (p *Panel) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// serveAppJS 返回前端脚本（1 小时缓存）。
func (p *Panel) serveAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(appJS)
}

// handleUsage 返回用量聚合（billing.Summary 原生形态）。
// 参数：key（子钥匙过滤）、hours（默认 24）。
func (p *Panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	if p.ledger == nil {
		writeJSON(w, billing.Summary{})
		return
	}
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n >= 0 && n <= 24*30 {
			hours = n
		}
	}
	sum, err := p.ledger.Query(r.URL.Query().Get("key"), time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询用量失败: "+err.Error())
		return
	}
	writeJSON(w, sum)
}

// handleKeepaliveAll 手动触发全量保活。
func (p *Panel) handleKeepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	if err := p.sched.TriggerNow("keepalive"); err != nil {
		writeError(w, http.StatusBadRequest, "触发失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "started": true})
}

// modelDirView 是模型目录接口形态：无缓存时 fetched_at 为 null，entries 为空数组。
type modelDirView struct {
	FetchedAt *string                `json:"fetched_at"`
	Entries   []upstream.ModelEntry `json:"entries"`
}

// handleModels 返回模型目录缓存（只读，不触发上游请求）。
func (p *Panel) handleModels(w http.ResponseWriter, r *http.Request) {
	v := modelDirView{Entries: []upstream.ModelEntry{}}
	if p.sched != nil {
		if entries, at, err := p.sched.ModelDirSnapshot(); err == nil {
			s := at.Format(time.RFC3339)
			v.FetchedAt = &s
			v.Entries = entries
		}
	}
	writeJSON(w, v)
}

// handleModelsRefresh 立即抓取一次模型目录（复用定时任务同一函数）。
func (p *Panel) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	n, err := p.sched.RefreshModelDir(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "抓取失败: "+err.Error())
		return
	}
	_, at, _ := p.sched.ModelDirSnapshot()
	writeJSON(w, map[string]any{"ok": true, "count": n, "fetched_at": at.Format(time.RFC3339)})
}

// accountView 是账号行视图：基础信息 + 运行态快照合并。
type accountView struct {
	ID              string  `json:"id"`
	UID             string  `json:"uid"`
	Region          string  `json:"region"`
	Disabled        bool    `json:"disabled"`
	CreditTotal     float64 `json:"credit_total"`
	CreditExpiresAt string  `json:"credit_expires_at"`
	Weight          float64 `json:"weight"`
	Fails           int     `json:"fails"`
	SoftStreak      int     `json:"soft_streak"`
	CooldownUntil   string  `json:"cooldown_until"`
	DegradeUntil    string  `json:"degrade_until"`
	EMACost         float64 `json:"ema_cost"`
	LastPick        string  `json:"last_pick"`
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// handleAccounts 返回账号列表（基础信息 + 运行态快照按 ID 合并）。
func (p *Panel) handleAccounts(w http.ResponseWriter, r *http.Request) {
	views := []accountView{}
	if p.auth == nil || p.pool == nil {
		writeJSON(w, views)
		return
	}
	accounts, err := p.auth.LoadAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "加载账号失败: "+err.Error())
		return
	}
	stats := map[string]pool.AccountStat{}
	for _, st := range p.pool.Snapshot() {
		stats[st.AccountID] = st
	}
	for _, a := range accounts {
		v := accountView{
			ID:              a.ID,
			UID:             a.UID,
			Region:          a.Region,
			Disabled:        a.Disabled,
			CreditTotal:     a.CreditTotal,
			CreditExpiresAt: fmtTime(a.CreditExpiresAt),
			Weight:          a.Weight,
		}
		if st, ok := stats[a.ID]; ok {
			v.Disabled = v.Disabled || st.Disabled
			v.Fails = st.Fails
			v.SoftStreak = st.SoftStreak
			v.CooldownUntil = fmtTime(st.CooldownUntil)
			v.DegradeUntil = fmtTime(st.DegradeUntil)
			v.EMACost = st.EMACost
			v.LastPick = fmtTime(st.LastPick)
		}
		views = append(views, v)
	}
	writeJSON(w, views)
}

// handleUsageSeries 返回用量时间序列。参数：key、hours（默认24）、buckets（默认24）。
// 桶时间戳是真实时间轴，供前端 SVG 趋势图按时间定位（不断档失真）。
func (p *Panel) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	if p.ledger == nil {
		writeJSON(w, []billing.Bucket{})
		return
	}
	q := r.URL.Query()
	hours := 24
	if h := q.Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 24*30 {
			hours = n
		}
	}
	buckets := 24
	if b := q.Get("buckets"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 && n <= 240 {
			buckets = n
		}
	}
	series, err := p.ledger.BucketSeries(q.Get("key"), time.Now().Add(-time.Duration(hours)*time.Hour), buckets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询用量序列失败: "+err.Error())
		return
	}
	writeJSON(w, series)
}

// handleUsageRecent 返回最近的用量记录（新→旧）。参数：key、limit（默认50，上限200）。
func (p *Panel) handleUsageRecent(w http.ResponseWriter, r *http.Request) {
	if p.ledger == nil {
		writeJSON(w, []types.UsageRecord{})
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	recs, err := p.ledger.Recent(r.URL.Query().Get("key"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询最近请求失败: "+err.Error())
		return
	}
	writeJSON(w, recs)
}

// handleCheckin 返回签到历史（只读，不触发上游请求）。
func (p *Panel) handleCheckin(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeJSON(w, []types.CheckinResult{})
		return
	}
	writeJSON(w, p.sched.History())
}

// handleTaskRun 手动触发一个定时任务（签到/保活/成长任务补跑）。
func (p *Panel) handleTaskRun(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	name := r.PathValue("name")
	if err := p.sched.TriggerNow(name); err != nil {
		writeError(w, http.StatusBadRequest, "触发任务失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "task": name})
}

// keyView 是子钥匙视图（不含 sha256 摘要）。
type keyView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Quota      float64  `json:"quota"`
	Used       float64  `json:"used"`
	Models     []string `json:"models"`
	Disabled   bool     `json:"disabled"`
	ExpiresAt  string   `json:"expires_at"`
	CreatedAt  string   `json:"created_at"`
	Concurrent int      `json:"concurrent"`
}

func toKeyView(k *types.APIKey) keyView {
	return keyView{
		ID: k.ID, Name: k.Name, Quota: k.Quota, Used: k.Used,
		Models: k.Models, Disabled: k.Disabled,
		ExpiresAt: fmtTime(k.ExpiresAt), CreatedAt: fmtTime(k.CreatedAt),
		Concurrent: k.Concurrent,
	}
}

// handleKeysList 返回子钥匙列表。
func (p *Panel) handleKeysList(w http.ResponseWriter, r *http.Request) {
	views := []keyView{}
	if p.keys == nil {
		writeJSON(w, views)
		return
	}
	for _, k := range p.keys.List() {
		views = append(views, toKeyView(k))
	}
	writeJSON(w, views)
}

// handleKeyCreate 新建子钥匙。明文只在本次响应里返回一次。
func (p *Panel) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	if p.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "apikeys 未初始化")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	plain, key, err := p.keys.Create(body.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "创建失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"plain": plain, "key": toKeyView(key)})
}

// handleKeyDelete 删除子钥匙。
func (p *Panel) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if p.keys == nil {
		writeError(w, http.StatusServiceUnavailable, "apikeys 未初始化")
		return
	}
	if err := p.keys.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, "删除失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
