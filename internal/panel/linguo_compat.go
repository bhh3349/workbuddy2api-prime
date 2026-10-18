package panel

// linguo_compat.go: linguo workbuddy2api-panel 前端（index.html + app.js）的 JSON 兼容层。
//
// 用户决定前端直接套用 linguo 的现成前端，本文件用我们现有模块
// （pool/auth/billing/scheduler/upstream）的数据，拼出 linguo 后端同形态的 JSON。
// 字段名均为 snake_case，与 linguo internal/panel/panel.go 的 writeJSON 输出对齐，
// 具体形态以该仓库实际代码为准（见 ~/workspace/workbuddy2api-analysis/docs/05-panel-ui-analysis.md）。
//
// 已有路由中与 linguo 前端调用完全一致的（checkin/balance/disable/checkin_all/
// balance_all）直接复用 accounts.go 的 handler；本文件只补：
//   - app.js 静态文件
//   - overview / packages / usage / request_metrics / request_logs
//   - login/start / login/poll（转调 device.go 的 startDeviceFlow/pollDeviceFlow）
//   - import/cockpit（multipart 文件导入，转调 device.go 的 importToken）
//   - accounts/{uid}/remove 的 POST 别名、revive（转调 enable）、pause/resume（501）
//   - keepalive_all（转调 scheduler.TriggerNow）

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api-prime/internal/types"
)

//go:embed app.js
var appJS []byte

// primeVersion 展示在面板导航栏的版本号（linguo 前端读 overview.version）。
const primeVersion = "1.0.0-prime"

// registerLinguoCompat 注册 linguo 前端需要的兼容接口。
func (p *Panel) registerLinguoCompat(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel/app.js", p.serveAppJS)
	mux.HandleFunc("GET /panel/api/overview", p.handleLinguoOverview)
	mux.HandleFunc("GET /panel/api/packages", p.handleLinguoPackages)
	mux.HandleFunc("GET /panel/api/usage", p.handleLinguoUsage)
	mux.HandleFunc("GET /panel/api/request_metrics", p.handleLinguoRequestMetrics)
	mux.HandleFunc("GET /panel/api/request_logs", p.handleLinguoRequestLogs)
	mux.HandleFunc("POST /panel/api/login/start", p.handleLinguoLoginStart)
	mux.HandleFunc("GET /panel/api/login/poll", p.handleLinguoLoginPoll)
	mux.HandleFunc("POST /panel/api/import/cockpit", p.handleLinguoImportCockpit)
	// linguo 前端用 POST 调 remove（我们原有的是 DELETE，保留），revive 转调 enable。
	mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.handleAccountRemove)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.handleAccountEnable)
	// pause/resume 我们没有暂停态：返回 501，前端已隐藏对应按钮。
	mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.handleLinguoNotImpl)
	mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.handleLinguoNotImpl)
	mux.HandleFunc("POST /panel/api/keepalive_all", p.handleLinguoKeepaliveAll)
}

// serveAppJS 返回 linguo 前端脚本（无缓存协商问题，加长缓存即可）。
func (p *Panel) serveAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(appJS)
}

func (p *Panel) handleLinguoNotImpl(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "prime 未实现该功能")
}

// linguoRealm 把我们的 region（cn/intl）映射为 linguo 口径（cn/global）。
func linguoRealm(region string) string {
	if region == "intl" {
		return "global"
	}
	return "cn"
}

