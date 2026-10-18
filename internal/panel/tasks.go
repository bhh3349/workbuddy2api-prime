// Package panel 的任务中心接口：成长任务队列 + 单账号任务操作。
// 后端是我们的成长任务引擎（scheduler.GrowthEngine，移植自 linguo），
// 接口形状对齐我们面板前端（linguo 前端）的任务中心调用点。
//
//   - POST /panel/api/tasks/scan_all              → 扫描全部账号待办
//   - POST /panel/api/tasks/run_queue            → 启动执行队列
//   - GET  /panel/api/tasks/queue                → 队列进度（轮询）
//   - GET  /panel/api/accounts/{uid}/tasks        → 单账号任务列表
//   - POST /panel/api/accounts/{uid}/tasks/accept      {task_codes}
//   - POST /panel/api/accounts/{uid}/tasks/accept_all
//   - POST /panel/api/accounts/{uid}/tasks/claim      {task_code}
//   - POST /panel/api/accounts/{uid}/tasks/auto       {task_code}
//   - POST /panel/api/accounts/{uid}/tasks/auto_all
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/upstream"
)

// taskQueueItem 队列视图的单个条目（对齐前端 qrowHTML/grouopsFromQueue）。
type taskQueueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Status   string `json:"status"` // pending|running|done|error|skipped
	Message  string `json:"message,omitempty"`
}

// taskQueueSession 一次队列会话（扫描快照 + 执行状态）。
type taskQueueSession struct {
	seq     int64
	items   []taskQueueItem
	started time.Time
}

var (
	taskQueueMu      sync.Mutex
	taskQueueSeq     int64
	taskQueueSess *taskQueueSession
)

// nicknameOf 取账号昵称（没有则用 uid 前缀）。
func (p *Panel) nicknameOf(uid string) string {
	if p.auth == nil {
		return uid
	}
	accounts, _ := p.auth.LoadAll()
	for _, a := range accounts {
		if a != nil && a.UID == uid && a.Nickname != "" {
			return a.Nickname
		}
	}
	if len(uid) > 12 {
		return uid[:12]
	}
	return uid
}

// handleTasksScanAll 扫描全部账号待办（只读）。
func (p *Panel) handleTasksScanAll(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	pending, err := p.sched.GrowthScan(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "扫描失败: "+err.Error())
		return
	}
	type growthRow struct {
		TaskCode string `json:"task_code"`
		Title    string `json:"title"`
		Target   int64  `json:"target"`
		Current  int64  `json:"current"`
	}
	type acctRow struct {
		UID      string     `json:"uid"`
		Nickname string     `json:"nickname"`
		Growth   []growthRow `json:"growth"`
	}
	accounts := []acctRow{}
	for uid, tasks := range pending {
		if len(tasks) == 0 {
			continue
		}
		ar := acctRow{UID: uid, Nickname: p.nicknameOf(uid)}
		for _, t := range tasks {
			ar.Growth = append(ar.Growth, growthRow{
				TaskCode: t.TaskCode, Title: t.Title, Target: t.Target, Current: t.Current,
			})
		}
		accounts = append(accounts, ar)
	}
	writeJSON(w, map[string]any{"accounts": accounts})
}

// handleTasksRunQueue 启动执行队列：先扫描，再异步执行。
func (p *Panel) handleTasksRunQueue(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	pending, err := p.sched.GrowthScan(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "扫描失败: "+err.Error())
		return
	}
	items := []taskQueueItem{}
	for uid, tasks := range pending {
		for _, t := range tasks {
			items = append(items, taskQueueItem{
				UID: uid, Nickname: p.nicknameOf(uid),
				Kind: "growth", Code: t.TaskCode, Status: "pending",
			})
		}
	}
	if len(items) == 0 {
		writeJSON(w, map[string]any{"started": false, "message": "没有待办任务"})
		return
	}
	ok, started := p.sched.GrowthRunNow()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "成长任务引擎未注入")
		return
	}
	seq := atomic.AddInt64(&taskQueueSeq, 1)
	taskQueueMu.Lock()
	taskQueueSess = &taskQueueSession{seq: seq, items: items, started: time.Now()}
	taskQueueMu.Unlock()
	writeJSON(w, map[string]any{"started": started, "seq": seq, "total": len(items)})
}

