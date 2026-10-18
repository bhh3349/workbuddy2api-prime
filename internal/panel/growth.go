// Package panel 的成长任务接口：任务中心后端 API（只改后端，前端由另一 agent 负责）。
//
//   - GET  /panel/api/growth/overview → 各账号任务状态（只读，不执行）
//   - POST /panel/api/growth/scan     → 触发全量扫描（只读）
//   - POST /panel/api/growth/run      → 异步执行自动完成队列
//   - GET  /panel/api/growth/queue    → 队列进度查询
package panel

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/upstream"
)

// growthTaskView 单个任务的对外视图。
type growthTaskView struct {
	Code     string `json:"code"`
	Title    string `json:"title"`
	Status   string `json:"status"`   // pending / claimable / claimed / locked / done
	Progress string `json:"progress"` // "3/5" / "claimed" 等
	Credit   int64  `json:"credit"`
	Energy   int64  `json:"energy"`
	Auto     bool   `json:"auto"` // 是否有自动完成动作
}

// growthAccountView 单个账号的任务视图。
type growthAccountView struct {
	UID      string           `json:"uid"`
	Nickname string           `json:"nickname"`
	Realm    string           `json:"realm"`
	Tasks    []growthTaskView `json:"tasks"`
	Pending  int              `json:"pending"`
	Done     int              `json:"done"`
}

// growthTaskStatus 推算任务展示状态。
func growthTaskStatus(t upstream.Task) string {
	switch {
	case t.Claimed:
		return "claimed"
	case t.Locked:
		return "locked"
	case t.Claimable:
		return "claimable"
	case t.Target > 0 && t.Current >= t.Target:
		return "done"
	default:
		return "pending"
	}
}

// handleGrowthOverview 只读拉取各账号任务状态（不执行任何动作）。
func (p *Panel) handleGrowthOverview(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil || p.auth == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	all, err := p.sched.GrowthTasks(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "拉取任务失败: "+err.Error())
		return
	}
	accounts, _ := p.auth.LoadAll()
	meta := map[string]*struct{ nickname, realm string }{}
	for _, a := range accounts {
		if a == nil {
			continue
		}
		meta[a.UID] = &struct{ nickname, realm string }{a.Nickname, a.Region}
	}
	views := []growthAccountView{}
	for uid, tasks := range all {
		v := growthAccountView{UID: uid, Tasks: []growthTaskView{}}
		if m, ok := meta[uid]; ok {
			v.Nickname, v.Realm = m.nickname, m.realm
		}
		for _, t := range tasks {
			st := growthTaskStatus(t)
			prog := ""
			if t.Target > 0 {
				prog = strconv.FormatInt(t.Current, 10) + "/" + strconv.FormatInt(t.Target, 10)
			} else {
				prog = st
			}
			v.Tasks = append(v.Tasks, growthTaskView{
				Code: t.TaskCode, Title: t.Title, Status: st, Progress: prog,
				Credit: t.Credit, Energy: t.Energy, Auto: scheduler.GrowthHasAutoAction(t.TaskCode),
			})
			switch st {
			case "pending", "claimable":
				v.Pending++
			case "claimed", "done":
				v.Done++
			}
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"accounts": views})
}

// handleGrowthScan 触发全量扫描（只读），返回各账号待办数。
func (p *Panel) handleGrowthScan(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	pending, err := p.sched.GrowthScan(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "扫描失败: "+err.Error())
		return
	}
	total := 0
	for _, tasks := range pending {
		total += len(tasks)
	}
	writeJSON(w, map[string]any{
		"ok": true, "accounts_scanned": len(pending), "total_pending": total,
	})
}

// handleGrowthRun 异步执行自动完成队列。
func (p *Panel) handleGrowthRun(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	ok, started := p.sched.GrowthRunNow()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "成长任务引擎未注入")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "started": started})
}

// handleGrowthQueue 查询队列进度。
func (p *Panel) handleGrowthQueue(w http.ResponseWriter, r *http.Request) {
	if p.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduler 未初始化")
		return
	}
	writeJSON(w, p.sched.GrowthProgress())
}