// overviewAccount 是 overview.accounts[] 的单项，字段对齐 linguo pool.Status 的 JSON。
func (p *Panel) overviewAccount(acc *types.Account, stat *poolStatLite, checkinDone map[string]bool) map[string]any {
	now := time.Now()
	credits := int64(acc.CreditTotal)
	item := map[string]any{
		"uid":      acc.UID,
		"nickname": "", // prime 未存昵称，前端回落显示 uid 前 8 位
		"realm":    linguoRealm(acc.Region),
		"credits":  credits,
		// credits_total 未知时 linguo 用 0 表示"未知"，前端按池内最高归一化进度条。
		"credits_total":            credits,
		"credits_earliest_remaining": credits,
		"disabled":                 acc.Disabled,
		"paused":                   false,
		"enterprise":               false,
		"checkin_done":             checkinDone[acc.ID],
		"success_count":            0,
		"err_total":                0,
		"in_flight":                0,
	}
	if !acc.CreditExpiresAt.IsZero() {
		item["credits_earliest_expiry"] = acc.CreditExpiresAt.Format(time.RFC3339)
	}
	if stat != nil {
		coolRem := int64(0)
		if stat.CooldownUntil.After(now) {
			coolRem = int64(stat.CooldownUntil.Sub(now).Seconds())
			item["breaker_until"] = stat.CooldownUntil.Format(time.RFC3339)
		}
		if stat.DegradeUntil.After(now) {
			if d := int64(stat.DegradeUntil.Sub(now).Seconds()); d > coolRem {
				coolRem = d
			}
			item["degrade_until"] = stat.DegradeUntil.Format(time.RFC3339)
		}
		item["cooling"] = coolRem > 0
		item["cool_remaining_sec"] = coolRem
		// cool_kind：linguo 用 hard_credit 区分"积分冷却"，其余走"限流冷却"。
		// prime 无积分耗尽独立档位：连续被限流记 soft，其余记 hard。
		if stat.SoftStreak > 0 {
			item["cool_kind"] = "soft"
		} else if coolRem > 0 {
			item["cool_kind"] = "hard"
		}
		item["soft_streak"] = stat.SoftStreak
		if len(stat.ModelCooldown) > 0 {
			rl := make([]map[string]any, 0, len(stat.ModelCooldown))
			for m, until := range stat.ModelCooldown {
				if until.After(now) {
					rl = append(rl, map[string]any{"model": m, "kind": "rate_limit", "until": until.Format(time.RFC3339)})
				}
			}
			if len(rl) > 0 {
				item["rate_limited_models"] = rl
			}
		}
		// EMA 成本台账：linguo 叫 model_costs（逐模型），prime 只有账号级 EMA，
		// 用一个 synthetic 行透出，避免前端 tooltip 为空。
		if stat.EMACost > 0 {
			item["model_costs"] = []map[string]any{{"model": "avg", "cost_per_1k": stat.EMACost}}
		}
	}
	return item
}

// poolStatLite 是兼容层需要的 pool 快照子集（避免直接依赖 pool 包内部结构）。
type poolStatLite struct {
	Disabled      bool
	SoftStreak    int
	CooldownUntil time.Time
	DegradeUntil  time.Time
	ModelCooldown map[string]time.Time
	EMACost       float64
}

// handleLinguoOverview: GET /panel/api/overview
// 形态对齐 linguo panel.go:232（version/uptime_sec/total/healthy/cooling/disabled/
// paused/accounts/model_locks，另加前端实际读取的 sticky_sessions/redis_mode/in_flight_full）。
func (p *Panel) handleLinguoOverview(w http.ResponseWriter, r *http.Request) {
	if p.pool == nil {
		writeError(w, http.StatusNotImplemented, "pool 未初始化")
		return
	}
	now := time.Now()
	stats := map[string]*poolStatLite{}
	for _, st := range p.pool.Snapshot() {
		stats[st.AccountID] = &poolStatLite{
			Disabled:      st.Disabled,
			SoftStreak:    st.SoftStreak,
			CooldownUntil: st.CooldownUntil,
			DegradeUntil:  st.DegradeUntil,
			ModelCooldown: st.ModelCooldown,
			EMACost:       st.EMACost,
		}
	}
	// 今日已签到集合（scheduler 历史，AccountID 口径）。
	checkinDone := map[string]bool{}
	if p.sched != nil {
		day := now.Format("2006-01-02")
		for _, h := range p.sched.History() {
			if h.OK && h.Time.Format("2006-01-02") == day {
				checkinDone[h.AccountID] = true
			}
		}
	}
	accs := p.pool.All()
	items := make([]map[string]any, 0, len(accs))
	var total, healthy, cooling, disabled int
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		total++
		st := stats[acc.ID]
		dis := acc.Disabled || (st != nil && st.Disabled)
		cool := st != nil && (st.CooldownUntil.After(now) || st.DegradeUntil.After(now))
		switch {
		case dis:
			disabled++
		case cool:
			cooling++
		default:
			healthy++
		}
		items = append(items, p.overviewAccount(acc, st, checkinDone))
	}
	writeJSON(w, map[string]any{
		"version":         primeVersion,
		"uptime_sec":      int(time.Since(p.startedAt).Seconds()),
		"auth_required":   true,
		"redis_mode":      "local",
		"sticky_sessions": 0,
		"in_flight_full":  0,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"paused":          0,
		"accounts":        items,
		"model_locks":     nil,
	})
}