// handleTasksQueue 队列进度查询（前端 3 秒轮询）。
func (p *Panel) handleTasksQueue(w http.ResponseWriter, r *http.Request) {
	taskQueueMu.Lock()
	sess := taskQueueSess
	taskQueueMu.Unlock()
	if sess == nil {
		writeJSON(w, map[string]any{"started": false})
		return
	}
	prog := p.sched.GrowthProgress()
	results := p.sched.GrowthResults()
	// 结果索引：uid/code → 状态
	resIdx := map[string]scheduler.GrowthItemResult{}
	for _, res := range results {
		resIdx[res.UID+"/"+res.Code] = res
	}
	items := make([]taskQueueItem, len(sess.items))
	copy(items, sess.items)
	for i, it := range items {
		if res, ok := resIdx[it.UID+"/"+it.Code]; ok {
			items[i].Status = res.Status
			items[i].Message = res.Message
			continue
		}
		if prog.Running && prog.CurrentUID == it.UID && prog.Current == it.Code {
			items[i].Status = "running"
		}
	}
	writeJSON(w, map[string]any{
		"started": true,
		"seq":     sess.seq,
		"running": prog.Running,
		"items":   items,
	})
}

// taskView 单账号任务列表的对外形态（对齐前端 loadTasks）。
type taskView struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title"`
	Current      int64  `json:"current"`
	Target       int64  `json:"target"`
	Credit       int64  `json:"credit"`
	Energy       int64  `json:"energy"`
	RewardBuddy  bool   `json:"reward_buddy"`
	Claimed      bool   `json:"claimed"`
	Claimable    bool   `json:"claimable"`
	Locked       bool   `json:"locked"`
	AcceptStatus string `json:"accept_status"`
	TaskDesc     string `json:"task_desc"`
	Description  string `json:"description"`
	Tag          string `json:"tag"`
	JumpURL      string `json:"jump_url"`
}

func toTaskView(t upstream.Task) taskView {
	return taskView{
		TaskCode: t.TaskCode, Title: t.Title, Current: t.Current, Target: t.Target,
		Credit: t.Credit, Energy: t.Energy, RewardBuddy: t.RewardBuddy,
		Claimed: t.Claimed, Claimable: t.Claimable, Locked: t.Locked,
		AcceptStatus: t.AcceptStatus, TaskDesc: t.TaskDesc,
		Description: t.Description, Tag: t.Tag, JumpURL: t.JumpURL,
	}
}

// handleAccountTasks 单账号任务列表。
func (p *Panel) handleAccountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	tasks, err := p.sched.GrowthAccountTasks(uid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, toTaskView(t))
	}
	writeJSON(w, map[string]any{"tasks": views})
}

// handleAccountTasksAccept 接受指定任务。
func (p *Panel) handleAccountTasksAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeError(w, http.StatusBadRequest, "task_codes 不能为空")
		return
	}
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	if err := p.sched.GrowthAccept(uid, body.TaskCodes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleAccountTasksAcceptAll 接受该账号全部待接受任务。
func (p *Panel) handleAccountTasksAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	n, err := p.sched.GrowthAcceptAll(uid)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"accepted": n})
}

// handleAccountTasksClaim 领取指定任务奖励。
func (p *Panel) handleAccountTasksClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeError(w, http.StatusBadRequest, "task_code 不能为空")
		return
	}
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	credit, energy, err := p.sched.GrowthClaim(uid, body.TaskCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "credit": credit, "energy": energy})
}

// handleAccountTasksAuto 一键完成单个任务。
func (p *Panel) handleAccountTasksAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeError(w, http.StatusBadRequest, "task_code 不能为空")
		return
	}
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	res, err := p.sched.GrowthRunSingle(uid, body.TaskCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"skipped": res.Skipped, "message": res.Message,
		"progress_before": res.ProgressBefore, "progress_after": res.ProgressAfter,
		"claimed": res.Claimed, "claimable": res.Claimable,
		"attempt": true,
	})
}

// handleAccountTasksAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) handleAccountTasksAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	results := p.sched.GrowthRunAllSingles(uid)
	writeJSON(w, map[string]any{"results": results})
}
