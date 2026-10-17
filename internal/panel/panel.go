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
	"time"

	"workbuddy2api-prime/internal/apikeys"
	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/billing"
	"workbuddy2api-prime/internal/pool"
	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/types"
)

//go:embed index.html
var indexHTML []byte

// Panel 聚合各后端模块，为面板 API 提供数据。
type Panel struct {
	pool   *pool.Pool
	sched  *scheduler.Scheduler
	ledger *billing.Ledger
	keys   *apikeys.Store
	auth   *auth.Store
}

// New 创建 Panel。各参数可为 nil（对应接口返回空数据），但正常使用应全传。
func New(p *pool.Pool, sched *scheduler.Scheduler, ledger *billing.Ledger, keys *apikeys.Store, auth *auth.Store) *Panel {
	return &Panel{pool: p, sched: sched, ledger: ledger, keys: keys, auth: auth}
}

// RegisterRoutes 把面板页面与 API 挂到 mux 上。
func (p *Panel) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/", p.serveIndex)
	mux.HandleFunc("GET /panel/api/accounts", p.handleAccounts)
	mux.HandleFunc("GET /panel/api/usage", p.handleUsage)
	mux.HandleFunc("GET /panel/api/checkin", p.handleCheckin)
	mux.HandleFunc("POST /panel/api/tasks/{name}/run", p.handleTaskRun)
	mux.HandleFunc("GET /panel/api/keys", p.handleKeysList)
	mux.HandleFunc("POST /panel/api/keys", p.handleKeyCreate)
	mux.HandleFunc("DELETE /panel/api/keys/{id}", p.handleKeyDelete)
}

// serveIndex 返回单页 HTML。
func (p *Panel) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
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

// handleUsage 返回用量聚合。参数：key（子钥匙 ID，空=全部）、hours（默认 24）。
func (p *Panel) handleUsage(w http.ResponseWriter, r *http.Request) {
	if p.ledger == nil {
		writeJSON(w, billing.Summary{})
		return
	}
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
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