// handleLinguoPackages: GET /panel/api/packages
// linguo 返回逐账号的积分包明细（逐个查上游）；prime 只有每账号总额+最早到期，
// 用单个 synthetic 包呈现（name=剩余额度），按余额降序（与 linguo 一致）。
func (p *Panel) handleLinguoPackages(w http.ResponseWriter, r *http.Request) {
	if p.pool == nil {
		writeJSON(w, []any{})
		return
	}
	type row struct {
		UID      string         `json:"uid"`
		Nickname string         `json:"nickname"`
		Realm    string         `json:"realm"`
		Remain   int64          `json:"remain"`
		Size     int64          `json:"size"`
		Packages []map[string]any `json:"packages"`
	}
	accs := p.pool.All()
	out := make([]row, 0, len(accs))
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		remain := int64(acc.CreditTotal)
		pkg := map[string]any{
			"name":   "剩余额度",
			"remain": remain,
			"used":   0,
			"size":   remain,
		}
		if !acc.CreditExpiresAt.IsZero() {
			pkg["end_time"] = acc.CreditExpiresAt.Format(time.RFC3339)
			pkg["expires_at"] = acc.CreditExpiresAt.UnixMilli()
		}
		out = append(out, row{
			UID:      acc.UID,
			Nickname: "",
			Realm:    linguoRealm(acc.Region),
			Remain:   remain,
			Size:     remain,
			Packages: []map[string]any{pkg},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, out)
}

// linguoUsageWindow 解析 linguo 口径的时间窗口参数（from/to/unix 秒或 RFC3339，hours）。
func linguoUsageWindow(q map[string][]string) (from, to time.Time) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	parse := func(v string) time.Time {
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 1e12 {
				return time.UnixMilli(n)
			}
			return time.Unix(n, 0)
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		return time.Time{}
	}
	to = time.Now()
	from = parse(get("from"))
	if t := parse(get("to")); !t.IsZero() {
		to = t
	}
	if from.IsZero() {
		hours := 72
		if v := get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				hours = n
			}
		}
		if hours > 1440 {
			hours = 1440
		}
		from = to.Add(-time.Duration(hours) * time.Hour)
	}
	return from, to
}

// handleLinguoUsage: GET /panel/api/usage?from=&to=&hours=
// 形态对齐 linguo 用量页：totals/series/credit_by_account/credit_by_model/buckets。
func (p *Panel) handleLinguoUsage(w http.ResponseWriter, r *http.Request) {
	if p.ledger == nil {
		writeError(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	from, to := linguoUsageWindow(r.URL.Query())
	hours := int(to.Sub(from).Hours())
	if hours < 1 {
		hours = 1
	}
	sum, err := p.ledger.Query("", from)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账本失败: "+err.Error())
		return
	}
	buckets := hours * 2
	if buckets < 24 {
		buckets = 24
	}
	if buckets > 96 {
		buckets = 96
	}
	series, err := p.ledger.BucketSeries("", from, buckets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取账本失败: "+err.Error())
		return
	}
	pts := make([]map[string]any, 0, len(series))
	for _, b := range series {
		tt := b.PromptTok + b.CompTok
		pts = append(pts, map[string]any{
			"t":                b.Time.Format(time.RFC3339),
			"prompt_tokens":    b.PromptTok,
			"completion_tokens": b.CompTok,
			"total_tokens":     tt,
			"requests":         b.Requests,
		})
	}
	totalTok := sum.PromptTokens + sum.CompletionTokens
	writeJSON(w, map[string]any{
		"totals": map[string]any{
			"total_tokens":      totalTok,
			"prompt_tokens":     sum.PromptTokens,
			"completion_tokens": sum.CompletionTokens,
			"requests":          sum.Requests,
			"errors":            0, // prime 账本未单独统计失败数
			"avg_latency_ms":    0,
			"credits":           sum.TotalCredit,
			"credit_tokens":     totalTok,
			"credit_samples":    sum.Requests,
		},
		"series":            pts,
		"credit_by_account": []any{},
		"credit_by_model":   []any{},
		"buckets":           len(pts),
		"window_from":       from.Format(time.RFC3339),
		"window_to":         to.Format(time.RFC3339),
	})
}

// handleLinguoRequestMetrics: GET /panel/api/request_metrics
// 形态对齐 linguo reqlog.Snapshot；prime 无内存指标环，用账本全量聚合近似。
func (p *Panel) handleLinguoRequestMetrics(w http.ResponseWriter, r *http.Request) {
	var completed int64
	if p.ledger != nil {
		if sum, err := p.ledger.Query("", time.Time{}); err == nil {
			completed = int64(sum.Requests)
		}
	}
	rate := 0.0
	if completed > 0 {
		rate = 1.0
	}
	writeJSON(w, map[string]any{
		"started_at":        p.startedAt.Format(time.RFC3339),
		"completed":         completed,
		"in_flight":         0,
		"succeeded":         completed,
		"failed":            0,
		"success_rate":      rate,
		"http_success_rate": rate,
		"avg_duration_ms":   0,
		"recent":            []any{},
		"archive":           map[string]any{"enabled": false},
	})
}

// handleLinguoRequestLogs: GET /panel/api/request_logs?limit=
// 运行日志视图已隐藏；仍按 linguo 信封返回账本最近记录，供可能的外部调用。
func (p *Panel) handleLinguoRequestLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	rows := []map[string]any{}
	if p.ledger != nil {
		if recs, err := p.ledger.Recent("", limit); err == nil {
			for _, rc := range recs {
				status := 200
				outcome := "ok"
				if !rc.OK {
					status = 500
					outcome = "error"
				}
				rows = append(rows, map[string]any{
					"time":             rc.Time.Format(time.RFC3339),
					"path":             "/v1/chat/completions",
					"account":          rc.AccountID,
					"model":            rc.Model,
					"status":           status,
					"ok":               rc.OK,
					"outcome":          outcome,
					"duration_ms":      rc.LatencyMs,
					"prompt_tokens":    rc.PromptTok,
					"completion_tokens": rc.CompTok,
					"total_tokens":     rc.PromptTok + rc.CompTok,
					"credit":           rc.Credit,
					"credit_known":     rc.Credit > 0,
				})
			}
		}
	}
	writeJSON(w, map[string]any{"entries": rows, "limit": limit})
}

// handleLinguoLoginStart: POST /panel/api/login/start {realm}
// 转调 device.go 的 startDeviceFlow；返回 linguo 形态 {ok,url,state,realm}。
func (p *Panel) handleLinguoLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body)
	realm := "cn"
	if body.Realm == "global" {
		realm = "global"
	}
	authURL, deviceCode, err := p.startDeviceFlow(realm)
	if err != nil {
		writeError(w, http.StatusBadGateway, "发起授权失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "url": authURL, "state": deviceCode, "realm": realm})
}

// handleLinguoLoginPoll: GET /panel/api/login/poll?state=
// 未完成 → {done:false,message}；完成 → {done:true,nickname,uid,realm,credits,credits_total}。
func (p *Panel) handleLinguoLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeError(w, http.StatusBadRequest, "missing state")
		return
	}
	done, acct, err := p.pollDeviceFlow(state)
	if err != nil {
		// 会话不存在/过期 → 404（linguo 口径：unknown or expired state）
		writeError(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	if !done {
		writeJSON(w, map[string]any{"done": false, "message": "waiting for login"})
		return
	}
	if berr := p.enrichNewAccount(acct); berr != "" {
		writeJSON(w, map[string]any{
			"done": true, "nickname": "", "uid": acct.UID,
			"realm": linguoRealm(acct.Region), "credits": -1,
			"balance_error": berr,
		})
		return
	}
	writeJSON(w, map[string]any{
		"done": true, "nickname": "", "uid": acct.UID,
		"realm": linguoRealm(acct.Region),
		"credits":       int64(acct.CreditTotal),
		"credits_total": int64(acct.CreditTotal),
	})
}

// cockpitAccount 是 import/cockpit 接受的单账号形态（linguo import.go）。
type cockpitAccount struct {
	ID          string `json:"id"`
	UID         string `json:"uid"`
	AccessToken string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Domain      string `json:"domain"`
	Nickname    string `json:"nickname"`
	Email       string `json:"email"`
	ExpiresAt   int64  `json:"expires_at"` // 毫秒时间戳（cockpit tools 口径）
}

// handleLinguoImportCockpit: POST /panel/api/import/cockpit（multipart，file 字段为 JSON 数组）
// 逐个转调 device.go 的 importToken；返回 {imported,skipped,errors}。
func (p *Panel) handleLinguoImportCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}
	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var imported, skipped int
	var errs []string
	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" {
			skipped++
			errs = append(errs, "missing required fields (id="+acc.ID+")")
			continue
		}
		region := "cn"
		if strings.Contains(strings.ToLower(acc.Domain), "workbuddy.ai") {
			region = "intl" // device.go 的 region 口径：cn/intl
		}
		if _, err := p.importToken(at, rt, uid, region); err != nil {
			skipped++
			errs = append(errs, "uid="+uid+": "+err.Error())
			continue
		}
		imported++
	}
	writeJSON(w, map[string]any{"imported": imported, "skipped": skipped, "errors": errs})
}

// handleLinguoKeepaliveAll: POST /panel/api/keepalive_all
// 转调 scheduler.TriggerNow("keepalive")。
func (p *Panel) handleLinguoKeepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusNotImplemented, "scheduler 未初始化")
		return
	}
	if err := p.sched.TriggerNow("keepalive"); err != nil {
		writeError(w, http.StatusBadRequest, "触发失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "started": true})
}
